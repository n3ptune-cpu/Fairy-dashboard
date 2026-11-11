package web

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/audit"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
)

// The browsers of these tests (User-Agent headers as real ones send them, shortened).
const (
	uaFirefoxWin = "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:140.0) Gecko/20100101 Firefox/140.0"
	uaSafariIOS  = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.5 Mobile/15E148 Safari/604.1"
	uaChromeMac  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
	proxyPeer    = "127.0.0.1:40000" // a request that comes through the trusted proxy
)

var sessWrite = []string{"X-Requested-With", "fairy", "Content-Type", "application/json"}

// listed is the "sessions" of a Sessions page answer.
func listed(t *testing.T, r resp) []map[string]any {
	t.Helper()
	if r.code != 200 {
		t.Fatalf("the list: %d %s", r.code, r.raw)
	}
	raw, _ := r.body["sessions"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, x := range raw {
		out = append(out, x.(map[string]any))
	}
	return out
}

// handleOf is the handle the page has for the session of this cookie.
func handleOf(e *env, cookie string) string { return e.s.codec.handle(sidOf(e, cookie)) }

// revoke posts a sign-out of the session with that handle, as the page does.
func (e *env) revoke(path, cookie, handle string, hdr ...string) resp {
	return e.do("POST", path, map[string]any{"session": handle}, append(append([]string{"Cookie", cookie}, sessWrite...), hdr...)...)
}

func works(e *env, cookie string, hdr ...string) bool {
	return e.do("GET", "/api/me", nil, append([]string{"Cookie", cookie}, hdr...)...).code == 200
}

func auditOf(e *env, action string) []map[string]any {
	entries, _ := audit.Read(e.s.Cfg.AuditLog, "", action, "", nil, 500)
	return entries
}

// noneOf fails when text holds one of the secrets (session ids, cookie values, handles).
func noneOf(t *testing.T, where, text string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if s != "" && strings.Contains(text, s) {
			t.Errorf("%s holds %q", where, s)
		}
	}
}

// cookieValue is the value of a "name=value" cookie.
func cookieValue(ck string) string { _, v, _ := strings.Cut(ck, "="); return v }

