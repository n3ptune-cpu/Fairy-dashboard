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

const lateWakeLeftOn = "Left ON: the run's first wake request for it got no answer, and it was already starting when a later one was accepted, so Fairy cannot tell that the run woke it."

// F1: "a wake request without an answer is not a wake the run knows of" (M1), but once a later request was
// accepted, everything was still measured from the lost one. The first wake request gets no answer and NO
// packet goes out. A person presses the power button during the wait. A minute into the wait the run's second
// wake request is accepted (its packet does nothing: the machine already runs). The agent then answers with
// an uptime that is fresh against the lost request and not against the accepted one: the system started
// BEFORE the only wake the run knows of. The run took it for woken, updated it and switched it off. Now it is
// measured from the first accepted wake: updated and left on.
func TestASystemThatStartedBeforeTheFirstAcceptedWakeIsLeftOn(t *testing.T) {
	for _, c := range []struct {
		name  string
		press func(w *world, pc *machine)
		after time.Duration // into the wait
	}{
		{"button 30 s into the wait: the system is up 20 s before the accepted wake, the agent answers 10 s after it", personPowersOn, 30 * time.Second},
		{"button 22 s into the wait: the agent answers about 2 s after the accepted wake", personPowersOn, 22 * time.Second},
		// the corner the fixer's report named: the system starts after the accepted wake (its uptime is fresh
		// against it too), but its agent answers sooner after it than a machine that was off can
		{"button 57 s into the wait: the system is up a moment after the accepted wake, the agent answers within 8 s of it", func(w *world, pc *machine) {
			pc.on, pc.bootedAt, pc.agentReadyAt = true, w.now.Add(5*time.Second), w.now.Add(9*time.Second)
		}, 57 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			pc := w.off("pc-a")
			pc.wakeLost, pc.wakeBoots = "never", false // first request: no answer, no packet; later packets find a machine that already runs
			w.start(StartRequest{Hosts: []string{"pc-a"}})
			w.advance(c.after)
			c.press(w, pc)
			h := host(w.finish(), "pc-a")
			t.Logf("%s/%s, wakes accepted %d, wake_unconfirmed %v, requests %q, boot uptime %v s, online %v s after the first request: %s",
				h.State, h.On, h.Wakes, h.WakeUnsure, w.callsFor("pc-a"), *h.BootUptimeS, *h.OnlineAfterS, h.Text)
			if pc.shutdowns != 0 || !pc.on {
				t.Fatalf("the run switched off a machine whose system started before the only wake request that was accepted: shutdown requests %d, on %v", pc.shutdowns, pc.on)
			}
			has(t, h, StLeftOn, lateWakeLeftOn+" It was updated to 17-bbbbbbb.")
			if h.On != OnYes || h.Wakes != 1 || !h.WakeUnsure || h.WakeOKAt == nil || !h.WakeOKAt.After(*h.WakeSentAt) || w.callsFor("pc-a") != "wake wake update" {
				t.Fatalf("power %q, wakes accepted %d, wake unconfirmed %v, first accepted at %v, requests %q", h.On, h.Wakes, h.WakeUnsure, h.WakeOKAt, w.callsFor("pc-a"))
			}
			if logHas(h, "the run treats it as woken") || !logHas(h, lateWakeNote) {
				t.Fatalf("the machine's history: %v", h.Log)
			}
		})
	}
}
