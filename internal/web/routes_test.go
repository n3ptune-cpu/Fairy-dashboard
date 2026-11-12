package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// The route and auth matrix (NEW-3): who may use every state-changing route of the panel, asked of the real
// handler chain. The table is the reviewer's, with the two rows this batch changed (cancelling an Add host;
// the new confirmation route). A route that is registered in the source and missing here fails the test, so a
// new write cannot be added without deciding these questions.

// writeRoute is one row: a route that changes something, a request for it, and who may send that request.
type writeRoute struct {
	pattern string // as registered
	path    string // the request's path
	body    any    // "POWER" / "CONFIRM" / "SETTINGS" / "SESSION" / "WAKERUN": filled in from the panel under test
	ctype   string
	// adminOnly: a viewer (role viewer, CSRF header, fresh sign-in) gets 403. Otherwise a viewer reaches it.
	adminOnly bool
	// pendingOK: a user who is still waiting for access reaches it (their own session and settings only).
	pendingOK bool
	// stepUp: an admin whose sign-in is an hour old gets 401 step_up_required.
	stepUp bool
	// each: also an admin who signed in this second is asked to confirm; the action never rides on an earlier
	// confirmation (stepUpEach). The admin of these rows has a second session: the one the request is about.
	each bool
	// readToken / controlToken: an API token of that scope reaches the handler (false: 403).
	readToken, controlToken bool
	// killSwitch: with control disabled an admin gets 423.
	killSwitch bool
}