// Everyone with a role sees their own sessions and nobody else's; an admin also has the list of everyone's.
// Nothing in an answer is a session id or a cookie.
func TestSessionsPageLists(t *testing.T) {
	e := setup(t)
	idp := newFakeIDP(t)
	a1 := signInFrom(e, idp, "u2", proxyPeer, "X-Forwarded-For", "203.0.113.7", "User-Agent", uaFirefoxWin)
	a2 := signInFrom(e, idp, "u2", proxyPeer, "X-Forwarded-For", "198.51.100.5", "User-Agent", uaSafariIOS)
	v := signInFrom(e, idp, "u7", proxyPeer, "User-Agent", uaChromeMac)
	p := signInFrom(e, idp, "u8", proxyPeer)
	if _, _, err := e.s.Access.SetRole("u7", "viewer", nil, "test", false); err != nil {
		t.Fatal(err)
	}
	if g := e.s.Access.Load().Users; g["u2"].Role != "admin" || g["u8"].Role != "pending" {
		t.Fatalf("roles: %+v", g)
	}
	secrets := []string{sidOf(e, a1), sidOf(e, a2), sidOf(e, v), sidOf(e, p), cookieValue(a1), cookieValue(a2), cookieValue(v)}

	r := e.do("GET", "/api/me/sessions", nil, "Cookie", a1, "X-Forwarded-For", "203.0.113.7")
	l := listed(t, r)
	if len(l) != 2 || r.body["idle_timeout_s"] != float64(14400) || r.body["max_age_s"] != float64(86400) {
		t.Fatalf("the admin's own sessions: %s", r.raw)
	}
	cur, other := l[0], l[1]
	if cur["current"] != true || cur["id"] != handleOf(e, a1) || cur["browser"] != "Firefox on Windows" || cur["user_agent"] != uaFirefoxWin ||
		cur["ip"] != "203.0.113.7" || cur["signed_in_ip"] != "203.0.113.7" || cur["signed_in"] == nil || cur["last_active"] == nil ||
		cur["location"] != "" || len(cur["also_from"].([]any)) != 0 {
		t.Fatalf("the session of the request, first: %v", cur)
	}
	if other["current"] != false || other["id"] != handleOf(e, a2) || other["browser"] != "Safari on iPhone" || other["ip"] != "198.51.100.5" ||
		other["signed_in_ip"] != "198.51.100.5" || other["last_active"] == nil {
		t.Fatalf("the other session: %v", other)
	}
	if _, has := cur["user"]; has {
		t.Fatal("one's own list names no account")
	}
	noneOf(t, "the list", string(r.raw), secrets...)

	// a viewer: their own, and only their own
	rv := e.do("GET", "/api/me/sessions", nil, "Cookie", v)
	if l := listed(t, rv); len(l) != 1 || l[0]["current"] != true || l[0]["browser"] != "Chrome on macOS" {
		t.Fatalf("a viewer's sessions: %s", rv.raw)
	}
	noneOf(t, "a viewer's list", string(rv.raw), append(secrets, handleOf(e, a1), handleOf(e, a2), "203.0.113.7", "198.51.100.5")...)

	// a person waiting for access, nobody, and an API token
	if r := e.do("GET", "/api/me/sessions", nil, "Cookie", p); r.code != 403 || !strings.Contains(string(r.raw), "pending_access") {
		t.Fatalf("waiting for access: %d %s", r.code, r.raw)
	}
	tok, _, _ := e.s.Tokens.Create("ops", "system", "control", "cli:test", 0)
	for _, path := range []string{"/api/me/sessions", "/api/admin/sessions"} {
		if r := e.do("GET", path, nil); r.code != 401 {
			t.Errorf("%s, not signed in: %d", path, r.code)
		}
		if r := e.do("GET", path, nil, "Authorization", "Bearer "+tok); r.code != 403 {
			t.Errorf("%s, an API token: %d", path, r.code)
		}
	}

	// everyone's sessions: admins only
	ra := e.do("GET", "/api/admin/sessions", nil, "Cookie", a1, "X-Forwarded-For", "203.0.113.7")
	all := listed(t, ra)
	if len(all) != 4 || all[0]["id"] != handleOf(e, a1) || all[1]["id"] != handleOf(e, a2) {
		t.Fatalf("everyone's sessions, the admin's own first: %s", ra.raw)
	}
	subs := map[string]int{}
	for _, x := range all {
		u, _ := x["user"].(map[string]any)
		subs[u["sub"].(string)]++
		if u["you"] != (u["sub"] == "u2") || u["email"] != "n@example.com" {
			t.Errorf("the account of a session: %v", u)
		}
	}
	if subs["u2"] != 2 || subs["u7"] != 1 || subs["u8"] != 1 {
		t.Fatalf("whose sessions: %v", subs)
	}
	noneOf(t, "everyone's list", string(ra.raw), secrets...)
	for name, ck := range map[string]string{"a viewer": v, "waiting for access": p} {
		if r := e.do("GET", "/api/admin/sessions", nil, "Cookie", ck); r.code != 403 || strings.Contains(string(r.raw), "sessions\"") {
			t.Errorf("everyone's sessions, %s: %d %s", name, r.code, r.raw)
		}
	}
	if d := auditOf(e, "auth.denied"); len(d) != 1 || d[0]["sub"] != "u7" || d[0]["target"] != "/api/admin/sessions" {
		t.Fatalf("the viewer's attempt is in the audit log: %v", d)
	}
}

