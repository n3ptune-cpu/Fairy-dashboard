//go:build fairytest

package web

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// testPanel is a server in test mode, as cmd/fairy sets one up in a test build: the config's control group
// is the test admin's.
func testPanel(t *testing.T) *env {
	t.Helper()
	e := setup(t)
	e.s.Cfg.OIDC.ControlGroups = []string{TestGroup}
	e.s.EnableTestMode()
	return e
}

// open loads the page like a browser does and returns the session cookie it was given.
func (e *env) open() string {
	e.t.Helper()
	r := e.do("GET", "/", nil)
	ck := strings.Split(r.hdr.Get("Set-Cookie"), ";")[0]
	if r.code != 200 || !strings.Contains(ck, "fairy_session=") {
		e.t.Fatalf("the page did not start a session: %d, Set-Cookie %q", r.code, r.hdr.Get("Set-Cookie"))
	}
	return ck
}

// Rules 1, 4 and 5 of the test build: the page asks for no sign-in and carries the banner; whoever opens it is
// the built-in admin, with a real session.
func TestTestModeSignsInWithoutPocketIDAndShowsTheBanner(t *testing.T) {
	e := testPanel(t)
	r := e.do("GET", "/", nil)
	body := string(r.raw)
	if i := strings.Index(body, "<body>"); r.code != 200 || i < 0 || !strings.HasPrefix(body[i+6:], testBannerHTML) || strings.Count(body, TestBanner) != 1 {
		t.Fatalf("the banner must be the first thing in the page, once: %d %.300s", r.code, body)
	}
	if TestBanner != "TEST BUILD: sign-in is off. Never expose this." {
		t.Fatal("the banner's text is what was agreed:", TestBanner)
	}
	ck := e.open()
	me := e.do("GET", "/api/me", nil, "Cookie", ck)
	if me.code != 200 || me.body["sub"] != "test-admin" || me.body["role"] != "admin" || me.body["can_control"] != true ||
		me.body["auth"] != "session" || me.body["demo"] != false {
		t.Fatalf("/api/me: %d %s", me.code, me.raw)
	}
	// a real session: listed like any other, ended by Sign out, and the sign-in link makes a new one
	if ss, err := e.s.Poller.Admin.ListSessions(); err != nil || len(ss) == 0 || ss[len(ss)-1].Sub != TestUser.Sub {
		t.Fatal("the session is not in sessions.json", ss, err)
	}
	if out := e.do("POST", "/auth/logout", nil, "Cookie", ck, "X-Requested-With", "fairy", "Accept", "application/json"); out.code != 204 {
		t.Fatal("sign out", out.code, string(out.raw))
	}
	if e.do("GET", "/api/me", nil, "Cookie", ck).code != 401 {
		t.Fatal("the signed-out session still works")
	}
	in := e.do("GET", "/auth/login?next=/%23/users", nil)
	if in.code != 303 || in.hdr.Get("Location") != "/#/users" || !strings.Contains(in.hdr.Get("Set-Cookie"), "fairy_session=") {
		t.Fatalf("/auth/login must sign in here and go back: %d %q", in.code, in.hdr.Get("Location"))
	}
	// the other HTML pages carry the banner too (/docs is on in the test config)
	if d := e.do("GET", "/docs", nil, "Cookie", e.open()); d.code != 200 || !strings.Contains(string(d.raw), "<body>"+testBannerHTML) {
		t.Fatalf("/docs without the banner: %d %.200s", d.code, d.raw)
	}
	if css := e.do("GET", "/fairytest.css", nil); css.code != 200 || !strings.Contains(string(css.raw), "#fairytest-banner") {
		t.Fatal("the banner's stylesheet", css.code)
	}
	// what the test build's script renames must exist in the page it runs in
	app, _ := fs.ReadFile(staticFS, "static/app.js")
	index, _ := fs.ReadFile(staticFS, "static/index.html")
	for _, id := range []string{`id="su-go"`, `id="su-note"`} {
		if !strings.Contains(string(app), id) {
			t.Errorf("app.js has no %s any more: testJS renames nothing", id)
		}
	}
	if !strings.Contains(string(index), `<section id="login"`) || !strings.Contains(testJS, `"#login a.btn"`) {
		t.Error("index.html's sign-in view and testJS do not match any more")
	}
}

