package wakerun

import (
	"strings"
	"testing"
	"time"
)

// Between the end of the question (nobody confirmed in time) and the machine's own switch-off there are
// minutes in which the machine is still up and armed. Its row must not go on saying "unless you confirm":
// nobody can confirm it any more. It says that it was not confirmed, when it switches itself off, and what
// still keeps it on.
func TestAnArmedMachineAfterTheQuestionSaysThatNobodyCanConfirmItAnyMore(t *testing.T) {
	w := newWorld(t)
	pc := w.checkedOff("pc-a")
	w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
	asked := w.waitFor(10*time.Minute, "the run asks", func(r *Run) bool { return r.Approval == "needed" })
	if h := host(asked, "pc-a"); !strings.HasPrefix(h.Text, "awake and armed: it switches itself off at about "+clock(*h.SelfOffAt)+" unless you confirm") {
		t.Fatalf("the row while the run asks: %q", h.Text)
	}
	w.waitFor(20*time.Minute, "the question is over", func(r *Run) bool { return r.Approval == "over" })
	w.advance(30 * time.Second)
	act, _ := w.m.Snapshot()
	h := host(act, "pc-a")
	if h.State != StApproval || !pc.on || len(w.m.MayGoOff(w.now)) != 1 {
		t.Fatalf("the machine is to be up and armed for some minutes more: %s on=%v may go off=%v", h.State, pc.on, w.m.MayGoOff(w.now))
	}
	want := "Nobody confirmed within 10 min: not confirmed, not updated. It is armed: it switches itself off at about " + clock(*h.SelfOffAt) + " (Keep on cancels that)"
	if h.Text != want {
		t.Fatalf("the row after the question:\n got %q\nwant %q", h.Text, want)
	}
	if strings.Contains(h.Text, "unless you confirm") {
		t.Fatalf("the row still offers a confirmation: %q", h.Text)
	}
	done := w.finish()
	has(t, host(done, "pc-a"), StOff, "It came online after the wake. Nobody confirmed within 10 min: not confirmed, not updated. It switched itself off again.")
}
