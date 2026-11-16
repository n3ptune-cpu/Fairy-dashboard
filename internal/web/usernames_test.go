package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/audit"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/enrollment"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// The panel names a person by the user name of their account. These tests are about every place where it says
// who did something, and about what leaves the panel: an answer to a browser or a token, a request to an agent,
// a message to a notification service. The accounts are made up; nobody has these addresses.
//
//	account   user name    address (verified)           name            role
//	sub-n     Juniper      owner@example.com (yes)      Real Name       admin
//	sub-s     PineTop    friend@example.com (no)      Other Person    admin (the second one)
//	sub-v     viewer-all   wide@example.com (yes)       Wide Viewer     viewer of every site (Alice, Bob)
//	sub-l     viewer-one   narrow@example.com (yes)     Narrow Viewer   viewer of the site Bob (the host tower)
//	sub-o     (none)       old@example.com (yes)        Old Timer       admin, has no user name at Pocket ID
//
// and the four accounts of the sites rig (sub-admin, sub-alice, sub-bob, sub-pending: each "<sub>@example.com",
// no user name: accounts that have not signed in since the panel keeps user names).

type namesAccount struct {
	sub, username, email string
	verified             bool
	name, role           string
	sites                []string
}

var namesAccounts = []namesAccount{
	{"sub-n", "Juniper", "owner@example.com", true, "Real Name", access.RoleAdmin, nil},
	{"sub-s", "PineTop", "friend@example.com", false, "Other Person", access.RoleAdmin, nil},
	{"sub-v", "viewer-all", "wide@example.com", true, "Wide Viewer", access.RoleViewer, []string{"Alice", "Bob"}},
	{"sub-l", "viewer-one", "narrow@example.com", true, "Narrow Viewer", access.RoleViewer, []string{"Bob"}},
	{"sub-o", "", "old@example.com", true, "Old Timer", access.RoleAdmin, nil},
}

// namesPrivate is what no answer may hold but the Users page's and the audit log's (and a person's own address
// and name in their own /api/me): every address and every name. (The names of the sites rig's accounts are
// their subjects, which is no name of a person.)
var namesPrivate = []string{"owner@example.com", "friend@example.com", "wide@example.com", "narrow@example.com", "old@example.com",
	"Real Name", "Other Person", "Wide Viewer", "Narrow Viewer", "Old Timer",
	"sub-admin@example.com", "sub-alice@example.com", "sub-bob@example.com", "sub-pending@example.com",
	// and parts of them: an answer that cut one, or wrote it in other letters
	"@example.com", "owner@", "friend@", "al Name", "d Timer"}

func (a namesAccount) user(withUsername bool) model.User {
	u := model.User{Sub: a.sub, Name: model.P(a.name), Email: model.P(a.email), EmailVerified: a.verified, Groups: []string{"viewers"}}
	if withUsername && a.username != "" {
		u.PreferredUsername = model.P(a.username)
	}
	return u
}

type namesRig struct {
	*env
	ck     map[string]string // sub -> cookie
	enroll string            // the id of an Add host that waits for its machine
}

func (r *namesRig) hdr(sub string) []string {
	return []string{"Cookie", r.ck[sub], "X-Requested-With", "fairy", "Content-Type", "application/json"}
}

