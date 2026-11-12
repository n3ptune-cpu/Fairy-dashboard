package wakerun

import (
	"strings"
	"testing"
	"time"
)

// Recheck of fix7r, RC-04: inside its own countdown the run's cancel called off whatever was pending.
//
// The agent has one cancel, and it calls off the pending action of the panel, whichever that is. The run
// sends it while its own countdown can still be running (R7A-03), without looking at what its agent shows
// (R7A-09, on purpose: a countdown the panel does not see must be called off too). So: a person calls the
// run's shutdown off on the machine's page and asks for a reboot. Then the kill switch goes on, or an admin
// cancels the run, or a guest starts on the node: the run's cancel took the person's reboot away, and the
// report said "its shutdown countdown was called off".
//
// The cancel is left out now when the machine's agent shows another action than a shutdown as pending, in an
// answer that is certainly from after it took the run's shutdown. In every other case it goes out as before.
//
// (Since the commit after this one the run lets go of such a machine at its next look anyway: stepShutting,
// TestAnotherPendingActionEndsTheWatch. What is left for callOff is the look in which the run is stopped or
// cancelled and sees the other action for the first time: in these tests both happen in the same second.)

// stoppers are the three things that make a run call its countdown off.
var stoppers = []struct {
	name string
	do   func(t *testing.T, w *world, r *Run, pc *machine)
	why  string
}{
	{"the kill switch", func(_ *testing.T, w *world, _ *Run, _ *machine) {
		w.blocked = "control is disabled by an administrator"
	},
		"the run stopped (control is disabled by an administrator)"},
	{"Cancel run", func(t *testing.T, w *world, r *Run, _ *machine) {
		if _, err := w.m.CancelRun(r.ID, "Bob", false); err != nil {
			t.Fatal(err)
		}
	}, "The run was cancelled by Bob"},
	{"a guest starts", func(_ *testing.T, _ *world, _ *Run, pc *machine) { pc.guests, pc.guestNames = 1, "vm-1" },
		"Left ON: 1 guest is running on it (vm-1) since its shutdown was asked for"},
}

func TestTheRunsCancelLeavesAnotherPendingActionAlone(t *testing.T) {
	for _, action := range []string{"reboot", "sleep"} {
		for _, s := range stoppers {
			w := newWorld(t)
			pc := w.off("pc-a")
			r := w.start(StartRequest{Hosts: []string{"pc-a"}})
			untilState(t, w, "pc-a", StShutting)
			w.advance(30 * time.Second) // the run has seen its countdown
			// a person, on the machine's page: the run's shutdown is called off, and another action asked for
			at := w.now.Add(30 * time.Minute)
			pc.pending = &Pending{Action: action, At: at}
			s.do(t, w, r, pc) // in the same second: the run's next look is the first that shows it
			h := host(over(t, w), "pc-a")
			if strings.Contains(w.callsFor("pc-a"), "cancel") || pc.pending == nil || pc.pending.Action != action {
				t.Errorf("%s, a %s is pending: the run sent a cancel (requests: %s); the %s a person asked for is still pending: %v",
					s.name, action, w.callsFor("pc-a"), action, pc.pending != nil)
			}
			want := s.why + ": another power action is pending on it now (" + action + " at " + clock(at) + ", by the machine's own clock), not the shutdown the run asked for, " +
				"so the run sent no cancel: its agent's cancel would call that one off. It came online after the wake and it is ON now. Fairy no longer watches it. " +
				"It was updated to 17-bbbbbbb."
			if s.name == "a guest starts" {
				// the watch of the countdown sees the other action before it asks about the guests, and lets go there
				want = "Its shutdown was called off, in the panel or on the machine, and another power action is pending on it now (" + action + " at " + clock(at) +
					", by the machine's own clock). It is ON now. Fairy no longer watches it. It was updated to 17-bbbbbbb."
			}
			if h.State != StLeftOn || h.On != OnYes || h.Text != want || !pc.on {
				t.Errorf("%s, a %s is pending: [%s] power %q, on %v: %s", s.name, action, h.State, h.On, pc.on, h.Text)
			}
			if strings.Contains(h.Text, "its shutdown countdown was called off") {
				t.Errorf("%s: the sentence says that a countdown was called off: %s", s.name, h.Text)
			}
		}
	}
}

