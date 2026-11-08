package web

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/n3ptune-cpu/Fairy/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy/internal/apierr"
	"github.com/n3ptune-cpu/Fairy/internal/model"
)

// sessionUser: browser session (cookie) only. Valid while listed in sessions.json (not revoked by the CLI),
// younger than session_max_age, and used within session_idle_timeout (activity recorded at most once a minute).
func (c *reqCtx) sessionUser() *model.User {
	if c.s.DemoUser != nil {
		u := *c.s.DemoUser
		return &u
	}
	s := c.session()
	now := time.Now().Unix()
	if s.User == nil || s.IAT == 0 || now-s.IAT > int64(c.s.Cfg.SessionMaxAge) {
		return nil
	}
	admin := c.s.Poller.Admin
	if !admin.SessionValid(s.SID, float64(s.IAT)) {
		c.clearSession()
		return nil
	}
	seen := s.Seen
	if seen == 0 {
		seen = s.IAT
	}
	if now-seen > int64(c.s.Cfg.SessionIdleTimeout) {
		_ = admin.RemoveSession(s.SID)
		c.clearSession()
		return nil
	}
	if c.s.Access.Err() == nil {
		if _, known := c.s.Access.Load().Users[s.User.Sub]; !known {
			// Removed from users.json, or a cookie from before users.json existed: sign in again. Roles are only
			// ever bootstrapped at a fresh sign-in, never from the groups cached in a (possibly days old) cookie.
			_ = admin.RemoveSession(s.SID)
			c.clearSession()
			return nil
		}
	}
	if now-seen > 60 {
		s.Seen = now
		c.dirty = true
	}
	u := *s.User
	u.Auth = "session"
	if u.Groups == nil {
		u.Groups = []string{}
	}
	return &u
}

// tokenUser: `Authorization: Bearer fy_...` personal API token (CLI-created).
func (c *reqCtx) tokenUser() (*model.User, error) {
	scheme, raw, _ := strings.Cut(c.r.Header.Get("Authorization"), " ")
	raw = strings.TrimSpace(raw)
	if !strings.EqualFold(scheme, "bearer") || raw == "" {
		return nil, nil
	}
	rec := c.s.Tokens.Verify(raw)
	if rec == nil {
		c.auditAnon("auth.token_rejected", "denied: unknown, revoked or expired token", "?", nil, nil)
		return nil, apierr.New(401, "invalid API token")
	}
	c.s.Tokens.Touch(rec)
	c.token = rec
	return &model.User{Sub: "token:" + rec.ID, Name: &rec.Name, Groups: []string{}, Auth: "token",
		TokenName: &rec.Name, TokenScope: &rec.Scope, TokenOwner: &rec.Owner}, nil
}

// requireUser: a session cookie or a personal API token. 401 JSON, never a redirect.
func (c *reqCtx) requireUser() (*model.User, error) {
	if c.user != nil {
		return c.user, nil
	}
	var u *model.User
	if c.r.Header.Get("Authorization") != "" {
		var err error
		if u, err = c.tokenUser(); err != nil {
			return nil, err
		}
	}
	if u == nil {
		u = c.sessionUser()
	}
	if u == nil {
		return nil, apierr.New(401, "not authenticated")
	}
	c.user = u
	return u, nil
}

// requireSession: session-only endpoints (UI settings): API tokens are refused here.
func (c *reqCtx) requireSession() (*model.User, error) {
	if c.r.Header.Get("Authorization") != "" {
		return nil, apierr.New(403, "API tokens are not accepted on this endpoint")
	}
	u := c.sessionUser()
	if u == nil {
		return nil, apierr.New(401, "not authenticated")
	}
	c.user = u
	return u, nil
}

// authAge: seconds since the user last authenticated at Pocket ID (nil for API tokens). A session without an
// auth_time from the identity provider counts as authenticated long ago (fail closed): control actions then
// need a step-up, and the step-up itself must return auth_time.
func (c *reqCtx) authAge(u *model.User) *float64 {
	switch u.Auth {
	case "token":
		return nil
	case "demo":
		c.s.demoMu.Lock()
		defer c.s.demoMu.Unlock()
		v := time.Since(c.s.demoAuthTime).Seconds()
		return &v
	}
	v := float64(time.Now().UnixNano())/1e9 - c.session().AuthTime // AuthTime 0 = unknown: very old
	return &v
}

