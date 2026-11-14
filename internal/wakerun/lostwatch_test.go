package wakerun

import (
	"strings"
	"testing"
	"time"
)

// Fourth review of build 14, F1. The run's shutdown request gets no answer, and no countdown of it has shown.
// A shutdown that shows after that is the run's request, arrived late, when it has the run's length (or its
// agent does not say the length): callOff has gone by that rule since the third review (S1). The watch
// (stepShutting) did not: it took every pending shutdown for the run's own, also one a person asked for on the
// machine's page with another length, and from then on the run believed that it had seen its countdown.

// The reviewer's sequence. The run's request arrives 100 s late. Meanwhile a person asks for a shutdown in 1
// minute on the machine's page and takes it back 10 s later. The run had taken that countdown for its own, so
// "no countdown any more" read as "cancelled": it ended at once with "Its shutdown was cancelled, in the panel
// or on the machine, so it is ON", power "on", and the run's own request then switched the machine off with
// nobody watching. Now the run goes on waiting for its own request, sees its countdown when it arrives, and
// says "switched off again" when the machine has gone.
func TestTheWatchDoesNotTakeAPersonsShutdownForTheRunsAfterALostRequest(t *testing.T) {
	w, pc, _ := lateShutdown(t, 100*time.Second)
	asked := w.mono
	w.advance(20 * time.Second)
	at := w.now.Add(60 * time.Second)
	pc.pending = &Pending{Action: "shutdown", At: at, DelayS: 60} // a person, on the machine's page
	w.advance(10 * time.Second)
	row := host(mustActive(t, w), "pc-a")
	if row.pendingSeen || !row.lostShutdown() {
		t.Fatalf("the run took a shutdown of 1 min for its own of 2 min (countdown seen: %v): %s", row.pendingSeen, row.Text)
	}
	if want := "a shutdown is pending on it now that is not the one the run asked for (a countdown of 60 s, due at " + clock(at) + " by the machine's own clock; the run asked for one of 2 min): " +
		"the run goes on waiting for its own request, which got no answer and may still arrive"; !logHas(row, want) {
		t.Errorf("the history does not say that a shutdown of another length showed: %v", logTexts(row))
	}
	pc.pending = nil // the person takes it back
	w.advance(30 * time.Second)
	mustActive(t, w) // the run has not let go: its own request can still arrive
	h := host(w.finish(), "pc-a")
	has(t, h, StOff, "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.")
	if h.On != OnNo || pc.on || !pc.wentOff {
		t.Errorf("power %q, the machine on: %v, went off: %v: %s", h.On, pc.on, pc.wentOff, h.Text)
	}
	// the machine went off by the run's own request: 100 s late, and its 2 minutes
	if off := asked + 100*time.Second + 120*time.Second; w.mono < off+20*time.Second || w.mono > off+30*time.Second {
		t.Errorf("the run ended %v after the machine went off, want 20 to 30 s", w.mono-off)
	}
	if got := w.callsFor("pc-a"); got != "wake update shutdown" {
		t.Errorf("requests %q", got)
	}
	if n := strings.Count(strings.Join(logTexts(h), "\n"), "that is not the one the run asked for"); n != 1 {
		t.Errorf("the history names the person's shutdown %d times, want once: %v", n, logTexts(h))
	}
}

// The same with a stop while the person's shutdown shows. The run had "seen its countdown", so the stop found
// "a shutdown that is not the run's in the place of the run's", sent no cancel and let go with "it is ON now";
// the person took their shutdown back, the run's request arrived, and the machine went off under a cancelled
// run (under the kill switch too). The request has not arrived, and the run knows that: no cancel while the
// person's shutdown shows, the cancels go on when it is gone, and the countdown that shows is called off.
func TestAStopWhileAPersonsShutdownShowsStillCallsOffTheLateRequest(t *testing.T) {
	for _, c := range stops {
		w, pc, r := lateShutdown(t, 100*time.Second)
		w.advance(20 * time.Second)
		mine := &Pending{Action: "shutdown", At: w.now.Add(60 * time.Second), DelayS: 60}
		pc.pending = mine
		w.advance(5 * time.Second)
		c.stop(w, r)
		w.advance(5 * time.Second)
		if pc.pending != mine || cancels(w) != 0 {
			t.Fatalf("%s: while the person's shutdown showed: it is pending: %v, %d cancel(s)", c.name, pc.pending == mine, cancels(w))
		}
		pc.pending = nil // the person takes it back
		h := host(w.finish(), "pc-a")
		has(t, h, StLeftOn, c.why+": its shutdown countdown was called off. It came online after the wake and it is ON.")
		w.advance(10 * time.Minute)
		if !pc.on || pc.wentOff || pc.pending != nil || h.On != OnYes || cancels(w) == 0 {
			t.Errorf("%s: the machine went off after the run was stopped (on: %v, went off: %v, pending %v, power %q, %d cancels): %s", c.name, pc.on, pc.wentOff, pc.pending, h.On, cancels(w), h.Text)
		}
	}
}

