package web

import (
	"fmt"
	"math"
	"net/url"

	"github.com/n3ptune-cpu/Fairy/internal/apierr"
	"github.com/n3ptune-cpu/Fairy/internal/model"
)

func (c *reqCtx) deny(u *model.User, status int, detail any, why string) error {
	c.audit(u, "auth.denied", c.r.URL.Path, map[string]any{"method": c.r.Method}, fmt.Sprintf("%d: %s", status, why))
	return apierr.New(status, detail)
}

// checkCSRF: browsers need the custom header (forces a CORS preflight; no CORS is enabled) + Origin check.
// API tokens are not sent automatically by browsers, so they need neither.
func (c *reqCtx) checkCSRF(u *model.User) error {
	if u.Auth == "token" {
		return nil
	}
	if c.r.Header.Get(CSRFHeader) != CSRFValue {
		return c.deny(u, 403, "missing "+CSRFHeader+": "+CSRFValue+" header", "missing CSRF header")
	}
	if o := c.r.Header.Get("Origin"); o != "" {
		ou, err1 := url.Parse(o)
		bu, err2 := url.Parse(c.s.Cfg.BaseURL())
		if err1 != nil || err2 != nil || ou.Scheme != bu.Scheme || ou.Host != bu.Host {
			return c.deny(u, 403, "cross-origin request refused", "cross-origin ("+o+")")
		}
	}
	if sfs := c.r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" {
		return c.deny(u, 403, "cross-site request refused", "cross-site")
	}
	return nil
}

// controlGuard: CSRF, control group / control-scope token, admin kill switch, step-up, rate limit.
func (c *reqCtx) controlGuard(bucket *rateLimiter) (*model.User, error) {
	u, err := c.requireUser()
	if err != nil {
		return nil, err
	}
	if err := c.checkCSRF(u); err != nil {
		return nil, err
	}
	cfg := c.s.Cfg
	if !canControl(u, cfg.OIDC) {
		d := "not in a control group"
		if u.Auth == "token" {
			d = "this API token has read scope"
		}
		return nil, c.deny(u, 403, d, "not allowed to control")
	}
	if off := c.s.Poller.Admin.ControlDisabled(); off != nil {
		reason := off.Reason
		if reason == "" {
			reason = "no reason given"
		}
		return nil, c.deny(u, 423, fmt.Sprintf("control is disabled by an administrator (%s): %s", off.By, reason),
			"control disabled (kill switch)")
	}
	if age := c.authAge(u); age != nil && *age > float64(cfg.StepUpWindowS) {
		c.audit(u, "auth.step_up", c.r.URL.Path, map[string]any{"auth_age_s": math.Round(*age)}, "required")
		return nil, apierr.New(401, map[string]any{"code": "step_up_required", "window_s": cfg.StepUpWindowS,
			"message": fmt.Sprintf("Confirm it's you: control actions need a Pocket ID sign-in from the last %d min.", cfg.StepUpWindowS/60)})
	}
	if !bucket.allow(u.Sub) {
		return nil, apierr.New(429, "too many control requests; slow down")
	}
	return u, nil
}

func (c *reqCtx) sessionCSRF() (*model.User, error) {
	u, err := c.requireSession()
	if err != nil {
		return nil, err
	}
	return u, c.checkCSRF(u)
}