var matrixRoutes = []writeRoute{
	{pattern: "POST /api/me/active", path: "/api/me/active", pendingOK: true, readToken: true, controlToken: true},
	{pattern: "DELETE /api/me/pending", path: "/api/me/pending", pendingOK: true, readToken: true, controlToken: true},
	{pattern: "POST /api/me/step-up", path: "/api/me/step-up", body: map[string]any{"pending_id": "x", "next": "/"}, pendingOK: true},
	// the Sessions page: a viewer may end their own other sessions (the row's request names an admin's session,
	// which is none of a viewer's: 404, not 403); everyone's sessions are for admins
	{pattern: "POST /api/me/sessions/revoke", path: "/api/me/sessions/revoke", body: "SESSION", stepUp: true, each: true},
	{pattern: "POST /api/me/sessions/revoke-others", path: "/api/me/sessions/revoke-others", stepUp: true, each: true},
	{pattern: "POST /api/admin/sessions/revoke", path: "/api/admin/sessions/revoke", body: "SESSION", adminOnly: true, stepUp: true, each: true},
	{pattern: "PUT /api/settings", path: "/api/settings", body: "SETTINGS", pendingOK: true},
	{pattern: "POST /api/settings/background", path: "/api/settings/background", body: tinyPNG, ctype: "image/png"},
	{pattern: "DELETE /api/settings/background", path: "/api/settings/background", pendingOK: true},
	// the sign-in wallpaper is the panel's, not a user's own: admins only; no confirmation, like the other settings
	{pattern: "POST /api/admin/signin-wallpaper", path: "/api/admin/signin-wallpaper", body: tinyPNG, ctype: "image/png", adminOnly: true},
	{pattern: "DELETE /api/admin/signin-wallpaper", path: "/api/admin/signin-wallpaper", adminOnly: true},
	{pattern: "POST /api/notifications/read", path: "/api/notifications/read", body: map[string]any{"all": true}, readToken: true, controlToken: true},
	{pattern: "POST /api/notifications/dismiss", path: "/api/notifications/dismiss", body: map[string]any{"ids": []int{1}}, readToken: true, controlToken: true},
	{pattern: "POST /api/systems/{id}/fan/mode", path: "/api/systems/tower/fan/mode", body: map[string]any{"mode": "full"}, adminOnly: true, stepUp: true, controlToken: true, killSwitch: true},
	{pattern: "POST /api/systems/{id}/fan/duty", path: "/api/systems/tower/fan/duty", body: map[string]any{"zone": 0, "percent": 50}, adminOnly: true, stepUp: true, controlToken: true, killSwitch: true},
	{pattern: "POST /api/systems/{id}/power", path: "/api/systems/laptop/power", body: "POWER", adminOnly: true, stepUp: true, controlToken: true, killSwitch: true},
	{pattern: "PUT /api/systems/{id}/poll", path: "/api/systems/laptop/poll", body: map[string]any{"interval_s": 5}, adminOnly: true, stepUp: true, controlToken: true, killSwitch: true},
	// a host's model name is a label an admin sets for everyone to see: guarded like the sign-in wallpaper (no
	// confirmation, no API token, and the kill switch, which stops device control, does not stop it)
	{pattern: "PUT /api/systems/{id}/custom-model", path: "/api/systems/laptop/custom-model", body: map[string]any{"custom_model": "My PC"}, adminOnly: true},
	{pattern: "POST /api/systems/{id}/helper/stop", path: "/api/systems/laptop/helper/stop", body: "CONFIRM", adminOnly: true, stepUp: true, killSwitch: true},
	{pattern: "POST /api/systems/{id}/helper/start", path: "/api/systems/laptop/helper/start", body: "CONFIRM", adminOnly: true, stepUp: true, killSwitch: true},
	{pattern: "POST /api/admin/updates", path: "/api/admin/updates", body: map[string]any{}, adminOnly: true, stepUp: true, controlToken: true, killSwitch: true},
	{pattern: "POST /api/admin/updates/{id}", path: "/api/admin/updates/laptop", body: map[string]any{}, adminOnly: true, stepUp: true, controlToken: true, killSwitch: true},
	{pattern: "POST /api/admin/update-check", path: "/api/admin/update-check", body: map[string]any{}, adminOnly: true},
	{pattern: "POST /api/admin/panel-update", path: "/api/admin/panel-update", body: map[string]any{}, adminOnly: true, stepUp: true, killSwitch: true},
	{pattern: "PUT /api/admin/users/{sub}", path: "/api/admin/users/u1", body: map[string]any{"role": "viewer", "sites": []string{}}, adminOnly: true, stepUp: true},
	{pattern: "DELETE /api/admin/users/{sub}", path: "/api/admin/users/u1", adminOnly: true, stepUp: true},
	{pattern: "POST /api/admin/sites", path: "/api/admin/sites", body: map[string]any{"name": "Attic"}, adminOnly: true, stepUp: true},
	{pattern: "PUT /api/admin/sites/{name}", path: "/api/admin/sites/Home", body: map[string]any{"name": "Loft"}, adminOnly: true, stepUp: true},
	{pattern: "DELETE /api/admin/sites/{name}", path: "/api/admin/sites/Home", adminOnly: true, stepUp: true},
	{pattern: "PUT /api/admin/assign", path: "/api/admin/assign", body: map[string]any{"target": "laptop", "site": ""}, adminOnly: true, stepUp: true},
	{pattern: "POST /api/admin/hosts", path: "/api/admin/hosts", body: map[string]any{"name": "SRV-WINPC", "os": "linux"}, adminOnly: true, stepUp: true, killSwitch: true},
	{pattern: "POST /api/admin/hosts/{id}/repair", path: "/api/admin/hosts/srv-winpc/repair", body: map[string]any{"confirm": "SRV-WINPC"}, adminOnly: true, stepUp: true, killSwitch: true},
	{pattern: "DELETE /api/admin/hosts/{id}", path: "/api/admin/hosts/srv-winpc", body: map[string]any{"confirm": "SRV-WINPC"}, adminOnly: true, stepUp: true, killSwitch: true},
	// NEW-3: was open to a control token and needed no step-up
	{pattern: "DELETE /api/admin/enrollments/{id}", path: "/api/admin/enrollments/nope", adminOnly: true, stepUp: true},
	// "Update unpowered hosts": browser sessions only. Starting a run asks to confirm every time; calling
	// one off is not kept back by the kill switch
	{pattern: "POST /api/admin/wake-update", path: "/api/admin/wake-update", body: "WAKERUN", adminOnly: true, stepUp: true, each: true, killSwitch: true},
	{pattern: "DELETE /api/admin/wake-update/{id}", path: "/api/admin/wake-update/nope", adminOnly: true, stepUp: true},
	{pattern: "PUT /api/systems/{id}/wake-update", path: "/api/systems/laptop/wake-update", body: map[string]any{"enabled": true}, adminOnly: true, stepUp: true, killSwitch: true},
}

// Writes outside /api/, and the GET routes that change something. They are not rows of the table (they are not
// "who may use this action" questions); each has its own tests, named here so the list stays whole:
//   - POST /auth/logout: TestNoCrossSiteSignOut (CSRF header before the session; ends the session).
//   - GET /auth/login: starts a sign-in, never runs and always forgets a kept action (TestNoLinkCanNameAKeptAction).
//   - GET /auth/callback: finishes a sign-in; runs a kept action only for the round trip and client it was
//     armed for (TestKeptActionIsBoundToTheClientThatAskedForIt).
//   - GET /api/me: hands the result of a confirmed action out once.
//   - GET /api/admin/enrollments/{id}?reveal=1: the join code once (TestJoinCodeIsNotRevealedToAPITokens,
//     TestAddHostAfterStepUpRevealsCodeOnce).
//   - POST /api/demo/view: registered in demo mode only (loopback).
var writesOutsideTheTable = map[string]bool{"POST /auth/logout": true, "POST /api/demo/view": true}

