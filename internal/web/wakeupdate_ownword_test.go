package web

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// The machine's own word decides (independent review of 270e7ab, P1), through the real routes, the real run
// and Fairy-agent's own gate: the ticket reaches the machine twice (a proxy, or anybody on a plain-HTTP path,
// repeats the POST). The machine takes the first copy and is armed; the panel gets its honest answer to the
// second copy, 409 run_refused. The panel reads the machine's approval block, which shows THIS run as armed:
// the row says armed, the strip names it with its time, it can be confirmed, and when nobody confirms the
// report says that it switched itself off.
func TestATicketThatArrivesTwiceArmsTheMachineWhateverThePanelWasAnswered(t *testing.T) {
	r := newRunRig(t, "pc-a")
	var second jsonx.M
	r.gb.Relay("pc-a", func(path string, body any) (int, jsonx.M) {
		if path != api.PathRunArm {
			return 0, nil
		}
		r.gb.Relay("pc-a", nil)
		if st, _ := r.gb.Forward("pc-a", path, body); st != 200 {
			t.Errorf("the first copy of the ticket was not taken: %d", st)
		}
		st, data := r.gb.Forward("pc-a", path, body)
		second = data
		return st, data
	})
	_, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}})
	if res["ok"] != true {
		t.Fatal(res)
	}
	run, _ := r.s.WakeRuns.Snapshot()
	r.until(t, "pc-a waiting", 300, func() bool { return r.host("pc-a").State == wakerun.StApproval })
	if h := r.host("pc-a"); h.Arm != wakerun.ArmRefused || strings.Contains(h.Text, "stays ON") {
		t.Fatalf("right after the refusal: arm %q, %q", h.Arm, h.Text)
	}
	r.tick(30)
	if jsonx.S(second, "reason") != api.RunReasonArmedThisBoot {
		t.Fatalf("the machine's answer to the second copy: %v", second)
	}
	st := r.m["pc-a"].Gate.StatusNow().Run
	if st == nil || st.ID != run.ID || st.State != api.RunArmed {
		t.Fatalf("the machine is not armed: %+v", st)
	}
	h := r.host("pc-a")
	if !strings.HasPrefix(h.Text, "awake and armed: it switches itself off at ") || !strings.Contains(h.Text, "by the machine's own clock unless you confirm") {
		t.Fatalf("the row while the machine shows this run as armed: %q", h.Text)
	}
	w := r.req("GET", "/api/admin/wake-update/waiting", nil)
	armed, _ := w.body["armed"].([]any)
	if len(armed) != 1 || armed[0].(map[string]any)["id"] != "pc-a" || armed[0].(map[string]any)["sure"] != true || w.body["run"] != run.ID {
		t.Fatalf("the strip: %s", w.raw)
	}
	r.until(t, "the run over", 3000, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a == nil })
	h = r.host("pc-a")
	r.b.Mu.Lock()
	on := r.f.on["pc-a"]
	r.b.Mu.Unlock()
	if on || h.State != wakerun.StOff || !strings.HasSuffix(h.Text, "not confirmed, not updated. It switched itself off again.") {
		t.Fatalf("the report: %s %q; the machine is on: %v", h.State, h.Text, on)
	}
	r.noTokenAnywhere(t)
}

