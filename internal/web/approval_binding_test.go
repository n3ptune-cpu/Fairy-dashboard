package web

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// An approval is for one machine (audit 5, r1-x1-01): the panel's half. The machines are testutil.Machine
// (Fairy-agent's real gate); the same cases against the real agent handler are in internal/agentcontract.

// entryOf is the entry of the last request to path that carried an approval: its machine id and how many
// entries the list has.
func entryOf(t *testing.T, m *testutil.Machine, path string) (machineID string, entries int) {
	t.Helper()
	asked := m.Asked()
	for i := len(asked) - 1; i >= 0; i-- {
		ap, _ := asked[i].Body["approval"].(map[string]any)
		if asked[i].Path != path || ap == nil {
			continue
		}
		list, _ := ap["entries"].([]any)
		for _, e := range list {
			machineID, _ = e.(map[string]any)["machine_id"].(string)
		}
		return machineID, len(list)
	}
	t.Fatalf("%s was sent no %s with an approval: %+v", m.ID, path, asked)
	return "", 0
}

// trustID is the machine id in a machine's trust file.
func trustID(t *testing.T, m *testutil.Machine) string {
	t.Helper()
	raw, err := os.ReadFile(m.TrustPath)
	if err != nil {
		t.Fatal(err)
	}
	id := jsonx.S(mustJSON(t, raw), "machine_id")
	if api.CheckMachineID(id) != nil {
		t.Fatalf("%s: the trust file has no machine id: %s", m.ID, raw)
	}
	return id
}