// What stays as it was: in every case but that one the cancel goes out, and the machine stays on.
func TestTheRunsCancelIsStillSentInEveryOtherCase(t *testing.T) {
	calledOff := ": its shutdown countdown was called off. It came online after the wake and it is ON. It was updated to 17-bbbbbbb."
	for _, s := range stoppers {
		// its agent shows the run's shutdown
		w := newWorld(t)
		pc := w.off("pc-a")
		r := w.start(StartRequest{Hosts: []string{"pc-a"}})
		untilState(t, w, "pc-a", StShutting)
		w.advance(30 * time.Second)
		s.do(t, w, r, pc)
		h := host(over(t, w), "pc-a")
		if w.callsFor("pc-a") != "wake update shutdown cancel" || pc.pending != nil || !pc.on || h.Text != s.why+calledOff {
			t.Errorf("%s, the run's shutdown is pending: requests %s, on %v: %s", s.name, w.callsFor("pc-a"), pc.on, h.Text)
		}

		// it has not answered since the request (the link dropped right after it): the cancel is tried, as before
		w = newWorld(t)
		pc = w.off("pc-a")
		r = w.start(StartRequest{Hosts: []string{"pc-a"}})
		untilState(t, w, "pc-a", StShutting)
		pc.unreachable = true
		pc.pending = &Pending{Action: "reboot", At: w.now.Add(30 * time.Minute)} // whatever is pending there: the panel has not seen it
		w.advance(10 * time.Second)
		pc.cancelOK = true // the cancel gets through all the same (another way to the agent)
		s.do(t, w, r, pc)
		over(t, w)
		if !strings.Contains(w.callsFor("pc-a"), "cancel") {
			t.Errorf("%s, no answer since the request: no cancel was sent (%s)", s.name, w.callsFor("pc-a"))
		}

		// it showed the other action once, before the run had seen its own countdown (one such answer is not
		// sure), and its latest poll failed: what is pending now is not known, the cancel is tried
		w = newWorld(t)
		pc = w.off("pc-a")
		r = w.start(StartRequest{Hosts: []string{"pc-a"}})
		untilState(t, w, "pc-a", StShutting)
		pc.pending = &Pending{Action: "reboot", At: w.now.Add(30 * time.Minute)}
		w.tick()
		pc.unreachable, pc.cancelOK = true, true
		w.tick()
		s.do(t, w, r, pc)
		over(t, w)
		if !strings.Contains(w.callsFor("pc-a"), "cancel") {
			t.Errorf("%s, the other action was seen once and the latest poll failed: no cancel was sent (%s)", s.name, w.callsFor("pc-a"))
		}
	}
}

// One answer that shows another action is not enough when the run has never seen its own countdown: it can
// come from a poll that was on its way when the shutdown was asked for, and tell of the time before. The
// cancel goes out then, as before; with the next answer it is left out.
func TestOneAnswerWithAnotherActionIsNotEnoughBeforeTheCountdownWasSeen(t *testing.T) {
	for _, wait := range []int{0, 1} { // Cancel run in the second of the first such answer, and a second later
		w := newWorld(t)
		pc := w.off("pc-a")
		r := w.start(StartRequest{Hosts: []string{"pc-a"}})
		untilState(t, w, "pc-a", StShutting)
		// in the second of the run's request, before any poll showed its countdown: called off, and a reboot asked for
		pc.pending = &Pending{Action: "reboot", At: w.now.Add(30 * time.Minute)}
		for i := 0; i < wait; i++ {
			w.tick()
		}
		if _, err := w.m.CancelRun(r.ID, "Bob", false); err != nil {
			t.Fatal(err)
		}
		h := host(over(t, w), "pc-a")
		sent := strings.Contains(w.callsFor("pc-a"), "cancel")
		switch {
		case wait == 0 && (!sent || !strings.Contains(h.Text, "The run was cancelled by Bob: its shutdown countdown was called off.")):
			t.Errorf("one answer with a reboot, the countdown never seen: the cancel must go out as before (requests %s): %s", w.callsFor("pc-a"), h.Text)
		case wait == 1 && (sent || pc.pending == nil || !strings.Contains(h.Text, "another power action is pending on it now (reboot at ")):
			t.Errorf("two answers with a reboot: the cancel must be left out (requests %s, still pending %v): %s", w.callsFor("pc-a"), pc.pending != nil, h.Text)
		}
	}
}

// A shutdown request that got no answer, and whose countdown the run never saw: the run does not know that it
// arrived, nor when, and it can still arrive. Its cancel is sent whatever its agent shows (R7A-09).
func TestAfterAShutdownRequestWithoutAnAnswerTheCancelIsSentWhateverIsPending(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	pc.shutLost = "never"
	r := w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	pc.pending = &Pending{Action: "reboot", At: w.now.Add(30 * time.Minute)}
	w.advance(10 * time.Second) // ten answers that show the reboot
	if _, err := w.m.CancelRun(r.ID, "Bob", false); err != nil {
		t.Fatal(err)
	}
	h := host(over(t, w), "pc-a")
	if !strings.Contains(w.callsFor("pc-a"), "cancel") || !strings.Contains(h.Text, "its shutdown request had got no answer, so its agent was asked to call off a countdown") {
		t.Errorf("requests %s: %s", w.callsFor("pc-a"), h.Text)
	}
}

// A cancel that is being tried again stops when the other action shows: the sentence says how many were sent.
func TestACancelThatIsTriedAgainStopsWhenAnotherActionShows(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	pc.cancelLost = "always arrives" // every answer is lost; the first cancel calls the countdown off
	r := w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	w.advance(30 * time.Second)
	if _, err := w.m.CancelRun(r.ID, "Bob", false); err != nil {
		t.Fatal(err)
	}
	w.advance(3 * time.Second) // the first cancel left, got no answer, and arrived
	at := w.now.Add(30 * time.Minute)
	pc.pending = &Pending{Action: "reboot", At: at} // a person, on the machine's page
	h := host(over(t, w), "pc-a")
	if n := strings.Count(w.callsFor("pc-a"), "cancel"); n != 1 || pc.pending == nil {
		t.Errorf("%d cancel(s) sent, want 1; the reboot is still pending: %v", n, pc.pending != nil)
	}
	want := "The run was cancelled by Bob: another power action is pending on it now (reboot at " + clock(at) + ", by the machine's own clock), not the shutdown the run asked for, " +
		"so the run sent no further cancel (1 did not get through): its agent's cancel would call that one off. It came online after the wake and it is ON now. " +
		"Fairy no longer watches it. It was updated to 17-bbbbbbb."
	if h.State != StLeftOn || h.On != OnYes || h.Text != want {
		t.Errorf("[%s] power %q: %s", h.State, h.On, h.Text)
	}
}
