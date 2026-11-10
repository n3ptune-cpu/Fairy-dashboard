package web

import (
	"fmt"
	"math"
	"net/url"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

func (c *reqCtx) deny(u *model.User, status int, detail any, why string) error {
	c.audit(u, "auth.denied", c.r.URL.Path, map[string]any{"method": c.r.Method}, fmt.Sprintf("%d: %s", status, why))
	return apierr.New(status, detail)
}

// checkCSRF: browsers need the custom header (forces a CORS preflight; no CORS is enabled) + Origin check.
// API tokens are not sent automatically by browsers, so they need neither.
func (c *reqCtx) checkCSRF(u *model.User) error {
	if u.Auth == "token" || c.resumed() { // resumed: the panel's own replay of a request that passed these checks
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
	if _, g, _ := c.grant(); !g.Admin() {
		d := "only admins can do this (an admin can change your role on the Users page)"
		if u.Auth == "token" {
			d = "this API token has read scope"
			if model.Deref(u.TokenScope) == "control" {
				d = "this API token's owner is not an admin"
			}
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
	if err := c.stepUp(u, "control actions"); err != nil {
		return nil, err
	}
	if !bucket.allow(u.Sub) {
		return nil, apierr.New(429, "too many control requests; slow down")
	}
	return u, nil
}

// stepUp: sensitive actions need a Pocket ID sign-in from the last step_up_window_s (API tokens: no check).
func (c *reqCtx) stepUp(u *model.User, what string) error {
	cfg := c.s.Cfg
	return c.stepUpWithin(u, what, cfg.StepUpWindowS)
}

// stepUpWithin is stepUp with its own window (the panel's own update wants a sign-in from the last 2 minutes).
// A refused browser request is kept and runs by itself right after the sign-in (pending.go).
func (c *reqCtx) stepUpWithin(u *model.User, what string, window int) error {
	if age := c.authAge(u); age != nil && *age > float64(window) {
		c.audit(u, "auth.step_up", c.r.URL.Path, map[string]any{"auth_age_s": math.Round(*age)}, "required")
		id := c.keepPending(u)
		msg := fmt.Sprintf("Confirm it's you: %s need a Pocket ID sign-in from the last %d min.", what, max(1, window/60))
		if window < 60 || window%60 != 0 {
			msg = fmt.Sprintf("Confirm it's you: %s need a Pocket ID sign-in from the last %d seconds.", what, window)
		}
		detail := map[string]any{"code": "step_up_required", "window_s": window, "pending": id != "", "message": msg}
		if id != "" {
			detail["pending_id"] = id // names the kept action in /auth/login?step_up=1&pending=<id>
		}
		return apierr.New(401, detail)
	}
	return nil
}

// adminGuard protects the Users page's changes: browser session only (no API tokens), CSRF, admin role,
// step-up within the same window as control actions, and a rate limit. The control kill switch does not apply:
// it stops device control, not the management of who may sign in.
func (c *reqCtx) adminGuard() (*model.User, error) {
	u, err := c.sessionCSRF()
	if err != nil {
		return nil, err
	}
	if _, g, _ := c.grant(); !g.Admin() {
		return nil, c.deny(u, 403, "only admins can manage users and sites", "not an admin")
	}
	if err := c.stepUp(u, "changes to users and sites"); err != nil {
		return nil, err
	}
	if !c.s.adminRL.allow(u.Sub) {
		return nil, apierr.New(429, "too many changes; slow down")
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
