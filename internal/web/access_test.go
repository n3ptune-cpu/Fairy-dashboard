package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/ups"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// The test homelab, by site:
//
//	Neptune  the Proxmox cluster (site: in config) with nodes node-nsx-kachina, node-nsx-snezhnaya and guests
//	         qemu-100 (win11), lxc-101 (minecraft); agent laptop (site: in config) with its desk plug
//	         (laptop/desk) and UPS ups1 (powers laptop and secret-box)
//	Straw    agent cyrene (assigned on the Users page) and guest qemu-120 "truenas" (assigned on the Users page,
//	         although its node NSX-SNEZHNAYA is Neptune's)
//	none     agent secret-box, with its own plug (secret-box/desk) and UPS (secret-box/ups2): admins only
const (
	subAdmin   = "sub-admin"
	subNeptune = "sub-neptune"
	subStraw   = "sub-straw"
	subPending = "sub-pending"
)

var (
	// What each viewer must never see in any response.
	hiddenFromNeptune = []string{"cyrene", "qemu-120", "truenas", "secret-box", "ups2"}
	hiddenFromStraw   = []string{"laptop", "node-nsx-kachina", "NSX-KACHINA", "node-nsx-snezhnaya", "NSX-SNEZHNAYA",
		"qemu-100", "win11", "lxc-101", "minecraft", "secret-box", "Desk plug", "ups1", "ups2"}
	// What a pending user and an anonymous client must not see.
	hiddenFromAll = []string{"cyrene", "laptop", "secret-box", "qemu-", "lxc-", "node-", "NSX-", "truenas", "ups1"}
)

func upsRow(name, status string) map[string]any {
	return ups.RowMap(devices.UPSFromVars(name, map[string]string{"ups.status": status, "battery.charge": "97"}, ""))
}

func setupSites(t *testing.T) *env {
	t.Helper()
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://cyrene")
	cfg.Proxmox[0].Site = "Neptune"
	cfg.Agents[0].Site = "Neptune"
	cfg.Agents[0].UPSPowers = map[string][]string{"ups1": {"laptop", "secret-box"}}
	cfg.Agents = append(cfg.Agents, config.Agent{ID: "secret-box", URL: "http://secret-box", TokenEnv: "TOK_LAPTOP"})
	b := testutil.NewBackend()
	b.Metrics["laptop"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["ups"] = []any{upsRow("ups1", "OL")}
		return m, nil
	}
	b.Metrics["secret-box"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["hostname"] = "secret-box"
		return m, nil
	}
	secrets := config.Secrets{SessionSecret: strings.Repeat("s", 48)}
	p, err := poller.New(cfg, secrets, b)
	if err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	// a UPS relayed by the hidden host goes on battery: its events are hidden from viewers
	p.UPS.UpdateFromAgent("secret-box", []map[string]any{upsRow("ups2", "OL")}, false, map[string][]string{"ups2": {"secret-box"}}, nil)
	p.UPS.UpdateFromAgent("secret-box", []map[string]any{upsRow("ups2", "OB")}, false, map[string][]string{"ups2": {"secret-box"}}, nil)
	p.Power.SampleHistory()
	p.Bump()
	s := New(cfg, secrets, p, nil, "test")
	st := s.Access
	for _, x := range []string{"Neptune", "Straw"} {
		if err := st.CreateSite(x); err != nil && !strings.Contains(err.Error(), "exists") {
			t.Fatal(err)
		}
	}
	mk := func(sub, role string, sites ...string) {
		if _, _, err := st.Seen(sub, sub+"@example.com", sub, access.RolePending, "test"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.SetRole(sub, role, sites, "test", true); err != nil {
			t.Fatal(err)
		}
	}
	mk(subAdmin, access.RoleAdmin)
	mk(subNeptune, access.RoleViewer, "Neptune")
	mk(subStraw, access.RoleViewer, "Straw")
	mk(subPending, access.RolePending)
	for target, site := range map[string]string{"cyrene": "Straw", "qemu-120": "Straw"} {
		if _, err := st.Assign(target, site); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, s: s, srv: srv, b: b}
}

func person(sub string) model.User {
	return model.User{Sub: sub, Name: model.P(sub), Email: model.P(sub + "@example.com"), Groups: []string{"viewers"}}
}

// streamBody reads the SSE stream for a moment and returns what was sent.
func (e *env) streamBody(path string, hdr ...string) (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body) // ends at the deadline
	return r.StatusCode, string(b)
}

