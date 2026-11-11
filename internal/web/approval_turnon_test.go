package web

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// "Turn on": switching the machines' own check on. The machines are testutil.Machine (Fairy-agent's real
// gate): what they accept here is what a real agent accepts.

func (a *approvalEnv) page(t *testing.T) (map[string]any, map[string]map[string]any) {
	t.Helper()
	r := a.do("GET", "/api/admin/approval", nil, "Cookie", a.admin)
	if r.code != 200 {
		t.Fatalf("approval page: %d %s", r.code, r.raw)
	}
	ms := map[string]map[string]any{}
	for _, m := range r.body["machines"].([]any) {
		ms[m.(map[string]any)["id"].(string)] = m.(map[string]any)
	}
	return r.body, ms
}

func (a *approvalEnv) turnOn(hosts ...string) resp {
	return a.post("/api/admin/approval/turn-on", map[string]any{"hosts": hosts})
}

func TestTurnOn(t *testing.T) {
	a := newApprovalEnv(t)
	logs := captureLog(t)
	// the page: both machines are ready, nothing is on, and each would get the admin as its one approver
	page, ms := a.page(t)
	if page["configured"] != true || page["ready"] != 2.0 || page["required"] != 0.0 || page["client"] != "Fairy PC actions" ||
		page["callback"] != "https://panel.example.com/auth/approval/callback" || len(page["keys"].([]any)) != 1 {
		t.Fatalf("page: %v", page)
	}
	for _, id := range []string{"laptop", "tower"} {
		us := ms[id]["users"].([]any)
		if ms[id]["ready"] != true || ms[id]["state"] != "off" || len(us) != 1 || us[0].(map[string]any)["sub"] != approvalAdminSub || us[0].(map[string]any)["why"] != "admin" {
			t.Fatalf("%s: %v", id, ms[id])
		}
	}
	// the panel sends nothing by itself: the machines were asked nothing so far
	if len(a.laptop.Asked())+len(a.tower.Asked()) != 0 {
		t.Fatal("a machine was asked something before Turn on was pressed")
	}
	// Turn on for both: one dialog that shows what each machine will trust
	r := a.turnOn("laptop", "tower")
	id, machines, ok := approvalAsked(r)
	if !ok || len(machines) != 2 {
		t.Fatalf("the dialog: %d %s", r.code, r.raw)
	}
	d := r.body["detail"].(map[string]any)
	items := fmt.Sprint(d["items"])
	key := page["keys"].([]any)[0].(map[string]any)
	for _, want := range []string{"Pocket ID: " + a.iss.URL(), "client " + testApprovalClient, "thumbprint " + key["thumbprint"].(string),
		"laptop (checked by its agent): may approve: Alice [u2, admin]", "tower (checked by its agent): may approve: Alice [u2, admin]", "locked"} {
		if !strings.Contains(items, want) {
			t.Errorf("the dialog lacks %q:\n%s", want, items)
		}
	}
	if d["action"] != "switch on the check of approvals on the machine itself" {
		t.Errorf("action: %v", d["action"])
	}
	// each machine was asked for a challenge for the hash of its proposal, and nothing was written yet
	for _, m := range []*testutil.Machine{a.laptop, a.tower} {
		asked := m.Asked()
		if len(asked) != 1 || asked[0].Path != api.PathApprovalChallenge || asked[0].Body["verb"] != "trust_set" || len(fmt.Sprint(asked[0].Body["trust_hash"])) != 43 {
			t.Fatalf("%s was asked: %+v", m.ID, asked)
		}
		if _, err := os.Stat(m.TrustPath); err == nil {
			t.Fatalf("%s has a trust file before the approval", m.ID)
		}
	}
	// one passkey for both
	if cb := a.approve(t, id); cb.code != 303 {
		t.Fatalf("callback: %d %s", cb.code, cb.raw)
	}
	if len(a.iss.Tokens()) != 1 {
		t.Fatalf("%d approvals were asked for", len(a.iss.Tokens()))
	}
	pr := a.result(t)
	if pr["ok"] != true || !strings.Contains(fmt.Sprint(pr["message"]), "2 of 2 machines now check approvals themselves") {
		t.Fatalf("result: %v", pr)
	}
	a.poll()
	for _, m := range []*testutil.Machine{a.laptop, a.tower} {
		ap := a.s.Poller.ApprovalOf(m.ID)
		if ap == nil || ap.State != api.ApprovalRequired || ap.Issuer != a.iss.URL() || ap.ClientID != testApprovalClient || ap.Users != 1 ||
			len(ap.Keys) != 1 || ap.Keys[0].Thumbprint != key["thumbprint"] || ap.Keys[0].Kid != a.iss.Kid {
			t.Fatalf("%s after Turn on: %+v", m.ID, ap)
		}
		// what was sent is the proposal whose hash the machine gave its challenge for, with the same list for both
		last := m.Asked()[len(m.Asked())-1]
		if last.Path != api.PathApprovalTrust || len(last.Body["approval"].(map[string]any)["entries"].([]any)) != 2 ||
			fmt.Sprint(last.Body["users"]) != "["+approvalAdminSub+"]" {
			t.Fatalf("%s was sent: %s %v", m.ID, last.Path, last.Body["users"])
		}
	}
	// the page: both on, the report of this Turn on, nothing ready
	page, ms = a.page(t)
	last := page["last"].(map[string]any)["results"].([]any)
	if page["ready"] != 0.0 || page["required"] != 2.0 || len(last) != 2 || last[0].(map[string]any)["outcome"] != "accepted" ||
		ms["laptop"]["ready"] != false || !strings.Contains(fmt.Sprint(ms["laptop"]["why"]), "changed only on the machine") || ms["laptop"]["stale_keys"] != nil {
		t.Fatalf("page after Turn on: %v %v", page, ms["laptop"])
	}
	audit, _ := os.ReadFile(a.s.Cfg.AuditLog)
	if strings.Count(string(audit), `"action":"approval.turn_on"`) != 2 || !strings.Contains(string(audit), `"result":"accepted"`) ||
		!strings.Contains(string(audit), `"users":["`+approvalAdminSub+`"]`) || !strings.Contains(string(audit), key["thumbprint"].(string)) {
		t.Errorf("audit:\n%s", audit)
	}
	a.noLeak(t, "the log", logs.take())
	a.noLeak(t, "the audit log", string(audit))
	a.noLeak(t, "the page", fmt.Sprint(page)+fmt.Sprint(pr)+string(r.raw))

	// Locked: a second Turn on is refused by the panel and by the machine, and an approved action now works.
	r = a.turnOn("laptop")
	if msg := fmt.Sprint(r.body["detail"]); r.code != 409 || !strings.Contains(msg, "laptop: already set") {
		t.Fatalf("a second Turn on: %d %s", r.code, r.raw)
	}
	id, _, ok = approvalAsked(a.post("/api/systems/laptop/power", a.shutdownBody()))
	if !ok {
		t.Fatal("after Turn on a shutdown must ask for the machine's approval")
	}
	if cb := a.approve(t, id); cb.code != 303 || a.result(t)["ok"] != true {
		t.Fatal("an approved shutdown after Turn on")
	}
	if p := a.laptop.Passed(); len(p) != 1 || p[0].Path != "/power" || !p[0].Approved {
		t.Fatalf("passed: %+v", p)
	}
}

