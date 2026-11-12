package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/audit"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/enrollment"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/updates"
)

type env struct {
	t   *testing.T
	s   *Server
	srv *httptest.Server
	b   *testutil.Backend
	// also is a cookie ("name=value") that do sends with every request, next to the ones a test passes: the
	// start key's cookie of a test build (testmode_test.go). Empty everywhere else.
	also string
}

func setup(t *testing.T) *env {
	t.Helper()
	return setupWith(t, func(b poller.Backend) poller.Backend { return b })
}

// setupWith is setup with the test backend wrapped by wrap before the poller gets it. A test that wants
// another backend wraps it here and does not replace Poller.Backend afterwards: the first poll leaves a
// lookup running in the background (a node's CPU model) that still reads that field.
func setupWith(t *testing.T, wrap func(poller.Backend) poller.Backend) *env {
	t.Helper()
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	b := testutil.NewBackend()
	p, err := poller.New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, wrap(b))
	if err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	s := New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, p, nil, "test")
	s.Enroll.Scripts = testInstallers // what a release build's check of the pinned release returns
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, s: s, srv: srv, b: b}
}

var viewer = model.User{Sub: "u1", Name: model.P("Alice"), Email: model.P("n@example.com"), Groups: []string{"viewers"}}

// controller is in a control group: admin on first sign-in. A different subject from viewer, because roles
// now belong to the user (users.json), not to the groups in one session.
func controller() model.User {
	u := viewer
	u.Sub = "u2"
	u.Groups = []string{"viewers", "controllers"}
	return u
}

// cookie mints a session like a real login: the sign-in is recorded in users.json (a new user may be
// bootstrapped, as at the OIDC callback), registered sid, auth_time as given.
func (e *env) cookie(u model.User, authAge time.Duration) string {
	e.signIn(u)
	return e.staleCookie(u, authAge)
}

// signIn records a fresh Pocket ID sign-in of u (what the OIDC callback does after verifying the ID token).
func (e *env) signIn(u model.User) {
	c := &reqCtx{s: e.s, w: httptest.NewRecorder(), r: httptest.NewRequest("GET", "/auth/callback", nil)}
	u.Auth = "session"
	e.s.recordUser(c, &u, true)
}

// staleCookie mints a registered session without a sign-in: like a cookie issued days ago, before users.json
// knew the user. Its Pocket ID sign-in (authAge ago) is recorded as confirmed by the test client (testIP, testUA).
func (e *env) staleCookie(u model.User, authAge time.Duration) string {
	sid := randToken(18)
	if err := e.s.Poller.Admin.AddSession(sid, adminstate.NewSession{Sub: u.Sub}); err != nil {
		e.t.Fatal(err)
	}
	now := e.s.now()
	rec := httptest.NewRecorder()
	e.s.codec.write(rec, &sessionData{SID: sid, User: &u, IAT: now.Unix(), Seen: now.Unix(),
		Client: e.s.codec.clientMark(client{ip: testIP, ua: sha256.Sum256([]byte(testUA))})})
	e.s.codec.live.confirm(sid, float64(now.Add(-authAge).Unix()), testIP, sha256.Sum256([]byte(testUA)), now)
	return strings.Split(rec.Header().Get("Set-Cookie"), ";")[0]
}

// The test client as the server sees it: e.do and the other helpers connect from loopback with Go's User-Agent.
const (
	testIP = "127.0.0.1"
	testUA = "Go-http-client/1.1"
)

type resp struct {
	code int
	body map[string]any
	raw  []byte
	hdr  http.Header
}

// addAlso adds the cookie every request of this env carries (env.also), if there is one.
func (e *env) addAlso(req *http.Request) {
	if e.also != "" {
		req.Header.Set("Cookie", strings.TrimPrefix(req.Header.Get("Cookie")+"; "+e.also, "; "))
	}
}

func (e *env) do(method, path string, body any, hdr ...string) resp {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	e.addAlso(req)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer r.Body.Close()
	raw, _ := io.ReadAll(r.Body)
	out := resp{code: r.StatusCode, raw: raw, hdr: r.Header}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func TestUnauthenticatedGets401JSONNeverRedirect(t *testing.T) {
	e := setup(t)
	for _, p := range []string{"/api/me", "/api/systems", "/api/summary", "/api/stream", "/api/audit", "/api/settings", "/api/openapi.json"} {
		r := e.do("GET", p, nil)
		if r.code != 401 || r.body["detail"] != "not authenticated" {
			t.Errorf("%s: %d %s", p, r.code, r.raw)
		}
	}
	if r := e.do("GET", "/healthz", nil); r.code != 200 || r.body["status"] != "ok" {
		t.Fatal("healthz")
	}
	if r := e.do("GET", "/docs", nil); r.code != 303 || r.hdr.Get("Location") != "/auth/login?next=/docs" {
		t.Fatal("docs gate", r.code)
	}
	r := e.do("GET", "/", nil)
	if r.code != 200 || !strings.Contains(string(r.raw), "Fairy") {
		t.Fatal("index")
	}
	for _, h := range []string{"Content-Security-Policy", "X-Frame-Options", "X-Content-Type-Options", "Referrer-Policy", "Permissions-Policy", "Strict-Transport-Security"} {
		if r.hdr.Get(h) == "" {
			t.Errorf("missing %s", h)
		}
	}
	if r := e.do("GET", "/api/nope", nil); r.code != 404 {
		t.Fatal("404")
	}
	if r := e.do("GET", "/static/", nil); r.code != 404 || strings.Contains(string(r.raw), "app.js") {
		t.Fatal("static directory listing", r.code)
	}
	if r := e.do("GET", "/static/app.js", nil); r.code != 200 {
		t.Fatal("static file", r.code)
	}
}

func TestSessionsAreRevocableAndTamperProof(t *testing.T) {
	e := setup(t)
	ck := e.cookie(viewer, 0)
	r := e.do("GET", "/api/me", nil, "Cookie", ck)
	if r.code != 200 || r.body["can_control"] != false || r.body["auth"] != "session" {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	bad := ck[:len(ck)-3] + "abc"
	if e.do("GET", "/api/me", nil, "Cookie", bad).code != 401 {
		t.Fatal("tampered cookie accepted")
	}
	if _, err := e.s.Poller.Admin.RevokeAllSessions(); err != nil { // `fairy admin sessions revoke-all`
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if r := e.do("GET", "/api/me", nil, "Cookie", ck); r.code != 401 {
		t.Fatal("revoked session still valid", r.code)
	}
}

func TestAPITokensScopesAndEndpoints(t *testing.T) {
	e := setup(t)
	raw, rec, err := e.s.Tokens.Create("ha", "system", "read", "cli:test", 0)
	if err != nil || !strings.HasPrefix(raw, "fy_") {
		t.Fatal(err)
	}
	ctl, _, _ := e.s.Tokens.Create("ops", "system", "control", "cli:test", 0)
	if _, _, err := e.s.Tokens.Create("ha", "system", "read", "cli:test", 0); err == nil {
		t.Fatal("duplicate active name accepted")
	}
	auth := func(tok string) []string { return []string{"Authorization", "Bearer " + tok} }
	if r := e.do("GET", "/api/me", nil, auth(raw)...); r.code != 200 || r.body["token_scope"] != "read" || r.body["auth"] != "token" {
		t.Fatalf("%s", r.raw)
	}
	if r := e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 5}, auth(raw)...); r.code != 403 {
		t.Fatal("read token controlled", r.code)
	}
	if r := e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 5}, auth(ctl)...); r.code != 200 || r.body["mode"] != "override" {
		t.Fatalf("control token (no CSRF header needed): %d %s", r.code, r.raw)
	}
	if r := e.do("GET", "/api/settings", nil, auth(raw)...); r.code != 403 {
		t.Fatal("tokens must not reach settings")
	}
	if r := e.do("GET", "/auth/login", nil, auth(raw)...); r.code != 403 {
		t.Fatal("tokens must not reach /auth")
	}
	if r := e.do("GET", "/api/me", nil, auth("fy_nope")...); r.code != 401 || r.body["detail"] != "invalid API token" {
		t.Fatal("bogus token")
	}
	if _, err := e.s.Tokens.Revoke(rec.ID); err != nil {
		t.Fatal(err)
	}
	if r := e.do("GET", "/api/me", nil, auth(raw)...); r.code != 401 {
		t.Fatal("revoked token still works")
	}
}