type route struct{ method, path string }

// readRoutes is every GET route, with hidden and visible ids.
func readRoutes() []route {
	var out []route
	for _, p := range []string{"/api/me", "/api/systems", "/api/systems?kind=agent", "/api/summary", "/api/power",
		"/api/power/history", "/api/power/history?source=secret-box/desk", "/api/power/history?source=laptop/desk",
		"/api/ups", "/api/ups?history=false", "/api/ups/events", "/api/audit", "/api/settings", "/api/openapi.json",
		"/api/admin/users", "/api/admin/updates"} {
		out = append(out, route{"GET", p})
	}
	for _, id := range []string{"laptop", "cyrene", "secret-box", "node-nsx-kachina", "node-nsx-snezhnaya", "qemu-100",
		"lxc-101", "qemu-120"} {
		for _, sub := range []string{"", "/processes", "/fan", "/power", "/poll"} {
			out = append(out, route{"GET", "/api/systems/" + id + sub})
		}
	}
	return out
}

// writeRoutes is every write, each with a valid body.
func writeRoutes() []struct {
	route
	body any
} {
	return []struct {
		route
		body any
	}{
		{route{"PUT", "/api/systems/laptop/poll"}, map[string]any{"interval_s": 5}},
		{route{"PUT", "/api/systems/cyrene/poll"}, map[string]any{"interval_s": 5}},
		{route{"PUT", "/api/systems/secret-box/poll"}, map[string]any{"interval_s": 5}},
		{route{"POST", "/api/systems/qemu-120/power"}, map[string]any{"action": "reboot"}},
		{route{"POST", "/api/systems/cyrene/fan/mode"}, map[string]any{"mode": "full"}},
		{route{"POST", "/api/systems/cyrene/fan/duty"}, map[string]any{"zone": 0, "percent": 50}},
		{route{"PUT", "/api/admin/users/" + subStraw}, map[string]any{"role": "viewer", "sites": []string{"Neptune"}}},
		{route{"DELETE", "/api/admin/users/" + subStraw}, nil},
		{route{"POST", "/api/admin/sites"}, map[string]any{"name": "Elsewhere"}},
		{route{"PUT", "/api/admin/sites/Straw"}, map[string]any{"name": "Strawberry"}},
		{route{"DELETE", "/api/admin/sites/Straw"}, nil},
		{route{"PUT", "/api/admin/assign"}, map[string]any{"target": "secret-box", "site": "Straw"}},
		{route{"POST", "/api/admin/updates/laptop"}, map[string]any{}},
		{route{"POST", "/api/admin/updates"}, map[string]any{}},
	}
}

func assertHidden(t *testing.T, who, what, body string, hidden []string) {
	t.Helper()
	for _, h := range hidden {
		if strings.Contains(body, h) {
			t.Errorf("%s: %s leaks %q: %.300s", who, what, h, body)
		}
	}
}