// probe is one request of the matrix; raw sends it (a body that is bytes goes as it is, anything else as JSON).
type probe struct {
	method, path string
	body         any
	ctype        string
}

// a 1x1 PNG, for the background upload
var tinyPNG = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0x0d, 0x49, 0x48, 0x44, 0x52, 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0,
	0x1f, 0x15, 0xc4, 0x89, 0, 0, 0, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0xf8, 0xcf, 0xc0, 0xf0, 0x1f, 0, 5, 0, 1, 0xff, 0x89, 0x99,
	0x3d, 0x1d, 0, 0, 0, 0, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82}

func (e *env) raw(p probe, hdr ...string) resp {
	var rd io.Reader
	switch b := p.body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(p.method, e.srv.URL+p.path, rd)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	e.addAlso(req)
	if req.Header.Get("Content-Type") == "" {
		if p.ctype != "" {
			req.Header.Set("Content-Type", p.ctype)
		} else if p.body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer r.Body.Close()
	rawb, _ := io.ReadAll(r.Body)
	out := resp{code: r.StatusCode, raw: rawb, hdr: r.Header}
	_ = json.Unmarshal(rawb, &out.body)
	return out
}

// registeredRoutes reads the routes out of the source, as TestOpenAPIListsEveryRoute does.
func registeredRoutes(t *testing.T) []string {
	t.Helper()
	consts := map[string]string{"activePath": activePath, "stepUpPath": stepUpPath}
	routeRe := regexp.MustCompile(`HandleFunc\("([A-Z]+) ([^"]*)"(?:\s*\+\s*(\w+))?`)
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range routeRe.FindAllStringSubmatch(string(src), -1) {
			path := m[2]
			if m[3] != "" {
				c, ok := consts[m[3]]
				if !ok {
					t.Fatalf("%s: route built from %q: add it to consts in this test", f.Name(), m[3])
				}
				path += c
			}
			out = append(out, m[1]+" "+path)
		}
	}
	sort.Strings(out)
	if len(out) < 50 {
		t.Fatalf("only %d routes found in the source: the pattern no longer matches", len(out))
	}
	return out
}

func refused(r resp) bool { return r.code == 401 || r.code == 403 }