func mustJSON(t *testing.T, raw []byte) jsonx.M {
	t.Helper()
	m := jsonx.M{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Turn on: the panel makes the machine's id up itself, before the machine is asked anything; it is in the
// request for the trust and in the entry the passkey signs; it comes on record when the machine answers 200,
// with who did it; and every later approval for that host is made with exactly that id.
func TestTurnOnTakesTheMachineIDOnRecord(t *testing.T) {
	a := newApprovalEnv(t)
	if b := a.s.Poller.BindingOf("laptop"); b.Known || b.Checks() {
		t.Fatalf("before Turn on nothing is on record: %+v", b)
	}
	r := a.turnOn("laptop")
	id, _, ok := approvalAsked(r)
	if !ok {
		t.Fatalf("Turn on: %d %s", r.code, r.raw)
	}
	if b := a.s.Poller.BindingOf("laptop"); b.Known {
		t.Fatalf("an id came on record before the machine took the trust: %+v", b)
	}
	if cb := a.approve(t, id); cb.code != 303 || a.result(t)["ok"] != true {
		t.Fatalf("the approved Turn on: %d", cb.code)
	}
	last := a.laptop.Asked()[len(a.laptop.Asked())-1]
	sent, _ := last.Body["machine_id"].(string)
	inEntry, n := entryOf(t, a.laptop, api.PathApprovalTrust)
	if last.Path != api.PathApprovalTrust || api.CheckMachineID(sent) != nil || len(sent) != 22 || inEntry != sent || n != 1 || trustID(t, a.laptop) != sent {
		t.Fatalf("the id in the request %q, in the entry %q, in the machine's trust file %q", sent, inEntry, trustID(t, a.laptop))
	}
	b := a.s.Poller.BindingOf("laptop")
	if !b.OK() || !b.Checks() || b.Record.MachineID != sent || b.Record.How != approval.PinTurnOn || !strings.HasPrefix(b.Record.By, "n@example.com") || b.Record.State != api.ApprovalRequired {
		t.Fatalf("on record after Turn on (before any poll): %+v", b)
	}
	// it is in the file, and a panel that starts again on that folder has it
	recs, err := adminstate.New(a.s.Cfg.DataDir).Machines()
	if err != nil || recs["laptop"].MachineID != sent || recs["laptop"].How != approval.PinTurnOn {
		t.Fatalf("machines.json: %v %+v", err, recs)
	}
	p2, err := poller.New(a.s.Cfg, a.s.Secrets, a.gb)
	if err != nil {
		t.Fatal(err)
	}
	if b := p2.BindingOf("laptop"); b.Record.MachineID != sent || !b.Checks() {
		t.Fatalf("after a restart, before any poll: %+v", b)
	}
	// no alarm from the moment between the 200 and the next poll (the last answer still said "off")
	if got := auditOutcomes(a.s.Cfg.AuditLog); strings.Contains(got, "approval.alarm") {
		t.Errorf("a false alarm right after Turn on:\n%s", got)
	}
	// the audit entry of the Turn on names the id in its short form only
	if entries := a.auditEntries("", "approval.turn_on"); len(entries) != 1 || entries[0]["params"].(map[string]any)["machine_id"] != approval.ShortID(sent) {
		t.Errorf("audit: %v", entries)
	}
	// and the id that came on record has its own entry: which id, for which host, how, and by whom (audit 5, r2-b2-09)
	if en := a.auditEntries("", "approval.machine_id"); len(en) != 1 || en[0]["target"] != "laptop" || en[0]["outcome"] != "on record" || en[0]["sub"] != approvalAdminSub ||
		fmt.Sprint(en[0]["params"]) != fmt.Sprint(map[string]any{"how": approval.PinTurnOn, "machine_id": approval.ShortID(sent)}) || strings.Contains(fmt.Sprint(en), sent) {
		t.Errorf("the entry for the id that came on record at Turn on: %v", en)
	}
	a.poll()
	if b := a.s.Poller.BindingOf("laptop"); !b.OK() || b.Report.MachineID != sent || b.Record.How != approval.PinTurnOn {
		t.Fatalf("after the next poll: %+v", b)
	}
	// a shutdown: its entry carries the id on record
	r = a.post("/api/systems/laptop/power", a.shutdownBody())
	id, _, ok = approvalAsked(r)
	if !ok {
		t.Fatalf("shutdown: %d %s", r.code, r.raw)
	}
	if cb := a.approve(t, id); cb.code != 303 || a.result(t)["ok"] != true {
		t.Fatalf("the approved shutdown: %d %v", cb.code, a.result(t))
	}
	if got, n := entryOf(t, a.laptop, "/power"); got != sent || n != 1 {
		t.Fatalf("the shutdown's entry names %q, on record is %q", got, sent)
	}
}

// First sight: a host that already has the check when the panel first hears from it gets the id it shows on
// record, once, with an audit entry. What it shows later does not change the record.
func TestFirstSightTakesTheIDOnRecordAndIsAudited(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop)
	want := machineIDOf("laptop")
	b := a.s.Poller.BindingOf("laptop")
	if !b.OK() || b.Record.MachineID != want || b.Record.How != approval.PinFirstSight || b.Record.By != "Fairy" || b.Record.At.IsZero() {
		t.Fatalf("%+v", b)
	}
	entries := a.auditEntries("", "approval.machine_id")
	if len(entries) != 1 || entries[0]["target"] != "laptop" || entries[0]["outcome"] != "on record" || entries[0]["actor"] != "Fairy" ||
		entries[0]["params"].(map[string]any)["how"] != approval.PinFirstSight || entries[0]["params"].(map[string]any)["machine_id"] != approval.ShortID(want) {
		t.Fatalf("audit: %v", entries)
	}
	raw, _ := os.ReadFile(a.s.Cfg.AuditLog)
	if strings.Contains(string(raw), want) {
		t.Error("the whole machine id is in the audit log")
	}
	for i := 0; i < 3; i++ {
		a.poll()
	}
	if n := len(a.auditEntries("", "approval.machine_id")); n != 1 {
		t.Errorf("%d entries for one first sight", n)
	}
	// "tower" has no check: nothing is on record for it, and no file entry
	recs, err := adminstate.New(a.s.Cfg.DataDir).Machines()
	if _, there := recs["tower"]; err != nil || there || len(recs) != 1 {
		t.Fatalf("machines.json: %v %+v", err, recs)
	}
}

// The relay (the auditor's case, r1-x1-01): "laptop" answers the question about an approval with ANOTHER
// machine's challenge ("tower") and hands the approved request on to that machine. The panel signs laptop's id
// on record, tower checks with its own, and refuses. Nothing passes tower's gate; the panel shows and records
// a refusal for laptop.
func TestARelayedChallengeIsRefusedByTheOtherMachine(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop, a.tower)
	relayed := 0
	a.gb.Relay("laptop", func(path string, body any) (int, jsonx.M) {
		switch path {
		case api.PathApprovalChallenge, "/power":
			relayed++
			return a.gb.Forward("tower", path, body)
		}
		return 0, nil
	})
	r := a.post("/api/systems/laptop/power", a.shutdownBody())
	id, machines, ok := approvalAsked(r)
	if !ok || len(machines) != 1 || machines[0] != "laptop" {
		t.Fatalf("the dialog (it names the host the admin chose): %d %s", r.code, r.raw)
	}
	if cb := a.approve(t, id); cb.code != 303 {
		t.Fatalf("callback: %d", cb.code)
	}
	res := a.result(t)
	if relayed != 2 || len(a.tower.Passed()) != 0 || len(a.laptop.Passed()) != 0 {
		t.Fatalf("relayed %d requests; passed tower's gate: %+v; laptop's: %+v", relayed, a.tower.Passed(), a.laptop.Passed())
	}
	msg := fmt.Sprint(res["message"])
	if res["ok"] == true || !strings.Contains(msg, "did not accept the approval") || !strings.Contains(msg, "not the machine Fairy has on record") {
		t.Fatalf("the result shown for laptop: %v", res)
	}
	// what was signed: laptop's id on record, never tower's
	asked := a.tower.Asked()
	last := asked[len(asked)-1]
	entry := last.Body["approval"].(map[string]any)["entries"].([]any)[0].(map[string]any)
	if last.Path != "/power" || entry["machine_id"] != machineIDOf("laptop") {
		t.Fatalf("the entry tower was handed: %v", entry)
	}
	if got := auditOutcomes(a.s.Cfg.AuditLog); !strings.Contains(got, "power.shutdown error 400: laptop did not accept the approval") ||
		!strings.Contains(got, "not the machine Fairy has on record") {
		t.Errorf("audit:\n%s", got)
	}
	// and tower itself still works, with its own approval
	a.gb.Relay("laptop", nil)
	r = a.post("/api/systems/tower/fan/mode", map[string]any{"mode": "full"})
	if id, _, ok = approvalAsked(r); !ok {
		t.Fatalf("tower's own fan change: %d %s", r.code, r.raw)
	}
	if cb := a.approve(t, id); cb.code != 303 || a.result(t)["ok"] != true || len(a.tower.Passed()) != 1 {
		t.Fatalf("tower's own approved fan change: %v", a.tower.Passed())
	}
}

