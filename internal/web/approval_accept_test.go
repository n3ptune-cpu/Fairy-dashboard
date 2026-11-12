package web

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	agentapproval "github.com/n3ptune-cpu/Fairy-agent/pkg/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// The alarms of the panel's record of machine ids (audit 5, r1-x1-01), and the one way out of them: an admin
// accepts what the machine shows now.

// setTrustID writes a trust file with the machine id id on the machine, in place of the one it has (what
// `helper-trust set --new-machine-id` or a copied disk leaves there), without the panel hearing of it.
func (a *approvalEnv) setTrustID(t *testing.T, m *testutil.Machine, id string) {
	t.Helper()
	_ = os.Remove(m.TrustPath)
	tr, err := agentapproval.NewTrust(agentapproval.Trust{Issuer: a.iss.URL(), ClientID: testApprovalClient, MachineID: id,
		Keys: []api.TrustKey{agentapproval.KeyFromRSA(a.iss.Kid, &a.iss.Key.PublicKey)}, Users: []string{approvalAdminSub}})
	if err != nil {
		t.Fatal(err)
	}
	if err := (agentapproval.Files{Path: m.TrustPath}).Create(tr); err != nil {
		t.Fatal(err)
	}
	m.Gate.StatusNow()
}

// clearTrust removes the machine's trust file (`helper-trust clear` on the machine).
func (a *approvalEnv) clearTrust(t *testing.T, m *testutil.Machine) {
	t.Helper()
	if err := os.Remove(m.TrustPath); err != nil {
		t.Fatal(err)
	}
	m.Gate.StatusNow()
}

// stopped fails unless the request is refused by the panel itself with want in its text, and nothing reached
// the machines, Pocket ID or the kept actions meanwhile.
func (a *approvalEnv) stopped(t *testing.T, what string, r resp, want string) {
	t.Helper()
	if _, _, ok := approvalAsked(r); ok || r.code != 409 || !strings.Contains(fmt.Sprint(r.body["detail"]), want) || !strings.Contains(fmt.Sprint(r.body["detail"]), "Nothing was sent.") {
		t.Fatalf("%s: %d %s", what, r.code, r.raw)
	}
	if keptID(a.env, a.admin) != "" {
		t.Fatalf("%s: an action is kept for a confirmation", what)
	}
}

// accept is an admin's accept of exactly what the host shows now (what the page showed and the request names).
func (a *approvalEnv) accept(id, by string) (string, approval.Binding, error) {
	return a.s.Poller.AcceptMachine(id, by, acceptShows(a.s.Poller.BindingOf(id)))
}

// alarms counts the alarms in the Notifications list: the ones that hold, and all of them since require.
func (a *approvalEnv) alarms() (active, all int) {
	for _, e := range a.s.Notes.List("", func(notifications.Event) bool { return true }, notifications.Filter{Limit: 200}).Events {
		if e.Kind == "approval_alarm" {
			all++
			if e.Active {
				active++
			}
		}
	}
	return active, all - a.noteBase
}

const otherID = "another-machine-id-0123456789"

