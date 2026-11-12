//go:build fairytest

package web

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
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
	e.s.EnableTestMode(startKey)
	e.also = TestKeyCookie(strings.TrimPrefix(e.srv.URL, "http://"), startKey) // a browser that opened the printed address
	return e
}

// startKey is the start key of the test panels in these tests (cmd/fairy makes a random one and keeps it in
// the data folder).
const startKey = "start-key-of-the-web-tests-0123456789-ABCDEF"

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
			req.Header.Set("Cookie", e.also) // with the start key's cookie: these checks do not lean on the key
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
	off := httptest.NewRequest("GET", "http://127.0.0.1/", nil)
	off.Header.Set("Cookie", TestKeyCookie("127.0.0.1", startKey))
	e.s.Handler().ServeHTTP(rec, off)
	if rec.Code != 403 || strings.Contains(rec.Body.String(), "<body>") || !strings.Contains(rec.Body.String(), "loopback") {
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
	if pr, _ := me.body["pending_result"].(map[string]any); pr["ok"] != true || pr["message"] != "that session is signed out" {
		t.Fatalf("the result: %s", me.raw)
	}
	// the window that was signed out from elsewhere, loaded again: a new session
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

// A page opened under another loopback name than the one in public_url (localhost for 127.0.0.1, or the other
// way round) would load, get a session and then fail at its first write ("cross-origin request refused"). The
// test build sends such a page to the one address before any session is made, and refuses everything else
// with that address in the answer. The write check itself is what it is in every build.
func TestTestModeSendsOtherLoopbackNamesToTheOneAddress(t *testing.T) {
	for _, tc := range []struct{ public, other string }{
		{"127.0.0.1", "localhost"}, {"127.0.0.1", "[::1]"}, {"localhost", "127.0.0.1"}, {"[::1]", "localhost"},
	} {
		e := setup(t)
		e.s.Cfg.OIDC.ControlGroups = []string{TestGroup}
		port := e.srv.URL[strings.LastIndex(e.srv.URL, ":"):]
		base := "http://" + tc.public + port
		e.s.Cfg.PublicURL = base
		e.s.EnableTestMode(startKey)
		ask := func(method, host, path string, hdr ...string) *http.Response {
			t.Helper()
			req, _ := http.NewRequest(method, e.srv.URL+path, nil)
			req.Host = host + port
			for i := 0; i+1 < len(hdr); i += 2 {
				req.Header.Set(hdr[i], hdr[i+1])
			}
			req.Header.Set("Cookie", strings.TrimPrefix(req.Header.Get("Cookie")+"; "+TestKeyCookie(req.Host, startKey), "; "))
			res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { res.Body.Close() })
			return res
		}
		for _, p := range [][2]string{{"GET", "/"}, {"HEAD", "/"}, {"GET", "/?x=1&y=%2F"}, {"GET", "/docs"}, {"GET", "/auth/login?next=/%23/users"}, {"GET", "/static/app.js"}} {
			res := ask(p[0], tc.other, p[1])
			if res.StatusCode != 307 || res.Header.Get("Location") != base+p[1] || res.Header.Get("Set-Cookie") != "" {
				t.Errorf("%s as %s, %s %s: %d to %q (cookie %q)", tc.public, tc.other, p[0], p[1], res.StatusCode, res.Header.Get("Location"), res.Header.Get("Set-Cookie"))
			}
		}
		csrf := []string{"X-Requested-With", "fairy", "Content-Type", "application/json", "Origin", "http://" + tc.other + port}
		for _, p := range [][2]string{{"GET", "/api/me"}, {"GET", "/api/stream"}, {"POST", "/api/admin/hosts"}, {"POST", "/api/me/step-up"},
			{"POST", "/auth/logout"}, {"DELETE", "/api/me/pending"}} {
			res := ask(p[0], tc.other, p[1], csrf...)
			body := make([]byte, 600)
			n, _ := res.Body.Read(body)
			if res.StatusCode != 403 || !strings.Contains(string(body[:n]), "open "+base+" instead") || res.Header.Get("Set-Cookie") != "" {
				t.Errorf("%s as %s, %s %s: %d %s", tc.public, tc.other, p[0], p[1], res.StatusCode, body[:n])
			}
			if strings.HasPrefix(p[1], "/api/") && (!strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") || !strings.HasPrefix(string(body[:n]), `{"detail":"`)) {
				t.Errorf("%s %s must be refused in the API's own form: %s", p[0], p[1], body[:n])
			}
		}
		if n := len(must(e.s.Poller.Admin.ListSessions())); n != 0 {
			t.Fatalf("%d sessions were made under the other name", n)
		}
		// under the one address everything is as before, and a write from its page passes the origin check
		res := ask("GET", tc.public, "/")
		ck := strings.Split(res.Header.Get("Set-Cookie"), ";")[0]
		if res.StatusCode != 200 || !strings.Contains(ck, "fairy_session=") {
			t.Fatalf("%s: the page under its own address: %d", base, res.StatusCode)
		}
		if res := ask("POST", tc.public, "/api/me/active", "Cookie", ck, "X-Requested-With", "fairy", "Origin", base); res.StatusCode != 204 {
			t.Fatalf("%s: a write from its own page: %d", base, res.StatusCode)
		}
		// the write check is not loosened: a write that names the other origin is refused as in every build
		if res := ask("POST", tc.public, "/api/me/active", "Cookie", ck, "X-Requested-With", "fairy", "Origin", "http://"+tc.other+port); res.StatusCode != 403 {
			t.Fatalf("%s: a write from %s: %d", base, tc.other, res.StatusCode)
		}
	}
	// r3-p1-07: a test build on port 80 (public_url http://127.0.0.1:80). The browser asks with "Host: 127.0.0.1":
	// that IS the one address, and it was redirected to itself for ever.
	{
		e := setup(t)
		e.s.Cfg.OIDC.ControlGroups = []string{TestGroup}
		e.s.Cfg.PublicURL = "http://127.0.0.1:80"
		e.s.EnableTestMode(startKey)
		ask := func(host, path string) *http.Response {
			req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
			req.Host = host
			req.Header.Set("Cookie", TestKeyCookie(host, startKey))
			res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { res.Body.Close() })
			return res
		}
		for _, host := range []string{"127.0.0.1", "127.0.0.1:80"} {
			if res := ask(host, "/"); res.StatusCode != 200 {
				t.Errorf("a test build on port 80, GET / with Host %s: %d to %q", host, res.StatusCode, res.Header.Get("Location"))
			}
		}
		if res := ask("localhost", "/"); res.StatusCode != 307 || res.Header.Get("Location") != "http://127.0.0.1:80/" {
			t.Errorf("a test build on port 80, GET / as localhost: %d to %q", res.StatusCode, res.Header.Get("Location"))
		}
	}
	// a public_url that is no loopback address (not what cmd/fairy sets up): no such rule, as before
	e := testPanel(t)
	if r := e.do("GET", "/", nil); r.code != 200 {
		t.Fatal(r.code)
	}
}

// r1-p1-01 / r1-p4-01 (audit 5): a reverse proxy that rewrites Host to the upstream and adds no header (nginx's
// bare proxy_pass), or a TCP forward, passes every per-request check: the request arrives on a loopback socket,
// for a loopback host name, without a proxy header. The auditors' proof got the page, an admin session and a
// confirmed admin action that way. What such a client does not have is the start key: without it nothing is
// answered, and with it the browser gets a cookie and the clean address.
func TestTestModeSignsInOnlyWithTheStartKey(t *testing.T) {
	e := testPanel(t)
	host := strings.TrimPrefix(e.srv.URL, "http://")
	cookie := e.also
	e.also = "" // a client that has only reached the port
	sessions := func() int { return len(must(e.s.Poller.Admin.ListSessions())) }
	// exactly what the proxied client of the proof sent: no cookie, the upstream's own Host, no proxy header
	for _, p := range [][2]string{{"GET", "/"}, {"GET", "/api/me"}, {"GET", "/api/systems"}, {"GET", "/api/stream"}, {"GET", "/auth/login"},
		{"GET", "/auth/login?step_up=1"}, {"GET", "/healthz"}, {"GET", "/static/app.js"}, {"GET", "/fairytest.js"}, {"GET", "/docs"},
		{"GET", "/branding/wallpaper"}, {"POST", "/api/me/step-up"}, {"POST", "/api/admin/sites"}, {"POST", "/auth/logout"},
		{"POST", "/api/me/sessions/revoke-others"}, {"PUT", "/api/settings"}, {"DELETE", "/api/me/pending"}} {
		r := e.do(p[0], p[1], map[string]any{"name": "x"}, "X-Requested-With", "fairy", "Origin", e.s.Cfg.BaseURL())
		if r.code != 403 || len(r.hdr.Values("Set-Cookie")) != 0 || !strings.Contains(string(r.raw), testKeyRefusal) || strings.Contains(string(r.raw), startKey) {
			t.Errorf("%s %s without the start key: %d %q (cookies %q)", p[0], p[1], r.code, r.raw, r.hdr.Values("Set-Cookie"))
		}
		if strings.HasPrefix(p[1], "/api/") && r.body["detail"] != testKeyRefusal {
			t.Errorf("%s %s must be refused in the API's own form: %s", p[0], p[1], r.raw)
		}
	}
	// the proof itself: a proxy that forwards like nginx's bare proxy_pass (Host becomes the upstream's address,
	// no X-Forwarded-* header), asked under a LAN name by a client that sets Origin as it likes
	up, _ := url.Parse(e.srv.URL)
	proxy := httptest.NewServer(&httputil.ReverseProxy{Director: func(r *http.Request) {
		r.URL.Scheme, r.URL.Host, r.Host = up.Scheme, up.Host, up.Host
		r.Header["X-Forwarded-For"] = nil
	}})
	defer proxy.Close()
	for _, p := range [][2]string{{"GET", "/"}, {"GET", "/api/me"}, {"POST", "/api/admin/sites"}, {"POST", "/api/me/step-up"}} {
		req, _ := http.NewRequest(p[0], proxy.URL+p[1], strings.NewReader(`{"name":"ProxySite"}`))
		req.Host = "panel.lan:8405"
		req.Header.Set("X-Requested-With", "fairy")
		req.Header.Set("Origin", e.s.Cfg.BaseURL())
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 403 || len(res.Header.Values("Set-Cookie")) != 0 || !strings.Contains(string(body), testKeyRefusal) {
			t.Errorf("through the proxy, %s %s: %d %q (cookies %q)", p[0], p[1], res.StatusCode, body, res.Header.Values("Set-Cookie"))
		}
	}
	// one line; it says that the address must be opened once by pasting it (second review, note A1: opened from
	// a link on another web page the plain address is refused also later, and the line used to say only "open
	// the address printed in its console window", which the person had just done)
	if testKeyRefusal != "Test build: open the address printed in its console window. Paste it into the address bar once; a link on another web page does not open the panel." ||
		strings.Contains(testKeyRefusal, "\n") {
		t.Fatal("the refusal's text is what was agreed:", testKeyRefusal)
	}
	// a wrong key, a key of another length, an empty one, the key in a header or a body, a made-up cookie
	for _, k := range []string{"", "x", strings.Repeat("a", len(startKey)), startKey + "a", startKey[1:], strings.ToLower(startKey)} {
		if r := e.do("GET", "/?key="+k, nil); r.code != 403 || len(r.hdr.Values("Set-Cookie")) != 0 {
			t.Errorf("key %q: %d", k, r.code)
		}
	}
	for _, h := range [][2]string{{"X-Key", startKey}, {"Authorization", "Bearer " + startKey}, {"Cookie", testKeyCookieName(host) + "=" + startKey},
		{"Cookie", testKeyCookie + "=" + testKeyCookieValue(startKey)}, {"Cookie", testKeyCookieName(host) + "=" + strings.Repeat("0", 64)}} {
		if r := e.do("GET", "/", nil, h[0], h[1]); r.code != 403 {
			t.Errorf("%s: %d", h[0], r.code)
		}
	}
	if r := e.do("POST", "/?key="+startKey, nil); r.code != 403 { // the key is taken from the address of a page, nothing else
		t.Error("a POST with the key:", r.code)
	}
	if r := e.do("GET", "/api/me?key="+startKey, nil); r.code != 403 || len(r.hdr.Values("Set-Cookie")) != 0 {
		t.Error("the API takes no key:", r.code)
	}
	if n := sessions(); n != 0 {
		t.Fatalf("%d sessions were made without the start key", n)
	}
	// the printed address: a cookie that scripts cannot read and other sites cannot send, and the clean address
	r := e.do("GET", "/?key="+startKey, nil)
	set := r.hdr.Values("Set-Cookie")
	// The answer is a page of the panel's own that goes on by itself, not a redirect (second review, note A1):
	// a redirect is part of the navigation that brought the browser here, and when that began on another site
	// (the address clicked in a chat) the browser did not send the Strict cookie it had just been given.
	if r.code != 200 || r.hdr.Get("Location") != "" || len(set) != 1 || !strings.HasPrefix(set[0], cookie+";") ||
		!strings.Contains(set[0], "HttpOnly") || !strings.Contains(set[0], "SameSite=Strict") || !strings.Contains(set[0], "Max-Age=315360000") ||
		!strings.Contains(set[0], "Path=/") || r.hdr.Get("Cache-Control") != "no-store" || r.hdr.Get("Referrer-Policy") != "no-referrer" ||
		!strings.HasPrefix(r.hdr.Get("Content-Type"), "text/html") {
		t.Fatalf("the address with the key: %d, %q, headers %v", r.code, set, r.hdr)
	}
	page0 := string(r.raw)
	if !strings.Contains(page0, `<meta http-equiv="refresh" content="0; url=/">`) || !strings.Contains(page0, `<a href="/" rel="noreferrer">Continue</a>`) ||
		!strings.Contains(page0, `<meta name="referrer" content="no-referrer">`) {
		t.Fatalf("the page must go on to the clean address by itself, and offer a link: %s", page0)
	}
	if strings.Contains(strings.ToLower(page0), "<script") || strings.Contains(page0, "style=") || strings.Contains(page0, "onclick") ||
		!strings.Contains(r.hdr.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("no inline script or style (the CSP holds): %s", page0)
	}
	if strings.Contains(strings.Join(set, " ")+r.hdr.Get("Location")+page0, startKey) || sessions() != 0 {
		t.Fatal("the answer to the key must not repeat it, and makes no session yet")
	}
	if h := e.do("HEAD", "/?key="+startKey, nil); h.code != 200 || len(h.raw) != 0 || len(h.hdr.Values("Set-Cookie")) != 1 {
		t.Fatalf("HEAD: %d, %d bytes", h.code, len(h.raw))
	}
	for from, to := range map[string]string{"/?x=1&key=" + startKey + "&y=%2F": "/?x=1&amp;y=%2F", "/docs?key=" + startKey: "/docs",
		"/?a=%22%3E%3Cscript%3Ealert(1)%3C/script%3E&key=" + startKey: "/?a=%22%3E%3Cscript%3Ealert%281%29%3C%2Fscript%3E",
		"//evil.example/?key=" + startKey:                             "/", "/x\\y?key=" + startKey: "/"} {
		r := e.do("GET", from, nil)
		if r.code == 307 || r.code == 301 { // net/http's own cleaning of a path with "//": it carries nothing
			continue
		}
		body := string(r.raw)
		if r.code != 200 || !strings.Contains(body, `content="0; url=`+to+`">`) || !strings.Contains(body, `<a href="`+to+`" rel`) ||
			strings.Contains(body, startKey) || strings.Contains(body, "<script>") || strings.Contains(body, "evil.example") {
			t.Errorf("%s: %d, want it to go on to %q: %s", strings.ReplaceAll(from, startKey, "KEY"), r.code, to, body)
		}
	}
	// with the cookie the test build is what it was: the page signs in, the API answers
	e.also = cookie
	page := e.do("GET", "/", nil)
	if page.code != 200 || !strings.Contains(page.hdr.Get("Set-Cookie"), "fairy_session=") || strings.Contains(string(page.raw), startKey) {
		t.Fatalf("the page with the cookie: %d", page.code)
	}
	ck := strings.Split(page.hdr.Get("Set-Cookie"), ";")[0]
	me := e.do("GET", "/api/me", nil, "Cookie", ck)
	if me.code != 200 || me.body["role"] != "admin" || strings.Contains(string(me.raw), startKey) || strings.Contains(string(me.raw), testKeyCookieValue(startKey)) {
		t.Fatalf("/api/me: %d %s", me.code, me.raw)
	}
	// a session cookie alone (copied out of the browser, or one a proxied client would be handed) is not enough
	e.also = ""
	if r := e.do("GET", "/api/me", nil, "Cookie", ck); r.code != 403 {
		t.Fatal("a session without the start key's cookie:", r.code)
	}
	// the key is in no audit entry and no answer of the API that lists what the panel knows
	e.also = cookie
	raw, _ := os.ReadFile(e.s.Cfg.AuditLog)
	for _, p := range []string{"/api/audit", "/api/me/sessions", "/api/admin/sessions", "/api/settings", "/api/summary"} {
		raw = append(raw, e.do("GET", p, nil, "Cookie", ck).raw...)
	}
	if bytes.Contains(raw, []byte(startKey)) || bytes.Contains(raw, []byte(testKeyCookieValue(startKey))) {
		t.Fatal("the start key (or its cookie) is in the audit log or an API answer")
	}
	// two test builds on one PC: the cookie's name carries the port, so neither takes the other's away
	if a, b := testKeyCookieName("127.0.0.1:8080"), testKeyCookieName("127.0.0.1:8081"); a == b || a != "fairytest_key_8080" || testKeyCookieName("localhost") != "fairytest_key_80" {
		t.Fatal("cookie names:", a, b)
	}
	for k, want := range map[string]bool{startKey: true, strings.Repeat("a", 32): true, strings.Repeat("a", 31): false, strings.Repeat("a", 129): false,
		strings.Repeat("a", 31) + " ": false, strings.Repeat("a", 31) + "&": false, "": false} {
		if ValidTestKey(k) != want {
			t.Errorf("ValidTestKey(%q) = %v", k, !want)
		}
	}
}

// Second review of audit 5's fixes (note A2) and r2-p1-01 / G3: two test panels in one browser shared the
// session cookie (fairy_session on both), so loading one signed the other out, back and forth. A test build's
// session cookie and its "signed out here" mark now carry the port in their names like the key cookie, and
// the session cookie is SameSite=Strict like the key cookie (a test build has no identity provider to come
// back from). The real panel's cookie is what it was.
func TestTestModeCookiesCarryThePortAndAreStrict(t *testing.T) {
	names := func(port string) []string {
		e := setup(t)
		e.s.Cfg.OIDC.ControlGroups = []string{TestGroup}
		e.s.codec.secure = false // as cmd/fairy runs a test build: plain http on loopback
		e.s.Cfg.PublicURL = "http://127.0.0.1:" + port
		e.s.EnableTestMode(startKey)
		return []string{e.s.codec.name(), testKeyCookieName("127.0.0.1:" + port), testPortName(testSignedOutCookie, "127.0.0.1:"+port)}
	}
	a, b := names("8080"), names("8081")
	if strings.Join(a, " ") != "fairy_session_8080 fairytest_key_8080 fairytest_signed_out_8080" ||
		strings.Join(b, " ") != "fairy_session_8081 fairytest_key_8081 fairytest_signed_out_8081" {
		t.Fatal(a, b)
	}
	// on the wire: the page of a test build on plain http sets the session cookie under that name, Strict
	e := setup(t)
	e.s.Cfg.OIDC.ControlGroups = []string{TestGroup}
	e.s.codec.secure = false
	e.s.Cfg.PublicURL = e.srv.URL
	e.s.EnableTestMode(startKey)
	host := strings.TrimPrefix(e.srv.URL, "http://")
	_, port, _ := net.SplitHostPort(host)
	e.also = TestKeyCookie(host, startKey)
	r := e.do("GET", "/", nil)
	set := strings.Join(r.hdr.Values("Set-Cookie"), "\n")
	if r.code != 200 || !strings.Contains(set, "fairy_session_"+port+"=") || !strings.Contains(set, "SameSite=Strict") ||
		strings.Contains(set, "SameSite=Lax") || strings.Contains(set, "fairy_session=") {
		t.Fatalf("the session cookie of a test build: %d\n%s", r.code, set)
	}
	ck := strings.Split(r.hdr.Get("Set-Cookie"), ";")[0]
	if me := e.do("GET", "/api/me", nil, "Cookie", ck); me.code != 200 || me.body["sub"] != "test-admin" {
		t.Fatal("the session under its new name:", me.code, string(me.raw))
	}
	// the other panel's cookie (same value, the other port's name) and the old plain name are not this panel's session
	other := strings.Replace(ck, "fairy_session_"+port+"=", "fairy_session_1=", 1)
	old := strings.Replace(ck, "fairy_session_"+port+"=", "fairy_session=", 1)
	for _, c := range []string{other, old} {
		if me := e.do("GET", "/api/me", nil, "Cookie", c); me.code != 401 {
			t.Fatalf("a cookie under another name is read as this panel's session: %s: %d", strings.SplitN(c, "=", 2)[0], me.code)
		}
	}
	// the real panel (no test mode): the names and SameSite=Lax of before, with and without FAIRY_INSECURE_COOKIES
	p := setup(t)
	if p.s.codec.name() != "__Host-fairy_session" || p.s.codec.sameSite() != http.SameSiteLaxMode {
		t.Fatal("the real panel's cookie changed:", p.s.codec.name())
	}
	p.s.codec.secure = false
	if p.s.codec.name() != "fairy_session" || p.s.codec.sameSite() != http.SameSiteLaxMode {
		t.Fatal("the real panel's plain cookie changed:", p.s.codec.name())
	}
}