// loginKey is the login rate limiter's bucket: the signed-in user for a request that carries a valid session
// (a step-up or re-login), else the client IP. So clients sharing an address with someone (or flooding
// /auth/login) can never stop a signed-in admin from confirming a control action.
func (c *reqCtx) loginKey() string {
	s := c.session()
	if s.User != nil && s.SID != "" && c.s.Poller.Admin.SessionValid(s.SID, float64(s.IAT)) &&
		time.Now().Unix()-s.IAT <= int64(c.s.Cfg.SessionMaxAge) {
		return "sub:" + s.User.Sub
	}
	return "ip:" + c.ip()
}

func (s *Server) authRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /auth/login", s.h(s.authLogin))
	m.HandleFunc("GET /auth/callback", s.h(s.authCallback))
	m.HandleFunc("POST /auth/logout", s.h(s.authLogout))
}

func (s *Server) authLogin(c *reqCtx) error {
	if c.r.Header.Get("Authorization") != "" {
		return apierr.New(403, "API tokens are not accepted on /auth")
	}
	if !s.login.allow(c.loginKey()) {
		c.auditAnon("auth.login", "denied: rate limited", "", nil, nil)
		return apierr.New(429, "too many login attempts; try again in a minute")
	}
	q := c.r.URL.Query()
	target := safeNext(q.Get("next"))
	stepUp := q.Get("step_up") == "1"
	if s.DemoUser != nil { // demo: simulate the Pocket ID round trip for the step-up screen
		s.demoMu.Lock()
		s.demoAuthTime = time.Now()
		s.demoMu.Unlock()
		d := s.DemoUser
		c.auditAnon("auth.step_up", "ok (demo: simulated Pocket ID re-authentication)", model.Deref(d.Email), &d.Sub, nil)
		c.redirect(target)
		return nil
	}
	// A valid session is kept until the round trip completes (the callback replaces it with a new session id),
	// so another site cannot sign someone out by sending them to /auth/login. Anything else is cleared.
	current := c.sessionUser()
	if current == nil {
		c.clearSession() // fresh session: avoids fixation of a pre-login session
	}
	p, err := s.oidc.provider(c.r.Context())
	if err != nil {
		slog.Error("OIDC discovery failed", "error", err)
		return apierr.New(502, "identity provider unreachable")
	}
	sess := c.session()
	sess.Next, sess.StepUp = target, stepUp && current != nil
	sess.State, sess.Nonce, sess.Verifier, sess.Started = randToken(24), randToken(24), oauth2.GenerateVerifier(), time.Now().Unix()
	c.dirty = true
	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(sess.Verifier), oidc.Nonce(sess.Nonce)}
	if sess.StepUp { // step-up: force a fresh authentication at Pocket ID
		opts = append(opts, oauth2.SetAuthURLParam("max_age", "0"))
		if s.Cfg.StepUpPromptLogin {
			opts = append(opts, oauth2.SetAuthURLParam("prompt", "login"))
		}
		c.auditAnon("auth.step_up", "redirected to Pocket ID", actorOf(current), &current.Sub, nil)
	}
	c.redirect(s.oidc.oauth(p).AuthCodeURL(sess.State, opts...))
	return nil
}

