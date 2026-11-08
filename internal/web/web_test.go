package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy/internal/audit"
	"github.com/n3ptune-cpu/Fairy/internal/config"
	"github.com/n3ptune-cpu/Fairy/internal/model"
	"github.com/n3ptune-cpu/Fairy/internal/poller"
	"github.com/n3ptune-cpu/Fairy/internal/testutil"
)

type env struct {
	t   *testing.T
	s   *Server
	srv *httptest.Server
	b   *testutil.Backend
}

func setup(t *testing.T) *env {
	t.Helper()
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://cyrene")
	b := testutil.NewBackend()
	p, err := poller.New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, b)
	if err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	s := New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, p, nil, "test")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, s: s, srv: srv, b: b}
}

var viewer = model.User{Sub: "u1", Name: model.P("Neptune"), Email: model.P("n@example.com"), Groups: []string{"viewers"}}

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
// knew the user.
func (e *env) staleCookie(u model.User, authAge time.Duration) string {
	sid := randToken(18)
	if err := e.s.Poller.Admin.AddSession(sid, adminstate.NewSession{Sub: u.Sub}); err != nil {
		e.t.Fatal(err)
	}
	now := time.Now()
	rec := httptest.NewRecorder()
	e.s.codec.write(rec, &sessionData{SID: sid, User: &u, IAT: now.Unix(), Seen: now.Unix(),
		AuthTime: float64(now.Add(-authAge).Unix())})
	return strings.Split(rec.Header().Get("Set-Cookie"), ";")[0]
}

type resp struct {
	code int
	body map[string]any
	raw  []byte
	hdr  http.Header
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
	// step-up: last Pocket ID sign-in 10 min ago (window 5 min)
	old := e.cookie(controller(), 10*time.Minute)
	r := e.do("PUT", "/api/systems/laptop/poll", body, append([]string{"Cookie", old}, csrf...)...)
	d, _ := r.body["detail"].(map[string]any)
	if r.code != 401 || d["code"] != "step_up_required" || d["window_s"] != 300.0 {
		t.Fatalf("step-up: %d %s", r.code, r.raw)
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
	if r.code != 200 || len(e.b.PVECalls) != 1 || e.b.PVECalls[0] != "pve /nodes/NSX-KACHINA/qemu/100/status/reboot " {
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
	if got["action"] != "poll.set" || got["target"] != "laptop" || got["outcome"] != "ok" || got["actor"] != "n@example.com" || got["source"] != "ui" {
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
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
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
	// signed-in actions are never throttled
	c := e.cookie(controller(), 0)
	for i := 0; i < 30; i++ {
		e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 5}, "Cookie", c)
	}
	if n := count("auth.denied"); n != 30 {
		t.Fatal("authenticated entries", n)
	}
}

// The panel never sends a fan duty below FanDutyFloor, even to an agent that would accept it.
func TestFanDutyFloor(t *testing.T) {
	e := setup(t)
	c := e.cookie(controller(), 0)
	hdr := []string{"Cookie", c, "X-Requested-With", "fairy"}
	for _, pct := range []int{0, 10, FanDutyFloor - 1} {
		if r := e.do("POST", "/api/systems/cyrene/fan/duty", map[string]any{"zone": 0, "percent": pct}, hdr...); r.code != 422 {
			t.Fatal("duty below the floor accepted", pct, r.code)
		}
	}
	if r := e.do("POST", "/api/systems/cyrene/fan/duty", map[string]any{"zone": 0, "percent": FanDutyFloor}, hdr...); r.code != 200 {
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
	if r := e.do("GET", "/branding/logo", nil); r.code != 404 {
		t.Fatal("no logo configured", r.code)
	}
	dir := t.TempDir()
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 100)...)
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
		"big.png":  append(append([]byte{}, png...), make([]byte, MaxLogoBytes)...),
	} {
		if loadLogo(write(name, b)) != nil {
			t.Fatal("accepted", name)
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
