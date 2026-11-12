package web

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
)

// A machine id comes on record through Turn on or through an admin's accept, never because a host shows it
// (audit 5, round 3, r3-x1-01). The panel's half; the same against the real agent is in
// internal/agentcontract/notonrecord_test.go.

const notOnRecordText = " checks approvals itself, and its machine id is not on record: an admin accepts it in the Approval box on its page first."

// A host that already has the check when the panel first hears from it (set on the machine, or by another
// panel). It used to get the id it showed on record at once ("first sight"; this test replaces
// TestFirstSightTakesTheIDOnRecordAndIsAudited). Now nothing comes on record: the host is "not on record", is
// asked for no approval and sent nothing, and says so to everyone who sees it. One button, behind "Confirm
// it's you", takes exactly the id the page showed on record.
func TestAMachineThatShowsTheCheckIsNotOnRecordUntilAnAdminAccepts(t *testing.T) {
	x := newAcceptAdmin(t)
	x.setTrust(t, x.laptop) // on the machine: helper-trust set
	for i := 0; i < 3; i++ {
		x.poll()
	}
	want := machineIDOf("laptop")
	b := x.s.Poller.BindingOf("laptop")
	if b.Status != approval.BindNoRecord || b.Record.MachineID != "" || b.Record.How != "" || !b.Record.Unaccepted || !b.Checks() || b.Report.MachineID != want {
		t.Fatalf("laptop must be \"not on record\", with nothing on record: %+v", b)
	}
	// the file: the host is known to have the check, and no id is in it
	recs, err := adminstate.New(x.s.Cfg.DataDir).Machines()
	if _, tower := recs["tower"]; err != nil || tower || len(recs) != 1 || recs["laptop"].MachineID != "" || !recs["laptop"].Unaccepted || recs["laptop"].Alarm != approval.BindNoRecord {
		t.Fatalf("machines.json: %v %+v", err, recs)
	}
	if raw, _ := os.ReadFile(adminstate.MachinesPath(x.s.Cfg.DataDir)); strings.Contains(string(raw), want) {
		t.Fatalf("machines.json holds the id the host showed:\n%s", raw)
	}
	// the audit log: no id came on record; one alarm for three polls, with the id's first six characters only
	if n := len(x.auditEntries("", "approval.machine_id")); n != 0 {
		t.Fatalf("%d ids came on record", n)
	}
	al := x.auditEntries("", "approval.alarm")
	if len(al) != 1 || al[0]["target"] != "laptop" || al[0]["outcome"] != "alarm: no_record" || al[0]["params"].(map[string]any)["shown"] != approval.ShortID(want) {
		t.Fatalf("the alarm in the audit log: %v", al)
	}
	if raw, _ := os.ReadFile(x.s.Cfg.AuditLog); strings.Contains(string(raw), want) {
		t.Error("the whole machine id is in the audit log")
	}
	if active, all := x.alarms(); active != 1 || all != 1 {
		t.Fatalf("notifications: %d active of %d", active, all)
	}
	// everyone who sees the host sees the flag, never the id
	blk := x.do("GET", "/api/systems/laptop", nil, "Cookie", x.sess)
	if ap, _ := blk.body["approval"].(map[string]any); ap["binding"] != approval.BindNoRecord || ap["state"] != "required" || strings.Contains(string(blk.raw), want) {
		t.Fatalf("the block: %s", blk.raw)
	}
	// the approval page (an admin): the one button, what it would accept, and no Turn on
	_, ms := x.page(t)
	if m := ms["laptop"]; m["binding"] != approval.BindNoRecord || m["can_accept"] != true || m["accept"] != "first" || m["shown_id"] != want || m["machine_id"] != nil ||
		m["pinned"] != nil || m["held_by"] != nil || m["ready"] == true || fmt.Sprint(m["binding_text"]) != "laptop"+notOnRecordText {
		t.Fatalf("the approval page: %v", m)
	}
	// nothing is asked and nothing is sent, also inside the panel's own window (its confirmation would not even
	// be asked): never a request without an approval to a machine that requires one
	fresh := x.cookie(controller(), 0)
	x.admin, x.csrf[1] = fresh, fresh
	asked := len(x.laptop.Asked())
	x.stopped(t, "shutdown", x.post("/api/systems/laptop/power", x.shutdownBody()), "laptop"+notOnRecordText)
	x.stopped(t, "update", x.post("/api/admin/updates/laptop", map[string]any{}), "laptop"+notOnRecordText)
	if len(x.laptop.Asked()) != asked || len(x.laptop.Passed()) != 0 || len(x.iss.Asked()) != 0 {
		t.Fatalf("the machine was asked or sent something: %+v", x.laptop.Asked()[asked:])
	}
	// the accept is for exactly the id the page showed: another id, or "off", is refused before anybody confirms
	for _, body := range []any{map[string]any{"machine_id": otherID}, map[string]any{"off": true}} {
		if r := x.press("laptop", body); r.code != 409 || pendingID(r) != "" || !strings.Contains(fmt.Sprint(r.body["detail"]), "laptop: nothing was accepted. What it shows is not what the dialog showed") {
			t.Fatalf("an accept of %v: %d %s", body, r.code, r.raw)
		}
	}
	if b := x.s.Poller.BindingOf("laptop"); b.Status != approval.BindNoRecord || b.Record.MachineID != "" {
		t.Fatalf("after the refused accepts: %+v", b)
	}
	// the button: the dialog names the id as the box does (first eight, last six), never in full; asking is not accepting
	r := x.press("laptop", map[string]any{"machine_id": want})
	if got := actionOf(r); !strings.Contains(got, "accept what laptop shows about its own check of approvals now") ||
		!strings.Contains(got, "it shows the machine id "+approval.CompareID(want)+", which is not on record") || strings.Contains(got, want) {
		t.Fatalf("the dialog: %q", got)
	}
	if b := x.s.Poller.BindingOf("laptop"); b.Status != approval.BindNoRecord {
		t.Fatal("asking is not accepting")
	}
	res := x.confirm(t, r)
	if res["ok"] != true || res["message"] != "laptop: the machine id it shows is on record now." {
		t.Fatalf("the result: %v", res)
	}
	b = x.s.Poller.BindingOf("laptop")
	if !b.OK() || b.Record.MachineID != want || b.Record.How != approval.PinAccepted || b.Record.By == "" || b.Record.By == "Fairy" || b.Record.Holder || b.Record.Unaccepted {
		t.Fatalf("after the accept: %+v", b)
	}
	if recs, err := adminstate.New(x.s.Cfg.DataDir).Machines(); err != nil || recs["laptop"].MachineID != want || recs["laptop"].How != approval.PinAccepted || recs["laptop"].Unaccepted {
		t.Fatalf("machines.json after the accept: %v %+v", err, recs)
	}
	// audit entries as for every accept, and the alarm's end
	var ok map[string]any
	for _, e := range x.auditEntries("", "approval.accept") {
		if e["outcome"] == "ok" {
			ok = e
		}
	}
	if p, _ := ok["params"].(map[string]any); ok == nil || ok["target"] != "laptop" || p["was"] != approval.BindNoRecord || p["accepted"] != "id" || p["machine_id"] != approval.ShortID(want) {
		t.Fatalf("the accept in the audit log: %v", x.auditEntries("", "approval.accept"))
	}
	if al := x.auditEntries("", "approval.alarm"); len(al) != 2 || al[0]["outcome"] != "cleared" {
		t.Fatalf("the alarm entries after the accept: %v", al)
	}
	if active, _ := x.alarms(); active != 0 {
		t.Fatalf("%d alarms still hold", active)
	}
	if _, ms := x.page(t); ms["laptop"]["binding"] != nil || ms["laptop"]["can_accept"] != nil || ms["laptop"]["machine_id"] != want || ms["laptop"]["pinned"] != approval.PinAccepted {
		t.Fatalf("the approval page after the accept: %v", ms["laptop"])
	}
	// and now the machine is asked, and approved with the id on record
	id, _, asks := approvalAsked(x.post("/api/systems/laptop/power", x.shutdownBody()))
	if !asks {
		t.Fatal("after the accept the shutdown asks for the approval")
	}
	if cb := x.approve(t, id); cb.code != 303 || x.result(t)["ok"] != true || len(x.laptop.Passed()) != 1 {
		t.Fatalf("the approved shutdown: %d", cb.code)
	}
	if got, _ := entryOf(t, x.laptop, "/power"); got != want {
		t.Fatalf("the entry names %q", got)
	}
}

