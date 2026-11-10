package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// L3: an API token skips the step-up prompt, so the most sensitive control actions refuse tokens altogether:
// helper stop / start, Add host, Re-pair and Remove. Everything else a control token could do, it still can.
func TestAPITokensRefusedOnSignInOnlyActions(t *testing.T) {
	e := setup(t)
	e.helperPoll("laptop", "running", helperAll...)
	ctl, _, _ := e.s.Tokens.Create("ops", "system", "control", "cli:test", 0)
	ro, _, _ := e.s.Tokens.Create("ha", "system", "read", "cli:test", 0)
	name := e.do("GET", "/api/systems/laptop", nil, "Authorization", "Bearer "+ctl).body["name"].(string)
	refused := []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/systems/laptop/helper/stop", map[string]any{"confirm": name}},
		{"POST", "/api/systems/laptop/helper/start", map[string]any{"confirm": name}},
		{"POST", "/api/admin/hosts", map[string]any{"name": "NSX-FURINA", "os": "linux"}},
		{"POST", "/api/admin/hosts/nsx-furina/repair", map[string]any{"confirm": "NSX-FURINA"}},
		{"DELETE", "/api/admin/hosts/nsx-furina", map[string]any{"confirm": "NSX-FURINA"}},
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
	if after := e.agentRequests(); after != before || len(e.s.Enroll.List()) != 0 {
		t.Fatal("a refused token request reached an agent or made an enrollment")
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
		{"POST", "/api/systems/cyrene/fan/mode", map[string]any{"mode": "full"}},
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
	if len(sessionOnlyControl) != 5 {
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
