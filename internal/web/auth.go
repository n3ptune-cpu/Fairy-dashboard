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

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// Why a cookie's session is not valid (sessionCheck); "" while it is.
const (
	sessNone    = "none"    // no session in the cookie
	sessMaxAge  = "max_age" // older than session_max_age
	sessRevoked = "revoked" // not (or no longer) in sessions.json
	sessIdle    = "idle"    // not really used for session_idle_timeout
	sessRemoved = "no_user" // the user is not in users.json (removed, or a cookie from before users.json)
	activeHdr   = "X-Fairy-Active"
	activePath  = "/api/me/active"
	seenPersist = 60 // seconds: the cookie's copy of "last used" is refreshed at most this often
)

// sessionCheck reports why sd is not a valid session right now ("" = valid). It changes nothing, so the live
// stream can ask it every second.
//
// A session is valid while it is listed in sessions.json (not revoked by the CLI), younger than
// session_max_age, and really used within session_idle_timeout. "Used" is kept on the server per session id
// (the cookie's copy only seeds it after a restart) and moves only on real use, see active.
func (s *Server) sessionCheck(sd *sessionData) string {
	now := s.now()
	if sd.User == nil || sd.IAT == 0 {
		return sessNone
	}
	if now.Unix()-sd.IAT > int64(s.Cfg.SessionMaxAge) {
		return sessMaxAge
	}
	if !s.Poller.Admin.SessionValid(sd.SID, float64(sd.IAT)) {
		return sessRevoked
	}
	seen := sd.Seen
	if seen == 0 {
		seen = sd.IAT
	}
	if now.Unix()-s.codec.live.lastSeen(sd.SID, seen, now) > int64(s.Cfg.SessionIdleTimeout) {
		return sessIdle
	}
	if s.Access.Err() == nil {
		if _, known := s.Access.Load().Users[sd.User.Sub]; !known {
			// Removed from users.json, or a cookie from before users.json existed: sign in again. Roles are only
			// ever bootstrapped at a fresh sign-in, never from the groups cached in a (possibly days old) cookie.
			return sessRemoved
		}
	}
	return ""
}

// endSession removes a session that sessionCheck found expired or orphaned from sessions.json, with everything
// the panel remembers under its id.
func (s *Server) endSession(sid string) {
	_ = s.Poller.Admin.RemoveSession(sid)
	s.codec.live.forget(sid)
}

// active reports whether this request shows a person using the panel, which is what keeps a session from idling
// out. Only these do: a write (the UI makes none on a timer), and a request the UI marks with X-Fairy-Active: 1
// because there was pointer, key, touch or wheel input in the last minute (POST /api/me/active is the UI's
// explicit signal for a user who only reads the live page). The page's background traffic (the live stream and
// its reconnects, the once-a-minute updates poll, every other timer) is unmarked and never counts.
//
// This tells an attended browser from an unattended one. It is not a defence against someone who holds the
// cookie: they can send the header.
func (c *reqCtx) active() bool {
	if c.resumed() {
		return false // the panel's own replay; the sign-in that started it already counted
	}
	return (c.r.Method != http.MethodGet && c.r.Method != http.MethodHead) || c.r.Header.Get(activeHdr) == "1"
}