// Rule 5: a control action is still refused until "Confirm it's you" is confirmed; the dialog's button confirms
// it here, the kept action runs once, and the session gets a new id as after a real step-up.
func TestTestModeConfirmsStepUpLocally(t *testing.T) {
	e := testPanel(t)
	ck := e.open()
	r := e.poll(ck, "X-Fairy-Action", "Poll laptop every 5 s")
	d, _ := r.body["detail"].(map[string]any)
	if !stepUpRequired(r) || d["pending"] != true || d["action"] != "polling of laptop" {
		t.Fatalf("a fresh test session must be asked to confirm, and the action kept: %d %s", r.code, r.raw)
	}
	id, _ := d["pending_id"].(string)
	body := map[string]any{"pending_id": id, "next": "/#/hosts"}
	if x := e.do("POST", stepUpPath, body, "Cookie", ck); x.code != 403 {
		t.Fatal("the confirmation is a write: it needs the CSRF header", x.code)
	}
	ok := e.do("POST", stepUpPath, body, "Cookie", ck, "X-Requested-With", "fairy")
	if ok.code != 200 || ok.body["url"] != "/#/hosts" || ok.body["action"] != "polling of laptop" {
		t.Fatalf("confirm: %d %s", ok.code, ok.raw)
	}
	ck2 := strings.Split(ok.hdr.Get("Set-Cookie"), ";")[0]
	if ck2 == "" || ck2 == ck || e.do("GET", "/api/me", nil, "Cookie", ck).code != 401 {
		t.Fatal("the step-up must give the session a new id and end the old one")
	}
	me := e.do("GET", "/api/me", nil, "Cookie", ck2)
	pr, _ := me.body["pending_result"].(map[string]any)
	if me.code != 200 || pr["ok"] != true || pr["label"] != "Poll laptop every 5 s" || me.body["auth_age_s"].(float64) > 60 {
		t.Fatalf("the kept action must have run once: %d %s", me.code, me.raw)
	}
	if again := e.do("POST", stepUpPath, body, "Cookie", ck2, "X-Requested-With", "fairy"); again.code != 409 {
		t.Fatal("a kept action runs once", again.code)
	}
	if r := e.poll(ck2); r.code != 200 {
		t.Fatal("inside the window after the confirmation", r.code, string(r.raw))
	}
	found := false
	for _, a := range e.auditEntries("", "auth.step_up") {
		found = found || strings.Contains(a["outcome"].(string), "test build: confirmed locally")
	}
	if !found {
		t.Fatal("the audit log must say the confirmation was a test build's")
	}
	// the dialog's link (an action that was not kept): confirmed here too, and nothing runs
	ck3 := e.open()
	if l := e.do("GET", "/auth/login?step_up=1&next=/%23/x", nil, "Cookie", ck3); l.code != 303 || l.hdr.Get("Location") != "/#/x" {
		t.Fatal("step-up link", l.code, l.hdr.Get("Location"))
	}
}