// A host that shows another machine id than the one on record: flagged, no challenge is asked, nothing is
// sent; an alarm in the audit log and the Notifications list; everyone sees the flag, nobody but an admin in a
// browser can accept; after the accept the new id is on record and approvals are made with it.
func TestIdentityChangedStopsApprovalsUntilAnAdminAccepts(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop, a.tower)
	a.setTrustID(t, a.laptop, otherID) // on the machine: helper-trust set --new-machine-id
	a.poll()
	b := a.s.Poller.BindingOf("laptop")
	if b.Status != approval.BindChanged || b.Record.MachineID != machineIDOf("laptop") || b.Report.MachineID != otherID || !a.s.Poller.BindingOf("tower").OK() {
		t.Fatalf("laptop: %+v", b)
	}
	asked, tokens := len(a.laptop.Asked()), len(a.iss.Asked())
	a.stopped(t, "shutdown", a.post("/api/systems/laptop/power", a.shutdownBody()), "shows another machine id than the one Fairy has on record")
	a.stopped(t, "update", a.post("/api/admin/updates/laptop", map[string]any{}), "shows another machine id")
	if len(a.laptop.Asked()) != asked || len(a.iss.Asked()) != tokens || len(a.laptop.Passed()) != 0 {
		t.Fatalf("the machine was asked or sent something: %+v", a.laptop.Asked()[asked:])
	}
	// the other machine is not affected
	if _, _, ok := approvalAsked(a.post("/api/systems/tower/fan/mode", map[string]any{"mode": "full"})); !ok {
		t.Fatal("tower must still be asked for its approval")
	}
	// the alarm: audit log (short ids only) and one notification that holds
	al := a.alarmEntries()
	if len(al) != 1 || al[0]["target"] != "laptop" || al[0]["outcome"] != "alarm: changed" || al[0]["params"].(map[string]any)["on_record"] != approval.ShortID(machineIDOf("laptop")) ||
		al[0]["params"].(map[string]any)["shown"] != approval.ShortID(otherID) {
		t.Fatalf("audit: %v", al)
	}
	if active, all := a.alarms(); active != 1 || all != 1 {
		t.Fatalf("notifications: %d active of %d", active, all)
	}
	a.poll()
	a.poll()
	if _, all := a.alarms(); all != 1 || len(a.alarmEntries()) != 1 {
		t.Fatal("the alarm is raised once, not at every poll")
	}
	// everyone who sees the host sees the flag; the ids are not in it
	x := a.do("GET", "/api/systems/laptop", nil, "Cookie", a.admin)
	if ap := x.body["approval"].(map[string]any); ap["binding"] != "changed" || strings.Contains(string(x.raw), otherID) {
		t.Fatalf("the block: %s", x.raw)
	}
	_, ms := a.page(t)
	if m := ms["laptop"]; m["binding"] != "changed" || m["can_accept"] != true || m["accept"] != "id" || m["shown_id"] != otherID || m["machine_id"] != machineIDOf("laptop") || m["ready"] == true {
		t.Fatalf("the approval page: %v", m)
	}
	// who may accept: not a viewer, not a token; an admin is asked to confirm, every time, and is told what
	const path = "/api/admin/approval/accept/laptop"
	v := viewer
	v.Sub = "viewer-9"
	if r := a.do("POST", path, nil, "Cookie", a.cookie(v, 0), "X-Requested-With", "fairy"); r.code != 403 {
		t.Fatalf("a viewer's accept: %d %s", r.code, r.raw)
	}
	ctl, _, _ := a.s.Tokens.Create("ops", approvalAdminSub, "control", "cli:test", 0)
	if r := a.do("POST", path, nil, "Authorization", "Bearer "+ctl); r.code != 403 || !strings.Contains(string(r.raw), "browser sign-in") {
		t.Fatalf("a token's accept: %d %s", r.code, r.raw)
	}
	if r := a.do("POST", path, nil, "Cookie", a.admin); r.code != 403 {
		t.Fatalf("without the CSRF header: %d", r.code)
	}
	fresh := a.cookie(controller(), 0)
	r := a.do("POST", path, map[string]any{"machine_id": otherID}, "Cookie", fresh, "X-Requested-With", "fairy", "Content-Type", "application/json")
	d, _ := r.body["detail"].(map[string]any)
	if r.code != 401 || d["code"] != "step_up_required" || d["approval"] != nil ||
		!strings.Contains(fmt.Sprint(d["action"]), "accept what laptop shows about its own check of approvals now") ||
		!strings.Contains(fmt.Sprint(d["action"]), "it shows the machine id "+approval.CompareID(otherID)+"; on record is "+approval.CompareID(machineIDOf("laptop"))) {
		t.Fatalf("an admin who signed in this second is still asked to confirm, with what would be accepted: %d %s", r.code, r.raw)
	}
	if b := a.s.Poller.BindingOf("laptop"); b.Status != approval.BindChanged {
		t.Fatal("asking is not accepting")
	}
	// the accept itself (what the kept request runs after the confirmation)
	what, before, err := a.accept("laptop", "nora@example.test")
	if err != nil || what != "id" || before.Status != approval.BindChanged {
		t.Fatalf("accept: %q %v", what, err)
	}
	b = a.s.Poller.BindingOf("laptop")
	if !b.OK() || b.Record.MachineID != otherID || b.Record.How != approval.PinAccepted || b.Record.By != "nora@example.test" || b.Record.Holder {
		t.Fatalf("after the accept: %+v", b)
	}
	if recs, err := adminstate.New(a.s.Cfg.DataDir).Machines(); err != nil || recs["laptop"].MachineID != otherID || recs["laptop"].How != approval.PinAccepted {
		t.Fatalf("machines.json: %v %+v", err, recs)
	}
	if active, all := a.alarms(); active != 0 || all != 1 {
		t.Fatalf("the alarm must be over: %d active of %d", active, all)
	}
	if got := auditOutcomes(a.s.Cfg.AuditLog); !strings.Contains(got, "approval.alarm cleared") {
		t.Errorf("audit:\n%s", got)
	}
	// and the machine is approved with the new id
	r = a.post("/api/systems/laptop/power", a.shutdownBody())
	id, _, ok := approvalAsked(r)
	if !ok {
		t.Fatalf("after the accept: %d %s", r.code, r.raw)
	}
	if cb := a.approve(t, id); cb.code != 303 || a.result(t)["ok"] != true || len(a.laptop.Passed()) != 1 {
		t.Fatalf("the approved shutdown: %d", cb.code)
	}
	if got, _ := entryOf(t, a.laptop, "/power"); got != otherID {
		t.Fatalf("the entry names %q", got)
	}
	// nothing to accept any more
	if what, _, err := a.accept("laptop", "nora"); what != "" || err == nil {
		t.Fatal("a second accept with nothing to accept")
	}
}