// newNamesRig is the sites rig as a panel of build 16 left it, and then updated.
//
// Before the update: the five accounts are in users.json without a user name (the file had no such field), and
// everything that names a person was stored with the address, as that version did: a host added with Add host,
// a name changed in Edit host, a run timing, a run of "Update unpowered hosts" that was cancelled, notifications,
// a countdown an agent still shows, sessions, and audit entries. The changes are made by the real requests where
// a request can make them: an account without a user name is recorded exactly as build 16 recorded it.
//
// After the update (signIn true): four of the five sign in again and have their user names from then on.
func newNamesRig(t *testing.T, signIn bool) *namesRig {
	t.Helper()
	e := setupSites(t)
	r := &namesRig{env: e, ck: map[string]string{}}
	for _, a := range namesAccounts {
		if _, _, err := e.s.Access.SeenIdentity(access.Identity{Sub: a.sub, Email: a.email, EmailVerified: a.verified, Name: a.name}, access.RolePending, "first sign-in"); err != nil {
			t.Fatal(err)
		}
		via := "panel by owner@example.com"
		if a.sub == "sub-n" {
			via = "bootstrap_admins"
		}
		if _, _, err := e.s.Access.SetRole(a.sub, a.role, a.sites, via, true); err != nil {
			t.Fatal(err)
		}
		r.ck[a.sub] = e.staleCookie(a.user(false), 0) // a session of then: its cookie has no user name either
	}
	// a session with what a sign-in records about the account
	name, mail := "Other Person", "friend@example.com"
	if err := e.s.Poller.Admin.AddSession(randToken(18), adminstate.NewSession{Sub: "sub-s", Name: &name, Email: &mail, IP: "192.0.2.9", UA: uaFirefoxWin}); err != nil {
		t.Fatal(err)
	}
	// Add host, by the admin and by the second admin (the hosts file keeps "added_by")
	for id, by := range map[string]string{"box-one": "owner@example.com", "box-two": "friend@example.com (unverified)", "box-three": "gone@example.com"} {
		if err := e.s.Managed.Put(managed.Host{ID: id, Name: strings.ToUpper(id), URL: "http://100.64.0.9:8765", OS: "linux", AddedAt: time.Now().UTC(), AddedBy: by,
			Pairing: managed.PairingV2}, managed.Secret{Token: "t-" + id + "-0123456789abcdef0123456789"}); err != nil {
			t.Fatal(err)
		}
	}
	// ... and an Add host of the admin's whose machine has not answered yet (Add host keeps the audit log's text)
	e.s.Enroll.Scripts, e.s.Enroll.PanelAddr = testInstallers, "100.64.0.10"
	e.s.Enroll.Pair = func(ctx context.Context, url string, _ []byte) (int, enroll.Response, string, error) {
		return 0, enroll.Response{}, "", errors.New("dial tcp: connection refused")
	}
	cr, err := e.s.Enroll.Create(t.Context(), enrollment.Request{Name: "Shed", OS: "linux"}, nil, "owner@example.com", e.s.nodeSystemID)
	if err != nil {
		t.Fatal(err)
	}
	r.enroll = cr.ID
	t.Cleanup(func() { e.s.Enroll.Cancel(cr.ID) })
	// Edit host and Run timings, through the page's own requests
	if p := e.do("PUT", "/api/admin/hosts/laptop/options", map[string]any{"name": "Den laptop"}, r.hdr("sub-n")...); p.code != 200 {
		t.Fatalf("Edit host: %d %s", p.code, p.raw)
	}
	if p := e.do("PUT", "/api/admin/hosts/tower/options", map[string]any{"name": "Tower of Bob"}, r.hdr("sub-s")...); p.code != 200 {
		t.Fatalf("Edit host: %d %s", p.code, p.raw)
	}
	// ... and a socket the second admin took out of the total (its line has "total_by")
	if p := e.do("PUT", "/api/admin/hosts/laptop/options", map[string]any{"plugs": []any{map[string]any{"id": "laptop/desk", "in_total": false}}}, r.hdr("sub-s")...); p.code != 200 {
		t.Fatalf("Edit host, a socket out of the total: %d %s", p.code, p.raw)
	}
	if _, _, err := e.s.Poller.Admin.EditRunTimings("friend@example.com (unverified)", func(o *adminstate.RunTimings) error { o.Set(config.KeySettle, 45); return nil }); err != nil {
		t.Fatal(err)
	}
	// The run journal of then: a run the admin started that the second admin cancelled, and one that was going
	// when the panel was stopped for the update. The panel reports that one at its start, before anybody has
	// signed in again (a notification and an audit entry), and never continues it.
	journal := `{"version":1,
	 "active":{"id":"aaaaaaaaaaaaaaaa","state":"running","by":"owner@example.com","by_sub":"sub-n","started_at":"2026-11-15T02:00:00Z","list_hash":"1a2b3c4d","hosts":[
	   {"id":"tower","name":"TOWER","mode":"update_only","state":"updating","text":"Updating.","log":[]}]},
	 "history":[{"id":"bbbbbbbbbbbbbbbb","state":"cancelled","by":"owner@example.com","by_sub":"sub-n","started_at":"2026-11-14T02:00:00Z","ended_at":"2026-11-14T02:05:00Z","list_hash":"5e6f7a8b",
	   "cancel":{"by":"friend@example.com (unverified)","at":"2026-11-14T02:04:00Z","switch_off":false},"confirmed_by":"owner@example.com",
	   "hosts":[{"id":"tower","name":"TOWER","mode":"update_only","state":"skipped","text":"The run was cancelled by friend@example.com (unverified)",
	     "log":[{"t":"2026-11-14T02:04:00Z","text":"The run was cancelled by friend@example.com (unverified)"}]}]}]}`
	if err := os.WriteFile(filepath.Join(e.s.Cfg.DataDir, "wake-update.json"), []byte(journal), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.s.WakeRuns.Load(); err != nil {
		t.Fatal(err)
	}
	if a, hist := e.s.WakeRuns.Snapshot(); a != nil || len(hist) != 2 || hist[0].State != wakerun.RunInterrupted {
		t.Fatalf("the journal of then: %v %d", a, len(hist))
	}
	// notifications of then, for a host of each site and for no host
	for _, n := range []notifications.Event{
		{Kind: "helper_stopped", Type: "host", Severity: notifications.Warning, SystemID: "tower", Host: "TOWER", Text: "elevated helper stopped by owner@example.com (fans on BMC automatic control)"},
		{Kind: "plug_switched", Type: "power", Severity: notifications.Warning, SystemID: "tower", Host: "TOWER", Text: "Lamp (Desk bar) switched off by friend@example.com (unverified)"},
		{Kind: "host_options", Type: "host", Severity: notifications.Warning, SystemID: "laptop", Host: "LAPTOP", Text: "links changed by old@example.com"},
		{Kind: "host_removed", Type: "enroll", Severity: notifications.Warning, Host: "GONE-BOX", Text: "removed from the panel by owner@example.com (the agent on the machine keeps running until you uninstall it)"},
	} {
		e.s.Notes.Add(n)
	}
	// a countdown on tower that its agent was told about by build 16, and one on laptop by somebody the panel
	// does not know (the agent keeps letters, digits, spaces and . _ @ - of what it is told)
	e.b.Mu.Lock()
	for id, by := range map[string]string{"tower": "owner@example.com", "laptop": "stranger@example.org unverified"} {
		e.b.Metrics[id] = func() (jsonx.M, error) {
			m := testutil.AgentMetrics()
			m["hostname"] = id
			m["power_control"] = jsonx.M{"enabled": true, "pending": jsonx.M{"action": "shutdown", "at": float64(time.Now().Add(10 * time.Minute).Unix()), "delay_s": 600.0, "requested_by": by}}
			if id == "laptop" {
				m["ups"] = []any{upsRow("ups1", "OL")}
			}
			return m, nil
		}
	}
	e.b.Mu.Unlock()
	e.s.Poller.PollOnce(t.Context())
	// audit entries as that version wrote them: an action, a change on the Users page (its target is the
	// account's address), and what the throttle wrote
	sub := func(s string) *string { return &s }
	for _, en := range []audit.Entry{
		{Actor: "owner@example.com", Sub: sub("sub-n"), Action: "power.shutdown", Target: "tower", Params: map[string]any{"delay_s": 60}, Outcome: "ok", Source: "ui"},
		{Actor: "friend@example.com (unverified)", Sub: sub("sub-s"), Action: "user.set_role", Target: "old@example.com", Params: map[string]any{"sub": "sub-o", "role": "admin"}, Outcome: "ok", Source: "ui"},
		{Actor: "fairy", Sub: sub("sub-s"), Action: "audit.suppressed", Params: map[string]any{"dropped": 3, "actor": "friend@example.com (unverified)", "sub": "sub-s"},
			Outcome: "3 refused requests (auth.denied) by friend@example.com (unverified) not written", Source: "fairy"},
		{Actor: "stranger@example.org", Sub: sub("x9"), Action: "auth.login", Outcome: "denied: not in an allowed group", Source: "ui"},
		// ... and one this version wrote, of an account that was removed since: the entry has the user name of then
		{Actor: "gone@example.com", Sub: sub("sub-gone"), Username: "Aspen", Action: "helper.stop", Target: "tower", Outcome: "ok", Source: "ui"},
	} {
		if _, err := audit.Write(e.s.Cfg.AuditLog, en); err != nil {
			t.Fatal(err)
		}
	}
	if signIn {
		r.signInAll()
	}
	return r
}

// signInAll: the accounts sign in again after the update. Those that have a user name at Pocket ID have it in
// users.json from then on.
func (r *namesRig) signInAll() {
	for _, a := range namesAccounts {
		r.ck[a.sub] = r.cookie(a.user(true), 0)
	}
	r.s.Poller.PollOnce(r.t.Context()) // the countdowns are read again
}

// every GET route of the API's own list, with the ids of this rig in place of the parameters
func namesGETs(r *namesRig) []string {
	ids := map[string][]string{
		"id":   {"laptop", "tower", "node-srv-pvea", "qemu-120", "secret-box", "box-one", "box-two", "box-three", r.enroll},
		"sub":  {"sub-n", "sub-o"},
		"name": {"Alice"},
	}
	seen := map[string]bool{}
	var out []string
	for _, d := range apiDoc {
		if d.method != "GET" || d.path == "/api/stream" || d.path == "/api/settings/background" {
			continue
		}
		paths := []string{d.path}
		if m := paramRe.FindStringSubmatch(d.path); m != nil {
			paths = nil
			for _, id := range ids[m[1]] {
				paths = append(paths, strings.Replace(d.path, m[0], id, 1))
			}
		}
		for _, p := range paths {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	for _, extra := range []string{"/api/power/history?source=laptop/desk", "/api/power/day?source=laptop/desk", "/api/notifications?limit=200", "/api/audit?limit=200",
		"/api/audit?q=Juniper", "/api/audit?q=owner@example.com", "/api/openapi.json", "/", "/static/app.js", "/health"} {
		if !seen[extra] {
			out = append(out, extra)
		}
	}
	sort.Strings(out)
	return out
}

// namesAllowed says whether the answer to path may hold an address or a name: the Users page and the audit log,
// for an admin. (/api/me is checked on its own: a person's own address and name are theirs.)
func namesAllowed(path string) bool {
	return path == "/api/admin/users" || strings.HasPrefix(path, "/api/audit")
}

func namesHeld(body string, own ...string) []string {
	var found []string
	low := strings.ToLower(body)
	for _, p := range namesPrivate {
		if strings.Contains(low, strings.ToLower(p)) {
			found = append(found, p)
		}
	}
	return found
}

// The check that is meant to catch a place that is forgotten later: every GET route the API lists (the test
// TestOpenAPIListsEveryRoute keeps that list complete), the page itself and the stream, asked by an admin, the
// second admin, an admin without a user name, a viewer of every site, a viewer of one site, a read token of
// every site, a read token of one site, a control token, and somebody who waits for access. No answer holds an
// address or a name of any account, but the Users page's and the audit log's for an admin, and one's own in
// /api/me. And the user names are there, so the answers are not just empty.
func TestNoAnswerNamesAPersonByAddressOrName(t *testing.T) {
	r := newNamesRig(t, true)
	e := r.env
	readAll, _, err := e.s.Tokens.Create("reader", "system", "read", "cli:test", 0)
	if err != nil {
		t.Fatal(err)
	}
	readBob, _, err := e.s.Tokens.CreateForSites("bob-reader", "sub-n", "read", "cli:test", 0, []string{"Bob"})
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := e.s.Tokens.Create("ci", "sub-n", "control", "cli:test", 0)
	if err != nil {
		t.Fatal(err)
	}
	pending := e.cookie(model.User{Sub: "sub-p", Name: model.P("Pending Person"), PreferredUsername: model.P("waiter"), Email: model.P("pending@example.com"), EmailVerified: true}, 0)
	who := []struct {
		what  string
		hdr   []string
		admin bool
		sub   string
	}{
		{"the admin", []string{"Cookie", r.ck["sub-n"]}, true, "sub-n"},
		{"the second admin", []string{"Cookie", r.ck["sub-s"]}, true, "sub-s"},
		{"an admin without a user name", []string{"Cookie", r.ck["sub-o"]}, true, "sub-o"},
		{"a viewer of every site", []string{"Cookie", r.ck["sub-v"]}, false, "sub-v"},
		{"a viewer of one site", []string{"Cookie", r.ck["sub-l"]}, false, "sub-l"},
		{"a read token of every site", []string{"Authorization", "Bearer " + readAll}, false, ""},
		{"a read token of one site", []string{"Authorization", "Bearer " + readBob}, false, ""},
		{"a control token", []string{"Authorization", "Bearer " + control}, true, ""},
		{"somebody who waits for access", []string{"Cookie", pending}, false, "sub-p"},
		{"nobody", nil, false, ""},
	}
	paths := namesGETs(r)
	if len(paths) < 60 {
		t.Fatalf("only %d GET addresses: the list of routes is not read any more", len(paths))
	}
	all := map[string]string{} // who -> everything they got
	answered := map[string]int{}
	for _, w := range who {
		for _, path := range paths {
			x := e.do("GET", path, nil, w.hdr...)
			body := string(x.raw)
			if x.code == 200 {
				answered[w.what]++
			}
			all[w.what] += "\n" + path + "\n" + body
			if path == "/api/me" && w.sub != "" && x.code == 200 {
				// one's own address and name, and nobody else's
				var me map[string]any
				_ = json.Unmarshal(x.raw, &me)
				mine := fmt.Sprint(me["email"]) + "|" + fmt.Sprint(me["name"])
				for _, p := range namesHeld(body) {
					if !strings.Contains(strings.ToLower(mine), strings.ToLower(p)) && p != "@example.com" {
						t.Errorf("%s: /api/me holds %q, which is not their own", w.what, p)
					}
				}
				continue
			}
			if w.admin && namesAllowed(path) && x.code == 200 {
				continue
			}
			if held := namesHeld(body); len(held) > 0 {
				t.Errorf("%s: GET %s (%d) holds %q:\n%s", w.what, path, x.code, held, clipText(body, 1500))
			}
		}
		// the stream: the systems, and the notifications made while it is open
		code, body := e.streamBody("/api/stream", w.hdr...)
		all[w.what] += "\n/api/stream\n" + body
		if held := namesHeld(body); len(held) > 0 {
			t.Errorf("%s: the stream (%d) holds %q", w.what, code, held)
		}
	}
	// the answers were answers: each of these got data, and the user names are in it
	for what, least := range map[string]int{"the admin": 50, "the second admin": 50, "an admin without a user name": 50, "a viewer of every site": 15, "a viewer of one site": 15,
		"a read token of every site": 12, "a read token of one site": 12, "a control token": 20} {
		if answered[what] < least {
			t.Errorf("%s got %d answers with 200, want at least %d", what, answered[what], least)
		}
	}
	for what, wants := range map[string][]string{
		"the admin": {`"added_by":"Juniper"`, `"added_by":"PineTop"`, `"added_by":"(address hidden)"`, `"created_by":"Juniper"`, // Add host
			`"by":"Juniper"`, `"by":"PineTop"`, `"total_by":"PineTop"`, // Edit host, Run timings, the run
			`"requested_by":"Juniper"`, `"requested_by":"(address hidden)"`, // the countdowns
			`stopped by Juniper (fans`, `switched off by PineTop"`, `links changed by (address hidden)"`, `removed from the panel by Juniper (the agent`, // notifications
			`"username":"PineTop"`,                                  // everyone's sessions
			`"shown_as":"Juniper"`, `"shown_as":"(address hidden)"`, // the Users page
			`"who":"Juniper"`, `"who":"PineTop"`, `"who":"(address hidden)"`, `"via":"panel by Juniper"`},
		"a viewer of every site":       {`"requested_by":"Juniper"`, `"requested_by":"(address hidden)"`, `stopped by Juniper (fans`, `switched off by PineTop"`, `links changed by (address hidden)"`, `"shown_as":"viewer-all"`},
		"a viewer of one site":         {`"requested_by":"Juniper"`, `stopped by Juniper (fans`, `switched off by PineTop"`, `"shown_as":"viewer-one"`},
		"a read token of one site":     {`"requested_by":"Juniper"`, `stopped by Juniper (fans`},
		"an admin without a user name": {`"shown_as":"(address hidden)"`, `"added_by":"Juniper"`},
	} {
		for _, want := range wants {
			if !strings.Contains(all[what], want) {
				t.Errorf("%s: no answer holds %s", what, want)
			}
		}
	}
	// a viewer of one site gets nothing of the other site's host, as before (the rig's own rule)
	if strings.Contains(all["a viewer of one site"], "links changed by") || strings.Contains(all["a viewer of one site"], `"added_by"`) {
		t.Error("a viewer of the site Bob got a notification of the laptop, or the hosts list")
	}
	// the page's own files hold no address of an account (they hold no data at all)
	for _, f := range []string{"/", "/static/app.js"} {
		if x := e.do("GET", f, nil); x.code != 200 && x.code != 303 {
			t.Errorf("GET %s: %d", f, x.code)
		}
	}
}

// What the two admin pages carry, and only for an admin: the Users page has each account's user name, how the
// panel names it, its address and its name; the audit log keeps every entry as it was written (actor, sub) and
// adds who: the user name. A search finds an entry by the user name, also an entry of an older version, and by
// the address it was written with.
func TestUsersPageAndAuditLogCarryTheAddressForAdminsOnly(t *testing.T) {
	r := newNamesRig(t, true)
	e := r.env
	users := e.do("GET", "/api/admin/users", nil, "Cookie", r.ck["sub-n"])
	if users.code != 200 {
		t.Fatal(users.code, string(users.raw))
	}
	got := map[string]map[string]any{}
	for _, x := range users.body["users"].([]any) {
		u := x.(map[string]any)
		got[u["sub"].(string)] = u
	}
	for _, c := range []struct{ sub, username, shown, email, name, via string }{
		{"sub-n", "Juniper", "Juniper", "owner@example.com", "Real Name", "bootstrap_admins"},
		{"sub-s", "PineTop", "PineTop", "friend@example.com", "Other Person", "panel by Juniper"},
		{"sub-v", "viewer-all", "viewer-all", "wide@example.com", "Wide Viewer", "panel by Juniper"},
		{"sub-o", "", "(address hidden)", "old@example.com", "Old Timer", "panel by Juniper"},
		{"sub-admin", "", "(address hidden)", "sub-admin@example.com", "sub-admin", "test"},
	} {
		u := got[c.sub]
		username, _ := u["username"].(string)
		if u == nil || username != c.username || u["shown_as"] != c.shown || u["email"] != c.email || u["name"] != c.name || u["via"] != c.via {
			t.Errorf("the Users page, %s: %v", c.sub, u)
		}
	}
	if got["sub-n"]["you"] != true || got["sub-s"]["you"] != false {
		t.Error("you")
	}
	// the audit log
	page := e.do("GET", "/api/audit?limit=200", nil, "Cookie", r.ck["sub-n"])
	if page.code != 200 {
		t.Fatal(page.code)
	}
	var old, now, gone int
	for _, x := range page.body["entries"].([]any) {
		en := x.(map[string]any)
		switch {
		case en["action"] == "power.shutdown" && en["sub"] == "sub-n": // written by build 16
			old++
			if en["actor"] != "owner@example.com" || en["who"] != "Juniper" || en["username"] != nil || en["target"] != "tower" {
				t.Errorf("an entry of an older version: %v", en)
			}
		case en["action"] == "user.set_role" && en["sub"] == "sub-s":
			if en["actor"] != "friend@example.com (unverified)" || en["who"] != "PineTop" || en["target"] != "old@example.com" {
				t.Errorf("a change on the Users page: %v", en)
			}
		case en["action"] == "auth.login" && en["sub"] == "x9":
			if en["actor"] != "stranger@example.org" || en["who"] != "(address hidden)" {
				t.Errorf("a refused sign-in of nobody the panel knows: %v", en)
			}
		case en["action"] == "helper.stop" && en["sub"] == "sub-gone":
			gone++
			if en["actor"] != "gone@example.com" || en["who"] != "Aspen" || en["username"] != "Aspen" {
				t.Errorf("an entry of an account that is gone, written with its user name: %v", en)
			}
		case en["action"] == "audit.suppressed":
			if en["actor"] != "fairy" || en["who"] != "PineTop" || en["params"].(map[string]any)["actor"] != "friend@example.com (unverified)" {
				t.Errorf("the throttle's entry: %v", en)
			}
		}
	}
	if old != 1 || gone != 1 {
		t.Fatalf("the entry of an older version: %d, the entry of the account that is gone: %d", old, gone)
	}
	if private, _ := page.body["private"].([]any); len(private) < 10 || private[0] != "sub-pending@example.com" {
		t.Errorf("what the page draws as dots: %v", page.body["private"])
	}
	// an entry written now: actor and sub as before, and the user name
	if p := e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 7}, r.hdr("sub-n")...); p.code != 200 {
		t.Fatal(p.code, string(p.raw))
	}
	if p := e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 8}, r.hdr("sub-o")...); p.code != 200 {
		t.Fatal(p.code, string(p.raw))
	}
	for _, en := range auditOf(e, "poll.set") {
		switch en["sub"] {
		case "sub-n":
			now++
			if en["actor"] != "owner@example.com" || en["username"] != "Juniper" || en["source"] != "ui" || en["target"] != "laptop" {
				t.Errorf("an entry written now: %v", en)
			}
		case "sub-o":
			now++
			if _, has := en["username"]; has || en["actor"] != "old@example.com" {
				t.Errorf("an entry of an account without a user name: %v", en)
			}
		}
	}
	if now != 2 {
		t.Fatalf("the entries written now: %d", now)
	}
	// the search: by the user name (old entries too, by their subject), by the address, by the id
	count := func(q string) (n int, subs map[string]bool) {
		subs = map[string]bool{}
		x := e.do("GET", "/api/audit?limit=200&q="+q, nil, "Cookie", r.ck["sub-n"])
		for _, y := range x.body["entries"].([]any) {
			n++
			subs[fmt.Sprint(y.(map[string]any)["sub"])] = true
		}
		return n, subs
	}
	if n, subs := count("Juniper"); n < 3 || len(subs) != 1 || !subs["sub-n"] {
		t.Errorf("a search for the user name: %d entries of %v", n, subs)
	}
	if n, subs := count("juni"); n < 3 || !subs["sub-n"] {
		t.Errorf("a search for a part of the user name: %d entries of %v", n, subs)
	}
	if n, subs := count("owner@example.com"); n < 2 || len(subs) != 1 || !subs["sub-n"] {
		t.Errorf("a search for the address: %d entries of %v", n, subs)
	}
	if n, subs := count("PineTop"); n < 2 || len(subs) != 1 || !subs["sub-s"] {
		t.Errorf("a search for the second admin: %d entries of %v", n, subs)
	}
	if n, _ := count("nobody-of-that-name"); n != 0 {
		t.Errorf("a search for nobody: %d", n)
	}
	// nobody but an admin gets either page
	for what, ck := range map[string]string{"a viewer of every site": r.ck["sub-v"], "a viewer of one site": r.ck["sub-l"]} {
		for _, path := range []string{"/api/admin/users", "/api/audit", "/api/admin/sessions"} {
			if x := e.do("GET", path, nil, "Cookie", ck); x.code != 403 || len(namesHeld(string(x.raw))) > 0 {
				t.Errorf("%s, %s: %d %s", what, path, x.code, x.raw)
			}
		}
	}
}