// Keep on while a ticket is still on its way (independent review of 270e7ab, P4). The arm request got no
// answer; the strip offers Keep on; the cancel finds no run and the machine shows none. That is not "stays
// ON": the ticket is valid for seven minutes and arrives a moment later. The panel says "not known until
// HH:MM", the run goes on cancelling, and the machine does stay on.
func TestKeepOnWhileTheTicketCanStillArriveIsNotSaidToHaveWorked(t *testing.T) {
	r := newRunRig(t, "pc-a")
	var late any
	r.gb.Relay("pc-a", func(path string, body any) (int, jsonx.M) {
		if path != api.PathRunArm {
			return 0, nil
		}
		late = body
		return 504, jsonx.M{} // no answer; the request is still on its way
	})
	_, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}})
	if res["ok"] != true {
		t.Fatal(res)
	}
	run, _ := r.s.WakeRuns.Snapshot()
	r.until(t, "pc-a waiting", 300, func() bool { return r.host("pc-a").State == wakerun.StApproval })
	if h := r.host("pc-a"); h.Arm != wakerun.ArmUnanswered || late == nil {
		t.Fatalf("pc-a: %s %q", h.Arm, h.Text)
	}
	until := run.StartedAt.Add(480 * time.Second).UTC().Format("15:04 UTC") // valid_s 420 and the 60 s a machine allows for the clocks
	r.admin = r.cookie(controller(), 0)
	r.csrf = []string{"Cookie", r.admin, "X-Requested-With", "fairy", "Content-Type", "application/json"}
	cancels := len(r.sent("pc-a", "/power"))
	yes := r.req("POST", "/api/systems/pc-a/wake-update/keep-on", map[string]any{})
	want := "Not known until " + until + " whether PC-A stays on: its agent shows no run now, but the run's ticket, whose request got no answer, may still reach it. The panel keeps cancelling until then, and the run's report says what came of it."
	if yes.code != 200 || yes.body["stays_on"] != false || yes.body["message"] != want {
		t.Fatalf("Keep on: %d %s\nwant %q", yes.code, yes.raw, want)
	}
	r.tick(5)
	r.gb.Relay("pc-a", nil)
	if st, data := r.gb.Forward("pc-a", api.PathRunArm, late); st != 200 { // the ticket arrives
		t.Fatalf("the model: the late ticket was not taken: %d %v", st, data)
	}
	if st := r.m["pc-a"].Gate.StatusNow().Run; st == nil || st.State != api.RunArmed {
		t.Fatalf("the machine after the late ticket: %+v", st)
	}
	r.until(t, "the run over", 1500, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a == nil })
	h := r.host("pc-a")
	if h.State != wakerun.StLeftOn || !strings.Contains(h.Text, ": its switch-off was called off (its agent shows the run as cancelled)") || !strings.HasSuffix(h.Text, "It is ON.") {
		t.Fatalf("the report: %s %q", h.State, h.Text)
	}
	if n := len(r.sent("pc-a", "/power")) - cancels; n < 2 {
		t.Fatalf("%d cancels were sent", n)
	}
	r.tick(1500) // past the ticket's wait and its notice, by the machine's own count
	r.b.Mu.Lock()
	on := r.f.on["pc-a"]
	r.b.Mu.Unlock()
	if st := r.m["pc-a"].Gate.StatusNow().Run; !on || st == nil || st.State != api.RunCancelled {
		t.Fatalf("after Keep on the machine is on: %v, its run: %+v", on, st)
	}
}