func TestAuthorizationMatrix(t *testing.T) {
	e := setupSites(t)
	csrf := []string{"X-Requested-With", "fairy"}
	visible := map[string]map[string]bool{
		subNeptune: {"laptop": true, "node-nsx-kachina": true, "node-nsx-snezhnaya": true, "qemu-100": true, "lxc-101": true},
		subStraw:   {"cyrene": true, "qemu-120": true},
	}
	cases := []struct {
		who    string
		sub    string // "" = not signed in
		hidden []string
	}{
		{"anonymous", "", hiddenFromAll},
		{"pending", subPending, hiddenFromAll},
		{"viewer-Neptune", subNeptune, hiddenFromNeptune},
		{"viewer-Straw", subStraw, hiddenFromStraw},
		{"admin", subAdmin, nil},
	}
	for _, tc := range cases {
		var hdr []string
		if tc.sub != "" {
			hdr = []string{"Cookie", e.cookie(person(tc.sub), 0)}
		}
		for _, rt := range readRoutes() {
			r := e.do(rt.method, rt.path, nil, hdr...)
			what := rt.method + " " + rt.path
			assertHidden(t, tc.who, what, string(r.raw), tc.hidden)
			want := 200
			id, rest, isSys := strings.Cut(strings.TrimPrefix(rt.path, "/api/systems/"), "/")
			isSys = isSys || (strings.HasPrefix(rt.path, "/api/systems/") && !strings.Contains(rt.path, "?"))
			if !isSys {
				id = ""
			}
			switch {
			case tc.sub == "":
				want = 401
			case rt.path == "/api/me" || rt.path == "/api/settings":
				want = 200
			case tc.who == "pending":
				want = 403
			case rt.path == "/api/audit" || rt.path == "/api/admin/users" || rt.path == "/api/admin/updates":
				want = map[bool]int{true: 200, false: 403}[tc.who == "admin"]
			case id != "" && tc.who != "admin" && !visible[tc.sub][id]:
				want = 404
			}
			if id != "" && want == 200 { // per-system endpoints the fixture can't serve
				switch {
				case rest == "processes" && !strings.HasPrefix(id, "laptop") && id != "secret-box":
					want = r.code // 404 / 502 explained elsewhere; only the leak check matters here
				case rest == "fan" && id != "cyrene":
					want = 404
				case rest == "fan" && id == "cyrene":
					want = r.code
				}
			}
			if r.code != want {
				t.Errorf("%s %s: got %d, want %d: %.200s", tc.who, what, r.code, want, r.raw)
			}
			if want == 404 && id != "" && r.body["detail"] != "system not found" && rest == "" {
				t.Errorf("%s %s: a hidden host must look like an unknown one: %s", tc.who, what, r.raw)
			}
		}
		// live stream
		code, body := e.streamBody("/api/stream?watch=secret-box", hdr...)
		assertHidden(t, tc.who, "/api/stream", body, tc.hidden)
		wantStream := map[string]int{"anonymous": 401, "pending": 403}[tc.who]
		if wantStream == 0 {
			wantStream = 200
			if !strings.Contains(body, "event: systems") || !strings.Contains(body, "event: summary") {
				t.Errorf("%s: stream sent no systems / summary: %.200s", tc.who, body)
			}
		}
		if code != wantStream {
			t.Errorf("%s: stream %d, want %d", tc.who, code, wantStream)
		}
		if tc.who == "admin" {
			for _, s := range []string{"secret-box", "cyrene", "laptop", "qemu-120"} {
				if !strings.Contains(body, s) {
					t.Errorf("admin stream misses %s", s)
				}
			}
		}
		// every write is refused for everyone but the admin (whose writes are tested below)
		for _, w := range writeRoutes() {
			if tc.who == "admin" {
				break
			}
			r := e.do(w.method, w.path, w.body, append(hdr, csrf...)...)
			assertHidden(t, tc.who, w.method+" "+w.path, string(r.raw), tc.hidden)
			if r.code != 401 && r.code != 403 {
				t.Errorf("%s %s %s: %d %s", tc.who, w.method, w.path, r.code, r.raw)
			}
		}
	}
	// what the viewers see, positively
	n := e.cookie(person(subNeptune), 0)
	r := e.do("GET", "/api/systems", nil, "Cookie", n)
	var ids []string
	for _, x := range r.body["systems"].([]any) {
		ids = append(ids, x.(map[string]any)["id"].(string))
	}
	if strings.Join(ids, ",") != "node-nsx-kachina,node-nsx-snezhnaya,laptop,lxc-101,qemu-100" {
		t.Fatal("Neptune sees", ids)
	}
	if u := e.do("GET", "/api/ups", nil, "Cookie", n); !strings.Contains(string(u.raw), `"powers":["laptop"]`) {
		t.Fatal("UPS powering a hidden host must list only the visible one", string(u.raw))
	}
	sm := e.do("GET", "/api/summary", nil, "Cookie", n).body
	if sm["counts"].(map[string]any)["total"] != 5.0 || len(sm["clusters"].([]any)) != 1 {
		t.Fatal("Neptune summary", sm)
	}
	pw := e.do("GET", "/api/power", nil, "Cookie", n).body
	if pw["total_w"] != 80.0 || len(pw["sources"].([]any)) != 1 {
		t.Fatal("Neptune power: only the laptop plug counts", pw)
	}
	s := e.cookie(person(subStraw), 0)
	q := e.do("GET", "/api/systems/qemu-120", nil, "Cookie", s)
	if q.code != 200 || q.body["parent"] != nil {
		t.Fatal("a visible guest on a hidden node must not name the node", q.code, string(q.raw))
	}
	sm = e.do("GET", "/api/summary", nil, "Cookie", s).body
	cl := sm["clusters"].([]any)[0].(map[string]any)
	if sm["counts"].(map[string]any)["total"] != 2.0 || cl["nodes_total"] != 0.0 || cl["guests_total"] != 1.0 {
		t.Fatal("Straw summary: cluster totals only over what Straw sees", sm)
	}
	if pw := e.do("GET", "/api/power", nil, "Cookie", s).body; pw["total_w"] != nil {
		t.Fatal("Straw has no power source", pw)
	}
	// admin: everything, exactly as the poller has it
	a := e.cookie(person(subAdmin), 0)
	if sm := e.do("GET", "/api/summary", nil, "Cookie", a).body; sm["counts"].(map[string]any)["total"] != float64(len(e.s.Poller.Snapshot())) {
		t.Fatal("admin summary", sm)
	}
}