// One id on two hosts (a machine made from a copy of another one's disk): BOTH are flagged and get no
// approval. The host that has the id on record can be said to keep it; the other stays flagged until it shows
// an id of its own, which an admin then accepts there.
//
// CHANGED (audit 5, round 3, r3-x1-01): tower, the copy, has no record when it shows laptop's id. It used to get
// that id on record at first sight, which made the two "on a par", each with the button "This host keeps its
// machine id". Now nothing comes on record for tower: laptop keeps the id, tower has no button and its
// sentence names laptop. Both are still flagged until the admin's word for laptop.
func TestOneIDOnTwoHostsFlagsBoth(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop)
	a.setTrustID(t, a.tower, machineIDOf("laptop")) // the copy
	a.poll()
	for _, id := range []string{"laptop", "tower"} {
		if b := a.s.Poller.BindingOf(id); b.Status != approval.BindDuplicate || len(b.With) != 1 {
			t.Fatalf("%s: %+v", id, b)
		}
	}
	asked := len(a.laptop.Asked()) + len(a.tower.Asked())
	if b := a.s.Poller.BindingOf("tower"); b.Record.MachineID != "" || fmt.Sprint(b.Held) != "[laptop]" {
		t.Fatalf("tower must have nothing on record, and laptop keeps the id it shows: %+v", b)
	}
	a.stopped(t, "laptop's shutdown", a.post("/api/systems/laptop/power", a.shutdownBody()), "laptop and tower have the same machine id")
	a.stopped(t, "tower's fan change", a.post("/api/systems/tower/fan/mode", map[string]any{"mode": "full"}), "tower has the same machine id as laptop, which keeps it")
	if len(a.laptop.Asked())+len(a.tower.Asked()) != asked || len(a.iss.Asked()) != 0 {
		t.Fatal("a machine with a shared id was asked for a challenge")
	}
	if active, _ := a.alarms(); active != 2 {
		t.Fatalf("%d alarms hold, want one per host", active)
	}
	_, ms := a.page(t)
	if m := ms["tower"]; m["binding"] != "duplicate" || fmt.Sprint(m["with"]) != "[laptop]" || m["can_accept"] != nil || m["accept"] != nil || fmt.Sprint(m["held_by"]) != "[laptop]" || m["machine_id"] != nil {
		t.Fatalf("the approval page, the copy: %v", m)
	}
	if m := ms["laptop"]; m["binding"] != "duplicate" || fmt.Sprint(m["with"]) != "[tower]" || m["can_accept"] != true || m["accept"] != "keep" {
		t.Fatalf("the approval page, the original: %v", m)
	}
	if what, _, err := a.accept("tower", "nora"); what != "" || err == nil {
		t.Fatalf("an accept on the copy: %q %v", what, err)
	}
	// the admin's word: laptop is the original
	if what, _, err := a.accept("laptop", "nora"); err != nil || what != "id" {
		t.Fatal(what, err)
	}
	if b := a.s.Poller.BindingOf("laptop"); !b.OK() || !b.Record.Holder {
		t.Fatalf("laptop after the accept: %+v", b)
	}
	if b := a.s.Poller.BindingOf("tower"); b.Status != approval.BindDuplicate {
		t.Fatalf("tower must stay flagged: %+v", b)
	}
	a.stopped(t, "tower's fan change", a.post("/api/systems/tower/fan/mode", map[string]any{"mode": "full"}), "tower has the same machine id as laptop, which keeps it")
	if _, _, ok := approvalAsked(a.post("/api/systems/laptop/power", a.shutdownBody())); !ok {
		t.Fatal("laptop is asked for its approval again")
	}
	// polls change nothing about that
	a.poll()
	if a.s.Poller.BindingOf("tower").Status != approval.BindDuplicate || !a.s.Poller.BindingOf("laptop").OK() {
		t.Fatal("after a poll")
	}
	// the copy gets its own id on the machine: now it is "not on record" (it never had one), and an admin
	// accepts it there
	a.setTrustID(t, a.tower, otherID)
	a.poll()
	if b := a.s.Poller.BindingOf("tower"); b.Status != approval.BindNoRecord || len(b.With) != 0 {
		t.Fatalf("tower with its own id: %+v", b)
	}
	if what, _, err := a.accept("tower", "nora"); err != nil || what != "id" {
		t.Fatal(what, err)
	}
	if b := a.s.Poller.BindingOf("tower"); !b.OK() || b.Record.MachineID != otherID || b.Record.Holder {
		t.Fatalf("tower after its accept: %+v", b)
	}
	if !a.s.Poller.BindingOf("laptop").OK() {
		t.Fatal("laptop")
	}
	if active, _ := a.alarms(); active != 0 {
		t.Fatalf("%d alarms still hold", active)
	}
}