// "A run is confirmed once" counts from the first machine that accepts (independent review of 270e7ab, P3).
// A second admin of the panel who is not on the machine's own list of people who may approve presses "Confirm
// it's you" and gives a passkey: the machine refuses (reason "user"). The run is not confirmed by that: the
// strip stays, with who tried, the admin whose passkey the machine takes can still confirm, and no
// notification says "confirmed by".
func TestAConfirmationNoMachineAcceptsDoesNotUseTheRunUp(t *testing.T) {
	r := newRunRig(t, "pc-a")
	_, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}})
	if res["ok"] != true {
		t.Fatal(res)
	}
	r.until(t, "asked", 300, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a != nil && a.Approval == "needed" })
	run, _ := r.s.WakeRuns.Snapshot()
	ann, annCSRF := r.admin, r.csrf
	other := controller()
	other.Sub, other.Name, other.Email = "u7", model.P("Bob"), model.P("bob@example.com")
	r.cookie(other, 0)
	if _, _, err := r.s.Access.SetRole(other.Sub, access.RoleAdmin, nil, "test", true); err != nil {
		t.Fatal(err)
	}
	r.admin = r.cookie(other, time.Hour)
	r.csrf = []string{"Cookie", r.admin, "X-Requested-With", "fairy", "Content-Type", "application/json"}
	r.iss.Set(func(i *testutil.Issuer) { i.Sub = other.Sub })
	_, res = r.approved(t, "POST", "/api/admin/wake-update/"+run.ID+"/confirm", map[string]any{})
	r.iss.Set(func(i *testutil.Issuer) { i.Sub = approvalAdminSub })
	until := run.ConfirmBy.UTC().Format("15:04 UTC")
	msg, _ := res["message"].(string)
	if res["ok"] != false || !strings.HasPrefix(msg, "No machine accepted this confirmation (by ") || !strings.HasSuffix(msg, "). The run is not confirmed and still asks until "+until+": an admin whose passkey the machines take can confirm.") {
		t.Fatalf("what the admin the machine does not take is told: %v", res)
	}
	if lines, _ := res["items"].([]any); len(lines) != 1 || lines[0] != "PC-A: NOT accepted (you are not on its list of people who may approve)" {
		t.Fatalf("the lines: %v", res["items"])
	}
	if st := r.m["pc-a"].Gate.StatusNow().Run; st == nil || st.State != api.RunArmed {
		t.Fatalf("the machine after that: %+v", st)
	}
	r.tick(2)
	cur, _ := r.s.WakeRuns.Snapshot()
	if cur.Approval != "needed" || cur.ConfirmedBy != "" || cur.Tried == "" {
		t.Fatalf("the run: approval %q, confirmed by %q, tried %q", cur.Approval, cur.ConfirmedBy, cur.Tried)
	}
	// the strip still asks, and says who tried
	r.admin, r.csrf = ann, annCSRF
	w := r.req("GET", "/api/admin/wake-update/waiting", nil)
	if w.body["run"] != run.ID || w.body["tried"] != cur.Tried {
		t.Fatalf("the strip: %s", w.raw)
	}
	for _, e := range r.s.Notes.List("", func(notifications.Event) bool { return true }, notifications.Filter{Limit: 200}).Events {
		if e.Kind == wuConfirmNote+"_over" {
			t.Fatalf("the question was ended: %q", e.Text)
		}
	}
	// the admin the machine takes confirms: the machine is asked for a fresh challenge and accepts
	_, res = r.approved(t, "POST", "/api/admin/wake-update/"+run.ID+"/confirm", map[string]any{})
	if res["ok"] != true || res["message"] != "confirmed on 1 of 1 machine" {
		t.Fatalf("the second confirmation: %v", res)
	}
	if st := r.m["pc-a"].Gate.StatusNow().Run; st == nil || st.State != api.RunConfirmed {
		t.Fatalf("the machine after it: %+v", st)
	}
	cur, _ = r.s.WakeRuns.Snapshot()
	if cur.Approval != "given" || cur.ConfirmedBy == "" || cur.ConfirmedBy == cur.Tried || cur.Tried != "" {
		t.Fatalf("the run: approval %q by %q, tried %q", cur.Approval, cur.ConfirmedBy, cur.Tried)
	}
	if again := r.req("POST", "/api/admin/wake-update/"+run.ID+"/confirm", map[string]any{}); again.code != 409 || !strings.Contains(string(again.raw), "confirmed already") {
		t.Fatalf("a third confirmation: %d %s", again.code, again.raw)
	}
	r.until(t, "the run over", 1500, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a == nil })
	if h := r.host("pc-a"); h.State != wakerun.StOff || !strings.Contains(h.Text, "updated from 16-aaaaaaa to 17-bbbbbbb") {
		t.Fatalf("the report: %s %q", h.State, h.Text)
	}
	r.noTokenAnywhere(t)
}

