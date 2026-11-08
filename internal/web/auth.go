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

// authAge: seconds since the user last authenticated at Pocket ID (nil for API tokens).
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
	s := c.session()
	at := s.AuthTime
	if at == 0 {
		at = float64(s.IAT)
	}
	v := float64(time.Now().UnixNano())/1e9 - at
	return &v
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
	if !s.login.allow(c.ip()) {
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
	var current *model.User
	if stepUp {
		current = c.sessionUser()
	}
	if current == nil {
		c.clearSession() // fresh session: avoids fixation of a pre-login session
	}
	p, err := s.oidc.provider(c.r.Context())
	if err != nil {
		slog.Error("OIDC discovery failed", "error", err)
		return apierr.New(502, "identity provider unreachable")
	}
	sess := c.session()
	sess.Next, sess.StepUp = target, current != nil
	sess.State, sess.Nonce, sess.Verifier, sess.Started = randToken(24), randToken(24), oauth2.GenerateVerifier(), time.Now().Unix()
	c.dirty = true
	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(sess.Verifier), oidc.Nonce(sess.Nonce)}
	if current != nil { // step-up: force a fresh authentication at Pocket ID
		opts = append(opts, oauth2.SetAuthURLParam("max_age", "0"))
		if s.Cfg.StepUpPromptLogin {
			opts = append(opts, oauth2.SetAuthURLParam("prompt", "login"))
		}
		c.auditAnon("auth.step_up", "redirected to Pocket ID", actorOf(current), &current.Sub, nil)
	}
	c.redirect(s.oidc.oauth(p).AuthCodeURL(sess.State, opts...))
	return nil
}

func (s *Server) authCallback(c *reqCtx) error {
	if !s.login.allow(c.ip()) {
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
	oc := s.oidc.oauth(p)
	ctx, cancel := context.WithTimeout(s.oidc.ctx(c.r.Context()), 15*time.Second)
	defer cancel()
	tok, err := oc.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(sess.Verifier))
	if err != nil {
		return fail("token exchange")
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		return fail("no id_token")
	}
	idt, err := p.Verifier(&oidc.Config{ClientID: s.Cfg.OIDC.ClientID}).Verify(ctx, rawID)
	if err != nil {
		return fail("invalid id_token")
	}
	if idt.Nonce != sess.Nonce {
		return fail("nonce mismatch")
	}
	claims := map[string]any{}
	if err := idt.Claims(&claims); err != nil {
		return fail("invalid claims")
	}
	if _, ok := claims[s.Cfg.OIDC.GroupsClaim]; !ok && slices.Contains(s.Cfg.OIDC.Scopes, "groups") {
		if ui, err := p.UserInfo(ctx, oauth2.StaticTokenSource(tok)); err == nil {
			extra := map[string]any{}
			if ui.Claims(&extra) == nil && fmt.Sprint(extra["sub"]) == idt.Subject {
				for k, v := range extra {
					claims[k] = v
				}
			}
		} else {
			slog.Warn("OIDC userinfo request failed")
		}
	}
	if idt.Subject == "" {
		return fail("no sub claim")
	}
	claims["sub"] = idt.Subject
	u := userFromClaims(claims, s.Cfg.OIDC.GroupsClaim)
	if !isAllowed(u, s.Cfg.OIDC) {
		slog.Warn("login denied", "sub", u.Sub)
		c.auditAnon("auth.login", "denied: not in an allowed group", actorOf(&u), &u.Sub, nil)
		c.clearSession()
		return apierr.New(403, "not allowed")
	}
	now := float64(time.Now().UnixNano()) / 1e9
	authTime := now
	if v, ok := claims["auth_time"].(float64); ok {
		authTime = v
	}
	target := safeNext(sess.Next)
	if sess.StepUp && now-authTime > 120 {
		c.auditAnon("auth.step_up", "failed: identity provider did not re-authenticate (auth_time too old)", actorOf(&u), &u.Sub, nil)
		return apierr.New(400, "re-authentication was not performed by the identity provider")
	}
	admin := s.Poller.Admin
	if sess.StepUp && sess.User != nil && sess.User.Sub == u.Sub && sess.SID != "" {
		sess.AuthTime, sess.Seen = authTime, int64(now)
		sess.StepUp, sess.Next, sess.State, sess.Nonce, sess.Verifier, sess.Started = false, "", "", "", "", 0
		c.dirty = true
		c.auditAnon("auth.step_up", "ok", actorOf(&u), &u.Sub, nil)
		c.redirect(target)
		return nil
	}
	_ = admin.RemoveSession(sess.SID)
	sid := randToken(18)
	if err := admin.AddSession(sid, map[string]any{"sub": u.Sub, "name": u.Name, "email": u.Email, "ip": c.ip()}); err != nil {
		return err
	}
	c.sess = &sessionData{SID: sid, User: &u, IAT: int64(now), Seen: int64(now), AuthTime: authTime}
	c.dirty = true
	slog.Info("login ok", "sub", u.Sub)
	c.auditAnon("auth.login", "ok", actorOf(&u), &u.Sub, nil)
	c.redirect(target)
	return nil
}

func (s *Server) authLogout(c *reqCtx) error {
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