func TestRouteAuthMatrix(t *testing.T) {
	inTable := map[string]writeRoute{}
	for _, wr := range matrixRoutes {
		inTable[wr.pattern] = wr
	}
	// 1. the table is whole: every registered write is a row, every row is a registered route
	registered := map[string]bool{}
	for _, pat := range registeredRoutes(t) {
		registered[pat] = true
		if strings.HasPrefix(pat, "GET ") || writesOutsideTheTable[pat] {
			continue
		}
		if _, ok := inTable[pat]; !ok {
			t.Errorf("%s changes something and is not in the route / auth matrix: add a row that says who may use it", pat)
		}
	}
	for pat := range inTable {
		if !registered[pat] {
			t.Errorf("%s is in the matrix but not a registered route", pat)
		}
	}
	// 2. the sign-in-only list and the matrix agree, and every route that asks to confirm has its own description
	for _, wr := range matrixRoutes {
		if only := sessionOnlyControl[wr.pattern]; only && (wr.readToken || wr.controlToken) {
			t.Errorf("%s is sign-in-only but the matrix lets a token in", wr.pattern)
		}
		if _, ok := actionText[wr.pattern]; wr.stepUp && !ok {
			t.Errorf("%s asks to confirm but has no description in actionText", wr.pattern)
		}
	}

	csrf := []string{"X-Requested-With", "fairy"}
	for _, wr := range matrixRoutes {
		method, _, _ := strings.Cut(wr.pattern, " ")
		// a fresh panel for the requests that are refused, and one for each that may go through
		fresh := func() (*env, probe, string) {
			e := setup(t)
			e.s.control, e.s.pollCtl, e.s.adminRL = newRateLimiter(1000), newRateLimiter(1000), newRateLimiter(1000)
			e.helperPoll("laptop", "running", helperAll...)
			_ = e.s.Access.EnsureSites([]string{"Home"})
			admin := e.cookie(controller(), 0)
			name, _ := e.do("GET", "/api/systems/laptop", nil, "Cookie", admin).body["name"].(string)
			q := probe{method: method, path: wr.path, body: wr.body, ctype: wr.ctype}
			switch wr.body {
			case "POWER":
				q.body = map[string]any{"action": "shutdown", "delay_s": 10, "confirm": name}
			case "CONFIRM":
				q.body = map[string]any{"confirm": name}
			case "SETTINGS":
				q.body = map[string]any{}
			case "WAKERUN": // laptop is on and switched on for these runs: a list the panel accepts
				if err := e.s.Poller.Admin.SetWakeUpdate("laptop", true); err != nil {
					t.Fatal(err)
				}
				q.body = map[string]any{"update_only": []string{"laptop"}}
			}
			if wr.each { // the admin is signed in a second time; a "SESSION" request is about that session
				other := e.s.codec.handle(sidOf(e, e.cookie(controller(), 0)))
				if wr.body == "SESSION" {
					q.body = map[string]any{"session": other}
				}
			}
			if _, got := e.s.mux.Handler(httptest.NewRequest(method, wr.path, nil)); got != wr.pattern {
				t.Fatalf("%s: the probe %s matches %q", wr.pattern, wr.path, got)
			}
			return e, q, admin
		}
		e, q, admin := fresh()
		if r := e.raw(q); r.code != 401 {
			t.Errorf("%s: not signed in: %d", wr.pattern, r.code)
		}
		for name, hdr := range map[string][]string{
			"without the CSRF header":     {"Cookie", admin},
			"from another origin":         append([]string{"Cookie", admin, "Origin", "https://evil.example"}, csrf...),
			"Sec-Fetch-Site: cross-site":  append([]string{"Cookie", admin, "Sec-Fetch-Site", "cross-site"}, csrf...),
			"Sec-Fetch-Site: same-site":   append([]string{"Cookie", admin, "Sec-Fetch-Site", "same-site"}, csrf...),
			"a form post (no JSON, no X)": {"Cookie", admin, "Content-Type", "application/x-www-form-urlencoded"},
		} {
			if r := e.raw(q, hdr...); r.code != 403 {
				t.Errorf("%s, an admin's cookie %s: %d (want 403)", wr.pattern, name, r.code)
			}
		}
		stale := e.raw(q, append([]string{"Cookie", e.staleCookie(controller(), time.Hour)}, csrf...)...)
		if wr.stepUp != stepUpRequired(stale) {
			t.Errorf("%s, an admin whose sign-in is an hour old: step-up %v, want %v (%d %s)", wr.pattern, stepUpRequired(stale), wr.stepUp, stale.code, stale.raw)
		}
		if !wr.stepUp && refused(stale) {
			t.Errorf("%s, an admin whose sign-in is an hour old: %d %s", wr.pattern, stale.code, stale.raw)
		}
		pend := e.raw(q, append([]string{"Cookie", e.cookie(model.User{Sub: "u8", Name: model.P("Pat"), Groups: []string{"viewers"}}, 0)}, csrf...)...)
		if wr.pendingOK == refused(pend) {
			t.Errorf("%s, a user waiting for access: %d (allowed: %v)", wr.pattern, pend.code, wr.pendingOK)
		}
		ro, _, _ := e.s.Tokens.Create("ha", "system", "read", "cli:test", 0)
		if r := e.raw(q, "Authorization", "Bearer "+ro); wr.readToken == (r.code == 403) || r.code == 401 {
			t.Errorf("%s, a read token: %d (allowed: %v)", wr.pattern, r.code, wr.readToken)
		}
		if !wr.controlToken {
			ctl, _, _ := e.s.Tokens.Create("ops", "system", "control", "cli:test", 0)
			if r := e.raw(q, "Authorization", "Bearer "+ctl); r.code != 403 {
				t.Errorf("%s, a control token: %d (want 403)", wr.pattern, r.code)
			}
		}
		if wr.adminOnly || wr.killSwitch {
			if err := e.s.Poller.Admin.SetControlDisabled(true, "test", "matrix"); err != nil {
				t.Fatal(err)
			}
			if r := e.raw(q, append([]string{"Cookie", admin}, csrf...)...); wr.killSwitch != (r.code == 423) {
				t.Errorf("%s with the kill switch on: %d (refused by it: %v)", wr.pattern, r.code, wr.killSwitch)
			}
		}

		// the requests that may go through, each on its own panel
		e, q, _ = fresh()
		v := model.User{Sub: "u7", Name: model.P("Vee"), Email: model.P("v@example.com"), Groups: []string{"viewers"}}
		ck := e.cookie(v, 0)
		if _, _, err := e.s.Access.SetRole("u7", "viewer", []string{"Home"}, "test", false); err != nil {
			t.Fatal(err)
		}
		if r := e.raw(q, append([]string{"Cookie", ck}, csrf...)...); wr.adminOnly != (r.code == 403) || r.code == 401 {
			t.Errorf("%s, a viewer: %d (admin only: %v)", wr.pattern, r.code, wr.adminOnly)
		}
		if wr.controlToken {
			e, q, _ = fresh()
			ctl, _, _ := e.s.Tokens.Create("ops", "system", "control", "cli:test", 0)
			if r := e.raw(q, "Authorization", "Bearer "+ctl); refused(r) {
				t.Errorf("%s, a control token: %d %s (want: accepted)", wr.pattern, r.code, r.raw)
			}
		}
		e, q, admin = fresh()
		if r := e.raw(q, append([]string{"Cookie", admin}, csrf...)...); wr.each != stepUpRequired(r) || (!wr.each && refused(r)) {
			t.Errorf("%s, an admin who just signed in: %d %s (asked to confirm every time: %v)", wr.pattern, r.code, r.raw, wr.each)
		}
	}
}

