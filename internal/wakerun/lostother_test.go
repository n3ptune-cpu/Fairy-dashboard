package wakerun

import (
	"strings"
	"testing"
	"time"
)

// Review of fix8, RF8-03. After a stop ("Cancel run", the kill switch, a guest on a node) with a shutdown
// request that got no answer, the run asks the machine's agent every 10 s to call a countdown off, in case the
// request still arrives. The agent's cancel names no action: a reboot that somebody asked for on the machine's
// page in that time was called off at the next cancel, and the row said "it answered: none is pending". No
// cancel is sent while the agent shows another action than a shutdown as pending, and the row says what was.

func cancels(w *world) int { return strings.Count(w.callsFor("pc-a"), "cancel") }

// lostAndStopped is a machine whose shutdown request never arrives, with the run stopped 5 s after it.
func lostAndStopped(t *testing.T, stop func(w *world, r *Run)) (*world, *machine) {
	t.Helper()
	w := newWorld(t)
	pc := w.off("pc-a")
	pc.shutLost = "never"
	r := w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	w.advance(5 * time.Second)
	stop(w, r)
	return w, pc
}

func TestTheRepeatedCancelLeavesAPersonsOwnActionAlone(t *testing.T) {
	for _, c := range stops {
		for _, action := range []string{"reboot", "sleep"} {
			w, pc := lostAndStopped(t, c.stop)
			w.advance(12 * time.Second)
			sent := cancels(w)
			at := w.now.Add(30 * time.Minute)
			pc.pending = &Pending{Action: action, At: at} // a person, on the machine's page
			called := false
			w.afterTick = func() { called = called || pc.pending == nil }
			h := host(w.finish(), "pc-a")
			if called || pc.pending == nil || pc.pending.Action != action {
				t.Errorf("%s, %s: the run's cancel called off the action a person asked for (requests: %s)", c.name, action, w.callsFor("pc-a"))
			}
			if n := cancels(w); sent < 1 || n != sent {
				t.Errorf("%s, %s: %d cancel(s) before the action showed, %d at the end: none may be sent while it shows", c.name, action, sent, n)
			}
			want := c.why + ": its shutdown request had got no answer, and no countdown of it showed. Another power action was pending on the machine meanwhile (" + action + " at " + clock(at) +
				", by the machine's own clock), which is not the run's: the run sent no cancel while it showed, since its agent's cancel would have called that one off. " +
				"The request may still arrive: the machine then switches off 2 min later unless someone cancels that on its page. Fairy no longer watches it. It was updated to 17-bbbbbbb."
			if h.State != StLeftOn || h.On != OnUnknown || h.Text != want {
				t.Errorf("%s, %s: [%s] power %q: %s", c.name, action, h.State, h.On, h.Text)
			}
			if all := h.Text + strings.Join(logTexts(h)[len(logTexts(h))-2:], " "); strings.Contains(all, "none is pending") || strings.Contains(all, ": nothing is pending") {
				t.Errorf("%s, %s: the end says that nothing was pending: %s", c.name, action, all)
			}
			if !logHas(h, "another power action is pending on it now ("+action+" at ") {
				t.Errorf("%s, %s: the history does not say that another action showed: %v", c.name, action, logTexts(h))
			}
		}
	}
}

// The person takes the reboot back, and the run's request arrives after that: the cancels go on when the other
// action is gone, and the countdown that shows is called off. The reboot was never the run's to call off.
func TestTheCancelsGoOnWhenTheOtherActionIsGone(t *testing.T) {
	w, pc, r := lateShutdown(t, 100*time.Second)
	w.advance(5 * time.Second)
	_, _ = w.m.CancelRun(r.ID, "Bob", false)
	w.advance(12 * time.Second)
	sent := cancels(w)
	pc.pending = &Pending{Action: "reboot", At: w.now.Add(30 * time.Minute)}
	w.advance(40 * time.Second)
	if pc.pending == nil || cancels(w) != sent {
		t.Fatalf("while the reboot showed: pending %v, %d cancels (%d before)", pc.pending, cancels(w), sent)
	}
	pc.pending = nil // the person, on the machine's page
	h := host(w.finish(), "pc-a")
	has(t, h, StLeftOn, "The run was cancelled by Bob: its shutdown countdown was called off. It came online after the wake and it is ON.")
	w.advance(10 * time.Minute)
	if !pc.on || pc.wentOff || h.On != OnYes || cancels(w) <= sent {
		t.Errorf("the machine on: %v, power %q, %d cancels (%d before the reboot): %s", pc.on, h.On, cancels(w), sent, h.Text)
	}
}

// The reviewer's case: a reboot that is due 60 s later. No cancel reaches the machine between the moment it
// shows and its own time.
func TestNoCancelBetweenAPersonsRebootAndItsTime(t *testing.T) {
	w, pc := lostAndStopped(t, stops[0].stop)
	w.advance(12 * time.Second)
	sent := cancels(w)
	pc.pending = &Pending{Action: "reboot", At: w.now.Add(60 * time.Second)}
	for i := 0; i < 58; i++ {
		w.tick()
		if pc.pending == nil || cancels(w) != sent {
			t.Fatalf("%d s after the reboot was asked for: pending %v, %d cancels (%d before): %s", i+1, pc.pending, cancels(w), sent, w.callsFor("pc-a"))
		}
	}
	h := host(w.finish(), "pc-a")
	if h.On != OnUnknown || strings.Contains(h.Text, "none is pending") {
		t.Errorf("power %q: %s", h.On, h.Text)
	}
}