func TestTokensFollowSitesAndOwners(t *testing.T) {
	e := setupSites(t)
	auth := func(tok string) []string { return []string{"Authorization", "Bearer " + tok} }
	all, _, _ := e.s.Tokens.Create("all", "system", "read", "cli:test", 0)
	straw, _, err := e.s.Tokens.CreateForSites("straw", "system", "read", "cli:test", 0, []string{"Straw"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.Tokens.CreateForSites("bad", "system", "control", "cli:test", 0, []string{"Straw"}); err == nil {
		t.Fatal("a control token cannot be limited to sites")
	}
	owned, _, _ := e.s.Tokens.Create("owned", subStraw, "read", "cli:test", 0)
	ctlOwned, _, _ := e.s.Tokens.Create("ctl-owned", subStraw, "control", "cli:test", 0)
	pend, _, _ := e.s.Tokens.Create("pend", subPending, "read", "cli:test", 0)
	adminCtl, _, _ := e.s.Tokens.Create("admin-ctl", subAdmin, "control", "cli:test", 0)

	if r := e.do("GET", "/api/systems/secret-box", nil, auth(all)...); r.code != 200 {
		t.Fatal("a read token without sites sees every site (as before)", r.code)
	}
	for _, tok := range []string{straw, owned, ctlOwned} {
		r := e.do("GET", "/api/systems", nil, auth(tok)...)
		assertHidden(t, "straw token", "/api/systems", string(r.raw), hiddenFromStraw)
		if !strings.Contains(string(r.raw), "cyrene") {
			t.Fatal("straw token sees Straw", string(r.raw))
		}
		_, body := e.streamBody("/api/stream", auth(tok)...)
		assertHidden(t, "straw token", "/api/stream", body, hiddenFromStraw)
	}
	if r := e.do("PUT", "/api/systems/cyrene/poll", map[string]any{"interval_s": 5}, auth(ctlOwned)...); r.code != 403 {
		t.Fatal("a control token of a viewer cannot control", r.code)
	}
	if r := e.do("PUT", "/api/systems/cyrene/poll", map[string]any{"interval_s": 5}, auth(adminCtl)...); r.code != 200 {
		t.Fatal("a control token of an admin controls", r.code, string(r.raw))
	}
	if r := e.do("GET", "/api/summary", nil, auth(pend)...); r.code != 403 {
		t.Fatal("token of a user without access", r.code)
	}
	if r := e.do("GET", "/api/admin/users", nil, auth(adminCtl)...); r.code != 403 {
		t.Fatal("tokens never reach the Users page", r.code)
	}
	if r := e.do("GET", "/api/audit", nil, auth(adminCtl)...); r.code != 200 {
		t.Fatal("control tokens read the audit log as before", r.code)
	}
	// demoting the owner limits their tokens at once
	if _, _, err := e.s.Access.SetRole(subStraw, access.RolePending, nil, "test", false); err != nil {
		t.Fatal(err)
	}
	if r := e.do("GET", "/api/systems", nil, auth(owned)...); r.code != 403 {
		t.Fatal("token of a demoted owner", r.code)
	}
}

func TestUserAndSiteChangesNeedAdminCSRFStepUp(t *testing.T) {
	e := setupSites(t)
	csrf := []string{"X-Requested-With", "fairy"}
	fresh := e.cookie(person(subAdmin), 0)
	old := e.cookie(person(subAdmin), 10*time.Minute)
	body := map[string]any{"role": "viewer", "sites": []string{"Neptune", "Straw"}}
	path := "/api/admin/users/" + subStraw
	if r := e.do("PUT", path, body, "Cookie", fresh); r.code != 403 {
		t.Fatal("missing CSRF header", r.code)
	}
	if r := e.do("PUT", path, body, "Cookie", fresh, "X-Requested-With", "fairy", "Origin", "https://evil.example"); r.code != 403 {
		t.Fatal("cross-origin", r.code)
	}
	r := e.do("PUT", path, body, append([]string{"Cookie", old}, csrf...)...)
	if d, _ := r.body["detail"].(map[string]any); r.code != 401 || d["code"] != "step_up_required" {
		t.Fatal("step-up", r.code, string(r.raw))
	}
	if g := e.s.Access.Load().Users[subStraw].Grant(); g.String() != "viewer:Straw" {
		t.Fatal("refused change was applied", g)
	}
	if r := e.do("PUT", path, body, append([]string{"Cookie", fresh}, csrf...)...); r.code != 200 {
		t.Fatal(r.code, string(r.raw))
	}
	if g := e.s.Access.Load().Users[subStraw].Grant(); g.String() != "viewer:Neptune,Straw" {
		t.Fatal(g)
	}
	if r := e.do("PUT", path, map[string]any{"role": "viewer", "sites": []string{"Mars"}}, append([]string{"Cookie", fresh}, csrf...)...); r.code != 422 {
		t.Fatal("unknown site", r.code)
	}
	// last admin: cannot demote or remove themselves
	self := "/api/admin/users/" + subAdmin
	if r := e.do("PUT", self, map[string]any{"role": "viewer", "sites": []string{"Neptune"}}, append([]string{"Cookie", fresh}, csrf...)...); r.code != 409 {
		t.Fatal("last admin demoted", r.code, string(r.raw))
	}
	if r := e.do("DELETE", self, nil, append([]string{"Cookie", fresh}, csrf...)...); r.code != 409 {
		t.Fatal("last admin removed", r.code)
	}
	// sites: create, assign, rename, delete; config sites are changed in config.yaml
	for _, w := range []struct {
		method, path string
		body         any
		want         int
	}{
		{"POST", "/api/admin/sites", map[string]any{"name": "Lab"}, 201},
		{"POST", "/api/admin/sites", map[string]any{"name": "Lab"}, 409},
		{"PUT", "/api/admin/assign", map[string]any{"target": "secret-box", "site": "Lab"}, 200},
		{"PUT", "/api/admin/assign", map[string]any{"target": "power:secret-box/desk", "site": "Lab"}, 200},
		{"PUT", "/api/admin/assign", map[string]any{"target": "nope", "site": "Lab"}, 404},
		{"PUT", "/api/admin/sites/Lab", map[string]any{"name": "Lab 2"}, 200},
		{"PUT", "/api/admin/sites/Neptune", map[string]any{"name": "Nep"}, 409},
		{"DELETE", "/api/admin/sites/Neptune", nil, 409},
		{"PUT", "/api/admin/users/" + subNeptune, map[string]any{"role": "viewer", "sites": []string{"Lab 2"}}, 200},
	} {
		if r := e.do(w.method, w.path, w.body, append([]string{"Cookie", fresh}, csrf...)...); r.code != w.want {
			t.Fatalf("%s %s: %d %s", w.method, w.path, r.code, r.raw)
		}
	}
	nep := e.cookie(person(subNeptune), 0)
	r = e.do("GET", "/api/systems", nil, "Cookie", nep)
	if !strings.Contains(string(r.raw), "secret-box") || strings.Contains(string(r.raw), `"laptop"`) {
		t.Fatal("Neptune now sees only the Lab 2 site", string(r.raw))
	}
	if p := e.do("GET", "/api/power", nil, "Cookie", nep); !strings.Contains(string(p.raw), "secret-box/desk") {
		t.Fatal("assigned power source", string(p.raw))
	}
	if r := e.do("DELETE", "/api/admin/sites/Lab%202", nil, append([]string{"Cookie", fresh}, csrf...)...); r.code != 200 {
		t.Fatal(r.code, string(r.raw))
	}
	if r := e.do("GET", "/api/systems", nil, "Cookie", nep); strings.Contains(string(r.raw), "secret-box") {
		t.Fatal("deleted site still visible")
	}
	page := e.do("GET", "/api/admin/users", nil, "Cookie", fresh)
	if page.code != 200 || len(page.body["users"].([]any)) != 4 || !strings.Contains(string(page.raw), `"from":"node"`) {
		t.Fatal("users page", page.code, string(page.raw))
	}
	// every change is in the audit log
	audit := e.do("GET", "/api/audit?action=site&limit=200", nil, "Cookie", fresh)
	if n := len(audit.body["entries"].([]any)); n < 8 {
		t.Fatal("site changes audited", n)
	}
	if a := e.do("GET", "/api/audit?action=user.set_role", nil, "Cookie", fresh); len(a.body["entries"].([]any)) < 3 {
		t.Fatal("role changes audited", string(a.raw))
	}
}

// Upgrading from a version without roles: while users.json has no admin, the first real sign-in of someone in
// control_groups (or with a verified email in bootstrap_admins) makes them admin; everyone else waits for
// access; the panel's role wins afterwards. Once there is an admin, nobody is bootstrapped any more.
func TestMigrationViaControlGroupsAndBootstrapAdmins(t *testing.T) {
	e := setup(t)
	e.s.Cfg.OIDC.BootstrapAdmins = []string{"Boss@Example.com"}
	// a session from before the upgrade (no users.json at all) must sign in again: roles are only bootstrapped
	// at a fresh sign-in, never from the groups cached in a cookie
	if r := e.do("GET", "/api/me", nil, "Cookie", e.staleCookie(controller(), 0)); r.code != 401 {
		t.Fatal("pre-upgrade cookie", r.code, string(r.raw))
	}
	// bootstrap_admins needs a verified email
	unverified := model.User{Sub: "boss-unverified", Email: model.P("boss@example.com"), Groups: []string{"viewers"}}
	if r := e.do("GET", "/api/me", nil, "Cookie", e.cookie(unverified, 0)); r.body["role"] != "pending" {
		t.Fatal("bootstrap_admins with an unverified email", string(r.raw))
	}
	boss := model.User{Sub: "boss", Email: model.P("boss@example.com"), EmailVerified: true, Groups: []string{"viewers"}}
	if r := e.do("GET", "/api/me", nil, "Cookie", e.cookie(boss, 0)); r.body["role"] != "admin" {
		t.Fatal("bootstrap_admins", string(r.raw))
	}
	// there is an admin now: a control-group member signing in for the first time waits like anyone else
	c := e.cookie(controller(), 0)
	if r := e.do("GET", "/api/me", nil, "Cookie", c); r.body["role"] != "pending" {
		t.Fatal("control_groups bootstrapped although an admin exists", string(r.raw))
	}
	if u := e.s.Access.Load().Users["u2"]; u.Role != access.RolePending {
		t.Fatal(u)
	}

	// a fresh install whose first sign-in is a control-group member
	e = setup(t)
	c = e.cookie(controller(), 0)
	if r := e.do("GET", "/api/me", nil, "Cookie", c); r.body["role"] != "admin" || r.body["can_control"] != true || r.body["can_manage_users"] != true {
		t.Fatal(string(r.raw))
	}
	if u := e.s.Access.Load().Users["u2"]; u.Role != access.RoleAdmin || u.Via != "control_groups" {
		t.Fatal(u)
	}
	v := e.cookie(viewer, 0)
	r := e.do("GET", "/api/me", nil, "Cookie", v)
	if r.code != 200 || r.body["role"] != "pending" || r.body["can_control"] != false {
		t.Fatal("pending user", string(r.raw))
	}
	if r := e.do("GET", "/api/systems", nil, "Cookie", v); r.code != 403 || !strings.Contains(string(r.raw), "pending_access") {
		t.Fatal("pending user reads data", r.code)
	}
	// the panel's role wins over control_groups from now on
	if _, _, err := e.s.Access.SetRole("u2", access.RoleViewer, []string{}, "test", true); err != nil {
		t.Fatal(err)
	}
	if r := e.do("GET", "/api/me", nil, "Cookie", c); r.body["role"] != "viewer" {
		t.Fatal("demoted control-group member", string(r.raw))
	}
	if _, _, err := e.s.Access.SetRole("u1", access.RoleAdmin, nil, "test", true); err != nil {
		t.Fatal(err)
	}
	audit := e.do("GET", "/api/audit?action=user.first_sign_in", nil, "Cookie", e.cookie(viewer, 0))
	if n := len(audit.body["entries"].([]any)); n != 2 {
		t.Fatal("first sign-ins audited", n, string(audit.raw))
	}
}

func TestDemoPreviewAsViewer(t *testing.T) {
	e := setupSites(t)
	u := person("demo-neptune")
	u.Groups = []string{"controllers"}
	u.Auth = "demo"
	e.s.DemoUser = &u
	e.s.demoRoutes()
	if _, _, err := e.s.Access.Seen(u.Sub, "", "", access.RolePending, "demo"); err != nil { // demo.SeedUsers
		t.Fatal(err)
	}
	if _, _, err := e.s.Access.SetRole(u.Sub, access.RoleAdmin, nil, "demo", true); err != nil {
		t.Fatal(err)
	}
	if r := e.do("GET", "/api/me", nil); r.body["role"] != "admin" || r.body["demo_view"] != "admin" {
		t.Fatal(string(r.raw))
	}
	if r := e.do("POST", "/api/demo/view", map[string]any{"view": "viewer:Straw"}); r.code != 403 {
		t.Fatal("demo switch needs the CSRF header", r.code)
	}
	if r := e.do("POST", "/api/demo/view", map[string]any{"view": "viewer:Straw"}, "X-Requested-With", "fairy"); r.code != 200 {
		t.Fatal(r.code, string(r.raw))
	}
	r := e.do("GET", "/api/systems", nil)
	assertHidden(t, "demo as Straw", "/api/systems", string(r.raw), hiddenFromStraw)
	if me := e.do("GET", "/api/me", nil); me.body["role"] != "viewer" || me.body["can_control"] != false {
		t.Fatal(string(me.raw))
	}
	if err := e.s.SetDemoView("pending"); err != nil {
		t.Fatal(err)
	}
	if r := e.do("GET", "/api/summary", nil); r.code != 403 {
		t.Fatal("demo as pending", r.code)
	}
	if err := e.s.SetDemoView("bogus"); err == nil {
		t.Fatal("bad view accepted")
	}
}

// Removing a user is final: their sessions and API tokens stop working at once, an old cookie (whose groups
// may still say control_groups) never makes them admin again, and neither does signing in again.
func TestRemovedUserRegainsNothing(t *testing.T) {
	e := setup(t)
	csrf := []string{"X-Requested-With", "fairy"}
	auth := func(tok string) []string { return []string{"Authorization", "Bearer " + tok} }
	ctl := controller()
	c := e.cookie(ctl, 0) // first sign-in of a control-group member: admin
	if u := e.s.Access.Load().Users[ctl.Sub]; u.Role != access.RoleAdmin {
		t.Fatal("bootstrap", u)
	}
	boss := person("boss")
	e.signIn(boss)
	if _, _, err := e.s.Access.SetRole("boss", access.RoleAdmin, nil, "test", false); err != nil {
		t.Fatal(err)
	}
	ctlTok, _, _ := e.s.Tokens.Create("ctl", ctl.Sub, "control", "cli:test", 0)
	ghost, _, _ := e.s.Tokens.Create("ghost", "never-signed-in", "read", "cli:test", 0)
	if r := e.do("GET", "/api/systems", nil, auth(ghost)...); r.code != 403 {
		t.Fatal("a token whose owner is not a panel user has no access", r.code)
	}
	if r := e.do("GET", "/api/me", nil, auth(ctlTok)...); r.code != 200 || r.body["role"] != "admin" {
		t.Fatal("setup token", string(r.raw))
	}

	r := e.do("DELETE", "/api/admin/users/"+ctl.Sub, nil, append([]string{"Cookie", e.cookie(boss, 0)}, csrf...)...)
	if r.code != 200 {
		t.Fatal("remove", r.code, string(r.raw))
	}
	if r := e.do("GET", "/api/me", nil, "Cookie", c); r.code != 401 {
		t.Fatal("the removed user's session still works", r.code, string(r.raw))
	}
	if r := e.do("GET", "/api/me", nil, auth(ctlTok)...); r.code != 401 {
		t.Fatal("the removed user's control token still works", r.code, string(r.raw))
	}
	// a cookie that was not revoked (minted before users.json knew them) does not bootstrap from its groups
	old := e.staleCookie(ctl, 0)
	if r := e.do("GET", "/api/systems", nil, "Cookie", old); r.code != 401 {
		t.Fatal("an old cookie of a removed control-group member", r.code, string(r.raw))
	}
	if _, ok := e.s.Access.Load().Users[ctl.Sub]; ok {
		t.Fatal("an old cookie re-created the user")
	}
	// signing in again: waits for access, never admin via control_groups
	c = e.cookie(ctl, 0)
	if r := e.do("GET", "/api/me", nil, "Cookie", c); r.body["role"] != "pending" {
		t.Fatal("removed user bootstrapped again", string(r.raw))
	}
	if d := e.s.Access.Load(); d.Removed[ctl.Sub].By == "" || d.Users[ctl.Sub].Role != access.RolePending {
		t.Fatal("tombstone", d.Removed, d.Users[ctl.Sub])
	}
}

// A laptop's battery reaches the API as part of its system, so it follows the host's site: Neptune's viewer and
// the admin see it, Straw's viewer sees neither the host nor the battery; hosts without one have battery null.
func TestBatteryFollowsTheHost(t *testing.T) {
	e := setupSites(t)
	e.b.Mu.Lock()
	e.b.Metrics["laptop"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["battery"] = jsonx.M{"percent": 64.0, "state": "discharging", "ac_online": false, "power_w": 11.4,
			"time_remaining_s": 12600.0, "health_pct": 88.5}
		return m, nil
	}
	e.b.Mu.Unlock()
	e.s.Poller.PollOnce(context.Background())
	e.s.Poller.Bump()
	battery := func(sub, id string) (map[string]any, bool) {
		r := e.do("GET", "/api/systems", nil, "Cookie", e.cookie(person(sub), 0))
		if r.code != 200 {
			t.Fatalf("%s: %d", sub, r.code)
		}
		for _, x := range r.body["systems"].([]any) {
			if s := x.(map[string]any); s["id"] == id {
				b, _ := s["battery"].(map[string]any)
				return b, true
			}
		}
		return nil, false
	}
	for _, sub := range []string{subAdmin, subNeptune} {
		b, found := battery(sub, "laptop")
		if !found || b == nil || b["percent"] != 64.0 || b["state"] != "discharging" || b["power_w"] != 11.4 ||
			b["ac_online"] != false || b["time_remaining_s"] != 12600.0 || b["health_pct"] != 88.5 {
			t.Fatalf("%s: %v", sub, b)
		}
	}
	if _, found := battery(subStraw, "laptop"); found {
		t.Fatal("Straw's viewer sees Neptune's laptop")
	}
	if b, found := battery(subStraw, "cyrene"); !found || b != nil {
		t.Fatalf("a host without a battery has battery null: %v %v", found, b)
	}
	r := e.do("GET", "/api/systems/laptop", nil, "Cookie", e.cookie(person(subStraw), 0))
	if r.code == 200 || strings.Contains(string(r.raw), "12600") {
		t.Fatalf("hidden host's details: %d %s", r.code, r.raw)
	}
}