func TestControlNeedsGroupCSRFStepUpAndKillSwitch(t *testing.T) {
	e := setup(t)
	csrf := []string{"X-Requested-With", "fairy"}
	body := map[string]any{"interval_s": 5}
	v := e.cookie(viewer, 0)
	if r := e.do("PUT", "/api/systems/laptop/poll", body, append([]string{"Cookie", v}, csrf...)...); r.code != 403 || !strings.HasPrefix(r.raw2(), `{"detail":"only admins`) {
		t.Fatalf("viewer: %d %s", r.code, r.raw)
	}
	c := e.cookie(controller(), 0)
	if r := e.do("PUT", "/api/systems/laptop/poll", body, "Cookie", c); r.code != 403 || !strings.Contains(r.raw2(), "X-Requested-With") {
		t.Fatalf("csrf: %d", r.code)
	}
	if r := e.do("PUT", "/api/systems/laptop/poll", body, "Cookie", c, "X-Requested-With", "fairy", "Origin", "https://evil.example"); r.code != 403 {
		t.Fatal("cross-origin accepted")
	}
	if r := e.do("PUT", "/api/systems/laptop/poll", body, "Cookie", c, "X-Requested-With", "fairy", "Sec-Fetch-Site", "cross-site"); r.code != 403 {
		t.Fatal("cross-site accepted")
	}
	// step-up: last Pocket ID sign-in 10 min ago (window 3 min, the default)
	old := e.cookie(controller(), 10*time.Minute)
	r := e.do("PUT", "/api/systems/laptop/poll", body, append([]string{"Cookie", old}, csrf...)...)
	d, _ := r.body["detail"].(map[string]any)
	if r.code != 401 || d["code"] != "step_up_required" || d["window_s"] != 180.0 || !strings.Contains(d["message"].(string), "last 3 min") {
		t.Fatalf("step-up: %d %s", r.code, r.raw)
	}
	// S-g: the refusal says how old the sign-in is now (the dialog shows it), not the page-load snapshot
	if age, _ := d["auth_age_s"].(float64); age < 595 || age > 660 {
		t.Fatalf("auth_age_s of a sign-in 10 min ago: %v", d["auth_age_s"])
	}
	other := e.do("PUT", "/api/systems/laptop/poll", body, append([]string{"Cookie", c, "X-Forwarded-For", "203.0.113.9"}, csrf...)...)
	od, _ := other.body["detail"].(map[string]any)
	if age, _ := od["auth_age_s"].(float64); other.code != 401 || age < 30*86400 {
		t.Fatalf("auth_age_s for a client that did not confirm must read as unknown (very old): %d %v", other.code, od["auth_age_s"])
	}
	if r := e.do("PUT", "/api/systems/laptop/poll", body, append([]string{"Cookie", c}, csrf...)...); r.code != 200 {
		t.Fatalf("fresh controller: %d %s", r.code, r.raw)
	}
	// kill switch (fairy admin control disable): everyone refused with 423
	_ = e.s.Poller.Admin.SetControlDisabled(true, "root (cli)", "maintenance")
	r = e.do("PUT", "/api/systems/laptop/poll", body, append([]string{"Cookie", c}, csrf...)...)
	if r.code != 423 || !strings.Contains(r.raw2(), "maintenance") {
		t.Fatalf("kill switch: %d %s", r.code, r.raw)
	}
	if me := e.do("GET", "/api/me", nil, "Cookie", c); me.body["control_disabled"] == nil {
		t.Fatal("me must show the kill switch")
	}
	caps := e.do("GET", "/api/systems/laptop/power", nil, "Cookie", c)
	for _, a := range caps.body["actions"].([]any) {
		if a.(map[string]any)["available"] == true {
			t.Fatal("power actions must be unavailable while control is disabled")
		}
	}
	_ = e.s.Poller.Admin.SetControlDisabled(false, "root", "")
	// per-host power-control flag
	_ = e.s.Poller.Admin.SetPowerDisabled("laptop", true)
	caps = e.do("GET", "/api/systems/laptop/power", nil, "Cookie", c)
	if !strings.Contains(string(caps.raw), "power-control enable") {
		t.Fatal("per-host flag")
	}
	_ = e.s.Poller.Admin.SetPowerDisabled("laptop", false)
	// typed confirmation, then the agent call with the requester name
	pw := map[string]any{"action": "shutdown", "delay_s": 120}
	if r := e.do("POST", "/api/systems/laptop/power", pw, append([]string{"Cookie", c}, csrf...)...); r.code != 400 {
		t.Fatalf("typed confirmation: %d %s", r.code, r.raw)
	}
	pw["confirm"] = "LAPTOP"
	r = e.do("POST", "/api/systems/laptop/power", pw, append([]string{"Cookie", c}, csrf...)...)
	if r.code != 200 || r.body["via"] != "agent" || !strings.Contains(strings.Join(e.b.Requests, "\n"), "laptop POST /power") {
		t.Fatalf("power: %d %s", r.code, r.raw)
	}
	// guest power goes to Proxmox with the power token path
	r = e.do("POST", "/api/systems/qemu-100/power", map[string]any{"action": "reboot"}, append([]string{"Cookie", c}, csrf...)...)
	if r.code != 200 || len(e.b.PVECalls) != 1 || e.b.PVECalls[0] != "pve /nodes/SRV-PVEA/qemu/100/status/reboot " {
		t.Fatalf("guest power: %d %s %v", r.code, r.raw, e.b.PVECalls)
	}
	// rate limit (control_rate_per_min default)
	limited := false
	for i := 0; i < 40 && !limited; i++ {
		limited = e.do("POST", "/api/systems/laptop/fan/mode", map[string]any{"mode": "full"}, append([]string{"Cookie", c}, csrf...)...).code == 429
	}
	if !limited {
		t.Fatal("no rate limit")
	}
}

