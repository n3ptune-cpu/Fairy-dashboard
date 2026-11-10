package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/power"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/ups"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Upgrade in place. These tests start the panel from the "v-before" fixture: config.yaml and the data directory
// exactly as the release before the security fixes wrote them (internal/testutil/testdata/v-before). The owner
// upgrades a running panel and is its only admin, so the one thing that must never happen is that the files of
// the old release keep them out.

// bootVBefore starts a panel on a private copy of the fixture, the way `fairy serve` does (cmd/fairy/serve.go,
// setupPanel), with a fake identity provider behind it.
func bootVBefore(t *testing.T) (*env, *testutil.VBefore, *fakeIDP) {
	t.Helper()
	v := testutil.NewVBefore(t)
	cfg := v.Load(t)
	if err := power.ValidateConfigs(cfg.PowerSources); err != nil {
		t.Fatal(err)
	}
	if err := ups.ValidateConfigs(cfg.UPS); err != nil {
		t.Fatal(err)
	}
	if _, skipped, err := managed.NewStore(cfg.DataDir).Merge(cfg); err != nil || len(skipped) != 0 {
		t.Fatal(skipped, err)
	}
	b := testutil.NewBackend()
	p, err := poller.New(cfg, v.Secrets, b)
	if err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	s := New(cfg, v.Secrets, p, nil, "test")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	e := &env{t: t, s: s, srv: srv, b: b}
	idp := newFakeIDP(t)
	s.Cfg.OIDC.Issuer = idp.srv.URL
	return e, v, idp
}

// signInAs runs the whole sign-in round trip with these ID-token claims and returns the callback's answer.
// A claim set to nil is left out of the token.
func (f *fakeIDP) signInAs(e *env, claims map[string]any) resp {
	e.t.Helper()
	f.mu.Lock()
	f.claims = map[string]any{"groups": []string{"homelab"}, "auth_time": time.Now().Unix()}
	for k, v := range claims {
		f.claims[k] = v
	}
	f.mu.Unlock()
	q, rt := f.login(e, "/auth/login", "")
	return e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt)
}

func usersFile(t *testing.T, dataDir string) map[string]map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dataDir, "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Users map[string]map[string]any `json:"users"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f.Users
}

// The old users.json has no email_verified field. The existing admin signs in again after the upgrade (the
// session cookie has a new name, so everyone does) and must be admin again, whatever the identity provider
// says about the e-mail address: the account is found by the OIDC subject, never by the address. Pocket ID
// v2.19.2 sends email_verified with the user's own flag whenever it sends an address, and that flag is false
// unless someone set it, so "false" is the case to expect on a real install.
func TestUpgradeExistingAdminStaysAdmin(t *testing.T) {
	for _, tc := range []struct {
		name     string
		claims   map[string]any
		verified any    // email_verified in users.json afterwards
		email    string // the address in users.json afterwards
	}{
		{"email_verified true", map[string]any{"email": "owner@example.com", "email_verified": true}, true, "owner@example.com"},
		{"email_verified false", map[string]any{"email": "owner@example.com", "email_verified": false}, false, "owner@example.com"},
		{"email_verified absent", map[string]any{"email": "owner@example.com"}, false, "owner@example.com"},
		{"email_verified as text", map[string]any{"email": "owner@example.com", "email_verified": "true"}, true, "owner@example.com"},
		{"no e-mail claim at all", map[string]any{"email": nil}, false, "owner@example.com"},
		{"another address now, not verified", map[string]any{"email": "new@example.com", "email_verified": false}, false, "new@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, v, idp := bootVBefore(t)
			before := usersFile(t, v.DataDir)
			if _, has := before[v.OwnerSub]["email_verified"]; has || before[v.OwnerSub]["role"] != "admin" {
				t.Fatal("the fixture must be an old file with the owner as admin", before[v.OwnerSub])
			}
			// before anyone signs in: the file loads, and the roles are what they were
			d := e.s.Access.Load()
			if err := e.s.Access.Err(); err != nil || d.Admins() != 1 || d.Users[v.OwnerSub].Role != access.RoleAdmin ||
				d.Users[v.ViewerSub].Role != access.RoleViewer || strings.Join(d.Users[v.ViewerSub].Sites, ",") != "Straw" {
				t.Fatal(err, d)
			}

			claims := map[string]any{"sub": v.OwnerSub, "name": "Owner"}
			for k, val := range tc.claims {
				claims[k] = val
			}
			r := idp.signInAs(e, claims)
			if r.code != 303 {
				t.Fatalf("the owner cannot sign in: %d %s", r.code, r.raw)
			}
			ck := sessionCookie(e, r.hdr)
			me := e.do("GET", "/api/me", nil, "Cookie", ck)
			if me.code != 200 || me.body["role"] != "admin" || me.body["can_control"] != true || me.body["can_manage_users"] != true ||
				me.body["all_sites"] != true {
				t.Fatalf("the owner is not admin after the upgrade: %d %s", me.code, me.raw)
			}
			// the admin pages and an admin change work (the sign-in is fresh, so no second confirmation)
			wr := []string{"Cookie", ck, "X-Requested-With", "fairy"}
			if r := e.do("GET", "/api/admin/users", nil, "Cookie", ck); r.code != 200 || len(r.body["users"].([]any)) != 3 {
				t.Fatalf("Users page: %d %s", r.code, r.raw)
			}
			if r := e.do("GET", "/api/audit", nil, "Cookie", ck); r.code != 200 {
				t.Fatalf("Audit page: %d %s", r.code, r.raw)
			}
			if r := e.do("PUT", "/api/admin/users/"+v.ViewerSub, map[string]any{"role": "viewer", "sites": []string{"Straw", "Neptune"}}, wr...); r.code != 200 {
				t.Fatalf("admin change: %d %s", r.code, r.raw)
			}
			if r := e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 7}, wr...); r.code != 200 {
				t.Fatalf("control action: %d %s", r.code, r.raw)
			}

			after := usersFile(t, v.DataDir)
			own := after[v.OwnerSub]
			if own["role"] != "admin" || own["via"] != "bootstrap_admins" || own["created_at"] != before[v.OwnerSub]["created_at"] ||
				own["email"] != tc.email || own["email_verified"] != tc.verified {
				t.Fatalf("owner in users.json: %v", own)
			}
			if len(after) != 3 || after["5d2e9f10-kaveh-0003"]["role"] != "pending" || after[v.ViewerSub]["role"] != "viewer" {
				t.Fatalf("the other users changed: %v", after)
			}
			if d := e.s.Access.Load(); d.Admins() != 1 || d.Assign["cyrene"] != "Straw" || d.Assign["power:desk-plug"] != "Neptune" ||
				len(d.Removed) != 1 || !d.HasSite("Straw") || !d.HasSite("Neptune") {
				t.Fatalf("users.json lost something: %+v", d)
			}
		})
	}
}