// Third review of build 14, S1. The same for a shutdown of a person's own: the run's request never arrived, the
// run is cancelled and repeats its cancel "in case the request still arrives", and a person asks on the
// machine's page for a shutdown in 1 minute (the Power menu's default; the run's countdown is 2 minutes). Every
// pending shutdown was taken for the run's there, whatever its length: the next cancel called the person's
// shutdown off, and the report said "its shutdown countdown was called off ... it is ON". A shutdown of another
// length than the run asked for is not the run's: no cancel while it shows, and the report says whose it is.
func TestTheRepeatedCancelLeavesAPersonsShutdownOfAnotherLengthAlone(t *testing.T) {
	for _, first := range []string{"while the cancel is being repeated", "before the first cancel"} {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.shutLost = "never"
		r := w.start(StartRequest{Hosts: []string{"pc-a"}})
		untilState(t, w, "pc-a", StShutting)
		w.tick()
		mine := &Pending{Action: "shutdown", At: w.now.Add(60 * time.Second), DelayS: 60}
		if first == "before the first cancel" {
			pc.pending = mine
		}
		if _, err := w.m.CancelRun(r.ID, "Bob", false); err != nil {
			t.Fatal(err)
		}
		if first != "before the first cancel" {
			w.advance(25 * time.Second) // the run asks again every 10 s
			if cancels(w) < 2 {
				t.Fatalf("%s: the fixture: %d cancel(s) so far", first, cancels(w))
			}
			mine.At = w.now.Add(60 * time.Second)
			pc.pending = mine
		}
		sent, at := cancels(w), mine.At
		called := false
		w.afterTick = func() { called = called || (pc.pending == nil && !pc.wentOff) }
		for i := 0; i < 55; i++ { // while the person's countdown runs, nothing is sent to the machine
			w.tick()
			if pc.pending != mine || cancels(w) != sent {
				t.Fatalf("%s: %d s after the person asked: their shutdown is pending: %v, %d cancel(s) (%d before)", first, i+1, pc.pending == mine, cancels(w), sent)
			}
		}
		h := host(w.finish(), "pc-a")
		if called || !pc.wentOff {
			t.Errorf("%s: A PERSON'S SHUTDOWN (1 min; the run asked for 2 min, and its request never arrived) was called off: %s", first, w.callsFor("pc-a"))
		}
		want := "The run was cancelled by Bob: its shutdown request had got no answer, and no countdown of it showed. A shutdown was pending on the machine meanwhile that is not the one the run asked for " +
			"(a countdown of 60 s, due at " + clock(at) + " by the machine's own clock; the run asked for one of 2 min): the run sent no cancel while it showed, since its agent's cancel would have called that one off, " +
			"and that shutdown switches the machine off unless someone cancels it on its page. " +
			"The request may still arrive: the machine then switches off 2 min later unless someone cancels that on its page. Fairy no longer watches it. It was updated to 17-bbbbbbb."
		if h.State != StLeftOn || h.On != OnUnknown || h.Text != want {
			t.Errorf("%s: [%s] power %q: %s", first, h.State, h.On, h.Text)
		}
		if strings.Contains(h.Text, "was called off") || strings.Contains(h.Text, "none is pending") {
			t.Errorf("%s: UNTRUE: %s", first, h.Text)
		}
		if !logHas(h, "a shutdown is pending on it now that is not the one the run asked for (a countdown of 60 s, due at "+clock(at)+" by the machine's own clock; the run asked for one of 2 min): "+
			"no cancel is sent while it shows, since its agent's cancel would call that one off") {
			t.Errorf("%s: the history does not say that a shutdown of another length showed: %v", first, logTexts(h))
		}
	}
}

// What stays: a shutdown of the run's own length that shows after a request without an answer IS the run's
// request, arrived late, and the next cancel calls it off (TestTheCancelsGoOnWhenTheOtherActionIsGone has the
// same without a length). And an agent that does not say the length (an agent from before delay_s): the run
// cannot tell its shutdown from its own and calls it off, as it did before the length was looked at.
func TestAShutdownOfTheRunsLengthOrWithoutALengthIsStillCalledOffAfterALostRequest(t *testing.T) {
	for name, delayS := range map[string]int{"the run's own length": 120, "an agent that does not say the length": 0} {
		w, pc := lostAndStopped(t, stops[0].stop)
		w.advance(12 * time.Second)
		sent := cancels(w)
		pc.pending = &Pending{Action: "shutdown", At: w.now.Add(120 * time.Second), DelayS: delayS}
		h := host(w.finish(), "pc-a")
		if pc.pending != nil || !pc.on || cancels(w) != sent+1 {
			t.Errorf("%s: pending %v, on %v, %d cancel(s) (%d before)", name, pc.pending, pc.on, cancels(w), sent)
		}
		has(t, h, StLeftOn, stops[0].why+": its shutdown countdown was called off. It came online after the wake and it is ON.")
	}
}
