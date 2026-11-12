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
// A session is valid while it is listed in sessions.json (not revoked by the CLI) for the account the cookie
// names (T-22: everything the panel keeps per session id, the step-up window and a kept action included, is
// reached only through this check), younger than session_max_age, and really used within session_idle_timeout. "Used" is kept on the server per session id
// (the cookie's copy only seeds it after a restart) and moves only on real use, see markedRead and used.
func (s *Server) sessionCheck(sd *sessionData) string {
	now := s.now()
	if sd.User == nil || sd.IAT == 0 {
		return sessNone
	}
	if now.Unix()-sd.IAT > int64(s.Cfg.SessionMaxAge) {
		return sessMaxAge
	}
	if !s.Poller.Admin.SessionValid(sd.SID, sd.User.Sub, float64(sd.IAT)) || s.codec.live.isEnded(sd.SID) {
		return sessRevoked // not in sessions.json, or ended while that file could not be written (dropSession)
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
// the panel remembers under its id; why is what sessionCheck said.
//
// A cookie older than session_max_age (sessMaxAge) can never be accepted again: its issue time is signed. So
// when its session cannot leave sessions.json there is nothing to remember and nothing to log, and there must
// not be: anybody who kept such a cookie can send it as often as they like, also for an id the panel can not
// look up because the file is unreadable.
func (s *Server) endSession(sid, why string) {
	if why == sessMaxAge {
		_ = s.Poller.Admin.RemoveSession(sid)
		s.codec.live.forget(sid)
		return
	}
	_ = s.dropSession(sid, "a session that expired")
}

// dropSession ends a session: it leaves sessions.json and everything the panel remembers under its id. When
// the file cannot be written (a full disk, a read-only folder, a wrong owner), the session cannot leave it;
// then it is ended in the panel's memory instead (liveSessions.end), so its cookie, and every copy of it,
// stops working at once, and the error is logged and returned (T-29: it was dropped, and the session stayed
// valid). What memory cannot do: after a restart the file is read again and such a session is back until it
// expires, which is why the log line says to look at the file. what names the occasion for the log.
//
// Only a session the file lists is ended in memory, and it is logged once. An id that a readable file does
// not list has nothing to end (its cookie is refused anyway), so it takes no place on the list and writes no
// log line: the list then holds registered sessions only and cannot be filled with other ids to push an
// entry out. While the file cannot be read the id is ended, because the panel cannot know.
func (s *Server) dropSession(sid, what string) error {
	if sid == "" {
		return nil
	}
	err := s.Poller.Admin.RemoveSession(sid)
	if err == nil {
		s.codec.live.forget(sid)
		return nil
	}
	if listed, ok := s.Poller.Admin.SessionListed(sid); ok && !listed {
		s.codec.live.forget(sid)
		return nil
	}
	if s.codec.live.end(sid, s.now()) {
		slog.Error("cannot remove "+what+" from sessions.json (disk full? read-only? wrong owner?): it is ended in "+
			"memory and stays out until the panel restarts. Make the file writable, then run `fairy admin sessions "+
			"revoke-all` if a cookie may have been copied", "error", err)
	}
	return err
}

// What keeps a session from idling out is a person using the panel, and only a request the panel accepted
// shows that (NEW-5: a request it refuses must not count, whoever sent it). Two kinds of request can count:
//
//   - a read the page marks with X-Fairy-Active: 1, because there was pointer, key, touch or wheel input in the
//     last minute (markedRead);
//   - a write that passed the CSRF checks (checkCSRF: the custom header, the panel's own origin, same-origin
//     fetch metadata). The UI makes no write on a timer; POST /api/me/active is its explicit signal for a user
//     who only reads the live page. A form post from another site or a sibling host carries the cookie but not
//     the header: it is refused and does not count.
//
// Either one only sets reqCtx.use. The session's "last used" moves when the answer is written and is not an
// error (used, called by every way of answering). The page's background traffic (the live stream and its
// reconnects, the once-a-minute updates poll, every other timer) is unmarked and never counts.
//
// This tells an attended browser from an unattended one. It is not a defence against someone who holds the
// cookie: they can send the header.
func (c *reqCtx) markedRead() bool {
	if c.resumed() || (c.r.Method != http.MethodGet && c.r.Method != http.MethodHead) || c.r.Header.Get(activeHdr) != "1" {
		return false
	}
	sfs := c.r.Header.Get("Sec-Fetch-Site")
	return sfs == "" || sfs == "same-origin"
}

// used moves the session's "last used" for a request that counts as use (reqCtx.use) and is being answered
// with status: only an answer that is not an error counts. On the server at once, in the cookie at most once a
// minute. Called before the answer's headers are written.
func (c *reqCtx) used(status int) {
	if !c.use || status >= 400 || c.sess == nil || c.sess.SID == "" || c.s.DemoUser != nil {
		return
	}
	c.use = false
	now := c.s.now()
	c.s.codec.live.touch(c.sess.SID, now)
	c.s.saveSeen(c.sess.SID, c.ip(), now)
	if now.Unix()-c.sess.Seen > seenPersist {
		c.sess.Seen = now.Unix()
		c.dirty = true
	}
}

// saveSeen writes a session's last use, and the address it came from, to sessions.json, at most once a minute
// per session (seenPersist). It is what the Sessions page shows as "last active" for a session the panel has
// not seen since it started; nothing is decided by it (the idle timeout goes by the panel's memory and the
// cookie). A write that fails is logged once, until one succeeds again.
func (s *Server) saveSeen(sid, ip string, now time.Time) {
	if !s.codec.live.dueSave(sid, now) {
		return
	}
	if err := s.Poller.Admin.TouchSession(sid, float64(now.Unix()), ip); err == nil {
		s.seenErr.Store(false)
	} else if s.seenErr.CompareAndSwap(false, true) {
		slog.Warn("cannot record in sessions.json when sessions were last used (disk full? wrong owner?): the "+
			"Sessions page shows older times after a restart; nothing else depends on it", "error", err)
	}
}

// sessionUser: browser session (cookie) only; see sessionCheck for what makes it valid. A session that reached
// session_max_age, idled out or lost its user is removed from sessions.json and its cookie cleared.
func (c *reqCtx) sessionUser() *model.User {
	if c.s.DemoUser != nil {
		u := *c.s.DemoUser
		return &u
	}
	s := c.session()
	switch why := c.s.sessionCheck(s); why {
	case "":
	case sessNone:
		if s.expiredSID != "" { // a genuine cookie older than session_max_age: its session ends like any other
			c.s.endSession(s.expiredSID, sessMaxAge)
			c.clearSession()
		}
		return nil
	case sessRevoked:
		c.clearSession()
		return nil
	default: // too old, idle, or the user is gone
		c.s.endSession(s.SID, why)
		c.clearSession()
		return nil
	}
	if c.markedRead() {
		c.use = true
	}
	c.s.codec.live.noteAddr(s.SID, c.ip(), c.s.now()) // for the Sessions page: where this session is used from
	u := *s.User
	u.Auth = "session"
	if u.Groups == nil {
		u.Groups = []string{}
	}
	return &u
}

// tokenUser: `Authorization: Bearer fy_...` personal API token (CLI-created). Checked once per request (the
// rejection is audited once), however often it is asked.
func (c *reqCtx) tokenUser() (*model.User, error) {
	if !c.tokDone {
		c.tokUser, c.tokErr = c.checkToken()
		c.tokDone = true
	}
	return c.tokUser, c.tokErr
}

func (c *reqCtx) checkToken() (*model.User, error) {
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
// /auth/login from elsewhere) do not use up a signed-in admin's budget for confirming a control action.
// What still does (T-17, known, not changed): requests that carry the admin's own session, that is a copy of
// the cookie, or the admin's own browser sent to GET /auth/login by a link on another site (SameSite=Lax).
// README and SECURITY-FIXES.md say so.
func (c *reqCtx) loginKey() string {
	s := c.session()
	if s.User != nil && s.SID != "" && c.s.Poller.Admin.SessionValid(s.SID, s.User.Sub, float64(s.IAT)) &&
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
	m.HandleFunc("POST "+stepUpPath, s.h(s.confirmStepUp))
}

// stepUpPath is "Continue" in the "Confirm it's you" dialog: the only way to arm a kept action (confirmStepUp).
const stepUpPath = "/api/me/step-up"

// meActive is the UI's "someone is using this page" signal (see markedRead): a write with the CSRF header, sent
// at most once a minute while there is input. Being an accepted write is what makes it count; it does nothing else.
func (s *Server) meActive(c *reqCtx) error {
	u, err := c.requireUser()
	if err != nil {
		return err
	}
	if err := c.checkCSRF(u); err != nil {
		return err
	}
	c.used(204)
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
	if s.DemoUser != nil { // demo: simulate the Pocket ID round trip for the step-up screen
		s.demoMu.Lock()
		s.demoAuthTime = s.now()
		s.demoMu.Unlock()
		d := s.DemoUser
		c.auditAnon("auth.step_up", "ok (demo: simulated Pocket ID re-authentication)", model.Deref(d.Email), &d.Sub, nil)
		s.codec.live.dropPending(demoKey) // a link never runs a kept action: see below
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
	// A sign-in that starts here never runs a kept action, and forgets the one the session has: this is a plain
	// link (GET), which another site can make a browser follow. Only the dialog's own "Continue" arms a kept
	// action (confirmStepUp: a POST with the CSRF header, from the client the action was kept for).
	s.codec.live.dropPending(sid)
	p, err := s.oidc.provider(c.r.Context())
	if err != nil {
		slog.Error("OIDC discovery failed", "error", err)
		return apierr.New(502, "identity provider unreachable")
	}
	loc := s.startRoundTrip(c, p, sess, target, stepUp && current != nil, randToken(24))
	if sess.StepUp {
		c.auditAnon("auth.step_up", "redirected to Pocket ID", actorOf(current), &current.Sub, nil)
	}
	c.redirect(loc)
	return nil
}

// startRoundTrip prepares a sign-in at Pocket ID in the session cookie (state, nonce, PKCE verifier, where to
// return to) and returns the address to send the browser to. A step-up asks for a fresh authentication.
func (s *Server) startRoundTrip(c *reqCtx, p *oidc.Provider, sess *sessionData, target string, stepUp bool, state string) string {
	sess.Next, sess.StepUp = target, stepUp
	sess.State, sess.Nonce, sess.Verifier, sess.Started = state, randToken(24), oauth2.GenerateVerifier(), s.now().Unix()
	c.dirty = true
	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(sess.Verifier), oidc.Nonce(sess.Nonce)}
	if stepUp { // step-up: force a fresh authentication at Pocket ID
		opts = append(opts, oauth2.SetAuthURLParam("max_age", "0"))
		if s.Cfg.StepUpPromptLogin {
			opts = append(opts, oauth2.SetAuthURLParam("prompt", "login"))
		}
	}
	return s.oidc.oauth(p).AuthCodeURL(sess.State, opts...)
}

// confirmStepUp is "Continue" in the "Confirm it's you" dialog (POST /api/me/step-up, body {"pending_id",
// "next"}): it arms the session's kept action for one sign-in round trip and answers with the address of that
// sign-in ({"url": ..., "action": what will run}). The page then goes there.
//
// This is the only way a kept action gets armed, and it cannot be done by a link or from somewhere else: it
// is a write (CSRF header, same origin), it needs the session, the id comes in the body from the page that was
// refused, and it must come from the client the action was kept for (armPending). The callback runs the
// action only for this round trip and only when the same client comes back (authCallback).
func (s *Server) confirmStepUp(c *reqCtx) error {
	u, err := c.requireSession()
	if err != nil {
		return err
	}
	if err := c.checkCSRF(u); err != nil {
		return err
	}
	if !s.login.allow(c.loginKey()) {
		c.auditAnon("auth.login", "denied: rate limited", "", nil, nil)
		return apierr.New(429, "too many login attempts; try again in a minute")
	}
	var b struct {
		PendingID string `json:"pending_id"`
		Next      string `json:"next"`
	}
	key := c.pendingKey()
	if err := c.decodeBody(&b); err != nil || len(b.PendingID) > 64 {
		s.codec.live.dropPending(key)
		return apierr.New(422, "invalid request body")
	}
	target := stepUpNext(safeNext(b.Next))
	round := randToken(24)
	kept := s.codec.live.armPending(key, b.PendingID, c.client(), round, s.now())
	if kept == nil {
		return apierr.New(409, map[string]any{"code": "nothing_kept", "message": "The action is no longer waiting: it was " +
			"cancelled, it is more than 3 minutes old, or it was asked for from another browser or address. Nothing was " +
			"done. Press the button again."})
	}
	params := map[string]any{"for": kept.Action}
	if s.DemoUser != nil { // demo: simulate the Pocket ID round trip
		s.demoMu.Lock()
		s.demoAuthTime = s.now()
		s.demoMu.Unlock()
		c.auditAnon("auth.step_up", "ok (demo: simulated Pocket ID re-authentication)", actorOf(u), &u.Sub, params)
		run, _ := s.codec.live.takePending(key, round, c.client(), s.now())
		s.resumePending(c, run)
		c.json(200, map[string]any{"url": target, "action": kept.Action})
		return nil
	}
	p, err := s.oidc.provider(c.r.Context())
	if err != nil {
		slog.Error("OIDC discovery failed", "error", err)
		s.codec.live.dropPending(key)
		return apierr.New(502, "identity provider unreachable")
	}
	loc := s.startRoundTrip(c, p, c.session(), target, true, round)
	c.auditAnon("auth.step_up", "redirected to Pocket ID", actorOf(u), &u.Sub, params)
	c.json(200, map[string]any{"url": loc, "action": kept.Action})
	return nil
}

// authCallback finishes the Pocket ID round trip: it checks state, exchanges the code (PKCE), verifies the ID
// token and nonce, applies the allowed groups / emails and either completes a step-up re-authentication or
// starts a new, server-registered session. Every failure is audited and answered with a generic 400.
func (s *Server) authCallback(c *reqCtx) error {
	sess := c.session()
	// Every refusal below is a page with one link back, not a line of JSON (callbackpage.go).
	page := c.callbackPage(sess)
	// The kept action leaves the store before anything is checked: whatever happens below, it can run at most
	// once, and a confirmation that fails (or is rate limited, or belongs to another round trip) leaves nothing
	// armed. Only a successful step-up for the same session runs it.
	// It is handed out only when it was armed for this very round trip (confirmStepUp put the round trip's state
	// on it) and the client that comes back is the one it was kept for.
	var kept *pendingAction
	var foreign bool
	if sess.SID != "" {
		kept, foreign = s.codec.live.takePending(sess.SID, sess.State, c.client(), s.now())
	}
	if !s.login.allow(c.loginKey()) {
		c.auditAnon("auth.login", "denied: rate limited", "", nil, nil)
		return page.limited(s.login, c.loginKey())
	}
	q := c.r.URL.Query()
	fail := func(f *loginFailure) error {
		slog.Warn("OIDC callback failed", append([]any{"reason", f.reason}, f.attrs...)...)
		c.auditAnon("auth.login", f.auditOutcome(), "", nil, nil)
		return page.failed(f.code, f.reason == staleState)
	}
	if e := q.Get("error"); e != "" {
		f := &loginFailure{reason: oauthCode(truncate(e, 60))}
		f.code = truncate(e, 60) // as sent: the page shows it only when it looks like an error code (callbackpage.go)
		if d := q.Get("error_description"); d != "" {
			f.attrs = []any{"error_description", safeText(d, 200)}
		}
		return fail(f)
	}
	// A repeat of the confirmation this session's cookie already came from (the person went back to the
	// callback's address): the same redirect, nothing else (steprepeat.go).
	if t, ok := s.codec.live.stepDone(sess.SID, q.Get("state"), q.Get("code"), c.client(), s.now()); ok {
		c.redirect(t)
		return nil
	}
	if sess.State == "" || q.Get("state") != sess.State || s.now().Unix()-sess.Started > 600 {
		return fail(&loginFailure{reason: staleState})
	}
	// A step-up callback is remembered while it works and for two minutes after it was accepted, so that a
	// repeat of exactly this callback (the same state and code from the same client: a reload, a lost answer)
	// is told that the action was already sent instead of "login failed" (steprepeat.go). The repeat gets a
	// page and never a cookie. Everything below is as strict as it was for the callback that does the work,
	// and for every other repeat.
	var step *stepRun
	if sess.StepUp && sess.SID != "" && sess.User != nil {
		first, run := s.codec.live.beginStep(sess.SID, sess.State, q.Get("code"), c.client(), s.now())
		switch {
		case first:
			step = run
			defer s.codec.live.endStep(step)
		case run != nil && s.repeatedStep(c, page, sess, run):
			return nil
		}
	}
	p, err := s.oidc.provider(c.r.Context())
	if err != nil {
		slog.Warn("OIDC callback failed", "reason", "discovery", "error", safeErr(err))
		return page.refuse(502, cbUnreachable)
	}
	ctx, cancel := context.WithTimeout(s.oidc.ctx(c.r.Context()), 15*time.Second)
	defer cancel()
	claims, groupsRead, lf := s.verifiedClaims(ctx, p, q.Get("code"), sess)
	if lf != nil {
		return fail(lf)
	}
	u := userFromClaims(claims, s.Cfg.OIDC.GroupsClaim)
	if !isAllowed(u, s.Cfg.OIDC) {
		if !groupsRead && emailAllowed(u, s.Cfg.OIDC) {
			// R7B-04. The groups were to come from userinfo and that request failed: Pocket ID has not said
			// that the account left the allowed groups, the panel could not ask. The sign-in or confirmation
			// fails and this browser's cookie is cleared (as it was before r3-p1-12), but no session is ended
			// on the server: a userinfo endpoint that is down for a moment must not sign the account out
			// everywhere.
			slog.Warn("login not decided: the groups could not be read from userinfo", "sub", u.Sub)
			c.auditAnon("auth.login", "failed: the groups could not be read from Pocket ID (userinfo); not decided, no session ended",
				actorOf(&u), &u.Sub, nil)
			c.clearSession()
			return page.refuse(502, cbNoGroups)
		}
		slog.Warn("login denied", "sub", u.Sub)
		ended, werr := s.endRefused(u.Sub, sess.SID)
		outcome, params := "denied: not in an allowed group", map[string]any(nil)
		if ended > 0 {
			params = map[string]any{"sessions_revoked": ended}
		}
		if werr != nil {
			outcome += "; sessions.json not writable: its sessions are ended in memory only"
		}
		c.auditAnon("auth.login", outcome, actorOf(&u), &u.Sub, params)
		c.clearSession()
		page.title = cbNotAllowed
		return page.refuse(403, cbRefused)
	}
	now := s.nowSec()
	authTime := 0.0 // unknown: counts as long ago (never as "just now")
	if v, ok := claims["auth_time"].(float64); ok && v > 0 {
		authTime = v
	}
	if authTime > now {
		// A sign-in time in the future (the identity provider's clock runs ahead, or it is wrong on purpose) would
		// keep the step-up window open for that much longer. The user cannot have signed in later than now.
		slog.Warn("identity provider sent an auth_time in the future; counted as now", "ahead_s", int64(authTime-now))
		authTime = now
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
	if sess.StepUp && !stepUpAuthTimeOK(float64(now-authTime)) {
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
			return page.err(err)
		}
		s.codec.live.stepAccepted(step, sid, target, kept != nil, s.now())
		s.codec.live.confirm(sid, authTime, c.ip(), uaHash(c.r), s.now())
		s.codec.live.touch(sid, s.now()) // confirming at Pocket ID is real use
		sess.Seen = int64(now)
		sess.StepUp, sess.Next, sess.State, sess.Nonce, sess.Verifier, sess.Started = false, "", "", "", "", 0
		c.dirty = true
		switch {
		case kept != nil:
			c.auditAnon("auth.step_up", "ok", actorOf(&u), &u.Sub, map[string]any{"runs": kept.Action})
		case foreign:
			c.auditAnon("auth.step_up", "ok; the action that waited was not run: the confirmation came back from "+
				"another browser or address than the one that asked for it", actorOf(&u), &u.Sub, nil)
		default:
			c.auditAnon("auth.step_up", "ok", actorOf(&u), &u.Sub, nil)
		}
		s.resumePending(c, kept)
		c.redirect(target)
		return nil
	}
	// a new session (another user, or no session before): nothing waits (kept was taken out above and is dropped)
	if err := s.startSession(c, sess.SID, u, now, authTime); err != nil {
		return page.err(err)
	}
	c.redirect(target)
	return nil
}

// endRefused ends sessions on the server when the identity provider has just said, with a verified ID token,
// that an account may not sign in (it left the allowed groups, or was never in one): every session of that
// account, and the session of the browser that came back, whoever's it is (its cookie is cleared in the same
// answer, as at a sign-out). r3-p1-12: only the cookie was cleared. At a "Confirm it's you" the session stayed
// registered with the role it had, so a copy of the cookie, the other tab and the account's other sessions
// went on until they idled out or reached a day. It returns how many sessions were ended and whether
// sessions.json could not be written (they are then ended in memory, as in dropSession).
func (s *Server) endRefused(sub, cookieSID string) (int, error) {
	var sids []string
	all, _ := s.Poller.Admin.Sessions()
	listed := false
	for _, x := range all {
		if x.Sub == sub || x.SID == cookieSID {
			sids = append(sids, x.SID)
			listed = listed || x.SID == cookieSID
		}
	}
	if len(sids) == 0 {
		if cookieSID == "" {
			return 0, nil
		}
		return 0, s.dropSession(cookieSID, "the session of a browser that came back with an account that is not allowed") // not listed, or the file cannot be read
	}
	if !listed && cookieSID != "" {
		_ = s.dropSession(cookieSID, "the session of a browser that came back with an account that is not allowed")
	}
	return len(sids), s.dropSessions(sids, "a session of an account that Pocket ID does not allow any more")
}

// verifiedClaims exchanges the authorization code (with the PKCE verifier), verifies the ID token and its nonce
// and returns the claims, merged with userinfo when the groups claim is missing from the ID token. groupsRead
// is false when the groups had to come from userinfo and userinfo could not be read: the claims then say
// nothing about the groups (not "none"). On failure it returns why, for the log and audit trail only (never
// shown to the client).
func (s *Server) verifiedClaims(ctx context.Context, p *oidc.Provider, code string, sess *sessionData) (claims map[string]any, groupsRead bool, lf *loginFailure) {
	redact := []string{code, sess.Verifier, s.oidc.secret}
	tok, err := s.oidc.oauth(p).Exchange(ctx, code, oauth2.VerifierOption(sess.Verifier))
	if err != nil {
		return nil, false, exchangeFailure(err, redact...)
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		return nil, false, &loginFailure{reason: "no id_token", attrs: []any{"error",
			"the token response has no id_token (is the openid scope requested?)"}}
	}
	redact = append(redact, rawID, tok.AccessToken, tok.RefreshToken)
	idt, err := p.Verifier(&oidc.Config{ClientID: s.Cfg.OIDC.ClientID}).Verify(ctx, rawID)
	if err != nil {
		return nil, false, &loginFailure{reason: "invalid id_token", attrs: []any{"error", safeErr(err, redact...)}}
	}
	if idt.Nonce != sess.Nonce {
		got := "different"
		if idt.Nonce == "" {
			got = "missing"
		}
		return nil, false, &loginFailure{reason: "nonce mismatch", attrs: []any{"id_token_nonce", got,
			"hint", "another sign-in was started in this browser, or the identity provider dropped the nonce"}}
	}
	claims = map[string]any{}
	if err := idt.Claims(&claims); err != nil {
		return nil, false, &loginFailure{reason: "invalid claims", attrs: []any{"error", safeErr(err, redact...)}}
	}
	groupsRead = true
	if _, ok := claims[s.Cfg.OIDC.GroupsClaim]; !ok && slices.Contains(s.Cfg.OIDC.Scopes, "groups") {
		groupsRead = mergeUserInfo(ctx, p, tok, idt.Subject, claims, redact)
	}
	if idt.Subject == "" {
		return nil, false, &loginFailure{reason: "no sub claim"}
	}
	claims["sub"] = idt.Subject
	return claims, groupsRead, nil
}

// mergeUserInfo adds the userinfo endpoint's claims (some providers only put groups there), but only when
// userinfo is about the same subject as the ID token. It returns false when userinfo could not be read (the
// request failed, the answer is not readable, or it is about another subject): what the account's groups are
// is then not known. A provider without a userinfo endpoint has said all it says in the ID token.
func mergeUserInfo(ctx context.Context, p *oidc.Provider, tok *oauth2.Token, subject string, claims map[string]any, redact []string) bool {
	if p.UserInfoEndpoint() == "" {
		return true
	}
	ui, err := p.UserInfo(ctx, oauth2.StaticTokenSource(tok))
	if err != nil {
		slog.Warn("OIDC userinfo request failed", "error", safeErr(err, redact...))
		return false
	}
	extra := map[string]any{}
	if err := ui.Claims(&extra); err != nil {
		slog.Warn("OIDC userinfo ignored", "error", safeErr(err, redact...))
		return false
	}
	if fmt.Sprint(extra["sub"]) != subject {
		slog.Warn("OIDC userinfo ignored", "error", "userinfo sub differs from the ID token's")
		return false
	}
	for k, v := range extra {
		claims[k] = v
	}
	return true
}

// startSession replaces the previous session (oldSID) with a fresh one: a new random id registered in
// sessions.json (so the CLI can revoke it) and a signed cookie holding the user. The previous session is ended
// only after the new one is registered (as in rotateSession): a sign-in that fails because sessions.json
// cannot be written leaves the session that worked alone.
func (s *Server) startSession(c *reqCtx, oldSID string, u model.User, now, authTime float64) error {
	admin := s.Poller.Admin
	sid := randToken(18)
	if err := admin.AddSession(sid, adminstate.NewSession{Sub: u.Sub, Name: u.Name, Email: u.Email, IP: c.ip(),
		UA: c.r.Header.Get("User-Agent")}); err != nil {
		slog.Error("sign-in failed: cannot register the session in sessions.json (disk full? wrong owner?)", "error", err)
		c.auditAnon("auth.login", "failed: sessions.json not writable", actorOf(&u), &u.Sub, nil)
		return apierr.New(503, "sign-in could not be recorded (the panel cannot write its session file); see the server log")
	}
	su := u
	su.Auth = "session"
	// a real sign-in: a new user may be bootstrapped (control_groups / bootstrap_admins)
	if _, err := s.recordUser(c, &su, true); err != nil {
		_ = admin.RemoveSession(sid)
		_ = s.dropSession(oldSID, "the session of a sign-in that failed") // this browser's cookie is cleared below
		c.clearSession()
		c.auditAnon("auth.login", "failed: users.json not writable", actorOf(&u), &u.Sub, nil)
		return apierr.New(503, "sign-in could not be recorded (the panel cannot write its users file); see the server log")
	}
	_ = s.dropSession(oldSID, "the session a new sign-in replaces")
	c.sess = &sessionData{SID: sid, User: &u, IAT: int64(now), Seen: int64(now), Client: s.codec.clientMark(c.client())}
	c.dirty = true
	s.codec.live.confirm(sid, authTime, c.ip(), uaHash(c.r), s.now())
	s.codec.live.touch(sid, s.now())
	s.codec.live.savedNow(sid, s.now()) // AddSession wrote the entry with this use
	slog.Info("login ok", "sub", u.Sub)
	c.auditAnon("auth.login", "ok", actorOf(&u), &u.Sub, nil)
	return nil
}

// rotateSession gives a session that just passed a step-up a new id: the new id is registered in sessions.json,
// the old one removed (with everything the panel remembered under it). The cookie's other contents stay. The
// new entry keeps when and from where the person signed in (adminstate.NewSession.Replaces); its browser and
// address are those of the client that just confirmed.
func (s *Server) rotateSession(c *reqCtx, sess *sessionData, u model.User) (string, error) {
	admin := s.Poller.Admin
	sid := randToken(18)
	if err := admin.AddSession(sid, adminstate.NewSession{Sub: u.Sub, Name: u.Name, Email: u.Email, IP: c.ip(),
		UA: c.r.Header.Get("User-Agent"), Replaces: sess.SID}); err != nil {
		slog.Error("step-up failed: cannot register the session in sessions.json (disk full? wrong owner?)", "error", err)
		c.auditAnon("auth.step_up", "failed: sessions.json not writable", actorOf(&u), &u.Sub, nil)
		return "", apierr.New(503, "the sign-in could not be recorded (the panel cannot write its session file); see the server log")
	}
	_ = s.dropSession(sess.SID, "the session id a step-up replaces")
	sess.SID = sid
	sess.Client = s.codec.clientMark(c.client()) // the session now belongs to the client that just confirmed
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
	if o := c.r.Header.Get("Origin"); o != "" && !sameOrigin(o, c.s.Cfg.BaseURL()) {
		return apierr.New(403, "cross-origin request refused")
	}
	u := c.sessionUser()
	var failed error
	if s.DemoUser == nil {
		failed = s.dropSession(c.session().SID, "a signed-out session")
		c.clearSession()
	}
	if failed != nil {
		// Not "done": the session could not leave sessions.json. This browser's cookie is cleared and the
		// session is ended in memory (dropSession); the answer, the log and the audit entry say what is left.
		if u != nil {
			c.auditAnon("auth.logout", "failed: sessions.json not writable; the session is ended in memory only", actorOf(u), &u.Sub, nil)
		}
		return apierr.New(503, "You are signed out in this browser, but the sign-out could not be recorded: the panel cannot "+
			"write sessions.json. The session is ended until the panel restarts; an administrator should look at the server log.")
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