// Ending another session always asks "Confirm it's you", for that one action: not inside the window of an
// earlier confirmation, not by a confirmation made for another action, and not from a copy of the cookie
// elsewhere. After the confirmation the other session is over at once, in every copy.
func TestEndingAnotherSessionNeedsItsOwnConfirmation(t *testing.T) {
	logs := captureLog(t)
	a := signedIn(t) // u1, admin; signed in an hour ago
	e := a.e
	first := a.sess
	b := signInFrom(e, a.idp, "u1", proxyPeer, "User-Agent", uaFirefoxWin)
	hB := handleOf(e, b)
	secrets := []string{sidOf(e, first), sidOf(e, b), hB, cookieValue(first), cookieValue(b)}

	if r := a.stepUp("&next=/", false); r.code != 303 {
		t.Fatal(r.code)
	}
	if r := e.poll(a.sess); r.code != 200 {
		t.Fatal("inside the window of a confirmation a control action runs", r.code)
	}
	// ... and a sign-out of another session still asks
	r := e.revoke("/api/me/sessions/revoke", a.sess, hB, "X-Fairy-Action", "Sign out a browser")
	d, _ := r.body["detail"].(map[string]any)
	if !stepUpRequired(r) || d["pending"] != true || d["per_action"] != true ||
		d["action"] != "sign out another session of yours (Firefox on Windows, 127.0.0.1)" || !works(e, b) {
		t.Fatalf("inside the window: %d %s", r.code, r.raw)
	}
	noneOf(t, "the refusal", string(r.raw), secrets...)
	// a copy of the cookie at another address: asked as well, and nothing is kept for it
	c := e.revoke("/api/me/sessions/revoke", a.sess, hB, otherAddr...)
	if cd, _ := c.body["detail"].(map[string]any); !stepUpRequired(c) || cd["pending"] != false || cd["not_kept"] != "new_client" {
		t.Fatalf("a copy of the cookie elsewhere: %d %s", c.code, c.raw)
	}
	if c := e.revoke("/api/me/sessions/revoke-others", a.sess, "", otherAddr...); !stepUpRequired(c) || !works(e, b) {
		t.Fatalf("a copy of the cookie, everywhere else: %d %s", c.code, c.raw)
	}

	// a confirmation for another action: the shutdown takes the sign-out's place and runs; no session ends
	a.clk.advance(time.Duration(e.s.Cfg.StepUpWindowS+5) * time.Second)
	id := a.shutdown()
	if id == "" || id == pendingID(r) {
		t.Fatal("the shutdown must be kept in place of the sign-out")
	}
	if cb := a.confirm(pendingID(r), false); cb.code != 409 {
		t.Fatal("the replaced sign-out can still be confirmed", cb.code)
	}
	id = a.shutdown()
	if cb := a.confirm(id, false); cb.code != 303 || a.powerCalls() != 1 {
		t.Fatal("the shutdown", cb.code, a.powerCalls())
	}
	if !works(e, b) || len(auditOf(e, "sessions")) != 0 || len(sessionsOf(e, "u1")) != 2 {
		t.Fatal("a confirmation for a shutdown ended a session")
	}
	// that confirmation is seconds old; the sign-out asks again, for itself
	r = e.revoke("/api/me/sessions/revoke", a.sess, hB, "X-Fairy-Action", "Sign out a browser")
	if !stepUpRequired(r) || pendingID(r) == "" {
		t.Fatalf("right after another confirmation: %d %s", r.code, r.raw)
	}
	code, stream := e.openStream("Cookie", b)
	if code != 200 {
		t.Fatal("stream", code)
	}
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal("the confirmation", cb.code, string(cb.raw))
	}
	if works(e, b) || works(e, b, otherAddr...) || works(e, b, otherBrowser...) {
		t.Fatal("the signed-out session still works")
	}
	if !ended(stream, 5*time.Second) {
		t.Fatal("the open live stream of the signed-out session goes on")
	}
	me := e.do("GET", "/api/me", nil, "Cookie", a.sess)
	pr, _ := me.body["pending_result"].(map[string]any)
	if me.code != 200 || pr["ok"] != true || pr["message"] != "Signed out: Firefox on Windows, 127.0.0.1" || pr["label"] != "Sign out a browser" {
		t.Fatalf("the result for the page: %s", me.raw)
	}
	if s := sessionsOf(e, "u1"); len(s) != 1 || s[0].SID != sidOf(e, a.sess) {
		t.Fatalf("what is left: %+v", s)
	}

	// the audit log: who, from where, whose session; the confirmation names what it runs
	got := auditOf(e, "sessions")
	if len(got) != 1 {
		t.Fatalf("audit: %v", got)
	}
	x, params := got[0], got[0]["params"].(map[string]any)
	if x["action"] != "sessions.revoke" || x["outcome"] != "ok" || x["actor"] != "n@example.com (unverified)" || x["sub"] != "u1" || x["ip"] != "127.0.0.1" ||
		x["source"] != "ui" || x["target"] != "n@example.com" || params["sub"] != "u1" || params["own"] != true ||
		params["browser"] != "Firefox on Windows" || params["ip"] != "127.0.0.1" {
		t.Fatalf("the audit entry: %v", x)
	}
	runs := false
	for _, s := range auditOf(e, "auth.step_up") {
		p, _ := s["params"].(map[string]any)
		if p["runs"] == "sign out another session of yours (Firefox on Windows, 127.0.0.1)" {
			runs = true
		}
	}
	if !runs {
		t.Fatal("the confirmation's audit entry does not name the sign-out")
	}
	secrets = append(secrets, sidOf(e, a.sess), cookieValue(a.sess), handleOf(e, a.sess))
	raw, _ := os.ReadFile(e.s.Cfg.AuditLog)
	noneOf(t, "the audit log", string(raw), secrets...)
	noneOf(t, "the server log", logs.take(), secrets...)

	// what is refused before anything is asked: one's own session, a session that is not there
	if r := e.revoke("/api/me/sessions/revoke", a.sess, handleOf(e, a.sess)); r.code != 409 {
		t.Fatalf("the session of the request: %d %s", r.code, r.raw)
	}
	for _, h := range []string{hB, "nothing", strings.Repeat("A", 22)} {
		if r := e.revoke("/api/me/sessions/revoke", a.sess, h); r.code != 404 {
			t.Fatalf("a session that is not there (%q): %d %s", h, r.code, r.raw)
		}
	}
	for _, body := range []any{map[string]any{}, map[string]any{"session": 5}, map[string]any{"session": "x", "sid": "y"},
		map[string]any{"session": strings.Repeat("A", 65)}} {
		r := e.do("POST", "/api/me/sessions/revoke", body, append([]string{"Cookie", a.sess}, sessWrite...)...)
		if r.code != 404 && r.code != 422 {
			t.Errorf("body %v: %d %s", body, r.code, r.raw)
		}
	}
}