// Old records. Before anybody has signed in again, the accounts have no user name: every place hides the
// address it was stored with. Nothing is rewritten for it: the hosts file, state.json and the run journal say
// what they said. After the sign-ins the same records are shown with the user names.
func TestOldRecordsAreShownByUsernameAndNotRewritten(t *testing.T) {
	r := newNamesRig(t, false)
	e := r.env
	files := func() map[string]string {
		out := map[string]string{}
		for _, f := range []string{"managed-hosts.yaml", "state.json", "wake-update.json", "notifications.json", "sessions.json"} {
			b, err := os.ReadFile(filepath.Join(e.s.Cfg.DataDir, f))
			if err != nil {
				t.Fatal(err)
			}
			out[f] = string(b)
		}
		return out
	}
	e.s.Notes.Flush()
	before := files()
	for f, want := range map[string]string{"managed-hosts.yaml": "added_by: owner@example.com", "state.json": `"by": "owner@example.com"`, "wake-update.json": `"by": "owner@example.com"`,
		"notifications.json": "stopped by owner@example.com", "sessions.json": "friend@example.com"} {
		if !strings.Contains(before[f], want) {
			t.Fatalf("the fixture: %s does not hold %q:\n%s", f, want, clipText(before[f], 800))
		}
	}
	read := func(sub string) string {
		var all string
		for _, path := range []string{"/api/admin/hosts", "/api/admin/hosts/laptop/options", "/api/admin/hosts/tower/options", "/api/admin/run-timings", "/api/admin/wake-update",
			"/api/notifications?limit=200", "/api/systems", "/api/systems/tower/power", "/api/admin/sessions", "/api/me"} {
			x := e.do("GET", path, nil, "Cookie", r.ck[sub])
			if x.code != 200 {
				t.Fatalf("GET %s: %d %s", path, x.code, x.raw)
			}
			if path != "/api/me" {
				if held := namesHeld(string(x.raw)); len(held) > 0 {
					t.Errorf("GET %s holds %q:\n%s", path, held, clipText(string(x.raw), 1200))
				}
			}
			all += "\n" + path + "\n" + string(x.raw)
		}
		return all
	}
	// nobody has a user name yet
	a := read("sub-n")
	for _, want := range []string{`"added_by":"(address hidden)"`, `"by":"(address hidden)"`, `"requested_by":"(address hidden)"`, `stopped by (address hidden) (fans`,
		`switched off by (address hidden)"`, `"username":"(address hidden)"`, `"shown_as":"(address hidden)"`} {
		if !strings.Contains(a, want) {
			t.Errorf("before the sign-ins: no answer holds %s", want)
		}
	}
	for _, never := range []string{"Juniper", "PineTop"} {
		if strings.Contains(a, never) {
			t.Errorf("before the sign-ins an answer holds %q", never)
		}
	}
	// everybody signs in again
	r.signInAll()
	a = read("sub-n")
	hosts := e.do("GET", "/api/admin/hosts", nil, "Cookie", r.ck["sub-n"])
	added := map[string]string{}
	for _, x := range hosts.body["managed"].([]any) {
		m := x.(map[string]any)
		added[m["id"].(string)] = fmt.Sprint(m["added_by"])
	}
	if added["box-one"] != "Juniper" || added["box-two"] != "PineTop" || added["box-three"] != "(address hidden)" {
		t.Errorf("Added ... by: %v", added)
	}
	opt := func(id string) string {
		h, _ := e.do("GET", "/api/admin/hosts/"+id+"/options", nil, "Cookie", r.ck["sub-n"]).body["host"].(map[string]any)
		n, _ := h["name"].(map[string]any)
		return fmt.Sprint(n["by"])
	}
	if opt("laptop") != "Juniper" || opt("tower") != "PineTop" {
		t.Errorf("Edit host, who changed the name: %q %q", opt("laptop"), opt("tower"))
	}
	var settle string
	for _, x := range e.do("GET", "/api/admin/run-timings", nil, "Cookie", r.ck["sub-n"]).body["timings"].([]any) {
		if m := x.(map[string]any); m["key"] == config.KeySettle {
			settle = fmt.Sprint(m["by"])
		}
	}
	if settle != "PineTop" {
		t.Errorf("Run timings, who changed it: %q", settle)
	}
	wu := e.do("GET", "/api/admin/wake-update", nil, "Cookie", r.ck["sub-n"])
	last, _ := wu.body["last"].([]any)
	if len(last) != 2 {
		t.Fatalf("the runs: %s", wu.raw)
	}
	stopped, run := last[0].(map[string]any), last[1].(map[string]any)
	cancel, _ := run["cancel"].(map[string]any)
	if run["by"] != "Juniper" || cancel["by"] != "PineTop" || run["confirmed_by"] != "Juniper" || run["by_actor"] != nil || stopped["by"] != "Juniper" || stopped["state"] != "interrupted" {
		t.Errorf("the runs: started by %v, cancelled by %v, confirmed by %v, by_actor %v; the interrupted one by %v", run["by"], cancel["by"], run["confirmed_by"], run["by_actor"], stopped["by"])
	}
	if raw := string(wu.raw); strings.Count(raw, "The run was cancelled by PineTop") != 2 { // the machine's line and its log
		t.Errorf("the run's own sentences: %s", clipText(raw, 1500))
	}
	// the run that the restart interrupted was reported before anybody had signed in again: its notification
	// names the starter now, and its audit entry is the admin's, as that version would have written it
	if !strings.Contains(a, "the panel restarted during the run Juniper started at 02:00 UTC; it is not continued") {
		t.Errorf("the notification of the interrupted run: %s", clipText(a[strings.Index(a, "/api/notifications"):], 1500))
	}
	if en := auditOf(e, "wakeupdate.interrupted"); len(en) != 1 || en[0]["actor"] != "owner@example.com" || en[0]["sub"] != "sub-n" {
		t.Errorf("the audit entry of the interrupted run: %v", en)
	}
	if pw := e.do("GET", "/api/systems/tower/power", nil, "Cookie", r.ck["sub-l"]); pw.code != 200 || !strings.Contains(string(pw.raw), `"requested_by":"Juniper"`) {
		t.Errorf("the countdown, for a viewer of the site: %d %s", pw.code, pw.raw)
	}
	for _, want := range []string{`stopped by Juniper (fans`, `switched off by PineTop"`, `links changed by (address hidden)"`, `"username":"PineTop"`, `"shown_as":"Juniper"`} {
		if !strings.Contains(a, want) {
			t.Errorf("after the sign-ins: no answer holds %s", want)
		}
	}
	// the files are what they were (but for users.json, which has the user names now, and the sessions made since)
	e.s.Notes.Flush()
	after := files()
	for _, f := range []string{"managed-hosts.yaml", "state.json", "wake-update.json", "notifications.json"} {
		if before[f] != after[f] {
			t.Errorf("%s was rewritten:\n%s\n---\n%s", f, clipText(before[f], 600), clipText(after[f], 600))
		}
	}
	if u := e.s.Access.Load().Users; u["sub-n"].Username != "Juniper" || u["sub-o"].Username != "" || u["sub-n"].Email != "owner@example.com" || u["sub-n"].Role != access.RoleAdmin {
		t.Errorf("users.json: %+v", u["sub-n"])
	}
}