// authCallback finishes the Pocket ID round trip: it checks state, exchanges the code (PKCE), verifies the ID
// token and nonce, applies the allowed groups / emails and either completes a step-up re-authentication or
// starts a new, server-registered session. Every failure is audited and answered with a generic 400.
func (s *Server) authCallback(c *reqCtx) error {
	if !s.login.allow(c.loginKey()) {
		c.auditAnon("auth.login", "denied: rate limited", "", nil, nil)
		return apierr.New(429, "too many login attempts; try again in a minute")
	}
	sess := c.session()
	q := c.r.URL.Query()
	fail := func(why string) error {
		slog.Warn("OIDC callback failed", "reason", why)
		c.auditAnon("auth.login", "failed: "+why, "", nil, nil)
		return apierr.New(400, "login failed")
	}
	if e := q.Get("error"); e != "" {
		return fail(truncate(e, 60))
	}
	if sess.State == "" || q.Get("state") != sess.State || time.Now().Unix()-sess.Started > 600 {
		return fail("mismatching_state")
	}
	p, err := s.oidc.provider(c.r.Context())
	if err != nil {
		return apierr.New(502, "identity provider unreachable")
	}
	ctx, cancel := context.WithTimeout(s.oidc.ctx(c.r.Context()), 15*time.Second)
	defer cancel()
	claims, why := s.verifiedClaims(ctx, p, q.Get("code"), sess)
	if why != "" {
		return fail(why)
	}
	u := userFromClaims(claims, s.Cfg.OIDC.GroupsClaim)
	if !isAllowed(u, s.Cfg.OIDC) {
		slog.Warn("login denied", "sub", u.Sub)
		c.auditAnon("auth.login", "denied: not in an allowed group", actorOf(&u), &u.Sub, nil)
		c.clearSession()
		return apierr.New(403, "not allowed")
	}
	now := float64(time.Now().UnixNano()) / 1e9
	authTime := 0.0 // unknown: counts as long ago (never as "just now")
	if v, ok := claims["auth_time"].(float64); ok && v > 0 {
		authTime = v
	}
	target := safeNext(sess.Next)
	if sess.StepUp && authTime == 0 {
		slog.Error("step-up failed: the identity provider sent no auth_time claim (OIDC Core requires it when " +
			"max_age is requested); control actions cannot be confirmed until it does")
		c.auditAnon("auth.step_up", "failed: identity provider sent no auth_time", actorOf(&u), &u.Sub, nil)
		return c.errorPage(400, "Could not confirm it's you",
			"Pocket ID did not say when you signed in (the ID token has no auth_time), so Fairy cannot tell that you "+
				"just signed in again. Control actions stay locked. An administrator must make the identity provider "+
				"send auth_time (it is required when Fairy asks for a fresh sign-in with max_age=0).", target)
	}
	if sess.StepUp && now-authTime > 120 {
		c.auditAnon("auth.step_up", "failed: identity provider did not re-authenticate (auth_time too old)", actorOf(&u), &u.Sub, nil)
		return c.errorPage(400, "Could not confirm it's you",
			"Pocket ID did not ask you to sign in again (your last sign-in there is more than two minutes old). "+
				"Try again; if this keeps happening, an administrator can set step_up_prompt_login: true in Fairy's config.", target)
	}
	if sess.StepUp && sess.User != nil && sess.User.Sub == u.Sub && sess.SID != "" {
		// Step-up for the same user: keep the session, record the fresh authentication time.
		sess.AuthTime, sess.Seen = authTime, int64(now)
		sess.StepUp, sess.Next, sess.State, sess.Nonce, sess.Verifier, sess.Started = false, "", "", "", "", 0
		c.dirty = true
		c.auditAnon("auth.step_up", "ok", actorOf(&u), &u.Sub, nil)
		c.redirect(target)
		return nil
	}
	if err := s.startSession(c, sess.SID, u, now, authTime); err != nil {
		return err
	}
	c.redirect(target)
	return nil
}

// verifiedClaims exchanges the authorization code (with the PKCE verifier), verifies the ID token and its nonce
// and returns the claims, merged with userinfo when the groups claim is missing from the ID token. On failure it
// returns a short reason for the log and audit trail instead (never shown to the client).
func (s *Server) verifiedClaims(ctx context.Context, p *oidc.Provider, code string, sess *sessionData) (map[string]any, string) {
	tok, err := s.oidc.oauth(p).Exchange(ctx, code, oauth2.VerifierOption(sess.Verifier))
	if err != nil {
		return nil, "token exchange"
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		return nil, "no id_token"
	}
	idt, err := p.Verifier(&oidc.Config{ClientID: s.Cfg.OIDC.ClientID}).Verify(ctx, rawID)
	if err != nil {
		return nil, "invalid id_token"
	}
	if idt.Nonce != sess.Nonce {
		return nil, "nonce mismatch"
	}
	claims := map[string]any{}
	if err := idt.Claims(&claims); err != nil {
		return nil, "invalid claims"
	}
	if _, ok := claims[s.Cfg.OIDC.GroupsClaim]; !ok && slices.Contains(s.Cfg.OIDC.Scopes, "groups") {
		mergeUserInfo(ctx, p, tok, idt.Subject, claims)
	}
	if idt.Subject == "" {
		return nil, "no sub claim"
	}
	claims["sub"] = idt.Subject
	return claims, ""
}