// oidc.bootstrap_admins after the upgrade: an address only makes someone admin when the identity provider
// verified it, and only while there is no admin at all. The fixture's config.yaml lists owner@example.com.
func TestUpgradeBootstrapAdminsNeedsAVerifiedAddress(t *testing.T) {
	role := func(e *env, r resp) string {
		t.Helper()
		if r.code != 303 {
			t.Fatalf("sign-in: %d %s", r.code, r.raw)
		}
		return e.do("GET", "/api/me", nil, "Cookie", sessionCookie(e, r.hdr)).body["role"].(string)
	}
	impostor := func(extra map[string]any) map[string]any {
		c := map[string]any{"sub": "impostor-0666", "name": "Owner", "email": "owner@example.com"}
		for k, v := range extra {
			c[k] = v
		}
		return c
	}

	// 1. The old file as it is (the owner is admin): somebody else with the owner's address gets nothing,
	// verified or not, and the role API does not find an account by an address nobody has verified.
	e, v, idp := bootVBefore(t)
	for _, extra := range []map[string]any{{"email_verified": false}, {}, {"email_verified": true}} {
		if got := role(e, idp.signInAs(e, impostor(extra))); got != access.RolePending {
			t.Fatalf("impostor %v: role %q", extra, got)
		}
	}
	if d := e.s.Access.Load(); d.Admins() != 1 || d.Users[v.OwnerSub].Role != access.RoleAdmin {
		t.Fatal(d.Users)
	}

	// 2. No admin at all (users.json lost): the unverified address still gets nothing; the verified one is the
	// owner's way back in, as before the upgrade.
	for _, extra := range []map[string]any{{"email_verified": false}, {}, {"email_verified": "false"}, {"email_verified": 1}} {
		e, v, idp = bootVBefore(t)
		if err := os.Remove(filepath.Join(v.DataDir, "users.json")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond) // the store re-reads the file at most once a second
		if d := e.s.Access.Load(); len(d.Users) != 0 {
			t.Fatal("users.json was not re-read", d.Users)
		}
		if got := role(e, idp.signInAs(e, impostor(extra))); got != access.RolePending {
			t.Fatalf("no admin, impostor with %v: role %q (bootstrap_admins must need a verified address)", extra, got)
		}
		if e.s.Access.Load().Admins() != 0 {
			t.Fatal("an admin was made")
		}
		// the real owner, with the address verified: admin through bootstrap_admins
		got := role(e, idp.signInAs(e, map[string]any{"sub": v.OwnerSub, "name": "Owner", "email": "owner@example.com", "email_verified": true}))
		if u := e.s.Access.Load().Users[v.OwnerSub]; got != access.RoleAdmin || u.Via != "bootstrap_admins" || !u.Verified() {
			t.Fatalf("the owner's way back: role %q, %+v", got, u)
		}
	}
}