// The laundered Turn on (the second review's note): "laptop" answers the questions of a Turn on with the
// answers of "tower", a machine without trust, and hands tower the proposal. Tower takes it: it now has the
// check, with the id the panel made up FOR LAPTOP. No machine can see that. The panel does, at tower's next
// answer: one id on two hosts. From then on neither is asked for anything, so the relay that this was the
// preparation for never gets an approval to hand on.
//
// CHANGED (audit 5, round 3, r3-x1-01): tower, the machine that really holds the id, used to get it on record
// at first sight, and an admin could then say "tower keeps it". Now tower has nothing on record: the id is on
// record for laptop (the panel issued it there), tower's box names laptop and has no button. Both stay
// without approvals. The way out is on the machine: tower gets an id of its own (helper-trust set
// --new-machine-id) and an admin accepts that one; laptop, which never had a trust file, is then "Check gone".
func TestALaunderedTurnOnIsFoundOutAsOneIDOnTwoHosts(t *testing.T) {
	for name, honestBlock := range map[string]bool{"the hostile host shows the id": false, "the hostile host goes on saying off": true} {
		t.Run(name, func(t *testing.T) {
			a := newApprovalEnv(t)
			a.gb.Relay("laptop", func(path string, body any) (int, jsonx.M) {
				switch path {
				case api.PathApprovalChallenge, api.PathApprovalTrust, "/power":
					return a.gb.Forward("tower", path, body)
				}
				return 0, nil
			})
			r := a.turnOn("laptop")
			id, machines, ok := approvalAsked(r)
			if !ok || len(machines) != 1 || machines[0] != "laptop" {
				t.Fatalf("Turn on: %d %s", r.code, r.raw)
			}
			if cb := a.approve(t, id); cb.code != 303 || a.result(t)["ok"] != true {
				t.Fatalf("the Turn on (the hostile host answered 200 in tower's name): %v", a.result(t))
			}
			made := a.s.Poller.BindingOf("laptop").Record.MachineID
			if api.CheckMachineID(made) != nil || trustID(t, a.tower) != made {
				t.Fatalf("the id the panel made up for laptop %q is the id tower has now: %q", made, trustID(t, a.tower))
			}
			if !honestBlock { // the hostile host shows what a machine with the check shows
				a.gb.Stale("laptop", jsonx.M{"state": "required", "enforced_by": "agent", "proto": 2.0, "machine_id": made,
					"issuer": a.iss.URL(), "client_id": testApprovalClient, "users": 1.0})
			}
			a.poll() // the poll every Turn on ends with
			lb, tb := a.s.Poller.BindingOf("laptop"), a.s.Poller.BindingOf("tower")
			if lb.Status != approval.BindDuplicate || tb.Status != approval.BindDuplicate || tb.Record.MachineID != "" || fmt.Sprint(tb.Held) != "[laptop]" || lb.Record.MachineID != made {
				t.Fatalf("laptop %+v\ntower %+v", lb, tb)
			}
			// the relay this prepared: a shutdown "of laptop" that tower would now accept. It is never asked for.
			asked, tokens := len(a.tower.Asked()), len(a.iss.Asked())
			r = a.post("/api/systems/laptop/power", a.shutdownBody())
			if _, _, ok := approvalAsked(r); ok || r.code != 409 {
				t.Fatalf("the shutdown of laptop: %d %s", r.code, r.raw)
			}
			a.stopped(t, "tower's own fan change", a.post("/api/systems/tower/fan/mode", map[string]any{"mode": "full"}), "tower has the same machine id as laptop, which keeps it")
			if len(a.tower.Asked()) != asked || len(a.iss.Asked()) != tokens || len(a.tower.Passed()) != 0 {
				t.Fatalf("tower was asked or sent something: %+v", a.tower.Asked()[asked:])
			}
			if got := auditOutcomes(a.s.Cfg.AuditLog); !strings.Contains(got, "approval.alarm alarm: duplicate") || !strings.Contains(got, "approval.machine_id on record") {
				t.Errorf("audit:\n%s", got)
			}
			// tower cannot be accepted for the id that is on record for laptop
			if what, _, err := a.accept("tower", "nora"); what != "" || err == nil {
				t.Fatalf("an accept on tower: %q %v", what, err)
			}
			// the way out: an id of its own on tower, accepted there; tower is approved by its own approvals again
			a.setTrustID(t, a.tower, otherID)
			a.poll()
			if b := a.s.Poller.BindingOf("tower"); b.Status != approval.BindNoRecord {
				t.Fatalf("tower with an id of its own: %+v", b)
			}
			if what, _, err := a.accept("tower", "nora"); what != "id" || err != nil {
				t.Fatalf("the accept of tower's own id: %q %v", what, err)
			}
			if b := a.s.Poller.BindingOf("tower"); !b.OK() || b.Record.MachineID != otherID || b.Record.How != approval.PinAccepted {
				t.Fatalf("tower after its accept: %+v", b)
			}
			if _, _, ok := approvalAsked(a.post("/api/systems/tower/fan/mode", map[string]any{"mode": "full"})); !ok {
				t.Fatal("tower is asked for its own approval again")
			}
			// laptop, the host that handed the Turn on on, gains nothing by that. When it shows no check it is
			// "Check gone" and gets nothing. When it goes on showing the id the panel issued for it, the panel
			// has no reason left to flag it, and no machine holds that id any more: an approval for "laptop"
			// that it hands on to tower is refused there (tower checks with its own id).
			lb = a.s.Poller.BindingOf("laptop")
			r = a.post("/api/systems/laptop/power", a.shutdownBody())
			if honestBlock {
				if lb.Status != approval.BindVanished || r.code != 409 {
					t.Fatalf("laptop afterwards: %+v, its shutdown %d %s", lb, r.code, r.raw)
				}
			} else {
				id, _, ok := approvalAsked(r)
				if !lb.OK() || !ok {
					t.Fatalf("laptop afterwards: %+v, its shutdown %d %s", lb, r.code, r.raw)
				}
				cb := a.approve(t, id)
				if res := a.result(t); cb.code != 303 || res["ok"] == true || !strings.Contains(fmt.Sprint(res), "nonce") {
					t.Fatalf("the approval for laptop, handed on to tower: %v", res)
				}
			}
			if len(a.tower.Passed()) != 0 {
				t.Fatalf("something reached tower: %+v", a.tower.Passed())
			}
		})
	}
}