// "Sign out everywhere else" ends every other session of the account, never the one that asked (which gets a
// new id from the confirmation, like after every "Confirm it's you") and nobody else's.
func TestSignOutEverywhereElse(t *testing.T) {
	a := signedIn(t)
	e := a.e
	b := signInFrom(e, a.idp, "u1", proxyPeer, "User-Agent", uaFirefoxWin)
	c := signInFrom(e, a.idp, "u1", proxyPeer, "User-Agent", uaSafariIOS)
	other := signInFrom(e, a.idp, "u7", proxyPeer)
	if _, _, err := e.s.Access.SetRole("u7", "viewer", nil, "test", false); err != nil {
		t.Fatal(err)
	}
	a.idp.claims["sub"] = "u1" // from here on the identity provider confirms the admin again
	before := sidOf(e, a.sess)
	r := e.do("POST", "/api/me/sessions/revoke-others", nil, append([]string{"Cookie", a.sess}, sessWrite...)...)
	d, _ := r.body["detail"].(map[string]any)
	if !stepUpRequired(r) || d["action"] != "sign out all your other sessions" || d["per_action"] != true || !works(e, b) || !works(e, c) {
		t.Fatalf("asked to confirm first: %d %s", r.code, r.raw)
	}
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal(cb.code, string(cb.raw))
	}
	me := e.do("GET", "/api/me", nil, "Cookie", a.sess) // the result is handed out once
	if pr, _ := me.body["pending_result"].(map[string]any); pr["ok"] != true || pr["message"] != "Signed out everywhere else (2 sessions)." {
		t.Fatalf("the result: %s", me.raw)
	}
	if works(e, b) || works(e, c) {
		t.Fatal("another session of the account still works")
	}
	if !works(e, a.sess) || !works(e, other) {
		t.Fatal("the session that asked, or another person's, was ended")
	}
	now := sidOf(e, a.sess)
	if s := sessionsOf(e, "u1"); len(s) != 1 || s[0].SID != now || now == before || len(sessionsOf(e, "u7")) != 1 {
		t.Fatalf("what is left: %+v", s)
	}
	got := auditOf(e, "sessions")
	if len(got) != 1 || got[0]["action"] != "sessions.revoke_others" || got[0]["outcome"] != "ok" || got[0]["sub"] != "u1" ||
		got[0]["params"].(map[string]any)["count"] != float64(2) {
		t.Fatalf("audit: %v", got)
	}
	// with no other session there is nothing to confirm, and nothing happens
	r = e.do("POST", "/api/me/sessions/revoke-others", nil, append([]string{"Cookie", a.sess}, sessWrite...)...)
	if r.code != 200 || r.body["count"] != float64(0) || !works(e, a.sess) || len(auditOf(e, "sessions")) != 1 {
		t.Fatalf("signed in nowhere else: %d %s", r.code, r.raw)
	}
	// the viewer has the action too, for their own sessions
	v2 := signInFrom(e, a.idp, "u7", proxyPeer)
	r = e.do("POST", "/api/me/sessions/revoke-others", nil, append([]string{"Cookie", other}, sessWrite...)...)
	if !stepUpRequired(r) || !works(e, v2) {
		t.Fatalf("a viewer: %d %s", r.code, r.raw)
	}
}