// What an account does from build 17 on is recorded with the user name where the record is a sentence or goes
// to a machine, and the audit entry has address, subject and user name. An account without a user name is not
// named by its address in any of it: the machine and the notification get the words that stand for it; a record
// that keeps "by" keeps what it kept before, and is shown hidden.
func TestWhatIsWrittenNowNamesByUsername(t *testing.T) {
	r := newNamesRig(t, true)
	e := r.env
	e.b.Mu.Lock()
	e.b.Requests = nil
	e.b.Mu.Unlock()
	var pushed []string
	e.s.sendOutside = func(title, message string) { pushed = append(pushed, title+" | "+message) } // in place of notify.Notify
	// Edit host, by the admin and by the admin without a user name
	if p := e.do("PUT", "/api/admin/hosts/laptop/options", map[string]any{"name": "Attic laptop"}, r.hdr("sub-n")...); p.code != 200 {
		t.Fatal(p.code, string(p.raw))
	}
	if p := e.do("PUT", "/api/admin/hosts/tower/options", map[string]any{"name": "Cellar tower"}, r.hdr("sub-o")...); p.code != 200 {
		t.Fatal(p.code, string(p.raw))
	}
	state := string(must(os.ReadFile(filepath.Join(e.s.Cfg.DataDir, "state.json"))))
	if !strings.Contains(state, `"by": "Juniper"`) || !strings.Contains(state, `"by": "old@example.com"`) || strings.Contains(state, "owner@example.com") {
		t.Errorf("state.json after two changes in Edit host:\n%s", clipText(state, 1500))
	}
	opt := func(id, sub string) string {
		h, _ := e.do("GET", "/api/admin/hosts/"+id+"/options", nil, "Cookie", r.ck[sub]).body["host"].(map[string]any)
		n, _ := h["name"].(map[string]any)
		return fmt.Sprint(n["by"])
	}
	if opt("laptop", "sub-s") != "Juniper" || opt("tower", "sub-s") != "(address hidden)" {
		t.Errorf("who changed the name: %q %q", opt("laptop", "sub-s"), opt("tower", "sub-s"))
	}
	// a role, by the admin: "panel by <who>" in users.json
	if p := e.do("PUT", "/api/admin/users/sub-l", map[string]any{"role": "viewer", "sites": []string{"Bob"}}, r.hdr("sub-n")...); p.code != 200 || p.body["message"] != "viewer-one is now viewer of Bob" {
		t.Fatal(p.code, string(p.raw))
	}
	if p := e.do("PUT", "/api/admin/users/sub-alice", map[string]any{"role": "viewer", "sites": []string{"Alice", "Bob"}}, r.hdr("sub-n")...); p.code != 200 || p.body["message"] != "This account is now viewer of Alice, Bob" {
		t.Fatal(p.code, string(p.raw))
	}
	if p := e.do("PUT", "/api/admin/users/sub-bob", map[string]any{"role": "pending"}, r.hdr("sub-n")...); p.code != 200 {
		t.Fatal(p.code, string(p.raw))
	}
	if via := e.s.Access.Load().Users["sub-bob"].Via; via != "panel by Juniper" {
		t.Errorf("via: %q", via)
	}
	if p := e.do("DELETE", "/api/admin/users/sub-pending", nil, r.hdr("sub-n")...); p.code != 200 || p.body["message"] != "This account was removed from the list" {
		t.Fatal(p.code, string(p.raw))
	}
	if rm := e.s.Access.Load().Removed["sub-pending"]; rm.By != "panel by Juniper" || rm.Label != "sub-pending@example.com" {
		t.Errorf("the removed account: %+v", rm)
	}
	// the audit entries of these changes keep the address as their target, the actor, the subject, and have the user name
	for _, en := range auditOf(e, "user") {
		if en["sub"] != "sub-n" {
			continue
		}
		if en["actor"] != "owner@example.com" || en["username"] != "Juniper" {
			t.Errorf("the audit entry of a change on the Users page: %v", en)
		}
		if en["action"] == "user.remove" && en["target"] != "sub-pending@example.com" {
			t.Errorf("the target of a removal: %v", en["target"])
		}
	}
	// What a machine is told: the cancel of the countdown the laptop shows, by the admin without a user name,
	// and then a shutdown with a countdown by the admin.
	if p := e.do("POST", "/api/systems/laptop/power", map[string]any{"action": "cancel"}, r.hdr("sub-o")...); p.code != 200 {
		t.Fatalf("cancel: %d %s", p.code, p.raw)
	}
	e.b.Mu.Lock()
	e.b.Metrics["laptop"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["ups"] = []any{upsRow("ups1", "OL")}
		return m, nil
	}
	e.b.Mu.Unlock()
	e.s.Poller.PollOnce(t.Context())
	name, _ := e.do("GET", "/api/systems/laptop", nil, "Cookie", r.ck["sub-n"]).body["name"].(string)
	if p := e.do("POST", "/api/systems/laptop/power", map[string]any{"action": "shutdown", "delay_s": 60, "confirm": name}, r.hdr("sub-n")...); p.code != 200 {
		t.Fatalf("shutdown: %d %s", p.code, p.raw)
	}
	e.b.Mu.Lock()
	told := strings.Join(e.b.Requests, "\n")
	e.b.Mu.Unlock()
	if !strings.Contains(told, "laptop POST /power map[action:cancel delay_s:60 requested_by:(address hidden)]") || !strings.Contains(told, "laptop POST /power map[action:shutdown delay_s:60 requested_by:Juniper]") {
		t.Errorf("what the machine was told:\n%s", told)
	}
	if held := namesHeld(told); len(held) > 0 {
		t.Errorf("a machine was told %q:\n%s", held, told)
	}
	// the notifications of all of it, as the list has them and as they are pushed
	e.s.Notes.Flush()
	notes := string(must(os.ReadFile(filepath.Join(e.s.Cfg.DataDir, "notifications.json"))))
	const lastOld = "removed from the panel by owner@example.com" // what the rig stored as build 16 comes first
	fresh := notes[strings.Index(notes, lastOld)+len(lastOld):]
	for _, want := range []string{`"text":"host edited by Juniper"`, `"text":"host edited by (address hidden)"`, `cancelled by (address hidden)"`, `"text":"shutdown requested by Juniper (`} {
		if !strings.Contains(fresh, want) {
			t.Errorf("notifications.json lacks %s:\n%s", want, clipText(fresh, 2000))
		}
	}
	if held := namesHeld(fresh); len(held) > 0 {
		t.Errorf("a notification written now holds %q:\n%s", held, clipText(fresh, 2000))
	}
	for _, p := range pushed {
		if held := namesHeld(p); len(held) > 0 {
			t.Errorf("pushed with %q: %s", held, p)
		}
	}
}

