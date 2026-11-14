package web

import (
	"context"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// overNotes are the texts of the notifications that ended a run's question, oldest first.
func (r *runRig) overNotes() (texts []string) {
	for _, e := range r.s.Notes.List("", func(notifications.Event) bool { return true }, notifications.Filter{Limit: 200}).Events {
		if e.Kind == wuConfirmNote+"_over" {
			texts = append([]string{e.Text}, texts...)
		}
	}
	return texts
}

// A confirmation that reaches the machine twice (second look at 4f94383, N2), through the real routes, the
// real run and Fairy-agent's own gate. The machine takes the first copy: it shows the run as confirmed and no
// longer switches itself off. The panel gets its answer to the second copy, 409 already_confirmed. The panel
// left the machine "not confirmed": the row said "it switches itself off unless you confirm", the strip went
// on asking, and in the end the machine was left ON, not updated, with the run's update and shutdown open on
// it. The machine's own word decides: it is confirmed, by the one who confirmed, and it is updated and
// switched off as every confirmed machine is.
func TestAConfirmationThatArrivesTwiceConfirmsTheMachineWhateverThePanelWasAnswered(t *testing.T) {
	r := newRunRig(t, "pc-a")
	if _, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}}); res["ok"] != true {
		t.Fatal(res)
	}
	r.until(t, "asked", 300, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a != nil && a.Approval == "needed" })
	run, _ := r.s.WakeRuns.Snapshot()
	var second jsonx.M
	copies := 0
	r.gb.Relay("pc-a", func(path string, body any) (int, jsonx.M) {
		if path != api.PathRunGo {
			return 0, nil
		}
		r.gb.Relay("pc-a", nil)
		copies++
		if st, data := r.gb.Forward("pc-a", path, body); st != 200 {
			t.Errorf("the first copy of the confirmation was not accepted: %d %v", st, data)
		}
		st, data := r.gb.Forward("pc-a", path, body)
		second = data
		return st, data
	})
	_, res := r.approved(t, "POST", "/api/admin/wake-update/"+run.ID+"/confirm", map[string]any{})
	if copies != 1 || jsonx.S(second, "reason") != api.RunReasonConfirmed {
		t.Fatalf("the machine's answer to the second copy: %v (%d confirmations left the panel)", second, copies)
	}
	if lines, _ := res["items"].([]any); len(lines) != 1 || lines[0] != "PC-A: NOT accepted (it says this run is confirmed already): if a confirmation did arrive, it is confirmed; its row says what its agent shows" {
		t.Fatalf("what the admin is told at once: %v", res)
	}
	st := r.m["pc-a"].Gate.StatusNow().Run
	if st == nil || st.ID != run.ID || st.State != api.RunConfirmed {
		t.Fatalf("the machine is not confirmed: %+v", st)
	}
	r.tick(5) // the machine's own word is read
	cur, _ := r.s.WakeRuns.Snapshot()
	h := r.host("pc-a")
	if cur == nil || cur.Approval != "given" || cur.ConfirmedBy == "" || cur.Tried != "" {
		t.Fatalf("the run: %+v", cur)
	}
	if h.Confirm != wakerun.ConfirmOK || h.State == wakerun.StApproval || strings.Contains(h.Text, "switches itself off") || strings.Contains(h.Text, "unless you confirm") {
		t.Fatalf("the row of a machine that shows the run as confirmed: confirm %q, %s %q", h.Confirm, h.State, h.Text)
	}
	if w := r.req("GET", "/api/admin/wake-update/waiting", nil); w.body["run"] != nil || w.body["armed"] != nil {
		t.Fatalf("the strip still asks, or still names the machine: %s", w.raw)
	}
	if got := r.overNotes(); len(got) != 1 || got[0] != "confirmed by "+cur.ConfirmedBy {
		t.Fatalf("the notifications that ended the question: %q", got)
	}
	if again := r.req("POST", "/api/admin/wake-update/"+run.ID+"/confirm", map[string]any{}); again.code != 409 || !strings.Contains(string(again.raw), "confirmed already") {
		t.Fatalf("another confirmation: %d %s", again.code, again.raw)
	}
	r.until(t, "the run over", 3000, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a == nil })
	h = r.host("pc-a")
	r.b.Mu.Lock()
	on := r.f.on["pc-a"]
	r.b.Mu.Unlock()
	if on || h.State != wakerun.StOff || h.Update != wakerun.UpdOK || !strings.Contains(h.Text, "updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.") {
		t.Fatalf("the report: %s, update %q: %q; the machine is on: %v", h.State, h.Update, h.Text, on)
	}
	if n := askedPaths(r.m["pc-a"], api.PathRunGo); n != 2 { // the two copies of the one confirmation
		t.Fatalf("%d confirmations reached the machine", n)
	}
	r.noTokenAnywhere(t)
}

// A confirmation the panel itself did not send (the machine is no longer in the panel, its binding stands in
// the way, the connection failed) is not one that "left for the machine": the run does not take "confirmed"
// from that machine's agent on the strength of it.
func TestAConfirmationThePanelDidNotSendIsNotOneThatLeft(t *testing.T) {
	r, _ := started(t, "pc-a")
	res := r.s.runGo(context.Background(), "no-such-agent", runGoHost{name: "PC-X"}, nil)
	if res.Sent || res.OK || res.Unanswered || !strings.HasPrefix(res.Text, "nothing was sent: ") {
		t.Fatalf("%+v", res)
	}
}