// The notification that ends the question says why it ended (independent review of 270e7ab, P5): "nobody
// confirmed in time ... switches itself off again" was also said when the only waiting machine had been kept
// on by an admin seconds after it was armed.
func TestTheNotificationThatEndsTheQuestionSaysWhy(t *testing.T) {
	over := func(r *runRig) string {
		t.Helper()
		for _, e := range r.s.Notes.List("", func(notifications.Event) bool { return true }, notifications.Filter{Limit: 200}).Events {
			if e.Kind == wuConfirmNote+"_over" {
				return e.Text
			}
		}
		t.Fatal("no notification ended the question")
		return ""
	}
	t.Run("kept on", func(t *testing.T) {
		r := newRunRig(t, "pc-a")
		if _, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}}); res["ok"] != true {
			t.Fatal(res)
		}
		r.until(t, "asked", 300, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a != nil && a.Approval == "needed" })
		r.admin = r.cookie(controller(), 0)
		r.csrf = []string{"Cookie", r.admin, "X-Requested-With", "fairy", "Content-Type", "application/json"}
		if yes := r.req("POST", "/api/systems/pc-a/wake-update/keep-on", map[string]any{}); yes.code != 200 || yes.body["stays_on"] != true {
			t.Fatalf("keep on: %d %s", yes.code, yes.raw)
		}
		r.until(t, "the run over", 300, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a == nil })
		if got := over(r); got != "no machine waits for the confirmation any more (kept on, or ended): nothing was confirmed; see the run's report" {
			t.Fatalf("the notification: %q", got)
		}
	})
	t.Run("nobody confirmed in time", func(t *testing.T) {
		r := newRunRig(t, "pc-a")
		if _, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}}); res["ok"] != true {
			t.Fatal(res)
		}
		r.until(t, "the question over", 1200, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a != nil && a.Approval == "over" })
		if got := over(r); got != "nobody confirmed in time: not confirmed, not updated. A machine that is armed switches itself off again; see the run's report" {
			t.Fatalf("the notification: %q", got)
		}
	})
}

// The strip's "time left" is counted on the run's steady clock, as the run itself ends its question
// (independent review of 270e7ab, P7): it was confirm_by less the panel's clock of the day, so after that
// clock was set back an hour (a time synchronisation of the panel's host) the strip said that more than an
// hour was left while the run stopped asking after its ten minutes.
func TestTheStripsTimeLeftDoesNotFollowThePanelsClockOfTheDay(t *testing.T) {
	r := newRunRig(t, "pc-a")
	if _, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}}); res["ok"] != true {
		t.Fatal(res)
	}
	r.until(t, "asked", 300, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a != nil && a.Approval == "needed" })
	left := func() float64 {
		v, _ := r.req("GET", "/api/admin/wake-update/waiting", nil).body["left_s"].(float64)
		return v
	}
	if got := left(); got < 598 || got > 600 {
		t.Fatalf("right after the question: left_s %v", got)
	}
	r.tick(100)
	f := r.f
	clock := r.s.Poller.Clock
	for _, shift := range []time.Duration{-time.Hour, time.Hour} {
		r.s.Poller.Clock = func() time.Time { return f.now.Add(shift) } // the clock of the day, set
		if got := left(); got < 497 || got > 500 {
			t.Errorf("the clock of the day set by %s, 100 s into the question: left_s %v", shift, got)
		}
	}
	r.s.Poller.Clock = clock
}

// Two starts in the same instant (independent review of 270e7ab, P8): the one that loses must not take the
// winner's ticket with it. Keeping the token replaces what is kept, and that happened before the run decided
// which start wins. Keeping and starting are one step now: a start that finds a run going touches nothing.
func TestAStartThatLosesDoesNotWipeTheWinnersTicket(t *testing.T) {
	r := newRunRig(t, "pc-a")
	if _, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}}); res["ok"] != true {
		t.Fatal(res)
	}
	run, _ := r.s.WakeRuns.Snapshot()
	held, open := r.s.tickets.held()
	if held != run.ID || open != 1 {
		t.Fatalf("the winner's ticket: %q %d", held, open)
	}
	r.s.tickets.mu.Lock()
	bytes := r.s.tickets.cur.token
	r.s.tickets.mu.Unlock()
	// the second start, past the checks before the passkey as it would be in the same instant
	plan, arm := ticketPlan(t)
	rs := &runStart{id: "00000000000000bb", numbers: r.s.runTimings(), arm: arm}
	g := &approvalGrant{plan: newApprovalPlan(plan), token: ticketToken, issued: r.s.now()}
	got, tooOld, err := r.s.startRun(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "x", BySub: "u9", ID: rs.id, Ticket: &rs.numbers, Checked: map[string]bool{"pc-a": true}}, rs, g)
	var se *wakerun.StartError
	if got != nil || tooOld || !errors.As(err, &se) || !se.Busy {
		t.Fatalf("the second start: %v %v %v", got, tooOld, err)
	}
	if held, open := r.s.tickets.held(); held != run.ID || open != 1 {
		t.Fatalf("after the start that lost, the keeper holds %q %d", held, open)
	}
	for _, b := range bytes[:8] {
		if b == 0 {
			t.Fatal("the winner's token was wiped")
		}
	}
	// and the winner's machine gets its ticket
	r.until(t, "pc-a armed", 300, func() bool { return r.host("pc-a").Arm == wakerun.ArmArmed })
}