// What goes to ntfy or Discord for an audited control action: the user name, never the address; the account a
// change of the Users page is about by its user name too; and a sentence made from an old record without the
// address in it.
func TestOutsideNotificationsNameByUsername(t *testing.T) {
	r := newNamesRig(t, true)
	e := r.env
	var got []string
	e.s.sendOutside = func(title, message string) { got = append(got, title+" | "+message) } // in place of notify.Notify
	if p := e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 9}, r.hdr("sub-n")...); p.code != 200 {
		t.Fatal(p.code, string(p.raw))
	}
	if p := e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 11}, r.hdr("sub-o")...); p.code != 200 {
		t.Fatal(p.code, string(p.raw))
	}
	if p := e.do("PUT", "/api/admin/users/sub-s", map[string]any{"role": "viewer", "sites": []string{"Bob"}}, r.hdr("sub-n")...); p.code != 200 {
		t.Fatal(p.code, string(p.raw))
	}
	if p := e.do("PUT", "/api/admin/users/sub-o", map[string]any{"role": "viewer", "sites": []string{"Bob"}}, r.hdr("sub-n")...); p.code != 200 {
		t.Fatal(p.code, string(p.raw))
	}
	// an account whose address does not look like one (a local provider), and one that has a name and no address:
	// the entry names each by that text, the message by the user name
	for _, id := range []access.Identity{{Sub: "sub-c", Email: "svc@localhost", Username: "svc-user"}, {Sub: "sub-b", Name: "Bare Name", Username: "bare"}} {
		if _, _, err := e.s.Access.SeenIdentity(id, access.RolePending, "first sign-in"); err != nil {
			t.Fatal(err)
		}
		if p := e.do("PUT", "/api/admin/users/"+id.Sub, map[string]any{"role": "viewer", "sites": []string{"Bob"}}, r.hdr("sub-n")...); p.code != 200 {
			t.Fatal(p.code, string(p.raw))
		}
	}
	if en := auditOf(e, "user.set_role"); len(en) < 4 || en[0]["target"] != "Bare Name" || en[1]["target"] != "svc@localhost" {
		t.Fatalf("the entries keep the account's own text as their target: %v", en)
	}
	// a warning made from what an older version stored (the store pushes warnings)
	e.s.Notes.Add(notifications.Event{Kind: "wakeupdate_interrupted", Type: "update", Severity: notifications.Warning, Host: "Fairy panel",
		Text: "the panel restarted during the run owner@example.com started at 10:00 UTC; it is not continued"})
	all := strings.Join(got, "\n")
	if strings.Contains(all, "svc@localhost") || strings.Contains(all, "Bare Name") {
		t.Errorf("a message to the notification service names an account by its address or name:\n%s", all)
	}
	for _, want := range []string{"Fairy: poll.set on laptop | Juniper via ui: ok", "(address hidden) via ui: ok", "Fairy: user.set_role on PineTop | Juniper via ui: ok",
		"Fairy: user.set_role on svc-user | Juniper via ui: ok", "Fairy: user.set_role on bare | Juniper via ui: ok",
		"Fairy: user.set_role on (address hidden) | Juniper via ui: ok", "the panel restarted during the run Juniper started at 10:00 UTC"} {
		if !strings.Contains(all, want) {
			t.Errorf("no message holds %q:\n%s", want, all)
		}
	}
	if held := namesHeld(all); len(held) > 0 {
		t.Errorf("a message to the notification service holds %q:\n%s", held, all)
	}
	if len(got) < 5 {
		t.Fatalf("%d messages", len(got))
	}
}