// machines.json of a build before this one: an id that came on record at first sight. It counts as none from
// the first start of this build: the host is "not on record" (one alarm, also across restarts), the id is
// taken out of the file, and records from Turn on or an accept are left alone.
func TestAnIDFromFirstSightInAnEarlierBuildsFileIsNotOnRecord(t *testing.T) {
	a := newApprovalEnv(t)
	a.setTrust(t, a.laptop, a.tower)
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	turnOn := approval.Record{MachineID: machineIDOf("tower"), How: approval.PinTurnOn, At: at, By: "nora", State: "required", StateAt: at}
	if err := adminstate.New(a.s.Cfg.DataDir).SetMachines(map[string]approval.Record{
		"laptop": {MachineID: machineIDOf("laptop"), How: approval.PinFirstSight, At: at, By: "Fairy", State: "required", StateAt: at, Holder: true},
		"tower":  turnOn,
	}, nil); err != nil {
		t.Fatal(err)
	}
	// before any answer: a machine with such a record is still one that checks approvals, with no id on record
	p0, err := poller.New(a.s.Cfg, a.s.Secrets, a.gb)
	if err != nil {
		t.Fatal(err)
	}
	if b := p0.BindingOf("laptop"); !b.Checks() || b.Record.MachineID != "" || b.Record.Holder || !b.Record.Unaccepted {
		t.Fatalf("laptop right after the start: %+v", b)
	}
	if b := p0.BindingOf("tower"); !b.OK() || b.Record.MachineID != machineIDOf("tower") || b.Record.How != approval.PinTurnOn {
		t.Fatalf("tower right after the start: %+v", b)
	}
	if err := p0.PinMachine("laptop", machineIDOf("laptop"), approval.PinFirstSight, "Fairy"); err == nil {
		t.Fatal("an id was taken on record as \"first-sight\"")
	}
	lines := func() (out []string) {
		for _, e := range a.auditEntries("", "approval.alarm") {
			out = append(out, fmt.Sprint(e["target"], " ", e["outcome"]))
		}
		return out
	}
	var s2 *Server
	for i := 0; i < 3; i++ { // the start, and two restarts while it holds
		s2 = a.restarted(t)
		if b := s2.Poller.BindingOf("laptop"); b.Status != approval.BindNoRecord || b.Record.MachineID != "" {
			t.Fatalf("start %d: laptop: %+v", i, b)
		}
		if b := s2.Poller.BindingOf("tower"); !b.OK() || b.Record.MachineID != turnOn.MachineID || b.Record.How != turnOn.How || b.Record.By != turnOn.By || !b.Record.At.Equal(at) {
			t.Fatalf("start %d: tower's record was touched: %+v", i, b)
		}
		if got := lines(); len(got) != 1 || got[0] != "laptop alarm: no_record" {
			t.Fatalf("start %d: the alarms in the audit log: %v", i, got)
		}
	}
	raw, _ := os.ReadFile(adminstate.MachinesPath(a.s.Cfg.DataDir))
	if strings.Contains(string(raw), machineIDOf("laptop")) || strings.Contains(string(raw), approval.PinFirstSight) || !strings.Contains(string(raw), `"unaccepted": true`) ||
		!strings.Contains(string(raw), machineIDOf("tower")) {
		t.Fatalf("machines.json after the start:\n%s", raw)
	}
	// the way on: an admin accepts the id the machine shows
	if what, before, err := s2.Poller.AcceptMachine("laptop", "nora", poller.AcceptWant{MachineID: machineIDOf("laptop")}); what != "id" || err != nil || before.Status != approval.BindNoRecord {
		t.Fatalf("the accept: %q %v", what, err)
	}
	if b := s2.Poller.BindingOf("laptop"); !b.OK() || b.Record.How != approval.PinAccepted || b.Record.Holder {
		t.Fatalf("laptop after the accept: %+v", b)
	}
}

