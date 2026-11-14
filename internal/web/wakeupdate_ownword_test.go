package web

import (
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
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
