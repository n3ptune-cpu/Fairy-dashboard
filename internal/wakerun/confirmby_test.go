package wakerun

import (
	"testing"
	"time"
)

// The time to confirm ends before the first machine can no longer be confirmed (independent review of 270e7ab,
// P2). A ticket's wait counts from each machine's own arming and does not cover the time the other machines
// spend in the queue for their wake (the relay is sent a few requests a minute, one per network card): in a
// run of sixteen machines the first ones began to switch themselves off while the strip still counted down
// "time left". The question now ends confirmMargin before the earliest switch-off of a machine that waits, and
// that is the time the strip counts: until then every waiting machine can be confirmed.
func TestTheTimeToConfirmEndsBeforeTheFirstMachineGoes(t *testing.T) {
	w := newWorld(t)
	var ids []string
	for i := 0; i < 16; i++ {
		id := "pc-" + string(rune('a'+i))
		mc := w.checkedOff(id)
		mc.info.MACs = 2 // a wired and a wireless card: two wake requests each
		ids = append(ids, id)
	}
	w.ms["pc-p"].wakeBoots = false // the last one never comes up: it is given up on after online_timeout_s
	w.start(StartRequest{Hosts: ids, Latest: "17-bbbbbbb"})
	act := w.waitFor(40*time.Minute, "the run asks for the confirmation", func(r *Run) bool { return r.Approval == "needed" })
	asked := w.now
	first := host(act, "pc-a")
	full := time.Duration(act.Ticket.ConfirmWaitS) * time.Second
	want := first.SelfOffAt.Add(-confirmMargin)
	if act.ConfirmBy == nil || act.ConfirmBy.Sub(want).Abs() > 2*time.Second || !act.ConfirmBy.Before(asked.Add(full)) {
		t.Fatalf("asked at %s; confirm_by %v; pc-a switches itself off at %s (so: by %s); the full wait would end at %s",
			clock(asked), act.ConfirmBy, clock(*first.SelfOffAt), clock(want), clock(asked.Add(full)))
	}
	if act.ConfirmForS <= 0 || time.Duration(act.ConfirmForS)*time.Second != act.ConfirmBy.Sub(asked).Round(time.Second) {
		t.Fatalf("confirm_for_s %d, the question is open for %s", act.ConfirmForS, act.ConfirmBy.Sub(asked))
	}
	// an admin who comes with a minute to spare by the strip's own count can confirm every machine that is up
	w.advance(act.ConfirmBy.Sub(w.now) - 60*time.Second)
	cur, _ := w.m.Snapshot()
	if cur == nil || cur.Approval != "needed" {
		t.Fatalf("the question ended early: %s", describe(cur))
	}
	ws, err := w.m.Waiting(cur.ID)
	if err != nil || len(ws) != 15 {
		t.Fatalf("with a minute left %d of the 15 machines that are up can be confirmed (%v)", len(ws), err)
	}
	// and when that time is over, the question is over: nobody is offered a machine that is about to go
	w.advance(62 * time.Second)
	cur, _ = w.m.Snapshot()
	if cur.Approval != "over" {
		t.Fatalf("after confirm_by: %q", cur.Approval)
	}
	if h := host(cur, "pc-a"); h.State != StApproval && h.State != StSelfOff {
		t.Fatalf("pc-a: %s %q", h.State, h.Text)
	}
	done := w.finish()
	has(t, host(done, "pc-a"), StOff, "Nobody confirmed within "+span(time.Duration(act.ConfirmForS)*time.Second)+": not confirmed, not updated. It switched itself off again.")
}

// One machine, or machines that come up together: the question is open for the whole configured wait, as before.
func TestTheTimeToConfirmIsTheConfiguredWaitWhenNoMachineGoesEarlier(t *testing.T) {
	w := newWorld(t)
	w.checkedOff("pc-a")
	w.checkedOff("pc-b")
	w.start(StartRequest{Hosts: []string{"pc-a", "pc-b"}, Latest: "17-bbbbbbb"})
	act := w.waitFor(10*time.Minute, "the run asks", func(r *Run) bool { return r.Approval == "needed" })
	if full := time.Duration(act.Ticket.ConfirmWaitS) * time.Second; !act.ConfirmBy.Equal(w.now.Add(full)) || act.ConfirmForS != act.Ticket.ConfirmWaitS {
		t.Fatalf("confirm_by %v (asked %s, wait %s), confirm_for_s %d", act.ConfirmBy, clock(w.now), full, act.ConfirmForS)
	}
}