// mergeUserInfo adds the userinfo endpoint's claims (some providers only put groups there), but only when
// userinfo is about the same subject as the ID token.
func mergeUserInfo(ctx context.Context, p *oidc.Provider, tok *oauth2.Token, subject string, claims map[string]any) {
	ui, err := p.UserInfo(ctx, oauth2.StaticTokenSource(tok))
	if err != nil {
		slog.Warn("OIDC userinfo request failed")
		return
	}
	extra := map[string]any{}
	if ui.Claims(&extra) == nil && fmt.Sprint(extra["sub"]) == subject {
		for k, v := range extra {
			claims[k] = v
		}
	}
}

// startSession replaces the previous session (oldSID) with a fresh one: a new random id registered in
// sessions.json (so the CLI can revoke it) and a signed cookie holding the user.
func (s *Server) startSession(c *reqCtx, oldSID string, u model.User, now, authTime float64) error {
	admin := s.Poller.Admin
	_ = admin.RemoveSession(oldSID)
	sid := randToken(18)
	if err := admin.AddSession(sid, adminstate.NewSession{Sub: u.Sub, Name: u.Name, Email: u.Email, IP: c.ip()}); err != nil {
		slog.Error("sign-in failed: cannot register the session in sessions.json (disk full? wrong owner?)", "error", err)
		c.auditAnon("auth.login", "failed: sessions.json not writable", actorOf(&u), &u.Sub, nil)
		return apierr.New(503, "sign-in could not be recorded (the panel cannot write its session file); see the server log")
	}
	su := u
	su.Auth = "session"
	// a real sign-in: a new user may be bootstrapped (control_groups / bootstrap_admins)
	if _, err := s.recordUser(c, &su, true); err != nil {
		_ = admin.RemoveSession(sid)
		c.clearSession()
		c.auditAnon("auth.login", "failed: users.json not writable", actorOf(&u), &u.Sub, nil)
		return apierr.New(503, "sign-in could not be recorded (the panel cannot write its users file); see the server log")
	}
	c.sess = &sessionData{SID: sid, User: &u, IAT: int64(now), Seen: int64(now), AuthTime: authTime}
	c.dirty = true
	slog.Info("login ok", "sub", u.Sub)
	c.auditAnon("auth.login", "ok", actorOf(&u), &u.Sub, nil)
	return nil
}

// authLogout needs the same CSRF header and same-origin checks as every other write, so another site cannot
// sign a user out with a cross-site form post (the UI posts it with fetch).
func (s *Server) authLogout(c *reqCtx) error {
	if c.r.Header.Get(CSRFHeader) != CSRFValue {
		return apierr.New(403, "missing "+CSRFHeader+": "+CSRFValue+" header")
	}
	if sfs := c.r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" {
		return apierr.New(403, "cross-site request refused")
	}
	if o := c.r.Header.Get("Origin"); o != "" && strings.TrimRight(o, "/") != c.s.Cfg.BaseURL() {
		return apierr.New(403, "cross-origin request refused")
	}
	u := c.sessionUser()
	if s.DemoUser == nil {
		_ = s.Poller.Admin.RemoveSession(c.session().SID)
		c.clearSession()
	}
	if u != nil {
		c.auditAnon("auth.logout", "ok", actorOf(u), &u.Sub, nil)
	}
	if strings.HasPrefix(c.r.Header.Get("Accept"), "application/json") {
		c.flush()
		c.w.WriteHeader(204)
		return nil
	}
	c.redirect("/")
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
