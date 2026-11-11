package web

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// L3: an API token skips the step-up prompt, so the most sensitive control actions refuse tokens altogether:
// helper stop / start, Add host, Re-pair, Remove, cancelling a waiting Add host (NEW-3), and "Update unpowered
// hosts" (starting a run, cancelling it, the per-host switch). Everything else a control token could
// do, it still can.
func TestAPITokensRefusedOnSignInOnlyActions(t *testing.T) {
	e := setup(t)
	e.helperPoll("laptop", "running", helperAll...)
	ctl, _, _ := e.s.Tokens.Create("ops", "system", "control", "cli:test", 0)
	ro, _, _ := e.s.Tokens.Create("ha", "system", "read", "cli:test", 0)
	name := e.do("GET", "/api/systems/laptop", nil, "Authorization", "Bearer "+ctl).body["name"].(string)
	// an Add host that an admin made and that is waiting: a token must not be able to call it off
	e.s.Enroll.PanelAddr = "100.64.0.10"
	e.s.Enroll.Interval = time.Hour
	mk := e.do("POST", "/api/admin/hosts", map[string]any{"name": "SRV-WAITING", "os": "linux"}, "Cookie", e.cookie(controller(), 0),
		"X-Requested-With", "fairy", "Content-Type", "application/json")
	enrollID, _ := mk.body["id"].(string)
	if mk.code != 201 || enrollID == "" {
		t.Fatal("add host", mk.code, string(mk.raw))
	}
	refused := []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/systems/laptop/helper/stop", map[string]any{"confirm": name}},
		{"POST", "/api/systems/laptop/helper/start", map[string]any{"confirm": name}},
		{"POST", "/api/admin/hosts", map[string]any{"name": "SRV-WINPC", "os": "linux"}},
		{"POST", "/api/admin/hosts/srv-winpc/repair", map[string]any{"confirm": "SRV-WINPC"}},
		{"DELETE", "/api/admin/hosts/srv-winpc", map[string]any{"confirm": "SRV-WINPC"}},
		{"DELETE", "/api/admin/enrollments/" + enrollID, nil},
		// "Update unpowered hosts": starting a run, calling one off, and the per-host switch
		{"POST", "/api/admin/wake-update", map[string]any{"update_only": []string{"laptop"}}},
		{"DELETE", "/api/admin/wake-update/0000000000000000", nil},
		{"PUT", "/api/systems/laptop/wake-update", map[string]any{"enabled": true}},
	}
	before := e.agentRequests()
	for _, rt := range refused {
		for scope, tok := range map[string]string{"control": ctl, "read": ro} {
			r := e.do(rt.method, rt.path, rt.body, "Authorization", "Bearer "+tok, "Content-Type", "application/json")
			if r.code != 403 || !strings.Contains(string(r.raw), "browser sign-in") {
				t.Errorf("%s token on %s %s: %d %s", scope, rt.method, rt.path, r.code, r.raw)
			}
		}
	}
	if after := e.agentRequests(); after != before || len(e.s.Enroll.List()) != 1 {
		t.Fatal("a refused token request reached an agent or made an enrollment")
	}
	if a, hist := e.s.WakeRuns.Snapshot(); a != nil || len(hist) != 0 || e.s.Poller.Admin.WakeUpdateEnabled("laptop") {
		t.Fatal("a refused token request started a run or switched a machine on for runs")
	}
	if en, ok := e.s.Enroll.Get(enrollID); !ok || en.Done {
		t.Fatalf("a token called the waiting Add host off: %+v", en)
	}
	if !strings.Contains(auditOutcomes(e.s.Cfg.AuditLog), "auth.denied 403: API token on an action that needs a browser sign-in") {
		t.Fatal("the refusal is not in the audit log")
	}

	// still accepted with a control token, without any step-up (the documented design for tokens)
	for _, rt := range []struct {
		method, path string
		body         any
	}{
		{"PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 5}},
		{"POST", "/api/systems/laptop/power", map[string]any{"action": "shutdown", "delay_s": 10, "confirm": name}},
		{"POST", "/api/systems/tower/fan/mode", map[string]any{"mode": "full"}},
		{"POST", "/api/admin/updates/laptop", map[string]any{}},
		{"POST", "/api/admin/updates", map[string]any{}},
		{"DELETE", "/api/me/pending", nil},
	} {
		r := e.do(rt.method, rt.path, rt.body, "Authorization", "Bearer "+ctl, "Content-Type", "application/json")
		if r.code == 401 || r.code == 403 {
			t.Errorf("control token on %s %s must still be accepted: %d %s", rt.method, rt.path, r.code, r.raw)
		}
	}
	// reads of the same pages stay open to tokens as before
	if r := e.do("GET", "/api/admin/hosts", nil, "Authorization", "Bearer "+ctl); r.code != 200 {
		t.Error("Add host page data with a control token", r.code)
	}

	// a browser session is not affected: it gets the step-up (or runs), never this refusal
	c := e.cookie(controller(), 0)
	r := e.do("POST", "/api/systems/laptop/helper/stop", map[string]any{"confirm": name}, "Cookie", c, "X-Requested-With", "fairy",
		"Content-Type", "application/json")
	if r.code != 200 {
		t.Fatal("helper stop from a signed-in admin", r.code, string(r.raw))
	}
}