// A host that had the check and shows none (its trust was cleared on the machine, or its agent was put back to
// a release from before the check): an alarm, and nothing is sent to it on the panel's own confirmation. Turn
// on is offered again when it says "off"; or an admin accepts that the check is off.
func TestAVanishedCheckIsAnAlarmNotAMachineWithoutTheCheck(t *testing.T) {
	for name, gone := range map[string]func(a *approvalEnv, t *testing.T){
		"it says off":       func(a *approvalEnv, t *testing.T) { a.clearTrust(t, a.laptop) },
		"no block any more": func(a *approvalEnv, t *testing.T) { a.gb.Stale("laptop", jsonx.M{}) },
	} {
		t.Run(name, func(t *testing.T) {
			a := newApprovalEnv(t)
			a.require(t, a.laptop)
			gone(a, t)
			a.poll()
			if b := a.s.Poller.BindingOf("laptop"); b.Status != approval.BindVanished || !b.Checks() {
				t.Fatalf("%+v", b)
			}
			fresh := a.cookie(controller(), 0) // inside the panel's own window: its confirmation would not even be asked
			a.admin, a.csrf[1] = fresh, fresh
			passed := len(a.laptop.Passed())
			a.stopped(t, "shutdown", a.post("/api/systems/laptop/power", a.shutdownBody()), "had the check on and now shows none")
			a.stopped(t, "update", a.post("/api/admin/updates/laptop", map[string]any{}), "had the check on and now shows none")
			if len(a.laptop.Passed()) != passed || len(a.iss.Asked()) != 0 {
				t.Fatalf("something was sent to the machine: %+v", a.laptop.Passed())
			}
			if active, _ := a.alarms(); active != 1 || len(a.alarmEntries()) != 1 {
				t.Fatalf("alarm: %d", active)
			}
			x := a.do("GET", "/api/systems/laptop", nil, "Cookie", a.admin)
			if ap, _ := x.body["approval"].(map[string]any); ap == nil || ap["binding"] != "vanished" {
				t.Fatalf("the host's block must say so, also when the agent sends none: %s", x.raw)
			}
			_, ms := a.page(t)
			m := ms["laptop"]
			if m["binding"] != "vanished" || m["can_accept"] != true || m["accept"] != "off" || (m["ready"] == true) != (name == "it says off") {
				t.Fatalf("the approval page: %v", m)
			}
			// an admin accepts that it is off: the record is gone, and it is a machine without the check
			if what, _, err := a.accept("laptop", "nora"); err != nil || what != "off" {
				t.Fatal(what, err)
			}
			if b := a.s.Poller.BindingOf("laptop"); !b.OK() || b.Known || b.Checks() {
				t.Fatalf("after the accept: %+v", b)
			}
			if recs, err := adminstate.New(a.s.Cfg.DataDir).Machines(); err != nil || len(recs) != 0 {
				t.Fatalf("machines.json: %v %+v", err, recs)
			}
			// (the test machine behind "no block any more" still has its trust file and gate: only the machine
			// that really says off takes a request without an approval)
			if r := a.post("/api/systems/laptop/power", a.shutdownBody()); name == "it says off" && (r.code != 200 || len(a.laptop.Passed()) != passed+1) {
				t.Fatalf("as a machine without the check: %d %s", r.code, r.raw)
			}
			if active, _ := a.alarms(); active != 0 {
				t.Fatal("the alarm is over")
			}
		})
	}
}

