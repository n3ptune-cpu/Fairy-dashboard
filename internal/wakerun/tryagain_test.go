package wakerun

import (
	"strings"
	"testing"
	"time"
)

// "A run is confirmed once" counts from the first machine that says yes (independent review of 270e7ab, P3).
// An admin whose passkey the machines do not take (he is not on their list) confirms: every machine refuses.
// That used up the run's one confirmation: the admin the machines do take was told "confirmed already", the
// strip was gone, and the machines switched themselves off, not updated, with most of the time left.

func ends(w *world) (asked, over int) {
	for _, e := range w.events {
		switch e.Kind {
		case EvApproval:
			asked++
		case EvApprovalEnd:
			over++
		}
	}
	return
}

func TestAConfirmationNoMachineAcceptsLeavesTheQuestionOpen(t *testing.T) {
	w := newWorld(t)
	a, b := w.checkedOff("pc-a"), w.checkedOff("pc-b")
	a.chk.goSays, b.chk.goSays = "user", "user"
	w.start(StartRequest{Hosts: []string{"pc-a", "pc-b"}, Latest: "17-bbbbbbb"})
	act := w.waitFor(10*time.Minute, "the run asks", func(r *Run) bool { return r.Approval == "needed" })
	by := *act.ConfirmBy
	if begun, err := w.confirm("Bob"); err != nil || len(begun) != 2 {
		t.Fatalf("Bob's confirmation: %v %v", begun, err)
	}
	act, _ = w.m.Snapshot()
	if act.Approval != "needed" || act.ConfirmedBy != "" || act.Tried != "Bob" || !act.ConfirmBy.Equal(by) {
		t.Fatalf("after a confirmation no machine accepted: approval %q, confirmed by %q, tried %q, until %v (was %v)", act.Approval, act.ConfirmedBy, act.Tried, act.ConfirmBy, by)
	}
	if _, over := ends(w); over != 0 {
		t.Fatal("the question was ended")
	}
	for _, id := range []string{"pc-a", "pc-b"} {
		h := host(act, id)
		if h.Confirm != "" || h.State != StApproval || !strings.Contains(logOf(h), "the confirmation by Bob was not accepted (run refused (user)): it can be confirmed again while the run asks") {
			t.Fatalf("%s: confirm %q, %s\n%s", id, h.Confirm, h.State, logOf(h))
		}
	}
	if ws, err := w.m.Waiting(act.ID); err != nil || len(ws) != 2 {
		t.Fatalf("the machines can be confirmed again: %+v %v", ws, err)
	}
	// Ann, whose passkey the machines take
	w.advance(time.Minute)
	a.chk.goSays, b.chk.goSays = "", ""
	if begun, err := w.confirm("Ann"); err != nil || len(begun) != 2 {
		t.Fatalf("Ann's confirmation: %v %v", begun, err)
	}
	act, _ = w.m.Snapshot()
	if act.Approval != "given" || act.ConfirmedBy != "Ann" {
		t.Fatalf("after Ann: %q by %q", act.Approval, act.ConfirmedBy)
	}
	if _, err := w.m.BeginConfirm(act.ID, "Bob", []string{"pc-a"}); err == nil || !strings.Contains(err.Error(), "this run was confirmed already (by Ann): a run is confirmed once") {
		t.Fatalf("a confirmation after one that was accepted: %v", err)
	}
	done := w.finish()
	for _, id := range []string{"pc-a", "pc-b"} {
		has(t, host(done, id), StOff, "updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.")
	}
	if asked, over := ends(w); asked != 1 || over != 1 {
		t.Fatalf("the run asked %d times and ended the question %d times", asked, over)
	}
	if a.chk.gos != 2 || b.chk.gos != 2 {
		t.Fatalf("confirmations sent: %d and %d", a.chk.gos, b.chk.gos)
	}
}

// One machine accepts, one does not: the run is confirmed, by the one who confirmed, and the other machine is
// reported as before; it is not asked again.
func TestAConfirmationOneMachineAcceptsConfirmsTheRun(t *testing.T) {
	w := newWorld(t)
	a, b := w.checkedOff("pc-a"), w.checkedOff("pc-b")
	b.chk.goSays = "user"
	w.start(StartRequest{Hosts: []string{"pc-a", "pc-b"}, Latest: "17-bbbbbbb"})
	w.waitFor(10*time.Minute, "the run asks", func(r *Run) bool { return r.Approval == "needed" })
	if _, err := w.confirm("Bob"); err != nil {
		t.Fatal(err)
	}
	act, _ := w.m.Snapshot()
	if act.Approval != "given" || act.ConfirmedBy != "Bob" || act.Tried != "" || host(act, "pc-b").Confirm != ConfirmRefused {
		t.Fatalf("approval %q by %q, tried %q, pc-b %q", act.Approval, act.ConfirmedBy, act.Tried, host(act, "pc-b").Confirm)
	}
	if _, err := w.m.Waiting(act.ID); err == nil || !strings.Contains(err.Error(), "confirmed already (by Bob)") {
		t.Fatalf("a second confirmation: %v", err)
	}
	done := w.finish()
	has(t, host(done, "pc-a"), StOff, "updated from 16-aaaaaaa to 17-bbbbbbb")
	has(t, host(done, "pc-b"), StOff, "Its confirmation was not accepted (run refused (user)): not confirmed, not updated. It switched itself off again.")
	if a.chk.gos != 1 || b.chk.gos != 1 {
		t.Fatalf("confirmations sent: %d and %d", a.chk.gos, b.chk.gos)
	}
}