// A session that was signed in before the update has the user name in its cookie, and users.json has none for
// its account yet. Its first request gives the account that user name: the person is named by it at once, to
// themselves and to everybody. A later sign-in's user name is never replaced from an older cookie, and a
// session whose cookie has none (the provider sends none) is "(address hidden)" to everybody and has a name
// in its own menu all the same.
func TestASessionFromBeforeTheUpdate(t *testing.T) {
	e := setupSites(t)
	for sub, mail := range map[string]string{"sub-n": "owner@example.com", "sub-o": "old@example.com"} {
		if _, _, err := e.s.Access.SeenIdentity(access.Identity{Sub: sub, Email: mail, EmailVerified: true, Name: "Real Name of " + sub}, access.RolePending, "first sign-in"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.s.Access.SetRole(sub, access.RoleAdmin, nil, "test", true); err != nil {
			t.Fatal(err)
		}
	}
	n := model.User{Sub: "sub-n", Name: model.P("Real Name of sub-n"), PreferredUsername: model.P("Juniper"), Email: model.P("owner@example.com"), EmailVerified: true}
	o := model.User{Sub: "sub-o", Name: model.P("Real Name of sub-o"), Email: model.P("old@example.com"), EmailVerified: true}
	ckN, ckO := e.staleCookie(n, time.Hour), e.staleCookie(o, time.Hour)
	if u := e.s.Access.Load().Users["sub-n"]; u.Username != "" {
		t.Fatal("the fixture: the account has a user name before its session's first request")
	}
	// the first request of the old session
	me := e.do("GET", "/api/me", nil, "Cookie", ckN)
	if me.code != 200 || me.body["shown_as"] != "Juniper" || me.body["role"] != "admin" || me.body["email"] != "owner@example.com" {
		t.Fatalf("the old session's /api/me: %d %s", me.code, me.raw)
	}
	if u := e.s.Access.Load().Users["sub-n"]; u.Username != "Juniper" || u.Role != access.RoleAdmin || u.Email != "owner@example.com" || u.Name != "Real Name of sub-n" {
		t.Fatalf("users.json after the old session's first request: %+v", u)
	}
	// ... and what it does is named by it, for everybody
	if p := e.do("PUT", "/api/admin/hosts/laptop/options", map[string]any{"name": "Loft laptop"}, "Cookie", e.staleCookie(n, 0), "X-Requested-With", "fairy", "Content-Type", "application/json"); p.code != 200 {
		t.Fatal(p.code, string(p.raw))
	}
	h, _ := e.do("GET", "/api/admin/hosts/laptop/options", nil, "Cookie", ckO).body["host"].(map[string]any)
	if by := h["name"].(map[string]any)["by"]; by != "Juniper" {
		t.Errorf("who changed the name, as another admin gets it: %v", by)
	}
	// a session without a user name: no crash, no empty name
	me = e.do("GET", "/api/me", nil, "Cookie", ckO)
	if me.code != 200 || me.body["shown_as"] != "(address hidden)" || me.body["preferred_username"] != nil || me.body["role"] != "admin" {
		t.Fatalf("a session without a user name: %d %s", me.code, me.raw)
	}
	if u := e.s.Access.Load().Users["sub-o"]; u.Username != "" {
		t.Fatalf("a user name out of nothing: %+v", u)
	}
	// a sign-in after the update records another user name; the old session's cookie does not put its own back
	later := n
	later.PreferredUsername = model.P("Juniper2")
	e.signIn(later)
	e.s.seenMu.Lock()
	delete(e.s.seen, "sub-n") // the old session's next request is one that writes "last seen" again
	e.s.seenMu.Unlock()
	me = e.do("GET", "/api/me", nil, "Cookie", ckN)
	if u := e.s.Access.Load().Users["sub-n"]; me.code != 200 || u.Username != "Juniper2" || me.body["shown_as"] != "Juniper2" {
		t.Fatalf("an older session replaced the user name of a later sign-in: %+v, /api/me %s", u, me.raw)
	}
	// a cookie cannot make the panel name its account by another account's address: the user name is not shown
	// (and the other account's address is not handed out for it)
	imp := model.User{Sub: "sub-i", Name: model.P("Imp"), PreferredUsername: model.P("owner@example.com"), Email: model.P("imp@example.com"), EmailVerified: true}
	ckI := e.cookie(imp, 0)
	if _, _, err := e.s.Access.SetRole("sub-i", access.RoleAdmin, nil, "test", true); err != nil {
		t.Fatal(err)
	}
	me = e.do("GET", "/api/me", nil, "Cookie", ckI)
	if me.code != 200 || me.body["shown_as"] != "(address hidden)" {
		t.Errorf("an account that calls itself by another's address: %s", me.raw)
	}
	if x := e.do("GET", "/api/me", nil, "Cookie", ckN); x.body["shown_as"] != "Juniper2" {
		t.Errorf("... and the account whose address it is: %s", x.raw)
	}
}

// The user name is cleaned where it comes in: the claim of the ID token, and of userinfo.
func TestUsernameClaimIsCleaned(t *testing.T) {
	for _, c := range []struct {
		claim any
		want  *string
	}{
		{"Juniper", model.P("Juniper")},
		{"  Juniper  ", model.P("Juniper")},
		{"Juni\x1b[31mper\x07", model.P("Juniper")},
		{"Juni\u202eper\nx", model.P("Juniper x")},
		{strings.Repeat("n", 300), model.P(strings.Repeat("n", 64))},
		{"\x00\n\t ", nil},
		{"", nil},
		{nil, nil},
		{42, nil},
	} {
		u := userFromClaims(map[string]any{"sub": "s", "preferred_username": c.claim, "name": "Real Name", "email": "owner@example.com"}, "groups")
		if (u.PreferredUsername == nil) != (c.want == nil) || (c.want != nil && *u.PreferredUsername != *c.want) {
			t.Errorf("the claim %q is kept as %q, want %q", c.claim, model.Deref(u.PreferredUsername), model.Deref(c.want))
		}
		if model.Deref(u.Name) != "Real Name" || model.Deref(u.Email) != "owner@example.com" {
			t.Errorf("the other claims: %+v", u)
		}
	}
}

// Nothing decides by the user name. An account that takes the admin's user name, the user name of a
// bootstrap admin's address or an allowed address gets nothing for it: no role, no sign-in. (Roles are by
// subject; allowed_emails and bootstrap_admins by the verified address, as before.)
func TestNoAccessDecisionReadsTheUsername(t *testing.T) {
	e := setupSites(t)
	e.s.Cfg.OIDC.BootstrapAdmins = []string{"owner@example.com"}
	e.s.Cfg.OIDC.AllowedEmails = []string{"owner@example.com"}
	imp := model.User{Sub: "sub-i", PreferredUsername: model.P("owner@example.com"), Name: model.P("owner@example.com"), Email: model.P("imp@example.com"), EmailVerified: true, Groups: []string{"viewers"}}
	if isAllowed(imp, e.s.Cfg.OIDC) || emailAllowed(imp, e.s.Cfg.OIDC) {
		t.Error("allowed_emails took a user name for an address")
	}
	if role, via := e.s.bootstrapRole(&imp); role == access.RoleAdmin {
		t.Errorf("bootstrap_admins took a user name for an address: %s %s", role, via)
	}
	ck := e.cookie(imp, 0)
	if me := e.do("GET", "/api/me", nil, "Cookie", ck); me.body["role"] != "pending" {
		t.Errorf("an account with the admin's address as its user name: %s", me.raw)
	}
	// the store finds an account by its subject or its verified address, never by a user name
	for _, ident := range []string{"owner@example.com", "Juniper"} {
		twin := model.User{Sub: "sub-t", PreferredUsername: model.P(ident), Email: model.P("twin@example.com"), EmailVerified: true}
		e.signIn(twin)
		if u, err := e.s.Access.Load().Find(ident); err == nil {
			t.Errorf("Find(%q) found %+v by its user name", ident, u)
		}
	}
	// the source: the only readers of the user name are the code that shows it
	root := filepath.Join("..", "..")
	re := regexp.MustCompile(`\.Username\b|PreferredUsername|preferred_username`)
	allowed := map[string]bool{
		"internal/access/people.go": true, "internal/access/store.go": true, "internal/model/model.go": true, "internal/audit/audit.go": true,
		"internal/web/people.go": true, "internal/web/policy.go": true, "internal/web/audit.go": true, "internal/web/auth.go": true, "internal/web/grant.go": true,
		"internal/web/api_sessions.go": true, "internal/web/api_wakeupdate.go": true, "internal/web/testmode.go": true,
		"internal/demo/demo.go": true, "internal/testutil/issuer.go": true,
		"internal/web/docs.go": true, // the description of the API names the claim
		// not an account's: the operating system's user of the command line, and the owner of a process
		"internal/admin/format.go": true, "internal/poller/httpbackend.go": true,
	}
	_ = filepath.WalkDir(filepath.Join(root, "internal"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, root+string(filepath.Separator)))
		b, _ := os.ReadFile(p)
		if re.Match(b) && !allowed[rel] {
			t.Errorf("%s reads the user name: is it for showing only? (add it to this test's list if so)", rel)
		}
		return nil
	})
	for _, f := range []string{"internal/web/policy.go", "internal/web/grant.go", "internal/web/guards.go", "internal/access/access.go"} {
		src := string(must(os.ReadFile(filepath.Join(root, f))))
		for _, fn := range []string{"func emailIn(", "func isAllowed(", "func emailAllowed(", "func canControl(", "func (s *Server) bootstrapRole(", "func (s *Server) userGrant(", "func (s *Server) tokenGrant("} {
			i := strings.Index(src, fn)
			if i < 0 {
				continue
			}
			body := src[i:]
			if j := strings.Index(body, "\n}\n"); j > 0 {
				body = body[:j]
			}
			// (userGrant hands the session's user name to FillUsername, which decides nothing: the grant is rec.Grant())
			if fn != "func (s *Server) userGrant(" && re.MatchString(body) {
				t.Errorf("%s: %s reads the user name", f, fn)
			}
		}
	}
}