// Rule 6: Add host is the real one. Here to a machine that is not on Tailscale (enroll.allow_non_tailscale),
// confirmed with the test build's button, then paired with a (fake) machine and polled.
func TestTestModeAddHost(t *testing.T) {
	e := testPanel(t)
	e.s.Enroll.Interval = 20 * time.Millisecond
	e.s.Enroll.PanelAddr = "192.168.50.10"
	e.s.Enroll.Pair = func(_ context.Context, url string, _ []byte) (int, enroll.Response, string, error) {
		if url != "http://192.168.50.20:9101" {
			t.Error("paired with", url)
		}
		return 200, enroll.Response{AgentVersion: "16-a791c55", Platform: "windows", Token: strings.Repeat("r", 32), ControlToken: strings.Repeat("c", 32)}, "", nil
	}
	e.b.Mu.Lock()
	e.b.Metrics["test-vm"] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
	e.b.Mu.Unlock()
	add := func(ck string) (resp, string) {
		body := map[string]any{"name": "TEST-VM", "os": "windows", "address": "192.168.50.20", "options": map[string]any{"power_control": true, "self_update": true}}
		r := e.do("POST", "/api/admin/hosts", body, "Cookie", ck, "X-Requested-With", "fairy")
		d, _ := r.body["detail"].(map[string]any)
		if !stepUpRequired(r) || d["action"] != "Add host (name: TEST-VM)" {
			t.Fatalf("Add host must ask to confirm first: %d %s", r.code, r.raw)
		}
		ok := e.do("POST", stepUpPath, map[string]any{"pending_id": d["pending_id"], "next": "/"}, "Cookie", ck, "X-Requested-With", "fairy")
		ck = strings.Split(ok.hdr.Get("Set-Cookie"), ";")[0]
		return e.do("GET", "/api/me", nil, "Cookie", ck), ck
	}
	// the Tailscale-only rule is the real panel's: without the switch the address is refused
	me, _ := add(e.open())
	if pr, _ := me.body["pending_result"].(map[string]any); pr["ok"] != false || !strings.Contains(pr["message"].(string), "allow_non_tailscale") {
		t.Fatalf("an address outside Tailscale without the switch: %s", me.raw)
	}
	e.s.Enroll.AllowNonTailscale = true
	me, ck := add(e.open())
	pr, _ := me.body["pending_result"].(map[string]any)
	id, _ := pr["ref"].(string)
	if pr["ok"] != true || id == "" {
		t.Fatalf("Add host: %s", me.raw)
	}
	st := ""
	for i := 0; i < 200 && st != "online"; i++ {
		time.Sleep(20 * time.Millisecond)
		e.s.Poller.PollOnce(context.Background())
		st, _ = e.do("GET", "/api/admin/enrollments/"+id, nil, "Cookie", ck).body["state"].(string)
	}
	if a, ok := e.s.Poller.AgentByID("test-vm"); st != "online" || !ok || !a.Managed || !a.PowerControl || !a.SelfUpdate {
		t.Fatalf("the host must be paired and online: state %q, %+v", st, a)
	}
}

// Rule 2, for every request: only on a loopback socket, only for a loopback host name, never through a proxy.
func TestTestModeAnswersThisPCOnly(t *testing.T) {
	e := testPanel(t)
	for name, hdr := range map[string][]string{
		"another host name": {"Host", "panel.example.com"},
		"X-Forwarded-For":   {"X-Forwarded-For", "203.0.113.9"},
		"X-Real-IP":         {"X-Real-IP", "203.0.113.9"},
		"Forwarded":         {"Forwarded", "for=203.0.113.9"},
	} {
		for _, path := range []string{"/", "/api/me", "/auth/login", "/healthz"} {
			req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
			if hdr[0] == "Host" {
				req.Host = hdr[1]
			} else {
				req.Header.Set(hdr[0], hdr[1])
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != 403 || res.Header.Get("Set-Cookie") != "" {
				t.Errorf("%s, %s: %d, want 403 and no session", name, path, res.StatusCode)
			}
		}
	}
	// a request that did not arrive on a loopback socket (here: on none at all)
	rec := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "http://127.0.0.1/", nil))
	if rec.Code != 403 || strings.Contains(rec.Body.String(), "<body>") {
		t.Fatal("not on a loopback socket:", rec.Code)
	}
	if r := e.do("GET", "/healthz", nil); r.code != 200 {
		t.Fatal("this PC is answered", r.code)
	}
}