// While a confirmation is on its way to the machines, a second one is not begun; and the question does not
// end under it.
func TestNoSecondConfirmationWhileOneIsOnItsWay(t *testing.T) {
	w := newWorld(t)
	w.checkedOff("pc-a")
	w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
	act := w.waitFor(10*time.Minute, "the run asks", func(r *Run) bool { return r.Approval == "needed" })
	if begun, err := w.m.BeginConfirm(act.ID, "Bob", []string{"pc-a"}); err != nil || len(begun) != 1 {
		t.Fatalf("%v %v", begun, err)
	}
	if _, err := w.m.BeginConfirm(act.ID, "Ann", []string{"pc-a"}); err == nil || !strings.Contains(err.Error(), "a confirmation by Bob is being handed to the machines right now") {
		t.Fatalf("a second one while the first is on its way: %v", err)
	}
	if _, err := w.m.Waiting(act.ID); err == nil || !strings.Contains(err.Error(), "a confirmation by Bob is being handed to the machines right now") {
		t.Fatalf("Waiting while one is on its way: %v", err)
	}
	if cur, _ := w.m.Snapshot(); cur.Approval != "needed" || cur.ConfirmedBy != "" {
		t.Fatalf("before any machine answered: %q by %q", cur.Approval, cur.ConfirmedBy)
	}
	w.m.Confirmed(act.ID, "pc-a", ConfirmResult{Reason: "user", Text: "run refused (user)"})
	if cur, _ := w.m.Snapshot(); cur.Approval != "needed" || cur.Tried != "Bob" {
		t.Fatalf("after its refusal: %q, tried %q", cur.Approval, cur.Tried)
	}
	if begun, err := w.m.BeginConfirm(act.ID, "Ann", []string{"pc-a"}); err != nil || len(begun) != 1 {
		t.Fatalf("Ann after Bob's was refused: %v %v", begun, err)
	}
	w.m.Confirmed(act.ID, "pc-a", ConfirmResult{OK: true})
	if cur, _ := w.m.Snapshot(); cur.Approval != "given" || cur.ConfirmedBy != "Ann" || cur.Tried != "" {
		t.Fatalf("after Ann's was accepted: %q by %q, tried %q", cur.Approval, cur.ConfirmedBy, cur.Tried)
	}
}

// Why the question ended is on record (independent review of 270e7ab, P5): "nobody confirmed in time" is said
// only when time was the reason.
func TestWhyTheQuestionEndedIsOnRecord(t *testing.T) {
	t.Run("its time was over", func(t *testing.T) {
		w := newWorld(t)
		w.checkedOff("pc-a")
		w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
		act := w.waitFor(30*time.Minute, "the question over", func(r *Run) bool { return r.Approval == "over" })
		if act.ApprovalEnd != EndTime {
			t.Fatalf("approval_end %q", act.ApprovalEnd)
		}
	})
	t.Run("nobody waits any more: the machine was kept on", func(t *testing.T) {
		w := newWorld(t)
		w.checkedOff("pc-a")
		w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
		w.waitFor(10*time.Minute, "the run asks", func(r *Run) bool { return r.Approval == "needed" })
		if !w.m.KeepOn("pc-a", "Ann") {
			t.Fatal("Keep on")
		}
		w.tick()
		w.tick()
		act, hist := w.m.Snapshot()
		if act == nil {
			act = hist[0]
		}
		if act.Approval != "over" || act.ApprovalEnd != EndNobodyWaits {
			t.Fatalf("approval %q, approval_end %q", act.Approval, act.ApprovalEnd)
		}
	})
	t.Run("the run was cancelled", func(t *testing.T) {
		w := newWorld(t)
		w.checkedOff("pc-a")
		run := w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
		w.waitFor(10*time.Minute, "the run asks", func(r *Run) bool { return r.Approval == "needed" })
		if _, err := w.m.CancelRun(run.ID, "Ann", false); err != nil {
			t.Fatal(err)
		}
		w.tick()
		w.tick()
		act, hist := w.m.Snapshot()
		if act == nil {
			act = hist[0]
		}
		if act.Approval != "over" || act.ApprovalEnd != EndCancelled {
			t.Fatalf("approval %q, approval_end %q", act.Approval, act.ApprovalEnd)
		}
	})
	t.Run("confirmed: no reason for an end without an answer", func(t *testing.T) {
		w := newWorld(t)
		w.checkedOff("pc-a")
		w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
		w.waitFor(10*time.Minute, "the run asks", func(r *Run) bool { return r.Approval == "needed" })
		if _, err := w.confirm("Ann"); err != nil {
			t.Fatal(err)
		}
		if act, _ := w.m.Snapshot(); act.Approval != "given" || act.ApprovalEnd != "" {
			t.Fatalf("approval %q, approval_end %q", act.Approval, act.ApprovalEnd)
		}
	})
}