// "Once required, now shows none" is an alarm also for a host that was never on record: it does not silently
// become a machine without the check, and nothing is sent to it on the panel's own confirmation until an admin
// has accepted that its check is off.
func TestAMachineThatWasNotOnRecordAndLosesItsCheckIsAnAlarm(t *testing.T) {
	for name, gone := range map[string]func(a *approvalEnv, t *testing.T){
		"it says off":       func(a *approvalEnv, t *testing.T) { a.clearTrust(t, a.laptop) },
		"no block any more": func(a *approvalEnv, t *testing.T) { a.gb.Stale("laptop", jsonx.M{}) },
	} {
		t.Run(name, func(t *testing.T) {
			a := newApprovalEnv(t)
			a.setTrust(t, a.laptop)
			a.poll()
			if b := a.s.Poller.BindingOf("laptop"); b.Status != approval.BindNoRecord {
				t.Fatalf("%+v", b)
			}
			gone(a, t)
			a.poll()
			if b := a.s.Poller.BindingOf("laptop"); b.Status != approval.BindVanished || !b.Checks() || b.Record.MachineID != "" {
				t.Fatalf("after the check went: %+v", b)
			}
			fresh := a.cookie(controller(), 0) // inside the panel's own window
			a.admin, a.csrf[1] = fresh, fresh
			passed := len(a.laptop.Passed())
			a.stopped(t, "shutdown", a.post("/api/systems/laptop/power", a.shutdownBody()), "had the check on and now shows none")
			if len(a.laptop.Passed()) != passed || len(a.iss.Asked()) != 0 {
				t.Fatalf("something was sent to the machine: %+v", a.laptop.Passed())
			}
			if al := a.auditEntries("", "approval.alarm"); len(al) != 2 || al[0]["outcome"] != "alarm: vanished" || al[1]["outcome"] != "alarm: no_record" {
				t.Fatalf("the alarms: %v", al)
			}
			_, ms := a.page(t)
			if m := ms["laptop"]; m["binding"] != "vanished" || m["can_accept"] != true || m["accept"] != "off" {
				t.Fatalf("the approval page: %v", m)
			}
			// a restart of the panel does not forget it: before any answer it is still a machine that checks
			// approvals (nothing is sent to it on the panel's own confirmation, it is in no run), and after
			// its answer the alarm stands
			p0, err := poller.New(a.s.Cfg, a.s.Secrets, a.gb)
			if err != nil || !p0.MachineChecks("laptop") {
				t.Fatalf("right after a restart: %v %+v", err, p0.BindingOf("laptop"))
			}
			if s2 := a.restarted(t); s2.Poller.BindingOf("laptop").Status != approval.BindVanished {
				t.Fatalf("after a restart: %+v", s2.Poller.BindingOf("laptop"))
			}
			if what, _, err := a.accept("laptop", "nora"); err != nil || what != "off" {
				t.Fatal(what, err)
			}
			if b := a.s.Poller.BindingOf("laptop"); !b.OK() || b.Known || b.Checks() {
				t.Fatalf("after the accept: %+v", b)
			}
		})
	}
}

