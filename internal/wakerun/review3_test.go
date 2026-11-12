package wakerun

import (
	"testing"
	"time"
)

// The second review of the batch fix6 (reviews/panel-fixes-fix6), the reviewer's own tests, taken over with
// what they must show now. They use the test world like every other test here, and its judge.

// F2: "a cancel the agent accepted before the due time counts as on at once" was measured with the time of
// the tick that SENT the cancel, not with the time its answer came. "Cancel run" is pressed 3 s before the
// countdown ends; the request takes 6 s to get its answer (a slow link; the limit is 12 s). The countdown ran
// out meanwhile: the machine is going down, and its agent still answers the cancel with 200 (the real agent
// answers 200 also when nothing is pending any more). The report said "its shutdown countdown was called off
// ... and it is ON" for a machine that is off. An answer that comes after the due time says nothing about
// the machine: the last look decides, and it sees the machine go quiet.
func TestACancelSentBeforeTheCountdownEndsAndAnsweredAfterItIsNotOn(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	pc.cancelOK = true
	r := w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	for i := 0; i < 5; i++ { // the countdown is seen
		w.tick()
	}
	due := *host(active(w), "pc-a").ShutdownAt
	for w.now.Before(due.Add(-3 * time.Second)) {
		w.tick()
	}
	w.during = func(call string) {
		if call != "cancel pc-a" {
			return
		}
		for i := 0; i < 6; i++ { // the request is on its way for 6 s; the machines and the poller go on
			w.clock.Lock()
			w.now = w.now.Add(time.Second)
			w.mono += time.Second
			w.clock.Unlock()
			w.sim()
			w.poll()
		}
	}
	if _, err := w.m.CancelRun(r.ID, "Bob", false); err != nil {
		t.Fatal(err)
	}
	h := host(w.finish(), "pc-a")
	t.Logf("really on: %v; report %s/%s: %s", pc.on, h.State, h.On, h.Text)
	if pc.on || !pc.wentOff {
		t.Fatalf("set-up: the countdown did not run out while the cancel was on its way (on %v)", pc.on)
	}
	if h.On == OnYes {
		t.Fatalf("the report says ON for a machine that went off: %s", h.Text)
	}
	has(t, h, StOff, "The run was cancelled by Bob: its shutdown countdown (due at "+clock(due)+") was asked to be called off.", "It is off (not answering)")
	if h.On != OnNo {
		t.Fatalf("power %q: %s", h.On, h.Text)
	}
}