func (r resp) raw2() string { return string(r.raw) }

func TestAuditLogAndPage(t *testing.T) {
	e := setup(t)
	c := e.cookie(controller(), 0)
	v := e.cookie(viewer, 0)
	csrf := []string{"X-Requested-With", "fairy"}
	e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 7}, append([]string{"Cookie", c}, csrf...)...)
	e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 7}, append([]string{"Cookie", v}, csrf...)...)
	if r := e.do("GET", "/api/audit", nil, "Cookie", v); r.code != 403 {
		t.Fatal("viewer read the audit log")
	}
	r := e.do("GET", "/api/audit?limit=1", nil, "Cookie", c)
	cursor := int64(r.body["next"].(float64))
	entries := r.body["entries"].([]any)
	first := entries[0].(map[string]any)
	if r.code != 200 || len(entries) != 1 || r.body["next"] == nil || first["action"] != "auth.denied" { // newest first: the 403s
		t.Fatalf("%s", r.raw)
	}
	r = e.do("GET", "/api/audit?action=poll", nil, "Cookie", c)
	got := r.body["entries"].([]any)[0].(map[string]any)
	if got["action"] != "poll.set" || got["target"] != "laptop" || got["outcome"] != "ok" || got["actor"] != "n@example.com (unverified)" || got["sub"] != "u2" || got["source"] != "ui" {
		t.Fatalf("%v", got)
	}
	if r := e.do("GET", "/api/audit?before="+itoa(cursor), nil, "Cookie", c); len(r.body["entries"].([]any)) == 0 {
		t.Fatal("pagination")
	}
	if r := e.do("GET", "/api/audit?action=Bad%20Action", nil, "Cookie", c); r.code != 422 {
		t.Fatal("filter validation")
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestSettingsAndBackgroundUpload(t *testing.T) {
	e := setup(t)
	c := e.cookie(viewer, 0) // settings are per user, no control group needed
	csrf := []string{"Cookie", c, "X-Requested-With", "fairy"}
	if r := e.do("GET", "/api/settings", nil, "Cookie", c); r.code != 200 || r.body["dim"] != 60.0 {
		t.Fatal("defaults")
	}
	if r := e.do("PUT", "/api/settings", map[string]any{"theme": "black", "background": map[string]any{"kind": "url", "url": "http://x"}, "dim": 50, "blur": 10, "card_opacity": 70}, csrf...); r.code != 422 {
		t.Fatal("http background url accepted")
	}
	if r := e.do("PUT", "/api/settings", map[string]any{"background": map[string]any{"kind": "upload"}}, csrf...); r.code != 400 {
		t.Fatal("upload kind without a file")
	}
	upload := func(ct string, data []byte) resp {
		req, _ := http.NewRequest("POST", e.srv.URL+"/api/settings/background", bytes.NewReader(data))
		req.Header.Set("Content-Type", ct)
		req.Header.Set("Cookie", c)
		req.Header.Set("X-Requested-With", "fairy")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		raw, _ := io.ReadAll(r.Body)
		out := resp{code: r.StatusCode, raw: raw}
		_ = json.Unmarshal(raw, &out.body)
		return out
	}
	png := pngOfSize(t, 4096)
	if r := upload("image/png", png); r.code != 403 {
		t.Fatal("a user waiting for access stored a file", r.code)
	}
	if _, _, err := e.s.Access.SetRole(viewer.Sub, "viewer", []string{}, "test", true); err != nil {
		t.Fatal(err)
	}
	if r := upload("image/svg+xml", []byte("<svg onload=alert(1)>")); r.code != 415 {
		t.Fatal("svg accepted")
	}
	if r := upload("image/png", []byte("GIF89a....")); r.code != 415 {
		t.Fatal("mismatched content accepted")
	}
	if r := upload("image/png", append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)); r.code != 415 { // r1-p3-05
		t.Fatal("a file that only begins like a PNG was accepted", r.code)
	}
	if r := upload("image/png", make([]byte, 8<<20+1)); r.code != 413 {
		t.Fatal("too large accepted", r.code)
	}
	r := upload("image/png", png)
	if r.code != 201 || r.body["background"].(map[string]any)["kind"] != "upload" {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	g := e.do("GET", "/api/settings/background", nil, "Cookie", c)
	if g.code != 200 || g.hdr.Get("Content-Type") != "image/png" || g.hdr.Get("Content-Security-Policy") != "default-src 'none'" {
		t.Fatal("background download", g.code)
	}
	if r := e.do("DELETE", "/api/settings/background", nil, csrf...); r.code != 200 {
		t.Fatal("delete")
	}
	limited := false
	for i := 0; i < uploadsPerMin && !limited; i++ {
		limited = upload("image/png", png).code == 429
	}
	if !limited {
		t.Fatal("uploads are not rate limited")
	}
}

func TestSafeNextAndLoginRateLimit(t *testing.T) {
	for in, want := range map[string]string{"/": "/", "/#/system/x": "/#/system/x", "//evil.com": "/", "https://evil.com": "/",
		"/\\evil": "/", "": "/", "javascript:alert(1)": "/"} {
		if got := safeNext(in); got != want {
			t.Errorf("%q -> %q", in, got)
		}
	}
	e := setup(t)
	e.s.Cfg.OIDC.Issuer = "http://127.0.0.1:1" // discovery fails fast (502) until the limiter kicks in
	codes := map[int]int{}
	for i := 0; i < 25; i++ {
		codes[e.do("GET", "/auth/login", nil).code]++
	}
	if codes[429] == 0 { // 20/min default (discovery fails: 502 before the limit)
		t.Fatal(codes)
	}
	// the flood (same address) does not block a signed-in admin's step-up: their bucket is their own
	c := e.cookie(controller(), 10*time.Minute)
	if r := e.do("GET", "/auth/login?step_up=1&next=/", nil, "Cookie", c); r.code == 429 {
		t.Fatal("step-up of a signed-in user blocked by other clients' login attempts")
	}
	// and the bucket follows the client IP behind a trusted nginx, not nginx's address
	e.s.Cfg.TrustedProxies = []string{"127.0.0.1/32"}
	if err := e.s.Cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if r := e.do("GET", "/auth/login", nil, "X-Forwarded-For", "203.0.113.50"); r.code == 429 {
		t.Fatal("another client behind nginx is limited by the flood")
	}
}

func TestDemoServesLoopbackOnly(t *testing.T) {
	e := setup(t)
	u := viewer
	u.Auth = "demo"
	e.s.DemoUser = &u
	// httptest listens on 127.0.0.1 -> served
	if r := e.do("GET", "/api/me", nil); r.code != 200 || r.body["demo"] != true {
		t.Fatal("demo on loopback", r.code)
	}
	if isLoopback(fakeAddr("192.168.1.5:8090")) || !isLoopback(fakeAddr("127.0.0.1:8090")) || !isLoopback(fakeAddr("[::1]:8090")) {
		t.Fatal("loopback detection")
	}
}

// r1-p3-07 (audit 5): the demo answered every Host name on its loopback socket, so a web page elsewhere that
// points a name of its own at 127.0.0.1 (DNS rebinding) could read the demo's data. It now answers only under a
// loopback name, as the test build does.
func TestDemoAnswersOnlyALoopbackName(t *testing.T) {
	e := setup(t)
	u := viewer
	u.Auth = "demo"
	e.s.DemoUser = &u
	as := func(host, path string) resp {
		req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
		req.Host = host
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		raw, _ := io.ReadAll(r.Body)
		out := resp{code: r.StatusCode, raw: raw, hdr: r.Header}
		_ = json.Unmarshal(raw, &out.body)
		return out
	}
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(e.srv.URL, "http://"))
	for _, host := range []string{"evil.example", "evil.example:" + port, "127.0.0.1.evil.example", "localhost.evil.example:" + port,
		"192.168.1.5:" + port, "0.0.0.0:" + port, "[2001:db8::1]:" + port, "panel.lan"} {
		for _, path := range []string{"/api/me", "/api/systems", "/", "/healthz"} {
			if r := as(host, path); r.code != 403 || r.body["detail"] != "demo mode only serves localhost" || strings.Contains(string(r.raw), "viewer") {
				t.Errorf("Host %s, %s: %d %s", host, path, r.code, r.raw)
			}
		}
	}
	for _, host := range []string{"127.0.0.1:" + port, "localhost:" + port, "localhost", "127.0.0.1", "[::1]:" + port, "127.0.0.2:" + port} {
		if r := as(host, "/api/me"); r.code != 200 || r.body["demo"] != true {
			t.Errorf("Host %s: %d %s", host, r.code, r.raw)
		}
	}
	for host, want := range map[string]bool{"localhost": true, "LOCALHOST": false, "localhost.": false, "": false, "::1": true, "[::1]": true, "127.0.0.1:1": true, "evil.example": false} {
		if loopbackName(host) != want {
			t.Errorf("loopbackName(%q) = %v", host, !want)
		}
	}
}