// NEW-3: cancelling a waiting Add host. A control token and a session whose sign-in is a day old both called
// it off before; now the token is refused, the old sign-in is asked to confirm (and the cancel then runs by
// itself), and the kill switch does not stand in the way.
func TestCancellingAnAddHostNeedsASignInAndAConfirmation(t *testing.T) {
	a := signedIn(t)
	e := a.e
	e.s.Enroll.PanelAddr = "100.64.0.10"
	e.s.Enroll.Interval = time.Hour
	csrf := []string{"X-Requested-With", "fairy", "Content-Type", "application/json"}
	if r := a.stepUp("&next=/", false); r.code != 303 { // the admin confirms once: inside the window from here
		t.Fatal(r.code)
	}
	mk := func() string {
		r := e.do("POST", "/api/admin/hosts", map[string]any{"name": "SRV-WINPC", "os": "linux"},
			append([]string{"Cookie", a.sess}, csrf...)...)
		id, _ := r.body["id"].(string)
		if r.code != 201 || id == "" {
			t.Fatal("add host", r.code, string(r.raw))
		}
		return id
	}
	waiting := func(id string) bool {
		en, ok := e.s.Enroll.Get(id)
		return ok && !en.Done
	}
	id := mk()
	for scope := range map[string]bool{"control": true, "read": true} {
		tok, _, _ := e.s.Tokens.Create("t-"+scope, "system", scope, "cli:test", 0)
		if r := e.do("DELETE", "/api/admin/enrollments/"+id, nil, "Authorization", "Bearer "+tok); r.code != 403 || !waiting(id) {
			t.Fatalf("%s token: %d %s", scope, r.code, r.raw)
		}
	}
	// the admin, once the confirmation is older than the window
	a.clk.advance(time.Duration(e.s.Cfg.StepUpWindowS+5) * time.Second)
	r := e.do("DELETE", "/api/admin/enrollments/"+id, nil, append([]string{"Cookie", a.sess}, csrf...)...)
	d, _ := r.body["detail"].(map[string]any)
	if !stepUpRequired(r) || !waiting(id) || d["action"] != "cancel the Add host "+id {
		t.Fatalf("an old sign-in: %d %s", r.code, r.raw)
	}
	if cb := a.confirm(pendingID(r), false); cb.code != 303 || waiting(id) {
		t.Fatal("after the confirmation the cancel runs:", cb.code, waiting(id))
	}
	if !strings.Contains(auditOutcomes(e.s.Cfg.AuditLog), "host.cancel ok") {
		t.Fatal("not audited")
	}
	// with the kill switch on, an admin inside the window can still call a pairing off
	id = mk()
	if err := e.s.Poller.Admin.SetControlDisabled(true, "test", "kill switch"); err != nil {
		t.Fatal(err)
	}
	if r := e.do("DELETE", "/api/admin/enrollments/"+id, nil, append([]string{"Cookie", a.sess}, csrf...)...); r.code != 200 || waiting(id) {
		t.Fatalf("kill switch on: %d %s", r.code, r.raw)
	}
	// a viewer and a user waiting for access cannot
	other := model.User{Sub: "u9", Name: model.P("Pat"), Groups: []string{"viewers"}}
	if r := e.do("DELETE", "/api/admin/enrollments/"+id, nil, append([]string{"Cookie", e.cookie(other, 0)}, csrf...)...); r.code != 403 {
		t.Fatal("not an admin:", r.code)
	}
}