// Add host: the answer with the join code and the answer about its progress name who made it by the user name.
// What Add host itself keeps is the audit log's text, as before (the entries the pairing writes later have it as
// their actor); the audit entry of the request has the address, the subject and the user name.
func TestAddHostAnswersNameByUsername(t *testing.T) {
	e := setup(t)
	e.s.control = newRateLimiter(100)
	e.s.Enroll.PanelAddr = "100.64.0.10"
	e.s.Enroll.Pair = func(ctx context.Context, url string, _ []byte) (int, enroll.Response, string, error) {
		return 0, enroll.Response{}, "", errors.New("dial tcp: connection refused") // nobody answers: the code stays waiting
	}
	hdr := []string{"Cookie", e.cookie(controller(), 0), "X-Requested-With", "fairy", "Content-Type", "application/json"}
	p := e.do("POST", "/api/admin/hosts", map[string]any{"name": "Garage", "os": "linux"}, hdr...)
	if p.code != 201 || p.body["created_by"] != "al-ctl" || strings.Contains(string(p.raw), "n@example.com") || strings.Contains(string(p.raw), "Alice") {
		t.Fatalf("Add host: %d %s", p.code, p.raw)
	}
	g := e.do("GET", "/api/admin/enrollments/"+p.body["id"].(string), nil, hdr[:2]...)
	if g.code != 200 || g.body["created_by"] != "al-ctl" || strings.Contains(string(g.raw), "n@example.com") || strings.Contains(string(g.raw), "Alice") {
		t.Fatalf("its progress: %d %s", g.code, g.raw)
	}
	if en, ok := e.s.Enroll.Get(p.body["id"].(string)); !ok || en.CreatedBy != "n@example.com"+unverifiedMark {
		t.Errorf("what Add host keeps: %q", en.CreatedBy)
	}
	if au := auditOf(e, "host.add"); len(au) != 1 || au[0]["actor"] != "n@example.com"+unverifiedMark || au[0]["sub"] != "u2" || au[0]["username"] != "al-ctl" {
		t.Errorf("the audit entry: %v", au)
	}
}