// A host that is first seen while it shows an id that is on record for another host is not "on a par" with
// it (the second review's F1, and the auditor's TestR3X1_ANewcomerWithAnotherHostsIDIsOnAPar): nothing comes
// on record for it, it has no button, and an accept for it is refused.
func TestAHostFirstSeenWithAnIDThatIsOnRecordElsewhereGetsNoButton(t *testing.T) {
	x := newAcceptAdmin(t)
	x.require(t, x.laptop)
	idL := machineIDOf("laptop")
	x.setTrustID(t, x.tower, idL) // tower is first seen with laptop's id
	x.poll()
	if b := x.s.Poller.BindingOf("tower"); b.Status != approval.BindDuplicate || b.Record.MachineID != "" || fmt.Sprint(b.Held) != "[laptop]" {
		t.Fatalf("tower: %+v", b)
	}
	_, ms := x.page(t)
	if m := ms["tower"]; m["can_accept"] != nil || m["accept"] != nil || fmt.Sprint(m["held_by"]) != "[laptop]" || m["machine_id"] != nil || m["pinned"] != nil {
		t.Fatalf("tower on the approval page: %v", m)
	}
	if m := ms["laptop"]; m["can_accept"] != true || m["accept"] != "keep" || m["held_by"] != nil {
		t.Fatalf("laptop on the approval page: %v", m)
	}
	r := x.press("tower", map[string]any{"machine_id": idL})
	if d := fmt.Sprint(r.body["detail"]); r.code != 409 || pendingID(r) != "" || !strings.Contains(d, "tower: nothing was accepted. The machine id it shows is kept by laptop, and one id is accepted for one host only.") ||
		!strings.Contains(d, "Give tower a new id on the machine itself (fairy-agent helper-trust set --new-machine-id), then accept the new id here.") {
		t.Fatalf("an accept on tower: %d %s", r.code, r.raw)
	}
	// the admin's word for laptop; tower still gets nothing on record
	res := x.confirm(t, x.press("laptop", map[string]any{"machine_id": idL}))
	if res["ok"] != true || res["message"] != "laptop keeps the machine id. tower stays flagged until it shows an id of its own (on it: fairy-agent helper-trust set --new-machine-id)." {
		t.Fatalf("the result: %v", res)
	}
	if r := x.press("tower", map[string]any{"machine_id": idL}); r.code != 409 || !strings.Contains(fmt.Sprint(r.body["detail"]), "kept by laptop") {
		t.Fatalf("tower after laptop's accept: %d %s", r.code, r.raw)
	}
	if b := x.s.Poller.BindingOf("laptop"); !b.OK() || !b.Record.Holder {
		t.Fatalf("laptop: %+v", b)
	}
	if b := x.s.Poller.BindingOf("tower"); b.Status != approval.BindDuplicate || b.Record.MachineID != "" {
		t.Fatalf("tower: %+v", b)
	}
}

