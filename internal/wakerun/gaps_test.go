package wakerun

import (
	"strings"
	"testing"
	"time"
)

// Tests for lines of the checked-machine steps that nothing held in place (independent review of 270e7ab: its
// mutants of these lines survived).

func none(t *testing.T, calls string, words ...string) {
	t.Helper()
	for _, x := range words {
		if strings.Contains(" "+calls+" ", " "+x+" ") {
			t.Fatalf("the run sent %q: %q", x, calls)
		}
	}
}

// A machine that shows this run as "confirmed" without a confirmation of the panel's that could have reached
// it is NOT taken for confirmed: the run sends it no update and no shutdown. (Only a confirmation that got no
// answer is settled by the machine's word.)
func TestAMachineThatShowsConfirmedWithoutAConfirmationOfThePanelsGetsNothing(t *testing.T) {
	w, pc, _ := armedRun(t, nil)
	c := w.bootOf(pc)
	c.state, c.withinEnds = BlockConfirmed, w.mono+20*time.Minute
	w.advance(2 * time.Minute)
	act, _ := w.m.Snapshot()
	if h := host(act, "pc-a"); h.Confirm != "" || h.State != StApproval {
		t.Fatalf("pc-a: confirm %q, %s %q", h.Confirm, h.State, h.Text)
	}
	done := w.finish()
	none(t, w.callsFor("pc-a"), "go", "update", "shutdown")
	if h := host(done, "pc-a"); h.Confirm == ConfirmOK || h.VersionAfter != "" {
		t.Fatalf("the report: confirm %q, %q", h.Confirm, h.Text)
	}
}

// What came of a confirmation is taken in while the run waits for it (confirmingMax). An answer that comes
// after the run stopped waiting changes nothing: the machine's own word decides from there on.
func TestALateAnswerToAConfirmationIsNotTakenIn(t *testing.T) {
	w, _, act := armedRun(t, nil)
	w.waitFor(10*time.Minute, "the run asks", func(r *Run) bool { return r.Approval == "needed" })
	if _, err := w.m.BeginConfirm(act.ID, "Ann", []string{"pc-a"}); err != nil {
		t.Fatal(err)
	}
	w.advance(confirmingMax + 5*time.Second)
	cur, _ := w.m.Snapshot()
	if h := host(cur, "pc-a"); h.Confirm != ConfirmUnanswered || h.ConfirmText != "the panel did not learn what came of it" {
		t.Fatalf("after the run stopped waiting: %q %q", h.Confirm, h.ConfirmText)
	}
	w.m.Confirmed(act.ID, "pc-a", ConfirmResult{OK: true}) // too late: the run no longer goes by it
	w.advance(5 * time.Second)
	cur, _ = w.m.Snapshot()
	if h := host(cur, "pc-a"); h.Confirm != ConfirmUnanswered || cur.Approval == "given" {
		t.Fatalf("a late answer was taken in: confirm %q, approval %q", h.Confirm, cur.Approval)
	}
	done := w.finish()
	none(t, w.callsFor("pc-a"), "update", "shutdown")
	has(t, host(done, "pc-a"), StOff, "Its confirmation got no answer (the panel did not learn what came of it), and its agent did not show the run as confirmed: not confirmed, not updated. It switched itself off again.")
}

// One answer that names no run does not disarm a machine for the run (an answer can be from the moment its
// checker starts): two in a row do.
func TestOneAnswerWithoutTheRunDoesNotDisarm(t *testing.T) {
	w, pc, _ := armedRun(t, nil)
	c := w.bootOf(pc)
	w.advance(10 * time.Second)
	kept, hidden := *c, false
	seen := w.Observe("pc-a").SeenAt
	w.afterTick = func() {
		switch o := w.Observe("pc-a"); {
		case !hidden:
			hidden = true
			c.run, c.state = "", "" // its next answer names no run
		case o.Run == nil && !o.SeenAt.Equal(seen) && c.state == "":
			c.run, c.state = kept.run, kept.state // and the one after it names the run again
		}
	}
	w.advance(30 * time.Second)
	w.afterTick = nil
	act, _ := w.m.Snapshot()
	h := host(act, "pc-a")
	if strings.Contains(logOf(h), "not armed any more") || !strings.HasPrefix(h.Text, "awake and armed: it switches itself off at ") {
		t.Fatalf("one answer without the run disarmed it: %q\n%s", h.Text, logOf(h))
	}
	if a := named(w.m.MayGoOff(w.now), "pc-a"); a == nil || !a.Sure {
		t.Fatalf("the strip's list: %+v", a)
	}
	done := w.finish()
	has(t, host(done, "pc-a"), StOff, "It switched itself off again.")
}

// A run that is cancelled while a confirmation is on its way to a machine sends that machine no cancel before
// it knows what came of the confirmation.
func TestACancelWaitsForAConfirmationThatIsOnItsWay(t *testing.T) {
	w, _, act := armedRun(t, nil)
	w.waitFor(10*time.Minute, "the run asks", func(r *Run) bool { return r.Approval == "needed" })
	if _, err := w.m.BeginConfirm(act.ID, "Ann", []string{"pc-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.m.CancelRun(act.ID, "Ann", false); err != nil {
		t.Fatal(err)
	}
	w.advance(20 * time.Second)
	none(t, w.callsFor("pc-a"), "cancel")
	cur, _ := w.m.Snapshot()
	if h := host(cur, "pc-a"); cur == nil || h.Final() || h.State != StApproval {
		t.Fatalf("while the confirmation is on its way: %s %q", h.State, h.Text)
	}
	// its answer: refused. Now the cancel's "leave on" calls its switch-off off.
	w.m.Confirmed(act.ID, "pc-a", ConfirmResult{Reason: "user", Text: "run refused (user)"})
	done := w.finish()
	if !strings.Contains(" "+w.callsFor("pc-a")+" ", " cancel ") {
		t.Fatalf("after the answer no cancel was sent: %q", w.callsFor("pc-a"))
	}
	has(t, host(done, "pc-a"), StLeftOn, "The run was cancelled by Ann: its switch-off was called off")
}

// A machine the run lets go of while it may still switch itself off is marked so, and the strip's list goes on
// naming it after the run: here the run's cancel never gets through.
func TestAMachineTheRunLetsGoOfArmedIsStillNamed(t *testing.T) {
	w, pc, act := armedRun(t, nil)
	pc.chk.cancelSays = "http 500 from its agent"
	if _, err := w.m.CancelRun(act.ID, "Ann", false); err != nil {
		t.Fatal(err)
	}
	var let *Run
	for i := 0; i < 3600 && let == nil; i++ {
		w.tick()
		if cur, hist := w.m.Snapshot(); cur != nil && strings.Contains(host(cur, "pc-a").Text, "Fairy no longer watches it") {
			let = cur
		} else if cur == nil && len(hist) > 0 {
			let = hist[0]
		}
	}
	h := host(let, "pc-a")
	if h == nil || !h.MayGoOff || !strings.Contains(h.Text, "its switch-off could not be called off (http 500 from its agent)") {
		t.Fatalf("the machine the run let go of: may_go_off=%v %q", h.MayGoOff, h.Text)
	}
	if a := named(w.m.MayGoOff(w.now), "pc-a"); a == nil || !a.Sure {
		t.Fatalf("the strip's list after the run let go of it: %+v", a)
	}
}