// The Approvals page: who may approve on a machine is named by the user name (the id next to it is what the
// machine goes by), and who put a machine's id on record too, also for a record an older version wrote with
// the address. Neither the address nor the name is in the answer.
func TestApprovalsPageNamesByUsername(t *testing.T) {
	a := newApprovalEnv(t) // its admin: u2, "al-ctl", n@example.com (not verified), Alice
	get := func() (string, map[string]map[string]any) {
		t.Helper()
		r := a.do("GET", "/api/admin/approval", nil, "Cookie", a.admin)
		if r.code != 200 {
			t.Fatal(r.code, string(r.raw))
		}
		ms := map[string]map[string]any{}
		for _, x := range r.body["machines"].([]any) {
			m := x.(map[string]any)
			ms[m["id"].(string)] = m
		}
		return string(r.raw), ms
	}
	raw, ms := get()
	us, _ := ms["laptop"]["users"].([]any)
	if len(us) != 1 || us[0].(map[string]any)["name"] != "al-ctl" || us[0].(map[string]any)["sub"] != "u2" {
		t.Fatalf("who may approve on the laptop: %v", us)
	}
	if strings.Contains(raw, "n@example.com") || strings.Contains(raw, "Alice") {
		t.Errorf("the page holds the address or the name:\n%s", clipText(raw, 1500))
	}
	// on record by the admin, as an older version wrote it down, and by somebody the panel does not know
	a.setTrust(t, a.laptop, a.tower)
	a.poll()
	if what, _, err := a.accept("laptop", "n@example.com"+unverifiedMark); what != "id" || err != nil {
		t.Fatal(what, err)
	}
	if what, _, err := a.accept("tower", "gone@example.test"); what != "id" || err != nil {
		t.Fatal(what, err)
	}
	if b := a.s.Poller.BindingOf("laptop"); b.Record.By != "n@example.com"+unverifiedMark {
		t.Fatalf("the record keeps what it was given: %q", b.Record.By)
	}
	raw, ms = get()
	if ms["laptop"]["pinned_by"] != "al-ctl" || ms["tower"]["pinned_by"] != "(address hidden)" {
		t.Errorf("who put the ids on record: %v, %v", ms["laptop"]["pinned_by"], ms["tower"]["pinned_by"])
	}
	if strings.Contains(raw, "n@example.com") || strings.Contains(raw, "gone@example.test") || strings.Contains(raw, "Alice") {
		t.Errorf("the page holds an address or the name:\n%s", clipText(raw, 1500))
	}
	// the notification of the accept: the sentence has the user name (and hides the address of nobody known)
	notes := a.do("GET", "/api/notifications?limit=50", nil, "Cookie", a.admin)
	if n := string(notes.raw); !strings.Contains(n, "its machine id was accepted by al-ctl") || !strings.Contains(n, "its machine id was accepted by (address hidden)") || strings.Contains(n, "@example") {
		t.Errorf("the notifications: %s", n)
	}
}

// Re-pair: its answer names who made the code by the user name, as Add host's does.
func TestRepairAnswerNamesByUsername(t *testing.T) {
	b := hedBackend()
	b.Metrics["loft"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["hostname"] = "loft"
		delete(m, "power_sources")
		return m, nil
	}
	r := hedStart(t, hedConfig(t), b)
	r.s.Enroll.Scripts, r.s.Enroll.PanelAddr, r.s.Enroll.Interval = testInstallers, "100.64.0.10", 10*time.Millisecond
	r.s.Enroll.Pair = func(ctx context.Context, url string, _ []byte) (int, enroll.Response, string, error) {
		return 200, enroll.Response{AgentVersion: "42-abc", Platform: "linux", Token: strings.Repeat("r", 32)}, "", nil
	}
	p := r.addHost(map[string]any{"name": "Loft", "os": "linux"})
	if p.code != 201 || p.body["created_by"] != "al-ctl" {
		t.Fatalf("Add host Loft: %d %s", p.code, p.raw)
	}
	if e := r.enrollment(p.body["id"].(string)); e["state"] != "online" || e["created_by"] != "al-ctl" {
		t.Fatalf("Loft: %v %v, made by %v", e["state"], e["message"], e["created_by"])
	}
	// the hosts file has what Add host kept, and the list shows the user name
	if h, ok := r.s.managedHost("loft"); !ok || h.AddedBy != "n@example.com"+unverifiedMark {
		t.Errorf("added_by in the hosts file: %q", h.AddedBy)
	}
	rp := r.do("POST", "/api/admin/hosts/loft/repair", map[string]any{"confirm": "Loft"}, append([]string{"Content-Type", "application/json"}, r.hdr...)...)
	if rp.code != 201 || rp.body["created_by"] != "al-ctl" || strings.Contains(string(rp.raw), "n@example.com") || strings.Contains(string(rp.raw), "Alice") {
		t.Fatalf("Re-pair: %d %s", rp.code, rp.raw)
	}
	// ... also in the list of the codes that wait (the Re-pair's)
	if g := r.do("GET", "/api/admin/hosts", nil, r.hdr[:2]...); !strings.Contains(string(g.raw), `"added_by":"al-ctl"`) || !strings.Contains(string(g.raw), `"created_by":"al-ctl"`) || strings.Contains(string(g.raw), "n@example.com") {
		i := strings.Index(string(g.raw), "n@example.com")
		t.Errorf("the list of added hosts (at %d): %s", i, clipText(string(g.raw)[max(i-300, 0):], 700))
	}
	// the Re-pair is let finish, so nothing of it is still at work when the test's folder goes
	if e := r.enrollment(rp.body["id"].(string)); e["created_by"] != "al-ctl" {
		t.Errorf("the Re-pair when it is done: made by %v (%v, %v)", e["created_by"], e["state"], e["message"])
	}
}

// Only the audit log names a person by the address. actorOf, which makes that text, is called where an audit
// entry is written (and for what Add host and a run keep for the entries they write later), and nowhere
// else: every other place names a person through who and whoShown. A new sentence, record or request that is
// written with actorOf fails here.
func TestOnlyTheAuditLogUsesTheAddress(t *testing.T) {
	allowed := []string{"auditAnon(", "audit.Entry{", "allowDenied(", "ByActor: actorOf(u)", "s.Enroll.Create(", "func actorOf(", "actorOf(sess.User), &sess.User.Sub, nil)",
		`"another browser or address than the one that asked for it", actorOf(&u), &u.Sub, nil)`}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) < 50 {
		t.Fatal(err, len(files))
	}
	calls := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "people.go" {
			continue // people.go: who and whoShown fall back to it for an account without a user name, and hide it
		}
		for i, line := range strings.Split(string(must(os.ReadFile(f))), "\n") {
			if !strings.Contains(line, "actorOf(") || strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			calls++
			ok := false
			for _, a := range allowed {
				ok = ok || strings.Contains(line, a)
			}
			if !ok {
				t.Errorf("%s:%d names a person by the audit log's text outside the audit log: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
	if calls < 25 {
		t.Fatalf("only %d calls of actorOf found: the pattern no longer matches", calls)
	}
	// the same for the accounts' own labels (the address, else the name): the audit log's targets, the store and
	// the command line use them; an answer does not
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		for i, line := range strings.Split(string(must(os.ReadFile(f))), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if (strings.Contains(line, ".Label()") || strings.Contains(line, "ownerLabel(")) && !strings.Contains(line, "c.audit(") && !strings.Contains(line, "func ownerLabel(") &&
				!strings.Contains(line, "target := ownerLabel(") && !strings.Contains(line, "p.Shown(ownerLabel(x))") && !strings.Contains(line, "strings.ToLower(ownerLabel(") {
				t.Errorf("%s:%d uses an account's address or name outside the audit log: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// A notification that is made while a page is open goes to it over the stream (the pop-ups). A sentence made
// from what an older version stored names its person by the address: on the way out it is the user name, for
// an admin and for a viewer alike, and an address nobody has is the words that stand for it.
func TestStreamNotesNameByUsername(t *testing.T) {
	r := newNamesRig(t, true)
	e := r.env
	bodies := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, sub := range []string{"sub-n", "sub-v"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, body := e.streamBody("/api/stream", "Cookie", r.ck[sub])
			mu.Lock()
			bodies[sub] = body
			mu.Unlock()
		}()
	}
	time.Sleep(300 * time.Millisecond) // the streams are open
	e.s.Notes.Add(notifications.Event{Kind: "plug_switched", Type: "power", Severity: notifications.Warning, SystemID: "tower", Host: "TOWER",
		Text: "Lamp (Desk bar) switched off by owner@example.com"})
	e.s.Notes.Add(notifications.Event{Kind: "helper_stopped", Type: "host", Severity: notifications.Warning, SystemID: "tower", Host: "TOWER",
		Text: "elevated helper stopped by gone@example.com (unverified) (fans on BMC automatic control)"})
	wg.Wait()
	for sub, body := range bodies {
		i := strings.Index(body, `"events":[{`)
		if i < 0 {
			t.Fatalf("%s: the stream has no new notifications: %s", sub, clipText(body, 300))
		}
		notes := body[i:]
		if !strings.Contains(notes, "switched off by Juniper") || !strings.Contains(notes, "stopped by (address hidden) (fans") {
			t.Errorf("%s: the stream's notifications do not name by the user name: %s", sub, clipText(notes, 500))
		}
		if held := namesHeld(notes); len(held) > 0 || strings.Contains(notes, "unverified") {
			t.Errorf("%s: the stream's notifications hold %q: %s", sub, held, clipText(notes, 500))
		}
	}
	if len(bodies) != 2 {
		t.Fatalf("%d streams", len(bodies))
	}
}
