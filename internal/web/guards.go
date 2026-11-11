package web

import (
	"fmt"
	"math"
	"net/http"
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
	if c.r.Method != http.MethodGet && c.r.Method != http.MethodHead {
		c.use = true // a write only the panel's own page can send: it counts as use once it is answered without an error
	}
	return nil
}

// sessionOnlyControl lists the control actions (by the route pattern they are registered under) that an API
// token may not run at all. A token cannot "confirm it's you": it skips the step-up, so for a token every other
// control action is one request away. These are the ones that change what the panel trusts or switch off a
// machine's protection: stop / start of the elevated helper (its fan watchdog), Add host, Re-pair, Remove, and
// cancelling an Add host or Re-pair that is waiting (NEW-3: it undoes what an admin just confirmed).
// They need a browser session, which then goes through the step-up like everything else.
var sessionOnlyControl = map[string]bool{
	"POST /api/systems/{id}/helper/stop":  true,
	"POST /api/systems/{id}/helper/start": true,
	"POST /api/admin/hosts":               true,
	"POST /api/admin/hosts/{id}/repair":   true,
	"DELETE /api/admin/hosts/{id}":        true,
	"DELETE /api/admin/enrollments/{id}":  true,
	// "Update machines that are off": a run works for many minutes after the one request that starts it, and
	// which machines it may touch is decided by the per-host switch. Starting, cancelling and that switch need
	// a person who can confirm.
	"POST /api/admin/wake-update":        true,
	"DELETE /api/admin/wake-update/{id}": true,
	"PUT /api/systems/{id}/wake-update":  true,
}

// controlGuard: CSRF, control group / control-scope token, admin kill switch, step-up, rate limit. API tokens
// are refused outright on the routes in sessionOnlyControl.
func (c *reqCtx) controlGuard(bucket *rateLimiter) (*model.User, error) {
	return c.guardControl(bucket, true)
}

// stopGuard is controlGuard for an action that only stops something the panel itself is doing (cancelling an
// Add host that waits): everything controlGuard checks, except that the kill switch does not refuse it. The
// kill switch stops device control; it must not keep an admin from calling a pairing off.
func (c *reqCtx) stopGuard(bucket *rateLimiter) (*model.User, error) {
	return c.guardControl(bucket, false)
}

func (c *reqCtx) guardControl(bucket *rateLimiter, killSwitch bool) (*model.User, error) {
	u, err := c.controlAllowed(killSwitch)
	if err != nil {
		return nil, err
	}
	if err := c.stepUp(u, "control actions"); err != nil {
		return nil, err
	}
	if !bucket.allow(u.Sub) {
		return nil, apierr.New(429, "too many control requests; slow down")
	}
	return u, nil
}