// Every state file of the old release still loads, and what was in it is still there: users and roles, sites and
// assignments, API tokens, the polling override, UI settings, notifications with their read marks, UPS events
// and the audit log. config.yaml loads with its integer session values and without the new keys.
func TestUpgradeOldFilesLoad(t *testing.T) {
	e, v, idp := bootVBefore(t)
	cfg := e.s.Cfg
	if cfg.SessionMaxAge != 604800 || cfg.SessionIdleTimeout != 43200 || cfg.StepUpWindowS != 300 || !cfg.StepUpPromptLogin ||
		cfg.StepUpPerAction || cfg.EnableDocs || cfg.RequestTimeout != 4 || cfg.OfflineAfter != 60 {
		t.Fatalf("config: max age %v, idle %v, window %v, prompt %v, per action %v", cfg.SessionMaxAge, cfg.SessionIdleTimeout,
			cfg.StepUpWindowS, cfg.StepUpPromptLogin, cfg.StepUpPerAction)
	}

	// the example config the old release shipped loads too, with the values it had (an explicit 300 s window is kept)
	if ex, err := config.Parse(testutil.OldExampleConfig(t)); err != nil || ex.SessionMaxAge != 604800 || ex.SessionIdleTimeout != 43200 ||
		ex.StepUpWindowS != 300 || ex.StepUpPerAction || len(ex.Proxmox) != 1 || len(ex.Agents) == 0 {
		t.Fatalf("the old release's config.example.yaml: %v", err)
	}

	// sessions.json: the three sessions of the old release are read (the CLI can list and revoke them)
	if list, err := e.s.Poller.Admin.ListSessions(); err != nil || len(list) != 3 {
		t.Fatal("sessions.json", len(list), err)
	}
	// The old cookie name is never read: a browser that still has the old cookie is simply not signed in.
	if !strings.HasPrefix(v.OwnerCookie, "fairy_session=") || e.s.codec.name() != "__Host-fairy_session" {
		t.Fatal(v.CookieName, e.s.codec.name())
	}
	if r := e.do("GET", "/api/me", nil, "Cookie", v.OwnerCookie); r.code != 401 {
		t.Fatalf("old cookie under its old name: %d %s", r.code, r.raw)
	}
	// Its value under the new name (nobody's browser does this): a genuine session of the old release at most.
	// It is never an error, and it never counts as a fresh sign-in: a control action asks to confirm.
	renamed := "__Host-" + v.OwnerCookie
	if r := e.do("GET", "/api/me", nil, "Cookie", renamed); r.code != 200 && r.code != 401 {
		t.Fatalf("old cookie value: %d %s", r.code, r.raw)
	} else if r.code == 200 {
		w := e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 9}, "Cookie", renamed, "X-Requested-With", "fairy")
		if d, _ := w.body["detail"].(map[string]any); w.code != 401 || d["code"] != "step_up_required" {
			t.Fatalf("an old session must confirm before a control action: %d %s", w.code, w.raw)
		}
	}

	// API tokens of the old release work as before: the system token reads everything, the viewer's is capped
	if r := e.do("GET", "/api/systems", nil, "Authorization", "Bearer "+v.TokenSystem); r.code != 200 || len(r.body["systems"].([]any)) < 3 {
		t.Fatalf("system token: %d %s", r.code, r.raw)
	}
	r := e.do("GET", "/api/systems", nil, "Authorization", "Bearer "+v.TokenViewer)
	if r.code != 200 || !strings.Contains(string(r.raw), `"id":"cyrene"`) || strings.Contains(string(r.raw), `"id":"laptop"`) {
		t.Fatalf("viewer token (site Straw: cyrene only): %d %s", r.code, r.raw)
	}

	// the owner signs in (Pocket ID's default: address not verified) and looks at what the old release left
	sr := idp.signInAs(e, map[string]any{"sub": v.OwnerSub, "name": "Owner", "email": "owner@example.com", "email_verified": false})
	if sr.code != 303 {
		t.Fatal(sr.code, string(sr.raw))
	}
	ck := sessionCookie(e, sr.hdr)
	expired := false // the sign-in also removes the old cookie from the browser
	for _, c := range sr.hdr.Values("Set-Cookie") {
		expired = expired || (strings.HasPrefix(c, "fairy_session=;") && strings.Contains(c, "Max-Age=0"))
	}
	if !expired {
		t.Errorf("the old cookie is not expired at sign-in: %v", sr.hdr.Values("Set-Cookie"))
	}
	get := func(path string) resp {
		t.Helper()
		r := e.do("GET", path, nil, "Cookie", ck)
		if r.code != 200 {
			t.Fatalf("%s: %d %s", path, r.code, r.raw)
		}
		return r
	}
	if r := get("/api/systems/laptop/poll"); !strings.Contains(string(r.raw), `5`) || r.body["mode"] == "default" {
		t.Fatalf("state.json: the polling override is gone: %s", r.raw)
	}
	if r := get("/api/settings"); r.body["theme"] != "black" || r.body["dim"] != 60.0 {
		t.Fatalf("settings.json: %s", r.raw)
	}
	notes := get("/api/notifications")
	events, _ := notes.body["events"].([]any)
	if len(events) != 4 {
		t.Fatalf("notifications.json: %d events: %s", len(events), notes.raw)
	}
	if c := get("/api/notifications/count"); c.body["unread"] != 1.0 || c.body["active_alerts"] != 1.0 {
		t.Fatalf("notifications.json: read marks or alarms lost: %s", c.raw)
	}
	// the new release adds to the old list (and merges repeats, L7) without losing it
	for range 3 {
		e.s.Notes.Add(notifications.Event{Kind: "update_failed", Type: "update", Severity: "warning", SystemID: "cyrene", Host: "cyrene", Text: "cyrene: update failed"})
	}
	if err := e.s.Notes.Flush(); err != nil {
		t.Fatal(err)
	}
	if raw := v.ReadData(t, "notifications.json"); !strings.Contains(raw, "cyrene is offline") || !strings.Contains(raw, `"count":3`) {
		t.Fatalf("notifications.json after a write: %s", raw)
	}
	// ups-events.jsonl is a log the panel only appends to (the page's list starts empty after every restart, as
	// before): a new event goes after the old lines
	row := func(status string) map[string]any {
		return ups.RowMap(devices.UPSFromVars("ups1", map[string]string{"ups.status": status, "battery.charge": "97"}, ""))
	}
	for _, st := range []string{"OL", "OB"} {
		e.s.Poller.UPS.UpdateFromAgent("laptop", []map[string]any{row(st)}, false, map[string][]string{"ups1": {"laptop"}}, nil)
	}
	var upsEvents []map[string]any
	if r := get("/api/ups/events"); json.Unmarshal(r.raw, &upsEvents) != nil || len(upsEvents) != 1 || upsEvents[0]["kind"] != "on_battery" {
		t.Fatalf("UPS events: %s", r.raw)
	}
	if lines := strings.Split(strings.TrimSpace(v.ReadData(t, "ups-events.jsonl")), "\n"); len(lines) != 3 ||
		!strings.Contains(lines[1], "power_restored") || !strings.Contains(lines[2], "on_battery") {
		t.Fatalf("ups-events.jsonl: %q", lines)
	}
	audit := get("/api/audit?limit=200")
	entries, _ := audit.body["entries"].([]any)
	seen := map[string]bool{}
	for _, x := range entries {
		seen[x.(map[string]any)["action"].(string)] = true
	}
	for _, want := range []string{"user.first_sign_in", "user.set_role", "user.remove", "site.assign", "token.create", "control.disable", "auth.denied", "auth.login"} {
		if !seen[want] {
			t.Errorf("audit.log: no %q entry of the old release in the Audit page", want)
		}
	}
	users := get("/api/admin/users")
	byRole := map[string]any{}
	for _, x := range users.body["users"].([]any) {
		u := x.(map[string]any)
		byRole[u["role"].(string)] = u["email_verified"]
	}
	// the owner signed in just now (false, from the claim); the others have not: null, "not known yet"
	if len(byRole) != 3 || byRole["admin"] != false || byRole["viewer"] != nil || byRole["pending"] != nil {
		t.Fatalf("Users page: %v", byRole)
	}
	// someone the owner removed before the upgrade is still never made admin by bootstrap
	gone := idp.signInAs(e, map[string]any{"sub": "c0ffee00-gone-0004", "name": "Gone", "email": "owner@example.com", "email_verified": true})
	if g := e.do("GET", "/api/me", nil, "Cookie", sessionCookie(e, gone.hdr)); g.body["role"] != "pending" {
		t.Fatalf("removed user: %s", g.raw)
	}
}