// Who learns a machine id: an admin in a browser, on the approval page. Nobody else: not a viewer, not an API
// token (read or control, also of an admin), and no answer of the panel's ordinary API carries one.
func TestMachineIDsAreForAdminsInABrowserOnly(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop, a.tower)
	ids := []string{machineIDOf("laptop"), machineIDOf("tower")}
	has := func(raw []byte) bool {
		return strings.Contains(string(raw), ids[0]) || strings.Contains(string(raw), ids[1])
	}
	u := viewer
	u.Sub = "viewer-9"
	v := a.cookie(u, 0)
	if err := a.s.Access.CreateSite("Den"); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"laptop", "tower"} {
		if _, err := a.s.Access.Assign(h, "Den"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := a.s.Access.SetRole(u.Sub, "viewer", []string{"Den"}, "test", true); err != nil {
		t.Fatal(err)
	}
	read, _, err := a.s.Tokens.Create("ha", approvalAdminSub, "read", "cli:test", 0)
	if err != nil {
		t.Fatal(err)
	}
	ctl, _, err := a.s.Tokens.Create("ops", approvalAdminSub, "control", "cli:test", 0)
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{"/api/systems", "/api/systems/laptop", "/api/systems/tower", "/api/me", "/api/summary", "/api/notifications", "/api/admin/approval",
		"/api/admin/updates", "/api/admin/wake-update", "/api/audit", "/api/sources"}
	for who, hdr := range map[string][]string{"a viewer": {"Cookie", v}, "a read token": {"Authorization", "Bearer " + read},
		"a control token of an admin": {"Authorization", "Bearer " + ctl}, "nobody": nil} {
		for _, p := range paths {
			if r := a.do("GET", p, nil, hdr...); has(r.raw) {
				t.Errorf("%s gets a machine id from GET %s (%d)", who, p, r.code)
			}
		}
	}
	// the admin in a browser: only the approval page
	for _, p := range paths {
		r := a.do("GET", p, nil, "Cookie", a.admin)
		if has(r.raw) != (p == "/api/admin/approval") {
			t.Errorf("an admin's GET %s: machine id in it: %v", p, has(r.raw))
		}
	}
	_, ms := a.page(t)
	if ms["laptop"]["machine_id"] != ids[0] || ms["laptop"]["shown_id"] != ids[0] || ms["laptop"]["pinned"] != approval.PinFirstSight || ms["laptop"]["pinned_by"] != "Fairy" {
		t.Fatalf("the approval page for an admin: %v", ms["laptop"])
	}
	// the page's stream and the system's own entry carry the decision, never an id
	x := a.do("GET", "/api/systems/laptop", nil, "Cookie", v)
	if ap, _ := x.body["approval"].(map[string]any); x.code != 200 || ap["state"] != "required" || ap["machine_id"] != nil || ap["binding"] != nil {
		t.Fatalf("a viewer's view of the block: %d %v", x.code, x.body["approval"])
	}
	a.s.Poller.PollOnce(context.Background())
}