// The check comes back by a new Turn on: the machine gets a new id, and that one is on record.
func TestTurnOnAgainAfterTheTrustWasCleared(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop)
	a.clearTrust(t, a.laptop)
	a.poll()
	r := a.turnOn("laptop")
	id, _, ok := approvalAsked(r)
	if !ok {
		t.Fatalf("Turn on for a machine whose check vanished: %d %s", r.code, r.raw)
	}
	if cb := a.approve(t, id); cb.code != 303 || a.result(t)["ok"] != true {
		t.Fatalf("%v", a.result(t))
	}
	a.poll()
	b := a.s.Poller.BindingOf("laptop")
	if !b.OK() || b.Record.How != approval.PinTurnOn || b.Record.MachineID == machineIDOf("laptop") || b.Record.MachineID != trustID(t, a.laptop) {
		t.Fatalf("%+v", b)
	}
	if active, _ := a.alarms(); active != 0 {
		t.Fatal("the alarm is over")
	}
}

// An agent whose approvals do not name the machine (the test build of 2026-11-11): nothing is approved there.
func TestAnAgentOfSchemeOneIsTooOld(t *testing.T) {
	a := newApprovalEnv(t)
	a.gb.Stale("laptop", jsonx.M{"state": "required", "enforced_by": "agent", "proto": 1.0, "issuer": a.iss.URL(), "client_id": testApprovalClient, "users": 1.0})
	a.poll()
	if b := a.s.Poller.BindingOf("laptop"); b.Status != approval.BindTooOld || b.Record.MachineID != "" {
		t.Fatalf("%+v", b)
	}
	a.stopped(t, "shutdown", a.post("/api/systems/laptop/power", a.shutdownBody()), "Install the current agent on it")
	if len(a.laptop.Asked()) != 0 {
		t.Fatal("it was asked for a challenge")
	}
	a.gb.Stale("laptop", jsonx.M{"state": "off", "enforced_by": "agent", "proto": 1.0})
	a.poll()
	_, ms := a.page(t)
	if m := ms["laptop"]; m["ready"] == true || !strings.Contains(fmt.Sprint(m["why"]), "agent too old") {
		t.Fatalf("Turn on for an agent of scheme 1: %v", m)
	}
}