// controlAllowed is the part of guardControl that comes before "Confirm it's you": who is asking (no API token
// on a sign-in-only route), CSRF, the admin role, and the kill switch. A handler that has something to check
// between this and the confirmation calls it by itself and then stepUp or stepUpEach (the start of an "Update
// machines that are off" run: the list is checked before anybody is asked for a passkey).
func (c *reqCtx) controlAllowed(killSwitch bool) (*model.User, error) {
	u, err := c.requireUser()
	if err != nil {
		return nil, err
	}
	if u.Auth == "token" && sessionOnlyControl[c.r.Pattern] {
		return nil, c.deny(u, 403, "this action needs a browser sign-in: API tokens cannot confirm it's you",
			"API token on an action that needs a browser sign-in")
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
	if off := c.s.Poller.Admin.ControlDisabled(); off != nil && killSwitch {
		reason := off.Reason
		if reason == "" {
			reason = "no reason given"
		}
		return nil, c.deny(u, 423, fmt.Sprintf("control is disabled by an administrator (%s): %s", off.By, reason),
			"control disabled (kill switch)")
	}
	return u, nil
}

// stepUp: sensitive actions need a Pocket ID sign-in from the last step_up_window_s (API tokens: no check).
func (c *reqCtx) stepUp(u *model.User, what string) error {
	cfg := c.s.Cfg
	return c.stepUpWithin(u, what, cfg.StepUpWindowS)
}

// perActionGrace is how old the confirming sign-in may be when the panel replays the one kept action in
// step_up_per_action mode (the callback accepts an auth_time of up to two minutes ago).
const perActionGrace = 150

// stepUpWithin is stepUp with its own window (the panel's own update wants a sign-in from the last 2 minutes).
// A refused browser request is kept and runs by itself right after the sign-in (pending.go).
//
// The sign-in counts only for the client that confirmed it (authAge). With step_up_per_action: true nothing is
// inherited at all: a confirmation authorises exactly the one kept action, which the panel replays itself right
// after the sign-in, and every other request (also from the same browser, a second later) is asked again.
func (c *reqCtx) stepUpWithin(u *model.User, what string, window int) error {
	return c.stepUpCheck(u, what, window, c.s.Cfg.StepUpPerAction)
}

// stepUpEach is stepUp for an action that needs its own confirmation every time, whatever step_up_per_action
// says: the request is refused, kept, and runs only as the panel's own replay right after the sign-in that was
// made for it. A confirmation made for another action does not count, also not inside its window. Used for
// ending other sessions (api_sessions.go): what a copied cookie would do first, and the one thing that must
// not ride on a confirmation the owner gave for something else a minute ago.
func (c *reqCtx) stepUpEach(u *model.User, what string) error {
	return c.stepUpCheck(u, what, c.s.Cfg.StepUpWindowS, true)
}

// stepUpCheck is the check behind stepUp, stepUpWithin and stepUpEach. perAction: nothing is inherited from an
// earlier confirmation (see stepUpWithin).
func (c *reqCtx) stepUpCheck(u *model.User, what string, window int, perAction bool) error {
	age := c.authAge(u)
	if age == nil {
		return nil
	}
	if perAction {
		if c.resumed() && *age <= float64(max(window, perActionGrace)) {
			return nil
		}
	} else if *age <= float64(window) {
		return nil
	}
	if c.bodyKept {
		// A body with more than one reading is refused here, before the request is described, kept and offered
		// for "Confirm it's you" (T-30, strictStepUpBody). Inside the window decodeBody refuses it.
		if err := c.strictStepUpBody(c.body); err != nil {
			return err
		}
	}
	c.audit(u, stepUpAction, c.r.URL.Path, map[string]any{"auth_age_s": math.Round(*age)}, stepUpAsked)
	kept, why := c.keepPending(u)
	msg := fmt.Sprintf("Confirm it's you: %s need a Pocket ID sign-in from the last %d min.", what, max(1, window/60))
	if window < 60 || window%60 != 0 {
		msg = fmt.Sprintf("Confirm it's you: %s need a Pocket ID sign-in from the last %d seconds.", what, window)
	}
	if perAction {
		msg = fmt.Sprintf("Confirm it's you: %s need their own Pocket ID sign-in, every time.", what)
	}
	// auth_age_s: how old the sign-in is right now, as this check sees it (for the dialog's "last sign-in" line;
	// /api/me's copy in the page is from when the page loaded). Very large = unknown for this client.
	detail := map[string]any{"code": "step_up_required", "window_s": window, "pending": kept != nil, "message": msg,
		"auth_age_s": math.Round(*age)}
	if kept != nil {
		// pending_id names the kept action when the dialog asks to confirm it (POST /api/me/step-up, in the body);
		// action is the panel's own description of what that confirmation will run.
		detail["pending_id"], detail["action"] = kept.ID, kept.Action
		if len(kept.Items) > 0 { // what it will do, line by line (the machines of a run)
			detail["items"] = kept.Items
		}
	} else if why != "" {
		detail["not_kept"] = why
	}
	if perAction {
		detail["per_action"] = true
	}
	return apierr.New(401, detail)
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