// A person's shutdown that is still pending when the watch lets go (15 minutes; the run waits 7): the machine
// is on, the run's request has not arrived, and the person's shutdown switches the machine off later. The
// sentence names it. (It said "its agent still shows a shutdown as pending" of it, as of a countdown of the
// run's that an agent counts longer; "its agent has shown no countdown since" would be untrue.)
func TestAPersonsShutdownThatIsPendingWhenTheWatchLetsGoIsNamed(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	pc.shutLost = "never"
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	w.advance(20 * time.Second)
	mine := &Pending{Action: "shutdown", At: w.now.Add(15 * time.Minute), DelayS: 900}
	pc.pending = mine
	h := host(w.finish(), "pc-a")
	want := "The shutdown request got no answer, and no countdown of it has shown since (7 min). A shutdown is pending on the machine now that is not the one the run asked for " +
		"(a countdown of 15 min, due at " + clock(mine.At) + " by the machine's own clock; the run asked for one of 2 min): it switches the machine off " +
		"unless someone cancels it on its page. The request may still arrive: the machine then switches off 2 min later unless someone cancels that on its page. " +
		"It is on now, and Fairy no longer watches it. It was updated to 17-bbbbbbb."
	if h.State != StLeftOn || h.On != OnUnknown || h.Text != want {
		t.Errorf("[%s] power %q: %s", h.State, h.On, h.Text)
	}
	if pc.pending != mine || !pc.on || w.callsFor("pc-a") != "wake update shutdown" {
		t.Errorf("the person's shutdown is pending: %v, the machine on: %v, requests %q", pc.pending == mine, pc.on, w.callsFor("pc-a"))
	}
}

// A person's shutdown that runs out while the run waits for its own request: the machine goes off by it, at
// its time. That is not the run's "switched off again" (the run never saw a countdown of its own), and no
// cancel went to it: the power is "not known", and the history says which shutdown was pending.
func TestAPersonsShutdownThatSwitchesTheMachineOffIsNotTheRunsOff(t *testing.T) {
	for _, delayS := range []int{10, 60} {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.shutLost = "never"
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		untilState(t, w, "pc-a", StShutting)
		w.advance(20 * time.Second)
		at := w.now.Add(time.Duration(delayS) * time.Second)
		pc.pending = &Pending{Action: "shutdown", At: at, DelayS: delayS}
		h := host(w.finish(), "pc-a")
		want := "It came online after the wake. Its shutdown request got no answer, Fairy never saw its countdown, and it does not answer now: not confirmed that it is off. It was updated to 17-bbbbbbb."
		if h.State != StLeftOn || h.On != OnUnknown || h.Text != want {
			t.Errorf("%d s: [%s] power %q: %s", delayS, h.State, h.On, h.Text)
		}
		if !pc.wentOff || !pc.offAt.Equal(at) || w.callsFor("pc-a") != "wake update shutdown" {
			t.Errorf("%d s: the machine went off: %v at %s (the person's time: %s), requests %q", delayS, pc.wentOff, clock(pc.offAt), clock(at), w.callsFor("pc-a"))
		}
		if !logHas(h, "a shutdown is pending on it now that is not the one the run asked for (a countdown of "+span(time.Duration(delayS)*time.Second)+", due at "+clock(at)+" by the machine's own clock") {
			t.Errorf("%d s: the history does not name the person's shutdown: %v", delayS, logTexts(h))
		}
	}
}

// noLengthOps is the world with agents that do not say the length of a countdown (delay_s missing: an agent
// from before the field).
type noLengthOps struct{ *world }

func (n noLengthOps) Observe(id string) Observation {
	o := n.world.Observe(id)
	if o.Pending != nil {
		p := *o.Pending
		p.DelayS = 0
		o.Pending = &p
	}
	return o
}

// What stays: the run's own request that arrives late on a machine whose agent does not say the length of its
// countdown. The watch must take that countdown for the run's, or it would never see its own shutdown on such
// an agent: the same rule as everywhere else (ownLength), with the same limit (a person's shutdown on such a
// machine cannot be told from the run's).
func TestTheWatchTakesALateCountdownWithoutALengthForTheRuns(t *testing.T) {
	for _, late := range []time.Duration{3 * time.Second, 100 * time.Second} {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.shutLost, pc.shutLate = "late", late
		w.m.Ops = noLengthOps{w}
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		h := host(w.finish(), "pc-a")
		has(t, h, StOff, "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.")
		if h.On != OnNo || pc.on || !pc.wentOff {
			t.Errorf("%v late: power %q, the machine on: %v: %s", late, h.On, pc.on, h.Text)
		}
	}
}