// r2-p1-04 (audit 5): `serve --demo` opened as http://localhost:PORT or http://[::1]:PORT loaded, and every
// change was then "cross-origin request refused" (the write check compares the page's origin with the demo's
// one address). The page is now sent on to that address when it is opened; the write check is as strict as
// before, for the demo and for everything else.
func TestDemoPageUnderAnotherLoopbackNameGoesToTheOneAddress(t *testing.T) {
	e := setup(t)
	u := controller()
	u.Auth = "demo"
	e.s.DemoUser = &u
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(e.srv.URL, "http://"))
	e.s.Cfg.PublicURL = "http://127.0.0.1:" + port // as cmd/fairy sets it: http://<listen>
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	as := func(method, host, path string, hdr ...string) resp {
		req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader("{}"))
		req.Host = host
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		r, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		raw, _ := io.ReadAll(r.Body)
		out := resp{code: r.StatusCode, raw: raw, hdr: r.Header}
		_ = json.Unmarshal(raw, &out.body)
		return out
	}
	one := "http://127.0.0.1:" + port
	for _, host := range []string{"localhost:" + port, "[::1]:" + port, "127.0.0.2:" + port, "localhost"} {
		r := as("GET", host, "/")
		if r.code != 307 || r.hdr.Get("Location") != one+"/" || r.hdr.Get("Cache-Control") != "no-store" {
			t.Errorf("GET / as %s: %d Location %q", host, r.code, r.hdr.Get("Location"))
		}
		if r := as("GET", host, "/docs?x=1", "Sec-Fetch-Dest", "document"); r.code != 307 || r.hdr.Get("Location") != one+"/docs?x=1" {
			t.Errorf("a document as %s: %d Location %q", host, r.code, r.hdr.Get("Location"))
		}
		// API requests and files under the other name are answered as before (a script, curl)
		if r := as("GET", host, "/api/me"); r.code != 200 || r.body["demo"] != true {
			t.Errorf("GET /api/me as %s: %d", host, r.code)
		}
		if r := as("GET", host, "/static/app.js", "Sec-Fetch-Dest", "script"); r.code != 200 {
			t.Errorf("a file as %s: %d", host, r.code)
		}
		// the write check is not loosened: a page that is still open under the other name is refused as before
		r = as("PUT", host, "/api/settings", "X-Requested-With", "fairy", "Content-Type", "application/json", "Origin", "http://"+host)
		if r.code != 403 || r.body["detail"] != "cross-origin request refused" {
			t.Errorf("a write from the page at %s: %d %s", host, r.code, r.raw)
		}
	}
	// under the one address nothing is redirected and a write passes the origin check
	if r := as("GET", "127.0.0.1:"+port, "/"); r.code != 200 {
		t.Fatal("the one address", r.code)
	}
	if r := as("PUT", "127.0.0.1:"+port, "/api/settings", "X-Requested-With", "fairy", "Content-Type", "application/json", "Origin", one); r.code == 403 {
		t.Fatal("a write from the one address", r.code, string(r.raw))
	}
	// a foreign name is still refused before anything else
	if r := as("GET", "evil.example:"+port, "/"); r.code != 403 || strings.Contains(r.hdr.Get("Location"), "http") {
		t.Fatal("a foreign name", r.code)
	}
	// not the demo: no redirect, whatever the name (the plain panel and the test build are not touched here)
	e.s.DemoUser = nil
	if r := as("GET", "localhost:"+port, "/"); r.code == 307 {
		t.Fatal("the real panel redirected")
	}
	// a demo behind its own address (FAIRY_DEMO_URL): no rule
	e.s.DemoUser = &u
	e.s.Cfg.PublicURL = "https://demo.example.com"
	if r := as("GET", "localhost:"+port, "/"); r.code != 200 {
		t.Fatal("FAIRY_DEMO_URL elsewhere", r.code)
	}
}

type fakeAddr string

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return string(a) }