// A handle is not a session id and not a cookie, cannot be made up, and names a session only for the person
// who may end it: a handle of someone else's session is nothing on one's own routes, and the route for
// everyone's sessions is for admins.
func TestSessionHandlesAreOpaqueAndNotTransferable(t *testing.T) {
	a := signedIn(t)
	e := a.e
	b := signInFrom(e, a.idp, "u1", proxyPeer, "User-Agent", uaFirefoxWin)
	v := signInFrom(e, a.idp, "u7", proxyPeer, "User-Agent", uaChromeMac)
	v2 := signInFrom(e, a.idp, "u7", proxyPeer, "User-Agent", uaSafariIOS)
	if _, _, err := e.s.Access.SetRole("u7", "viewer", nil, "test", false); err != nil {
		t.Fatal(err)
	}
	a.idp.claims["sub"] = "u1" // from here on the identity provider confirms the admin again
	sid, h := sidOf(e, b), handleOf(e, b)
	if len(h) != 22 || h == sid || strings.Contains(h, sid[:6]) || strings.Contains(sid, h[:6]) || strings.Contains(cookieValue(b), h) {
		t.Fatalf("the handle %q and the session id must have nothing in common", h)
	}
	if h != handleOf(e, b) || h == handleOf(e, v) {
		t.Fatal("a handle is the same every time, and another one per session")
	}
	// another panel (another session secret) has other handles for the same id
	if newCodec(strings.Repeat("t", 48), config.Seconds(60), true).handle(sid) == h {
		t.Fatal("the handle does not depend on the session secret")
	}
	// the viewer with the admin's handle, a session id, part of one, the handle of their own current session
	for name, try := range map[string]string{"an admin's handle": h, "a session id": sid, "the CLI's prefix": sid[:8],
		"their own session's id": sidOf(e, v2), "a changed handle": h[:21] + "A"} {
		r := e.revoke("/api/me/sessions/revoke", v, try)
		if r.code != 404 || stepUpRequired(r) {
			t.Errorf("a viewer with %s: %d %s", name, r.code, r.raw)
		}
		if r := e.revoke("/api/admin/sessions/revoke", v, try); r.code != 403 {
			t.Errorf("a viewer on the admin route with %s: %d %s", name, r.code, r.raw)
		}
	}
	if !works(e, b) || !works(e, a.sess) || !works(e, v2) || keptID(e, v) != "" {
		t.Fatal("something was ended or kept")
	}
	// their own other session: asked to confirm, like anyone (from the browser the viewer signed in with)
	if r := e.revoke("/api/me/sessions/revoke", v, handleOf(e, v2), "User-Agent", uaChromeMac); !stepUpRequired(r) || keptID(e, v) == "" {
		t.Fatalf("a viewer's own other session: %d %s", r.code, r.raw)
	}
	// the admin ends the viewer's session through the admin route, not through their own
	if r := e.revoke("/api/me/sessions/revoke", a.sess, handleOf(e, v)); r.code != 404 {
		t.Fatalf("another person's session on one's own route: %d %s", r.code, r.raw)
	}
	if r := e.revoke("/api/admin/sessions/revoke", a.sess, handleOf(e, a.sess)); r.code != 409 {
		t.Fatalf("the admin's own current session: %d %s", r.code, r.raw)
	}
	r := e.revoke("/api/admin/sessions/revoke", a.sess, handleOf(e, v))
	d, _ := r.body["detail"].(map[string]any)
	if !stepUpRequired(r) || d["action"] != "sign out a session (of n@example.com: Chrome on macOS, 127.0.0.1)" {
		t.Fatalf("an admin, another person's session: %d %s", r.code, r.raw)
	}
	if cb := a.confirm(pendingID(r), false); cb.code != 303 || works(e, v) || !works(e, v2) || !works(e, b) || !works(e, a.sess) {
		t.Fatal("after the confirmation", cb.code)
	}
	got := auditOf(e, "sessions")
	if len(got) != 1 || got[0]["sub"] != "u1" || got[0]["target"] != "n@example.com" || got[0]["params"].(map[string]any)["sub"] != "u7" ||
		got[0]["params"].(map[string]any)["own"] != false {
		t.Fatalf("audit: whose session it was: %v", got)
	}
}