// Every pattern in sessionOnlyControl must be a route the server really has: a renamed route would otherwise
// drop out of the list without anyone noticing.
func TestSignInOnlyActionsAreRealRoutes(t *testing.T) {
	e := setup(t)
	if len(sessionOnlyControl) != 10 { // 10th: switching on a machine's own check of approvals (it needs a passkey)
		t.Fatal("the list changed: update this test and the docs", len(sessionOnlyControl))
	}
	for pat := range sessionOnlyControl {
		method, path, _ := strings.Cut(pat, " ")
		req := httptest.NewRequest(method, strings.ReplaceAll(path, "{id}", "x"), nil)
		if _, got := e.s.mux.Handler(req); got != pat {
			t.Errorf("%q is not a registered route (the request matched %q)", pat, got)
		}
	}
}

// countingBody records how much of a request body the server read.
type countingBody struct {
	r    *strings.Reader
	read int
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read += n
	return n, err
}
func (b *countingBody) Close() error { return nil }

// I24: a write from someone who is not signed in is answered before any of its body is read or parsed. The
// four Users-page routes used to parse the body first (422 for a bad one, without a sign-in).
func TestBodyIsNotReadBeforeSignIn(t *testing.T) {
	e := setup(t)
	garbage := strings.Repeat("{", 60<<10) // 60 KiB that is not JSON
	send := func(method, path string, hdr ...string) (int, int) {
		body := &countingBody{r: strings.NewReader(garbage)}
		req := httptest.NewRequest(method, path, body)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Requested-With", "fairy")
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		e.s.Handler().ServeHTTP(rec, req)
		return rec.Code, body.read
	}
	admin4 := [][2]string{{"PUT", "/api/admin/users/u9"}, {"POST", "/api/admin/sites"}, {"PUT", "/api/admin/sites/Bob"}, {"PUT", "/api/admin/assign"}}
	others := [][2]string{{"POST", "/api/admin/hosts"}, {"PUT", "/api/systems/laptop/poll"}, {"POST", "/api/systems/laptop/power"},
		{"PUT", "/api/settings"}, {"POST", "/api/notifications/read"}, {"POST", "/api/me/active"}, {"DELETE", "/api/me/pending"}}
	for _, rt := range append(admin4, others...) {
		if code, read := send(rt[0], rt[1]); code != 401 || read != 0 {
			t.Errorf("no sign-in, %s %s: %d, %d bytes of the body read (want 401, 0)", rt[0], rt[1], code, read)
		}
		if code, read := send(rt[0], rt[1], "Cookie", "__Host-fairy_session=forged.value"); code != 401 || read != 0 {
			t.Errorf("forged cookie, %s %s: %d, %d bytes read", rt[0], rt[1], code, read)
		}
		// a rejected API token: 401 where tokens are taken, 403 where they are not, and still nothing read
		if code, read := send(rt[0], rt[1], "Authorization", "Bearer fy_nope"); (code != 401 && code != 403) || read != 0 {
			t.Errorf("bogus token, %s %s: %d, %d bytes read", rt[0], rt[1], code, read)
		}
	}
	// a valid API token on the Users-page routes: refused (403) before the body is parsed
	ctl, _, _ := e.s.Tokens.Create("ops", "system", "control", "cli:test", 0)
	for _, rt := range admin4 {
		body := &countingBody{r: strings.NewReader(garbage)}
		req := httptest.NewRequest(rt[0], rt[1], body)
		req.Header.Set("Authorization", "Bearer "+ctl)
		rec := httptest.NewRecorder()
		e.s.Handler().ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Errorf("token on %s %s: %d, want 403 (not a 422 from parsing the body)", rt[0], rt[1], rec.Code)
		}
	}
	// signed in: the body is read and a bad one is still told so
	c := e.cookie(controller(), 0)
	if code, read := send("POST", "/api/admin/sites", "Cookie", c); code != 422 || read == 0 {
		t.Errorf("signed-in admin with a bad body: %d, %d bytes read (want 422)", code, read)
	}
	// one rejected token is audited once per request, not once per check
	if n := strings.Count(auditOutcomes(e.s.Cfg.AuditLog), "auth.token_rejected"); n == 0 || n > len(admin4)+len(others) {
		t.Errorf("auth.token_rejected entries: %d for %d requests", n, len(admin4)+len(others))
	}
}