func TestClaimsPolicy(t *testing.T) {
	u := userFromClaims(map[string]any{"sub": "x", "email": "A@B.c", "groups": []any{"viewers"}}, "groups")
	o := config.OIDC{AllowedGroups: []string{"viewers"}, ControlGroups: []string{"controllers"}}
	if !isAllowed(u, o) || canControl(&u, o) {
		t.Fatal("viewer")
	}
	if isAllowed(userFromClaims(map[string]any{"sub": "y", "groups": []any{"other"}}, "groups"), o) {
		t.Fatal("other group allowed")
	}
	if isAllowed(u, config.OIDC{AllowedEmails: []string{"a@b.C"}}) {
		t.Fatal("allowed_emails must need email_verified")
	}
	for _, ev := range []any{true, "true"} {
		u := userFromClaims(map[string]any{"sub": "x", "email": "A@B.c", "email_verified": ev}, "groups")
		if !isAllowed(u, config.OIDC{AllowedEmails: []string{"a@b.C"}}) {
			t.Fatal("emails are case-insensitive", ev)
		}
	}
	if u := userFromClaims(map[string]any{"sub": "x", "email": "a@b.c", "email_verified": false}, "groups"); isAllowed(u, config.OIDC{AllowedEmails: []string{"a@b.c"}}) {
		t.Fatal("email_verified=false allowed")
	}
	if canControl(&u, config.OIDC{}) {
		t.Fatal("no control groups -> nobody controls")
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// A corrupt (or unreadable) users.json is never read as "no users": nobody gets access, the file is not
// rewritten, and control-group members are not made admin again.
func TestCorruptUsersFileFailsClosed(t *testing.T) {
	e := setup(t)
	c := e.cookie(controller(), 0)
	if r := e.do("GET", "/api/me", nil, "Cookie", c); r.body["role"] != "admin" {
		t.Fatal("setup", string(r.raw))
	}
	garbage := []byte(`{"users": {`)
	if err := os.WriteFile(e.s.Access.Path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // users.json is re-checked once a second
	for _, ck := range []string{c, e.cookie(controller(), 0), e.cookie(viewer, 0)} {
		if r := e.do("GET", "/api/systems", nil, "Cookie", ck); r.code != 403 {
			t.Fatal("access with a corrupt users.json", r.code, string(r.raw))
		}
	}
	if r := e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 5}, "Cookie", c, "X-Requested-With", "fairy"); r.code != 403 {
		t.Fatal("control with a corrupt users.json", r.code)
	}
	if b, _ := os.ReadFile(e.s.Access.Path); string(b) != string(garbage) {
		t.Fatal("corrupt users.json was rewritten", string(b))
	}
}

// r2-p4-10 (audit 5): users.json and state.json of 0 bytes (what a power cut could leave) were read as "no
// users yet" and "no kill switch": the next member of a control group became the first admin again, and
// control was allowed. An empty file is a damaged file: nobody gets access, nobody is made admin, control is
// off, and the files are left as they are.
func TestEmptyStateFilesFailClosed(t *testing.T) {
	e := setup(t)
	c := e.cookie(controller(), 0)
	if r := e.do("GET", "/api/me", nil, "Cookie", c); r.body["role"] != "admin" {
		t.Fatal("setup", string(r.raw))
	}
	_ = e.s.Poller.Admin.SetControlDisabled(true, "root (cli)", "maintenance")
	statePath := e.s.Poller.Admin.StatePath
	for _, p := range []string{e.s.Access.Path, statePath} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(1100 * time.Millisecond) // the files are re-checked once a second
	if e.s.Access.Err() == nil || !strings.Contains(e.s.Access.Err().Error(), "is empty") {
		t.Fatal("users.json of 0 bytes must be an error:", e.s.Access.Err())
	}
	if off := e.s.Poller.Admin.ControlDisabled(); off == nil {
		t.Fatal("state.json of 0 bytes: the kill switch reads as off")
	}
	newcomer := controller() // another member of the control group, never seen before
	newcomer.Sub = "u-new"
	for _, ck := range []string{c, e.cookie(newcomer, 0), e.cookie(viewer, 0)} {
		// 401: with state.json unusable no session counts at all; 403: with only users.json unusable nobody has a role
		if r := e.do("GET", "/api/systems", nil, "Cookie", ck); r.code != 401 && r.code != 403 {
			t.Fatal("access with empty state files", r.code, string(r.raw))
		}
		if r := e.do("GET", "/api/me", nil, "Cookie", ck); r.code == 200 && r.body["role"] == "admin" {
			t.Fatal("an admin was made from an empty users.json", string(r.raw))
		}
	}
	if r := e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 5}, "Cookie", c, "X-Requested-With", "fairy"); r.code != 401 && r.code != 403 && r.code != 423 {
		t.Fatal("control with empty state files", r.code)
	}
	// users.json alone (state.json is whole again): signed in, and nobody has a role, also not a newcomer
	// from the control group
	if err := os.WriteFile(statePath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	for _, ck := range []string{e.cookie(newcomer, 0), e.cookie(controller(), 0)} {
		if r := e.do("GET", "/api/systems", nil, "Cookie", ck); r.code != 403 {
			t.Fatal("access with an empty users.json", r.code, string(r.raw))
		}
	}
	if err := os.WriteFile(statePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{e.s.Access.Path, statePath} {
		if b, err := os.ReadFile(p); err != nil || len(b) != 0 {
			t.Fatalf("%s was rewritten: %q %v", p, b, err)
		}
	}
}

// Anyone can send a bogus Bearer token: those audit entries are throttled per client IP and summed up, so an
// anonymous flood cannot fill the disk with the audit log.
func TestAnonymousAuditEntriesAreThrottled(t *testing.T) {
	e := setup(t)
	for i := 0; i < 200; i++ {
		if r := e.do("GET", "/api/me", nil, "Authorization", "Bearer fy_bogus"); r.code != 401 {
			t.Fatal(r.code)
		}
	}
	count := func(action string) int {
		return strings.Count(auditOutcomes(e.s.Cfg.AuditLog), action+" ")
	}
	if n := count("auth.token_rejected"); n != anonPerIP {
		t.Fatal("rejected-token entries written", n)
	}
	e.s.anonAudit.mu.Lock()
	e.s.anonAudit.window = e.s.anonAudit.window.Add(-61 * time.Second) // a minute later
	e.s.anonAudit.mu.Unlock()
	e.do("GET", "/api/me", nil, "Authorization", "Bearer fy_bogus")
	entries, _ := audit.Read(e.s.Cfg.AuditLog, "", "audit.suppressed", "", nil, 10)
	if len(entries) != 1 || entries[0]["params"].(map[string]any)["dropped"] != 190.0 {
		t.Fatal("summary entry", entries)
	}
	if n := count("auth.token_rejected"); n != anonPerIP+1 {
		t.Fatal("next minute logs again", n)
	}
	// a signed-in user's refused requests have their own budget, per subject (auditthrottle_test.go)
	c := e.cookie(controller(), 0)
	for i := 0; i < 30; i++ {
		e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 5}, "Cookie", c)
	}
	if n := count("auth.denied"); n != deniedPerSub {
		t.Fatal("authenticated entries", n)
	}
}