// A copy of a session cookie used from somewhere else is the same session, so it is not another line of the
// list. It shows as another address of that session.
func TestCopiedCookieShowsAsAnotherAddress(t *testing.T) {
	e := setup(t)
	idp := newFakeIDP(t)
	ck := signInFrom(e, idp, "u2", proxyPeer, "X-Forwarded-For", "203.0.113.7", "User-Agent", uaFirefoxWin)
	own := []string{"Cookie", ck, "X-Forwarded-For", "203.0.113.7"}
	if l := listed(t, e.do("GET", "/api/me/sessions", nil, own...)); len(l) != 1 || len(l[0]["also_from"].([]any)) != 0 {
		t.Fatalf("one browser, one address: %v", l)
	}
	if !works(e, ck, "X-Forwarded-For", "198.51.100.66") { // the copy: an unmarked read, like a script
		t.Fatal("the copy")
	}
	l := listed(t, e.do("GET", "/api/me/sessions", nil, own...))
	also, _ := l[0]["also_from"].([]any)
	if len(l) != 1 || l[0]["ip"] != "203.0.113.7" || len(also) != 1 || also[0].(map[string]any)["ip"] != "198.51.100.66" {
		t.Fatalf("the copy's address: %v", l)
	}
}

// sessions.json of the release before the Sessions page: the page lists its sessions, with "not known" for
// what that file does not have.
func TestSessionsPageWithAnOldSessionsFile(t *testing.T) {
	e := setup(t)
	e.signIn(controller())
	ts := time.Now().Unix() - 3600
	_, err := statefile.Update(e.s.Poller.Admin.SessionsPath, func(d statefile.Data) error {
		rec := func(ip string) map[string]any {
			return map[string]any{"sub": "u2", "name": "Alice", "email": "n@example.com", "ip": ip, "gen": 2,
				"created": "2026-11-11T08:00:00Z", "created_ts": float64(ts)}
		}
		d["sessions"] = map[string]any{"sid-old-one-0001": rec("203.0.113.7"), "sid-old-two-0002": rec("198.51.100.5")}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = e.s.Poller.Admin.RemoveSession("sid-none") // the panel reads the file again
	u := controller()
	rec := newRecorderCookie(e, &sessionData{SID: "sid-old-one-0001", User: &u, IAT: ts, Seen: ts + 60})
	r := e.do("GET", "/api/me/sessions", nil, "Cookie", rec)
	l := listed(t, r)
	if len(l) != 2 {
		t.Fatalf("%s", r.raw)
	}
	cur, other := l[0], l[1]
	if cur["current"] != true || cur["browser"] != "Unknown browser" || cur["user_agent"] != "" || cur["signed_in_ip"] != "203.0.113.7" ||
		cur["signed_in"] == nil || cur["ip"] != "127.0.0.1" {
		t.Fatalf("the old session in use: %v", cur)
	}
	if other["current"] != false || other["browser"] != "Unknown browser" || other["user_agent"] != "" || other["last_active"] != nil ||
		other["ip"] != "" || other["signed_in_ip"] != "198.51.100.5" || other["signed_in"] == nil {
		t.Fatalf("the old session nobody used since: %v", other)
	}
	if r := e.revoke("/api/me/sessions/revoke", rec, other["id"].(string)); !stepUpRequired(r) {
		t.Fatalf("an old session can be signed out like any other: %d %s", r.code, r.raw)
	}
}

// newRecorderCookie is the cookie of a session that is already registered.
func newRecorderCookie(e *env, sd *sessionData) string {
	rec := httptest.NewRecorder()
	e.s.codec.write(rec, sd)
	return strings.Split(rec.Header().Get("Set-Cookie"), ";")[0]
}

// A session whose cookie the panel would refuse is not on the list: one that was not used for
// session_idle_timeout (by the panel's memory, or by sessions.json for one it has not seen since it started),
// and one that is older than session_max_age.
func TestSessionsThatCannotBeUsedAreNotListed(t *testing.T) {
	e := setup(t)
	clk := e.fakeClock()
	a, b, c := e.cookie(controller(), 0), e.cookie(controller(), 0), e.cookie(controller(), 0)
	active := func(ck string) {
		if r := e.do("GET", "/api/me", nil, "Cookie", ck, "X-Fairy-Active", "1"); r.code != 200 {
			t.Fatal(r.code)
		}
	}
	ids := func() map[string]bool {
		out := map[string]bool{}
		for _, x := range listed(t, e.do("GET", "/api/me/sessions", nil, "Cookie", a, "X-Fairy-Active", "1")) {
			out[x["id"].(string)] = true
		}
		return out
	}
	active(b)
	if l := ids(); len(l) != 3 {
		t.Fatal("three sessions", l)
	}
	idle := time.Duration(e.s.Cfg.SessionIdleTimeout) * time.Second
	clk.advance(idle / 2)
	active(a)
	clk.advance(idle/2 + time.Minute)
	// b was used and then left alone for longer than the idle timeout: the panel knows. c was not used since
	// its sign-in, which sessions.json has: one minute over the timeout it is still listed (the cookie's own
	// copy of the last use may be newer than the file's by that much), three minutes over it is not
	if l := ids(); len(l) != 2 || l[handleOf(e, b)] || !l[handleOf(e, c)] {
		t.Fatalf("one minute after the idle timeout: %v", l)
	}
	clk.advance(2 * time.Minute)
	if l := ids(); len(l) != 1 || !l[handleOf(e, a)] || works(e, b) || works(e, c) {
		t.Fatalf("three minutes after the idle timeout: %v", l)
	}
	// with no usable session elsewhere, "everywhere else" asks nothing and does nothing
	r := e.do("POST", "/api/me/sessions/revoke-others", nil, append([]string{"Cookie", a}, sessWrite...)...)
	if r.code != 200 || r.body["count"] != float64(0) {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	// a session from before the Sessions page that nobody used since: no last use is known, so it is listed,
	// until its sign-in is longer ago than session_max_age
	d := e.cookie(controller(), 0)
	edit := func(fn func(rec map[string]any)) {
		t.Helper()
		_, err := statefile.Update(e.s.Poller.Admin.SessionsPath, func(f statefile.Data) error {
			fn(f["sessions"].(map[string]any)[sidOf(e, d)].(map[string]any))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = e.s.Poller.Admin.RemoveSession("sid-none") // the panel reads the file again
	}
	edit(func(rec map[string]any) {
		for _, k := range []string{"ua", "login_ts", "login_ip", "seen_ts", "seen_ip"} {
			delete(rec, k)
		}
		rec["created_ts"] = float64(clk.now().Unix() - int64(e.s.Cfg.SessionMaxAge) + 600)
	})
	if l := ids(); len(l) != 2 || !l[handleOf(e, d)] {
		t.Fatalf("an old entry inside session_max_age: %v", l)
	}
	edit(func(rec map[string]any) {
		rec["created_ts"] = float64(clk.now().Unix() - int64(e.s.Cfg.SessionMaxAge) - 60)
	})
	if l := ids(); len(l) != 1 || !l[handleOf(e, a)] {
		t.Fatalf("after session_max_age: %v", l)
	}
}

// The demo has no sign-in: the page is empty there, and there is nothing to sign out.
func TestSessionsPageInTheDemo(t *testing.T) {
	e := setup(t)
	u := controller()
	u.Auth = "demo"
	e.s.DemoUser = &u
	for _, path := range []string{"/api/me/sessions", "/api/admin/sessions"} {
		r := e.do("GET", path, nil)
		if l := listed(t, r); len(l) != 0 || !strings.Contains(string(r.raw), `"sessions":[]`) {
			t.Fatalf("%s: %s", path, r.raw)
		}
	}
	for _, path := range []string{"/api/me/sessions/revoke", "/api/me/sessions/revoke-others", "/api/admin/sessions/revoke"} {
		if r := e.do("POST", path, map[string]any{"session": "x"}, sessWrite...); r.code != 409 {
			t.Fatalf("%s: %d %s", path, r.code, r.raw)
		}
	}
}

func TestBrowserOf(t *testing.T) {
	for ua, want := range map[string]string{
		uaFirefoxWin: "Firefox on Windows",
		uaSafariIOS:  "Safari on iPhone",
		uaChromeMac:  "Chrome on macOS",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36 Edg/140.0.0.0":       "Edge on Windows",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36 OPR/120.0.0.0":       "Opera on Windows",
		"Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0":                                                              "Firefox on Linux",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/140.0.0.0 Safari/537.36":                       "Chrome (headless) on Linux",
		"Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Mobile Safari/537.36":               "Chrome on Android",
		"Mozilla/5.0 (Linux; Android 15; SM-S938B) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/28.0 Chrome/130.0.0.0 Safari/537.36": "Samsung Internet on Android",
		"Mozilla/5.0 (Android 15; Mobile; rv:140.0) Gecko/140.0 Firefox/140.0":                                                                "Firefox on Android",
		"Mozilla/5.0 (iPad; CPU OS 18_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/140.0.0.0 Mobile/15E148 Safari/604.1":   "Chrome on iPad",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.5 Safari/605.1.15":               "Safari on macOS",
		"Mozilla/5.0 (X11; CrOS x86_64 16000.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36":                      "Chrome on ChromeOS",
		"curl/8.5.0":                        "curl",
		"Go-http-client/1.1":                "Go program",
		"python-requests/2.32.0":            "Python program",
		"Mozilla/5.0 (Windows NT 10.0)":     "Unknown browser on Windows",
		"":                                  "Unknown browser",
		"<img src=x onerror=alert(1)>":      "Unknown browser",
		"Firefox":                           "Unknown browser",
		strings.Repeat("Chrome", 40) + "/1": "Chrome",
	} {
		if got := browserOf(ua); got != want {
			t.Errorf("%q: %q, want %q", ua, got, want)
		}
	}
	// whatever the header is, the answer is one of the fixed words: nothing of the header itself
	for _, ua := range []string{"<script>Firefox/1 Windows NT</script>", "Chrome/\"><b>", "Edg/' onload='x"} {
		if got := browserOf(ua); strings.ContainsAny(got, "<>\"'/") {
			t.Errorf("%q gave %q", ua, got)
		}
	}
}