// machines.json cannot be read: nothing is on record, so nothing is approved and nothing is sent, and the file
// is not written over by what a machine shows. When it can be read again, the panel goes on.
func TestAnUnreadableRecordStopsEverything(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop)
	path := adminstate.MachinesPath(a.s.Cfg.DataDir)
	good, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(`{"machines": [`), 0o600); err != nil {
		t.Fatal(err)
	}
	p2, err := poller.New(a.s.Cfg, a.s.Secrets, a.gb) // the panel starts with the damaged file
	if err != nil {
		t.Fatal(err)
	}
	p2.PollOnce(t.Context())
	for _, id := range []string{"laptop", "tower"} {
		if b := p2.BindingOf(id); b.Status != approval.BindNoStore || !b.Checks() {
			t.Fatalf("%s: %+v", id, b)
		}
	}
	if err := p2.PinMachine("laptop", otherID, approval.PinTurnOn, "nora"); err == nil {
		t.Fatal("an id came on record while the record cannot be read")
	}
	if now, _ := os.ReadFile(path); string(now) != `{"machines": [` {
		t.Fatalf("the damaged file was written over: %s", now)
	}
	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatal(err)
	}
	p2.PollOnce(t.Context())
	if b := p2.BindingOf("laptop"); !b.OK() || b.Record.MachineID != machineIDOf("laptop") {
		t.Fatalf("with the file back: %+v", b)
	}
}