// Rule 6 through the route, from the moment the run decides to arm (independent review of 270e7ab): "Stop
// helper" was not refused in the seconds the arm request is on its way (the run had no answer yet, so the
// machine was not "armed" for the panel). The helper is what takes the ticket's wait: it is not stopped from
// the moment the request goes out. Keep on in that moment cancels nothing, and says so.
func TestStopHelperIsRefusedFromTheMomentTheRunArms(t *testing.T) {
	r := newRunRig(t, "pc-a")
	r.b.Mu.Lock()
	inner := r.b.Metrics["pc-a"]
	r.b.Metrics["pc-a"] = func() (jsonx.M, error) { // a Windows PC with its elevated helper running
		m, err := inner()
		if err == nil {
			m["helper"] = jsonx.M{"state": "running", "needed": true, "since": "2026-11-09T10:00:00Z", "features": []any{}}
		}
		return m, err
	}
	r.b.Mu.Unlock()
	fresh := r.cookie(controller(), 0)
	hdr := []string{"Cookie", fresh, "X-Requested-With", "fairy", "Content-Type", "application/json"}
	stop := func() resp {
		return r.do("POST", "/api/systems/pc-a/helper/stop", map[string]any{"confirm": "PC-A"}, hdr...)
	}
	var during, keep *resp
	r.gb.Relay("pc-a", func(path string, body any) (int, jsonx.M) {
		if path == api.PathRunArm && during == nil {
			x, y := stop(), r.do("POST", "/api/systems/pc-a/wake-update/keep-on", map[string]any{}, hdr...)
			during, keep = &x, &y
		}
		return 0, nil // the request goes on to the machine
	})
	if _, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}}); res["ok"] != true {
		t.Fatal(res)
	}
	r.until(t, "pc-a armed", 300, func() bool { return r.host("pc-a").Arm == wakerun.ArmArmed })
	if during == nil {
		t.Fatal("no arm request went out")
	}
	if during.code != 409 || !strings.Contains(string(during.raw), "PC-A is in a run of \\\"Update unpowered hosts\\\" (its ticket is on its way to it)") || !strings.Contains(string(during.raw), "Nothing was sent.") {
		t.Fatalf("Stop helper while the ticket is on its way: %d %s", during.code, during.raw)
	}
	if keep.code != 409 || !strings.Contains(string(keep.raw), "PC-A is being handed the run's ticket this very moment: nothing was cancelled. Press Keep on again in a few seconds.") {
		t.Fatalf("Keep on while the ticket is on its way: %d %s", keep.code, keep.raw)
	}
	// armed: refused through the route as well
	if x := stop(); x.code != 409 || !strings.Contains(string(x.raw), "(armed") {
		t.Fatalf("Stop helper while it is armed: %d %s", x.code, x.raw)
	}
	if n := len(r.sent("pc-a", api.PathHelperStop)); n != 0 {
		t.Fatalf("%d helper stops were sent", n)
	}
	if n := len(r.sent("pc-a", "/power")); n != 0 {
		t.Fatalf("%d cancels were sent", n)
	}
}