// sessionUser: browser session (cookie) only; see sessionCheck for what makes it valid. An expired or orphaned
// session is removed from sessions.json and its cookie cleared. Real use (active) moves "last used" on the
// server at once and in the cookie at most once a minute.
func (c *reqCtx) sessionUser() *model.User {
	if c.s.DemoUser != nil {
		u := *c.s.DemoUser
		return &u
	}
	s := c.session()
	switch c.s.sessionCheck(s) {
	case "":
	case sessNone, sessMaxAge:
		return nil
	case sessRevoked:
		c.clearSession()
		return nil
	default: // idle, or the user is gone
		c.s.endSession(s.SID)
		c.clearSession()
		return nil
	}
	if c.active() {
		now := c.s.now()
		c.s.codec.live.touch(s.SID, now)
		if now.Unix()-s.Seen > seenPersist {
			s.Seen = now.Unix()
			c.dirty = true
		}
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

// authAge: seconds since the user last authenticated at Pocket ID (nil for API tokens), as far as this request
// may rely on it. The time is kept on the server per session id and counts only for the client (address and
// browser) the confirmation came back to: a session without a recorded confirmation (none since the panel
// started, or an ID token without auth_time), or the same cookie presented from another address or browser,
// counts as authenticated long ago (fail closed). Control actions then need a step-up, and the step-up itself
// must return auth_time.
func (c *reqCtx) authAge(u *model.User) *float64 {
	switch u.Auth {
	case "token":
		return nil
	case "demo":
		c.s.demoMu.Lock()
		defer c.s.demoMu.Unlock()
		v := c.s.now().Sub(c.s.demoAuthTime).Seconds()
		return &v
	}
	at, _ := c.s.codec.live.confirmedAt(c.session().SID, c.ip(), uaHash(c.r)) // 0 = unknown: very old
	v := c.s.nowSec() - at
	return &v
}

// loginKey is the login rate limiter's bucket: the signed-in user for a request that carries a valid session
// (a step-up or re-login), else the client IP. So clients sharing an address with someone (or flooding
// /auth/login) can never stop a signed-in admin from confirming a control action.
func (c *reqCtx) loginKey() string {
	s := c.session()
	if s.User != nil && s.SID != "" && c.s.Poller.Admin.SessionValid(s.SID, float64(s.IAT)) &&
		c.s.now().Unix()-s.IAT <= int64(c.s.Cfg.SessionMaxAge) {
		return "sub:" + s.User.Sub
	}
	return "ip:" + c.ip()
}

func (s *Server) authRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /auth/login", s.h(s.authLogin))
	m.HandleFunc("GET /auth/callback", s.h(s.authCallback))
	m.HandleFunc("POST /auth/logout", s.h(s.authLogout))
	m.HandleFunc("POST "+activePath, s.h(s.meActive))
}

// meActive is the UI's "someone is using this page" signal (see active): a write with the CSRF header, sent at
// most once a minute while there is input. Being a write is what makes it count; it does nothing else.
func (s *Server) meActive(c *reqCtx) error {
	u, err := c.requireUser()
	if err != nil {
		return err
	}
	if err := c.checkCSRF(u); err != nil {
		return err
	}
	c.flush()
	c.w.WriteHeader(204)
	return nil
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
	if stepUp {
		target = stepUpNext(target)
	}
	pendID := q.Get("pending") // the kept action this step-up is for (see pending.go); anything else disarms it
	if len(pendID) > 64 {
		pendID = ""
	}
	if s.DemoUser != nil { // demo: simulate the Pocket ID round trip for the step-up screen
		s.demoMu.Lock()
		s.demoAuthTime = s.now()
		s.demoMu.Unlock()
		d := s.DemoUser
		c.auditAnon("auth.step_up", "ok (demo: simulated Pocket ID re-authentication)", model.Deref(d.Email), &d.Sub, nil)
		s.resumePending(c, s.codec.live.takePending(demoKey, pendID, s.now()))
		c.redirect(target)
		return nil
	}
	// A valid session is kept until the round trip completes (the callback replaces it with a new session id),
	// so another site cannot sign someone out by sending them to /auth/login. Anything else is cleared.
	sid := c.session().SID
	current := c.sessionUser()
	if current == nil {
		c.clearSession() // fresh session: avoids fixation of a pre-login session
	}
	sess := c.session()
	// Only a step-up that names the session's kept action may run it. Every other sign-in that starts for this
	// session (a link on another site, a second tab, a plain re-login) forgets it, before anything can fail.
	sess.PendingID = ""
	if stepUp && current != nil && pendID != "" && s.codec.live.pendingID(sid, s.now()) == pendID {
		sess.PendingID = pendID
	} else {
		s.codec.live.dropPending(sid)
	}
	p, err := s.oidc.provider(c.r.Context())
	if err != nil {
		slog.Error("OIDC discovery failed", "error", err)
		s.codec.live.dropPending(sid)
		return apierr.New(502, "identity provider unreachable")
	}
	sess.Next, sess.StepUp = target, stepUp && current != nil
	sess.State, sess.Nonce, sess.Verifier, sess.Started = randToken(24), randToken(24), oauth2.GenerateVerifier(), s.now().Unix()
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
	sess := c.session()
	// The kept action leaves the store before anything is checked: whatever happens below, it can run at most
	// once, and a confirmation that fails (or is rate limited, or belongs to another round trip) leaves nothing
	// armed. Only a successful step-up for the same session runs it.
	var kept *pendingAction
	if sess.SID != "" {
		kept = s.codec.live.takePending(sess.SID, sess.PendingID, s.now())
	}
	if !s.login.allow(c.loginKey()) {
		c.auditAnon("auth.login", "denied: rate limited", "", nil, nil)
		return apierr.New(429, "too many login attempts; try again in a minute")
	}
	q := c.r.URL.Query()
	fail := func(f *loginFailure) error {
		slog.Warn("OIDC callback failed", append([]any{"reason", f.reason}, f.attrs...)...)
		c.auditAnon("auth.login", f.auditOutcome(), "", nil, nil)
		return apierr.New(400, "login failed")
	}
	if e := q.Get("error"); e != "" {
		f := &loginFailure{reason: oauthCode(truncate(e, 60))}
		if d := q.Get("error_description"); d != "" {
			f.attrs = []any{"error_description", safeText(d, 200)}
		}
		return fail(f)
	}
	if sess.State == "" || q.Get("state") != sess.State || s.now().Unix()-sess.Started > 600 {
		return fail(&loginFailure{reason: "mismatching_state"})
	}
	p, err := s.oidc.provider(c.r.Context())
	if err != nil {
		slog.Warn("OIDC callback failed", "reason", "discovery", "error", safeErr(err))
		return apierr.New(502, "identity provider unreachable")
	}
	ctx, cancel := context.WithTimeout(s.oidc.ctx(c.r.Context()), 15*time.Second)
	defer cancel()
	claims, lf := s.verifiedClaims(ctx, p, q.Get("code"), sess)
	if lf != nil {
		return fail(lf)
	}
	u := userFromClaims(claims, s.Cfg.OIDC.GroupsClaim)
	if !isAllowed(u, s.Cfg.OIDC) {
		slog.Warn("login denied", "sub", u.Sub)
		c.auditAnon("auth.login", "denied: not in an allowed group", actorOf(&u), &u.Sub, nil)
		c.clearSession()
		return apierr.New(403, "not allowed")
	}
	now := s.nowSec()
	authTime := 0.0 // unknown: counts as long ago (never as "just now")
	if v, ok := claims["auth_time"].(float64); ok && v > 0 {
		authTime = v
	}
	target := safeNext(sess.Next)
	if sess.StepUp {
		target = stepUpNext(target)
	}
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
	if sess.StepUp && sess.User != nil && sess.User.Sub == u.Sub && sess.SID != "" && c.sessionUser() != nil {
		// Step-up for the same user of a session that is still valid: the session continues (same sign-in time,
		// so session_max_age is not extended) under a NEW session id, and the fresh authentication is recorded on
		// the server for this client only. A copy of the cookie taken before this moment stops working.
		sid, err := s.rotateSession(c, sess, u)
		if err != nil {
			return err
		}
		s.codec.live.confirm(sid, authTime, c.ip(), uaHash(c.r), s.now())
		s.codec.live.touch(sid, s.now()) // confirming at Pocket ID is real use
		sess.Seen = int64(now)
		sess.StepUp, sess.Next, sess.State, sess.Nonce, sess.Verifier, sess.Started, sess.PendingID = false, "", "", "", "", 0, ""
		c.dirty = true
		c.auditAnon("auth.step_up", "ok", actorOf(&u), &u.Sub, nil)
		s.resumePending(c, kept)
		c.redirect(target)
		return nil
	}
	// a new session (another user, or no session before): nothing waits (kept was taken out above and is dropped)
	if err := s.startSession(c, sess.SID, u, now, authTime); err != nil {
		return err
	}
	c.redirect(target)
	return nil
}

// verifiedClaims exchanges the authorization code (with the PKCE verifier), verifies the ID token and its nonce
// and returns the claims, merged with userinfo when the groups claim is missing from the ID token. On failure it
// returns why, for the log and audit trail only (never shown to the client).
func (s *Server) verifiedClaims(ctx context.Context, p *oidc.Provider, code string, sess *sessionData) (map[string]any, *loginFailure) {
	redact := []string{code, sess.Verifier, s.oidc.secret}
	tok, err := s.oidc.oauth(p).Exchange(ctx, code, oauth2.VerifierOption(sess.Verifier))
	if err != nil {
		return nil, exchangeFailure(err, redact...)
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		return nil, &loginFailure{reason: "no id_token", attrs: []any{"error",
			"the token response has no id_token (is the openid scope requested?)"}}
	}
	redact = append(redact, rawID, tok.AccessToken, tok.RefreshToken)
	idt, err := p.Verifier(&oidc.Config{ClientID: s.Cfg.OIDC.ClientID}).Verify(ctx, rawID)
	if err != nil {
		return nil, &loginFailure{reason: "invalid id_token", attrs: []any{"error", safeErr(err, redact...)}}
	}
	if idt.Nonce != sess.Nonce {
		got := "different"
		if idt.Nonce == "" {
			got = "missing"
		}
		return nil, &loginFailure{reason: "nonce mismatch", attrs: []any{"id_token_nonce", got,
			"hint", "another sign-in was started in this browser, or the identity provider dropped the nonce"}}
	}
	claims := map[string]any{}
	if err := idt.Claims(&claims); err != nil {
		return nil, &loginFailure{reason: "invalid claims", attrs: []any{"error", safeErr(err, redact...)}}
	}
	if _, ok := claims[s.Cfg.OIDC.GroupsClaim]; !ok && slices.Contains(s.Cfg.OIDC.Scopes, "groups") {
		mergeUserInfo(ctx, p, tok, idt.Subject, claims, redact)
	}
	if idt.Subject == "" {
		return nil, &loginFailure{reason: "no sub claim"}
	}
	claims["sub"] = idt.Subject
	return claims, nil
}

// mergeUserInfo adds the userinfo endpoint's claims (some providers only put groups there), but only when
// userinfo is about the same subject as the ID token.
func mergeUserInfo(ctx context.Context, p *oidc.Provider, tok *oauth2.Token, subject string, claims map[string]any, redact []string) {
	ui, err := p.UserInfo(ctx, oauth2.StaticTokenSource(tok))
	if err != nil {
		slog.Warn("OIDC userinfo request failed", "error", safeErr(err, redact...))
		return
	}
	extra := map[string]any{}
	if err := ui.Claims(&extra); err != nil {
		slog.Warn("OIDC userinfo ignored", "error", safeErr(err, redact...))
		return
	}
	if fmt.Sprint(extra["sub"]) != subject {
		slog.Warn("OIDC userinfo ignored", "error", "userinfo sub differs from the ID token's")
		return
	}
	for k, v := range extra {
		claims[k] = v
	}
}

// startSession replaces the previous session (oldSID) with a fresh one: a new random id registered in
// sessions.json (so the CLI can revoke it) and a signed cookie holding the user.
func (s *Server) startSession(c *reqCtx, oldSID string, u model.User, now, authTime float64) error {
	admin := s.Poller.Admin
	_ = admin.RemoveSession(oldSID)
	s.codec.live.forget(oldSID)
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
	c.sess = &sessionData{SID: sid, User: &u, IAT: int64(now), Seen: int64(now)}
	c.dirty = true
	s.codec.live.confirm(sid, authTime, c.ip(), uaHash(c.r), s.now())
	s.codec.live.touch(sid, s.now())
	slog.Info("login ok", "sub", u.Sub)
	c.auditAnon("auth.login", "ok", actorOf(&u), &u.Sub, nil)
	return nil
}

// rotateSession gives a session that just passed a step-up a new id: the new id is registered in sessions.json,
// the old one removed (with everything the panel remembered under it). The cookie's other contents stay.
func (s *Server) rotateSession(c *reqCtx, sess *sessionData, u model.User) (string, error) {
	admin := s.Poller.Admin
	sid := randToken(18)
	if err := admin.AddSession(sid, adminstate.NewSession{Sub: u.Sub, Name: u.Name, Email: u.Email, IP: c.ip()}); err != nil {
		slog.Error("step-up failed: cannot register the session in sessions.json (disk full? wrong owner?)", "error", err)
		c.auditAnon("auth.step_up", "failed: sessions.json not writable", actorOf(&u), &u.Sub, nil)
		return "", apierr.New(503, "the sign-in could not be recorded (the panel cannot write its session file); see the server log")
	}
	_ = admin.RemoveSession(sess.SID)
	s.codec.live.forget(sess.SID)
	sess.SID = sid
	return sid, nil
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
		s.codec.live.forget(c.session().SID)
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

// stepUpNext is where a step-up may return to: the panel's start page, with or without a fragment (the page the
// dialog was opened on). Nothing else, so a step-up link cannot be used to land on another path.
func stepUpNext(t string) string {
	if t == "/" || strings.HasPrefix(t, "/#") {
		return t
	}
	return "/"
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