// The panel has no request that changes or removes a machine's trust: the only routes about it are the page
// and Turn on, and Turn on never reaches a machine that is already on.
func TestNoRemoteChangeOfTrust(t *testing.T) {
	var got []string
	for _, r := range registeredRoutes(t) {
		if strings.Contains(r, "approval") {
			got = append(got, r)
		}
	}
	want := "GET /api/admin/approval|GET /auth/approval/callback|POST /api/admin/approval/turn-on"
	if strings.Join(got, "|") != want {
		t.Fatalf("routes about approvals: %v, want %s", got, want)
	}
	a := newApprovalEnv(t)
	a.require(t, a.laptop)
	before, _ := os.ReadFile(a.laptop.TrustPath)
	n := len(a.laptop.Asked())
	if r := a.turnOn("laptop"); r.code != 409 {
		t.Fatalf("Turn on for a machine that is on: %d %s", r.code, r.raw)
	}
	after, _ := os.ReadFile(a.laptop.TrustPath)
	if string(before) != string(after) || len(a.laptop.Asked()) != n || len(a.iss.Asked()) != 0 {
		t.Fatal("a machine that is already on was asked or changed")
	}
}

// One machine refuses the proof (its clock is ten minutes off), one accepts, one does not answer: each result
// is reported for itself, and the machine that refused is still open.
func TestTurnOnResultsPerMachine(t *testing.T) {
	a := newApprovalEnv(t)
	off := time.Now().Add(10 * time.Minute)
	a.tower.SetClock(func() time.Time { return off }, time.Hour)
	r := a.turnOn("laptop", "tower")
	id, _, ok := approvalAsked(r)
	if !ok {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	if cb := a.approve(t, id); cb.code != 303 {
		t.Fatal(cb.code)
	}
	pr := a.result(t)
	if pr["ok"] != true || !strings.Contains(fmt.Sprint(pr["message"]), "1 of 2 machines") || !strings.Contains(fmt.Sprint(pr["message"]), "tower: proof refused") {
		t.Fatalf("result: %v", pr)
	}
	page, _ := a.page(t)
	res := map[string]map[string]any{}
	for _, x := range page["last"].(map[string]any)["results"].([]any) {
		res[x.(map[string]any)["id"].(string)] = x.(map[string]any)
	}
	if res["laptop"]["outcome"] != "accepted" || res["tower"]["outcome"] != "proof_refused" || !strings.Contains(fmt.Sprint(res["tower"]["text"]), "proof refused (") ||
		!strings.Contains(fmt.Sprint(res["tower"]["text"]), "still open") {
		t.Fatalf("results: %v", res)
	}
	if _, err := os.Stat(a.tower.TrustPath); err == nil {
		t.Fatal("the machine that refused the proof has a trust file")
	}
	if got := auditOutcomes(a.s.Cfg.AuditLog); !strings.Contains(got, "approval.turn_on ok") || !strings.Contains(got, "approval.turn_on error: proof_refused") {
		t.Errorf("audit:\n%s", got)
	}
	// its clock is right again: it can be switched on now (nothing was locked)
	a.tower.SetClock(time.Now, time.Hour)
	a.poll()
	id, _, ok = approvalAsked(a.turnOn("tower"))
	if !ok {
		t.Fatal("the machine that refused cannot be tried again")
	}
	if cb := a.approve(t, id); cb.code != 303 || a.result(t)["ok"] != true {
		t.Fatal("second try")
	}

	// a machine that does not answer the question, next to one that does
	b := newApprovalEnv(t)
	b.gb.Fail("tower", devices.Errf("timeout"))
	r = b.turnOn("laptop", "tower")
	id, machines, ok := approvalAsked(r)
	if !ok || len(machines) != 1 || !strings.Contains(fmt.Sprint(r.body["detail"].(map[string]any)["items"]), "tower: NOT switched on") {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	if cb := b.approve(t, id); cb.code != 303 {
		t.Fatal(cb.code)
	}
	if pr := b.result(t); !strings.Contains(fmt.Sprint(pr["message"]), "tower: offline") || !strings.Contains(fmt.Sprint(pr["message"]), "laptop: accepted") {
		t.Fatalf("result: %v", pr)
	}
	// nobody answers: said at once, nobody is asked for a passkey
	c := newApprovalEnv(t)
	c.gb.Fail("laptop", devices.Errf("timeout"))
	if r := c.turnOn("laptop"); r.code != 409 || !strings.Contains(fmt.Sprint(r.body["detail"]), "laptop: offline") || len(c.iss.Asked()) != 0 {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	if got := auditOutcomes(c.s.Cfg.AuditLog); !strings.Contains(got, "approval.turn_on error: offline") {
		t.Errorf("a Turn on that reached no machine is audited too:\n%s", got)
	}
}

// Who may switch it on: admins in a browser. Not a viewer, not an API token of either scope, not without the
// CSRF header or from another origin, not with the kill switch on; and the page is for admins.
func TestTurnOnIsForAdminsInABrowser(t *testing.T) {
	a := newApprovalEnv(t)
	body := map[string]any{"hosts": []string{"laptop"}}
	json := []string{"Content-Type", "application/json"}
	v := a.cookie(viewer, 0)
	for name, c := range map[string]struct {
		hdr  []string
		want int
	}{
		"not signed in":             {[]string{"X-Requested-With", "fairy"}, 401},
		"a viewer":                  {[]string{"Cookie", v, "X-Requested-With", "fairy"}, 403},
		"without the CSRF header":   {[]string{"Cookie", a.admin}, 403},
		"from another origin":       {[]string{"Cookie", a.admin, "X-Requested-With", "fairy", "Origin", "https://evil.example"}, 403},
		"Sec-Fetch-Site cross-site": {[]string{"Cookie", a.admin, "X-Requested-With", "fairy", "Sec-Fetch-Site", "cross-site"}, 403},
	} {
		if r := a.do("POST", "/api/admin/approval/turn-on", body, append(c.hdr, json...)...); r.code != c.want {
			t.Errorf("%s: %d %s", name, r.code, r.raw)
		}
	}
	for _, scope := range []string{"read", "control"} {
		tok, _, err := a.s.Tokens.Create("t-"+scope, approvalAdminSub, scope, "test", 0)
		if err != nil {
			t.Fatal(err)
		}
		if r := a.do("POST", "/api/admin/approval/turn-on", body, append([]string{"Authorization", "Bearer " + tok}, json...)...); r.code != 403 {
			t.Errorf("a %s token: %d %s", scope, r.code, r.raw)
		}
		if scope == "read" {
			continue
		}
	}
	_ = a.s.Poller.Admin.SetControlDisabled(true, "root (cli)", "maintenance")
	if r := a.turnOn("laptop"); r.code != 423 {
		t.Errorf("kill switch: %d", r.code)
	}
	_ = a.s.Poller.Admin.SetControlDisabled(false, "root", "")
	if len(a.laptop.Asked()) != 0 || len(a.iss.Asked()) != 0 {
		t.Fatal("a machine or Pocket ID was asked")
	}
	if r := a.do("GET", "/api/admin/approval", nil, "Cookie", v); r.code != 403 {
		t.Errorf("the page for a viewer: %d", r.code)
	}
	if r := a.do("GET", "/api/admin/approval", nil); r.code != 401 {
		t.Errorf("the page without a sign-in: %d", r.code)
	}
	for name, bad := range map[string]any{"no machine": map[string]any{"hosts": []string{}}, "twice": map[string]any{"hosts": []string{"laptop", "laptop"}},
		"an unknown field": map[string]any{"hosts": []string{"laptop"}, "users": []string{"me"}}, "an unknown machine": map[string]any{"hosts": []string{"nope"}}} {
		if r := a.post("/api/admin/approval/turn-on", bad); r.code != 422 && r.code != 400 {
			t.Errorf("%s: %d %s", name, r.code, r.raw)
		}
	}
}

// Who may approve on a machine: the panel's admins, the owner when the configuration says which Pocket ID
// user the owner label is, and approval.users. Never somebody whose display name matches a label.
func TestTurnOnUsers(t *testing.T) {
	a := newApprovalEnv(t)
	// "laptop" has the owner label of the test configuration's group; give it an owner label and a user of that name
	sneaky := viewer
	sneaky.Sub, sneaky.Name = "u-sneaky", model.P("Bob")
	a.cookie(sneaky, 0)
	ag, _ := a.s.Poller.AgentByID("laptop") // the same agent with an owner label, as config.yaml would have it
	ag.Owner = model.P("Bob")
	a.s.Poller.RemoveAgent("laptop")
	if err := a.s.Poller.AddAgent(ag, nil); err != nil {
		t.Fatal(err)
	}
	a.poll()
	_, ms := a.page(t)
	if us := ms["laptop"]["users"].([]any); len(us) != 1 {
		t.Fatalf("a user whose name is the owner label got onto the list: %v", us)
	}
	a.s.Cfg.Approval.Owners = map[string]string{"Bob": "pocket-bob"}
	a.s.Cfg.Approval.Users = []string{"extra-user", approvalAdminSub}
	_, ms = a.page(t)
	var subs, whys []string
	for _, u := range ms["laptop"]["users"].([]any) {
		subs, whys = append(subs, u.(map[string]any)["sub"].(string)), append(whys, u.(map[string]any)["why"].(string))
	}
	if strings.Join(subs, " ") != "u2 pocket-bob extra-user" || strings.Join(whys, "|") != "admin|owner (Bob)|approval.users" {
		t.Fatalf("laptop's users: %v %v", subs, whys)
	}
	if us := ms["tower"]["users"].([]any); len(us) != 2 {
		t.Fatalf("a machine with another owner got the owner: %v", us)
	}
	// the proposal that is sent is that list, in that order
	id, _, ok := approvalAsked(a.turnOn("laptop"))
	if !ok {
		t.Fatal("not asked")
	}
	if cb := a.approve(t, id); cb.code != 303 || a.result(t)["ok"] != true {
		t.Fatal("Turn on")
	}
	sent := a.laptop.Asked()[len(a.laptop.Asked())-1]
	if fmt.Sprint(sent.Body["users"]) != "[u2 pocket-bob extra-user]" {
		t.Fatalf("users sent: %v", sent.Body["users"])
	}
}

// A machine that pins a key Pocket ID no longer publishes is marked: its approvals will be refused there.
func TestStaleKeysAreMarked(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop)
	if _, ms := a.page(t); ms["laptop"]["stale_keys"] != nil {
		t.Fatal("the machine pins the current key")
	}
	other := testutil.NewIssuer(t)
	a.iss.Set(func(i *testutil.Issuer) { i.Key, i.Kid = other.Key, "rotated" })
	a.s.approvalKeys = issuerKeys{}
	if _, ms := a.page(t); ms["laptop"]["stale_keys"] != true {
		t.Fatalf("after a key rotation at Pocket ID: %v", ms["laptop"])
	}
}