// The page: "not on record" has its sign on the Overview like every other flag, one sentence, one button, and
// behind the info button why and what to compare before accepting. Every value of the server goes through esc().
func TestUINotOnRecord(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`no_record: ["Not on record", "Fairy has no machine id on record for it. No approvals until an admin accepts its id."],`,
		`no_record: ["Fairy did not turn this check on: it was set on the machine itself, or from another panel.",`,
		`"Before you accept, compare the id with the machine's own: on the machine <code>fairy-agent helper-status</code>, line <code>machine id:</code>. A machine that answers in another's name would show the other's id.",`,
		// (second review, G-02) what the comparison is worth, said plainly
		`"The comparison catches something that sits between the panel and the machine, and an agent that answers in another's name. It does not catch a host whose own administrator is hostile. Only the real machine being on record in this panel stops that."],`,
		`"Before you press, compare the id with the machine's own: on the machine <code>fairy-agent helper-status</code>, line <code>machine id:</code>."],`,
		`first: ["Accept this machine's id", (name) => ` + "`Accept the machine id of ${name}`],",
		`const b = a.binding ? AP_BINDING[a.binding] : null;`, // the Overview's sign: "not on record" is not left out any more
		"else if (m?.shown_id) more.push(`It shows: <code>${esc(m.shown_id)}</code>. Nothing is on record.`);",
		`(rec.machine_id || a.binding === "no_record") ? row("Machine id it shows", ` + "`<code id=\"ap-shown\">${esc(shortKey(rec.shown_id))}</code>`",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	for _, gone := range []string{`a.binding !== "no_record"`, "No machine id on record yet", "Fairy takes its id on record with the machine's next answer", "first-sight"} {
		if strings.Contains(js, gone) {
			t.Errorf("app.js still has %q", gone)
		}
	}
}