// The panel never sends a fan duty below FanDutyFloor, even to an agent that would accept it.
func TestFanDutyFloor(t *testing.T) {
	e := setup(t)
	c := e.cookie(controller(), 0)
	hdr := []string{"Cookie", c, "X-Requested-With", "fairy"}
	for _, pct := range []int{0, 10, FanDutyFloor - 1} {
		if r := e.do("POST", "/api/systems/tower/fan/duty", map[string]any{"zone": 0, "percent": pct}, hdr...); r.code != 422 {
			t.Fatal("duty below the floor accepted", pct, r.code)
		}
	}
	if r := e.do("POST", "/api/systems/tower/fan/duty", map[string]any{"zone": 0, "percent": FanDutyFloor}, hdr...); r.code != 200 {
		t.Fatal("duty at the floor", r.code, string(r.raw))
	}
	if strings.Contains(strings.Join(e.b.Requests, "\n"), "percent:0") {
		t.Fatal("a refused duty reached the agent", e.b.Requests)
	}
}

// Revoking an API token also ends the SSE streams it opened.
func TestRevokedTokenEndsItsStream(t *testing.T) {
	e := setup(t)
	raw, rec, _ := e.s.Tokens.Create("ha", "system", "read", "cli:test", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/api/stream", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	r, err := http.DefaultClient.Do(req)
	if err != nil || r.StatusCode != 200 {
		t.Fatal(err)
	}
	defer r.Body.Close()
	start := time.Now()
	if _, err := e.s.Tokens.Revoke(rec.ID); err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(r.Body) // returns when the server ends the stream (or at the 8 s deadline)
	if d := time.Since(start); d > 4*time.Second {
		t.Fatal("stream of a revoked token kept running", d)
	}
}

// branding.logo: read once at startup, served without sign-in at a fixed path with its sniffed type; anything
// but a small raster image is ignored (built-in logo) without stopping the panel.
func TestCustomLogo(t *testing.T) {
	e := setup(t)
	// no logo set: 204, not an error (r1-b1-16: the page asks on every load, and a 404 was a console error each time)
	if r := e.do("GET", "/branding/logo", nil); r.code != 204 || len(r.raw) != 0 {
		t.Fatal("no logo configured", r.code)
	}
	dir := t.TempDir()
	png := pngOfSize(t, 300)
	write := func(name string, b []byte) string {
		p := dir + "/" + name
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, b := range map[string][]byte{
		"logo.svg": []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`),
		"fake.png": []byte("<html>not an image</html>"),
		"big.png":  pngOfSize(t, MaxLogoBytes+1),
	} {
		if loadLogo(write(name, b)) != nil {
			t.Fatal("accepted", name)
		}
	}
	// The logo is checked by its first bytes only (root's own file from config.yaml). For one commit it had to be
	// a whole picture like the sign-in picture (r2-p3-13); that made an existing install lose its logo for bytes
	// after the picture's end that every browser ignores, and is out again (second review, note 11). What such a
	// file is served as does not depend on its content: the type of its first bytes, "nosniff", a policy that
	// runs nothing.
	for name, b := range map[string][]byte{
		"newline-after-the-end.png":    append(append([]byte{}, png...), '\n'),
		"zero-bytes-after-the-end.png": append(append([]byte{}, png...), make([]byte, 16)...),
		"cut-off.png":                  png[:len(png)-20],
		"begins-like.png":              append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 100)...),
		"png-then-html.png":            append(append([]byte{}, png...), "<html><script>alert(1)</script></html>"...),
	} {
		l := loadLogo(write(name, b))
		if l == nil {
			t.Fatal("an existing install's logo is no longer used:", name)
		}
		e.s.logo = l
		if r := e.do("GET", "/branding/logo", nil); r.code != 200 || r.hdr.Get("Content-Type") != "image/png" || !bytes.Equal(r.raw, b) ||
			r.hdr.Get("X-Content-Type-Options") != "nosniff" || r.hdr.Get("Content-Security-Policy") != "default-src 'none'" {
			t.Fatal(name, r.code, r.hdr)
		}
	}
	if loadLogo(dir+"/missing.png") != nil || loadLogo(dir) != nil {
		t.Fatal("missing file or directory accepted")
	}
	e.s.logo = loadLogo(write("logo.png", png))
	r := e.do("GET", "/branding/logo?path=/etc/passwd", nil) // no parameters are used
	if r.code != 200 || r.hdr.Get("Content-Type") != "image/png" || !bytes.Equal(r.raw, png) ||
		r.hdr.Get("X-Content-Type-Options") != "nosniff" || r.hdr.Get("Content-Security-Policy") != "default-src 'none'" {
		t.Fatal("logo", r.code, r.hdr)
	}
	if r2 := e.do("GET", "/branding/logo", nil, "If-None-Match", r.hdr.Get("ETag")); r2.code != 304 {
		t.Fatal("etag", r2.code)
	}
	for _, p := range []string{"/branding/logo/../../etc/passwd", "/branding/", "/branding/x"} {
		if r := e.do("GET", p, nil); r.code == 200 && !bytes.Equal(r.raw, png) {
			t.Fatal("served something else", p)
		}
	}
}

func TestUpdatesPageAndButton(t *testing.T) {
	e := setup(t)
	e.s.Updates = updates.Static{Tag: "build-42", Version: "42-bbbbbbb", Build: model.P(42), Verified: true}
	e.b.Mu.Lock()
	e.b.Metrics["laptop"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["agent_version"] = "41-aaaaaaa"
		m["features"].(jsonx.M)["self_update"] = true
		m["self_update"] = jsonx.M{"state": "idle", "since": 1.0}
		return m, nil
	}
	e.b.Mu.Unlock()
	e.s.Poller.PollOnce(context.Background())
	csrf := []string{"X-Requested-With", "fairy"}

	// viewers: neither the page nor the button
	v := e.cookie(viewer, 0)
	if r := e.do("GET", "/api/admin/updates", nil, "Cookie", v); r.code != 403 || strings.Contains(r.raw2(), "41-aaaaaaa") {
		t.Fatalf("viewer page: %d %s", r.code, r.raw)
	}
	if r := e.do("POST", "/api/admin/updates/laptop", map[string]any{}, append([]string{"Cookie", v}, csrf...)...); r.code != 403 {
		t.Fatalf("viewer update: %d %s", r.code, r.raw)
	}
	if r := e.do("POST", "/api/admin/updates", map[string]any{}, append([]string{"Cookie", v}, csrf...)...); r.code != 403 {
		t.Fatalf("viewer update all: %d %s", r.code, r.raw)
	}
	// admin page
	c := e.cookie(controller(), 0)
	r := e.do("GET", "/api/admin/updates", nil, "Cookie", c)
	if r.code != 200 {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	rows := map[string]map[string]any{}
	for _, x := range r.body["agents"].([]any) {
		rows[x.(map[string]any)["id"].(string)] = x.(map[string]any)
	}
	if l := rows["laptop"]; l["version"] != "41-aaaaaaa" || l["behind"] != true || l["can_update"] != true || l["configured"] != true {
		t.Fatalf("laptop row %v", l)
	}
	if cy := rows["tower"]; cy["can_update"] != false || cy["configured"] != false {
		t.Fatalf("tower row %v", cy)
	}
	if r.body["latest"].(map[string]any)["version"] != "42-bbbbbbb" || r.body["panel_behind"] != nil || r.body["behind"] != 1.0 {
		t.Fatal(r.body)
	}
	if l := rows["laptop"]; l["system_id"] != "laptop" {
		t.Fatal(l)
	}
	// the panel's own version against the latest Fairy-dashboard release (shown, never installed from here)
	e.s.PanelUpdates = updates.Static{Tag: "build-9", Version: "9-ccccccc", Build: model.P(9), Verified: true}
	e.s.Version = "8-ddddddd"
	r = e.do("GET", "/api/admin/updates", nil, "Cookie", c)
	if r.body["panel_behind"] != true || r.body["behind"] != 2.0 || r.body["panel_latest"].(map[string]any)["tag"] != "build-9" {
		t.Fatal(r.body)
	}
	// the button: CSRF, step-up, kill switch, configured agents only
	if r := e.do("POST", "/api/admin/updates/laptop", map[string]any{}, "Cookie", c); r.code != 403 {
		t.Fatalf("no CSRF header: %d", r.code)
	}
	old := e.cookie(controller(), 10*time.Minute)
	r = e.do("POST", "/api/admin/updates/laptop", map[string]any{}, append([]string{"Cookie", old}, csrf...)...)
	if d, _ := r.body["detail"].(map[string]any); r.code != 401 || d["code"] != "step_up_required" {
		t.Fatalf("step-up: %d %s", r.code, r.raw)
	}
	r = e.do("POST", "/api/admin/updates", map[string]any{}, append([]string{"Cookie", old}, csrf...)...)
	if d, _ := r.body["detail"].(map[string]any); r.code != 401 || d["code"] != "step_up_required" {
		t.Fatalf("step-up (all): %d %s", r.code, r.raw)
	}
	if strings.Contains(strings.Join(e.b.Requests, "\n"), "/update") {
		t.Fatal("an agent was asked to update without step-up")
	}
	if r := e.do("POST", "/api/admin/updates/tower", map[string]any{}, append([]string{"Cookie", c}, csrf...)...); r.code != 404 {
		t.Fatalf("not configured: %d %s", r.code, r.raw)
	}
	_ = e.s.Poller.Admin.SetControlDisabled(true, "root (cli)", "maintenance")
	if r := e.do("POST", "/api/admin/updates/laptop", map[string]any{}, append([]string{"Cookie", c}, csrf...)...); r.code != 423 {
		t.Fatalf("kill switch: %d", r.code)
	}
	_ = e.s.Poller.Admin.SetControlDisabled(false, "root", "")
	r = e.do("POST", "/api/admin/updates/laptop", map[string]any{}, append([]string{"Cookie", c}, csrf...)...)
	if r.code != 200 || r.body["ok"] != true || !strings.Contains(strings.Join(e.b.Requests, "\n"), "laptop POST /update map[requested_by:Alice]") {
		t.Fatalf("update: %d %s %v", r.code, r.raw, e.b.Requests)
	}
	r = e.do("POST", "/api/admin/updates", map[string]any{}, append([]string{"Cookie", c}, csrf...)...)
	if r.code != 200 || !strings.Contains(r.raw2(), `"id":"laptop","ok":true`) {
		t.Fatalf("update all: %d %s", r.code, r.raw)
	}
	// audited
	entries, _ := audit.Read(e.s.Cfg.AuditLog, "", "update", "laptop", nil, 20)
	n := 0
	for _, x := range entries {
		p, _ := x["params"].(map[string]any)
		if x["action"] == "update.agent" && x["target"] == "laptop" && x["outcome"] == "ok" && p["from"] == "41-aaaaaaa" && p["latest"] == "42-bbbbbbb" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("audit entries: %v", entries)
	}
}

// Errors the agent reports (it answered) are never 502/504: Cloudflare replaces those bodies with its own error
// page, and the user would not see "BMC refused the fan mode". 502 only means "agent unreachable".
func TestAgentErrorsAreNot502(t *testing.T) {
	e := setup(t)
	e.s.control = newRateLimiter(100)
	c := e.cookie(controller(), 0)
	hdr := []string{"Cookie", c, "X-Requested-With", "fairy"}
	var reply struct {
		st   int
		body jsonx.M
		err  error
	}
	e.b.Reply = func(string, string) (int, jsonx.M, error) { return reply.st, reply.body, reply.err }
	refused := "BMC refused the fan mode optimal: BMC completion code 0xCC (invalid data field); this board's BMC does not support it"
	for _, tc := range []struct {
		name, method, path string
		body               any
		st                 int
		agent              jsonx.M
		err                error
		want               int
		detail             string
	}{
		{"unsupported mode", "POST", "/api/systems/tower/fan/mode", map[string]any{"mode": "optimal"}, 422,
			jsonx.M{"error": refused, "code": "fan_mode_unsupported"}, nil, 422, "0xCC"},
		{"BMC failure", "POST", "/api/systems/tower/fan/mode", map[string]any{"mode": "standard"}, 502,
			jsonx.M{"error": "BMC refused the fan mode: BMC completion code 0xC3 (timeout)"}, nil, 424, "0xC3"},
		{"duty conflict", "POST", "/api/systems/tower/fan/duty", map[string]any{"zone": 0, "percent": 50}, 409,
			jsonx.M{"error": "fan mode is standard; set mode 'full' first"}, nil, 409, "full"},
		{"wrong control token", "POST", "/api/systems/tower/fan/mode", map[string]any{"mode": "full"}, 401, nil, nil, 424, "control token"},
		{"agent 500 without a body", "POST", "/api/systems/tower/fan/mode", map[string]any{"mode": "full"}, 500, nil, nil, 424, "agent error"},
		{"agent rate limit", "POST", "/api/systems/tower/fan/mode", map[string]any{"mode": "full"}, 429, jsonx.M{"error": "slow down"}, nil, 429, "slow down"},
		{"unreachable", "POST", "/api/systems/tower/fan/mode", map[string]any{"mode": "full"}, 0, nil, devices.Errf("connection refused"), 502, "agent unreachable"},
		{"fan status error", "GET", "/api/systems/tower/fan", nil, 503, jsonx.M{"error": "BMC busy"}, nil, 424, "BMC busy"},
		{"invalid fan status", "GET", "/api/systems/tower/fan", nil, 200, jsonx.M{"mode": 5}, nil, 424, "invalid fan status"},
		{"update failed", "POST", "/api/admin/updates/laptop", map[string]any{}, 500, jsonx.M{"error": "cannot download the release"}, nil, 424, "cannot download"},
		{"power: agent error", "POST", "/api/systems/laptop/power", map[string]any{"action": "reboot", "delay_s": 60, "confirm": "LAPTOP"}, 500,
			jsonx.M{"error": "shutdown.exe failed"}, nil, 424, "shutdown.exe failed"},
		{"power: control token", "POST", "/api/systems/laptop/power", map[string]any{"action": "reboot", "delay_s": 60, "confirm": "LAPTOP"}, 401,
			jsonx.M{"error": "unauthorized"}, nil, 424, "control token"}, // the agent's own 401, with its body
		{"power: a 401 that is not the agent's", "POST", "/api/systems/laptop/power", map[string]any{"action": "reboot", "delay_s": 60, "confirm": "LAPTOP"}, 401,
			nil, nil, 502, "agent unreachable: http 401, and not an answer of the agent"},
		{"power: a proxy's 504 without a body", "POST", "/api/systems/laptop/power", map[string]any{"action": "reboot", "delay_s": 60, "confirm": "LAPTOP"}, 504,
			nil, nil, 502, "agent unreachable: http 504, and not an answer of the agent"},
	} {
		reply.st, reply.body, reply.err = tc.st, tc.agent, tc.err
		r := e.do(tc.method, tc.path, tc.body, hdr...)
		if r.code != tc.want || !strings.Contains(fmt.Sprint(r.body["detail"]), tc.detail) {
			t.Fatalf("%s: %d %s", tc.name, r.code, r.raw)
		}
	}
	e.b.ProcErr = &devices.HTTPStatusError{Code: 500}
	if r := e.do("GET", "/api/systems/laptop/processes", nil, "Cookie", c); r.code != 424 {
		t.Fatal("processes: agent error", r.code, string(r.raw))
	}
	e.b.ProcErr = devices.Errf("timeout")
	if r := e.do("GET", "/api/systems/laptop/processes", nil, "Cookie", c); r.code != 502 {
		t.Fatal("processes: unreachable", r.code, string(r.raw))
	}
}

// Sleep: offered when the host's agent says the machine can sleep (Fairy-agent v0.4.2), typed name like
// shutdown, through the agent, cancellable; afterwards the host shows as asleep (from the panel) while offline.
func TestSleepFromThePanel(t *testing.T) {
	e := setup(t)
	c := e.cookie(controller(), 0)
	csrf := []string{"X-Requested-With", "fairy"}
	sleepCap := func() map[string]any {
		for _, a := range e.do("GET", "/api/systems/laptop/power", nil, "Cookie", c).body["actions"].([]any) {
			if m := a.(map[string]any); m["action"] == "sleep" {
				return m
			}
		}
		return nil
	}
	poll := func(pc jsonx.M) {
		e.b.Mu.Lock()
		e.b.Metrics["laptop"] = func() (jsonx.M, error) {
			m := testutil.AgentMetrics()
			if pc != nil {
				m["power_control"] = pc
			}
			return m, nil
		}
		e.b.Mu.Unlock()
		e.s.Poller.PollOnce(context.Background())
	}
	poll(nil) // an older agent says nothing about sleep
	if s := sleepCap(); s == nil || s["available"] != false || !strings.Contains(s["reason"].(string), "v0.4.2") {
		t.Fatal(s)
	}
	poll(jsonx.M{"enabled": true, "sleep": false, "sleep_note": "only Modern Standby"})
	if s := sleepCap(); s["available"] != false || !strings.Contains(s["reason"].(string), "Modern Standby") {
		t.Fatal(s)
	}
	poll(jsonx.M{"enabled": true, "sleep": true})
	if s := sleepCap(); s["available"] != true || s["confirm"] != "typed" || s["delayed"] != true {
		t.Fatal(s)
	}
	// Proxmox nodes are not offered sleep
	for _, a := range e.do("GET", "/api/systems/node-srv-pvea/power", nil, "Cookie", c).body["actions"].([]any) {
		if a.(map[string]any)["action"] == "sleep" {
			t.Fatal("sleep offered for a node")
		}
	}
	pw := map[string]any{"action": "sleep", "delay_s": 10}
	if r := e.do("POST", "/api/systems/laptop/power", pw, append([]string{"Cookie", c}, csrf...)...); r.code != 400 {
		t.Fatal("typed confirmation", r.code, string(r.raw))
	}
	pw["confirm"] = "laptop"
	r := e.do("POST", "/api/systems/laptop/power", pw, append([]string{"Cookie", c}, csrf...)...)
	if r.code != 200 || !strings.Contains(strings.Join(e.b.Requests, "\n"), "laptop POST /power map[action:sleep delay_s:10") {
		t.Fatal(r.code, string(r.raw), e.b.Requests)
	}
	// the agent goes quiet: asleep, not just offline
	e.b.Mu.Lock()
	e.b.Metrics["laptop"] = func() (jsonx.M, error) { return nil, context.DeadlineExceeded }
	e.b.Mu.Unlock()
	e.s.Poller.NoteSleep("laptop", model.P(time.Now().Add(-time.Second)))
	for i := 0; i < 3; i++ {
		e.s.Poller.PollOnce(context.Background())
	}
	if x := e.do("GET", "/api/systems/laptop", nil, "Cookie", c).body; x["asleep"] != true {
		t.Fatal("asleep", x["status"], x["asleep"])
	}
	// cancel forgets it
	e.s.Poller.NoteSleep("laptop", nil)
	if x := e.do("GET", "/api/systems/laptop", nil, "Cookie", c).body; x["asleep"] != nil {
		t.Fatal("still asleep")
	}
}

var testInstallers = enrollment.Fixed{Repo: "n3ptune-cpu/Fairy-agent", Tag: "build-7", Version: "7-6155fe6",
	Base: "https://github.com/n3ptune-cpu/Fairy-agent/releases/download/build-7",
	SH:   strings.Repeat("a1", 32), PS1: strings.Repeat("b2", 32), Sums: strings.Repeat("c3", 32), PinSH: true, PinPS1: true,
	PairV2SH: true, PairV2PS1: true, StdinSH: true, EnvPS1: true, MemoryPS1: true}