// The tag alone changes nothing: a server nobody called EnableTestMode on is the real panel (the whole test
// suite of this package runs under the tag as well and says the same).
func TestTestModeIsOffUntilEnabled(t *testing.T) {
	e := setup(t)
	r := e.do("GET", "/", nil)
	if r.code != 200 || strings.Contains(string(r.raw), "fairytest") || r.hdr.Get("Set-Cookie") != "" {
		t.Fatal("a plain server shows the banner or signs in")
	}
	if e.do("GET", "/fairytest.css", nil).code != 404 || e.do("GET", "/api/me", nil).code != 401 {
		t.Fatal("a plain server has test routes, or no sign-in")
	}
}

// The Sessions page in a test build: two browsers that open the page are two sessions of the test admin; one
// signs the other out after "Confirm it's you" (confirmed here), and a window that was signed out signs in
// again as a new session when it is loaded again.
func TestTestModeSessionsPage(t *testing.T) {
	e := testPanel(t)
	a, b := e.open(), e.open()
	l := listed(t, e.do("GET", "/api/me/sessions", nil, "Cookie", a))
	if len(l) != 2 || l[0]["current"] != true || l[0]["id"] != handleOf(e, a) || l[1]["id"] != handleOf(e, b) || l[1]["browser"] != "Go program" {
		t.Fatalf("two browsers, two sessions: %v", l)
	}
	if all := listed(t, e.do("GET", "/api/admin/sessions", nil, "Cookie", a)); len(all) != 2 || all[0]["user"].(map[string]any)["sub"] != "test-admin" {
		t.Fatalf("everyone's sessions: %v", all)
	}
	confirm := func(ck string, r resp) string {
		t.Helper()
		d, _ := r.body["detail"].(map[string]any)
		if !stepUpRequired(r) || d["pending"] != true {
			t.Fatalf("must be asked to confirm, and kept: %d %s", r.code, r.raw)
		}
		c := e.do("POST", stepUpPath, map[string]any{"pending_id": pendingID(r), "next": "/#/sessions"}, append([]string{"Cookie", ck}, sessWrite...)...)
		if c.code != 200 || c.body["url"] != "/#/sessions" {
			t.Fatalf("the dialog's button: %d %s", c.code, c.raw)
		}
		return sessionCookie(e, c.hdr)
	}
	r := e.revoke("/api/me/sessions/revoke", a, handleOf(e, b))
	if d, _ := r.body["detail"].(map[string]any); d["action"] != "sign out another session of yours (Go program, 127.0.0.1)" {
		t.Fatalf("what the dialog names: %s", r.raw)
	}
	a = confirm(a, r)
	if works(e, b) {
		t.Fatal("the other window is still signed in")
	}
	me := e.do("GET", "/api/me", nil, "Cookie", a)
	if pr, _ := me.body["pending_result"].(map[string]any); pr["ok"] != true || pr["message"] != "Signed out: Go program, 127.0.0.1" {
		t.Fatalf("the result: %s", me.raw)
	}
	// the window that was signed out, loaded again: a new session, as after Sign out
	again := e.do("GET", "/", nil, "Cookie", b)
	if again.code != 200 || !strings.Contains(again.hdr.Get("Set-Cookie"), "fairy_session=") || len(sessionsOf(e, "test-admin")) != 2 {
		t.Fatalf("reloading the signed-out window: %d %q", again.code, again.hdr.Get("Set-Cookie"))
	}
	b = sessionCookie(e, again.hdr)
	c := e.open()
	a = confirm(a, e.do("POST", "/api/me/sessions/revoke-others", nil, append([]string{"Cookie", a}, sessWrite...)...))
	if works(e, b) || works(e, c) || !works(e, a) || len(sessionsOf(e, "test-admin")) != 1 {
		t.Fatal("everywhere else")
	}
	if got := auditOutcomes(e.s.Cfg.AuditLog); !strings.Contains(got, "sessions.revoke ok") || !strings.Contains(got, "sessions.revoke_others ok") {
		t.Fatalf("audit: %s", got)
	}
}
