package wakerun

import (
	"strings"
	"testing"
	"time"
)

// Review of fix8, RF8-02. A shutdown request that got no answer is watched until 5 minutes after the time it
// would have been due, because it can still arrive (audit 6, r4-p2-02). "Cancel run" or the kill switch in
// those 5 minutes found the run's due time over: no cancel was sent, the record said power "on" ("... so the
// run sent no cancel. It came online after the wake and it is ON now."), the request arrived and the machine
// went off, under the kill switch too. Now the cancel is asked for as long as the watch itself would wait.

var stops = []struct {
	name string
	stop func(w *world, r *Run)
	why  string
}{
	{"Cancel run", func(w *world, r *Run) { _, _ = w.m.CancelRun(r.ID, "Bob", false) }, "The run was cancelled by Bob"},
	{"the kill switch", func(w *world, _ *Run) { w.blocked = "control is disabled by an administrator" }, "the run stopped (control is disabled by an administrator)"},
}

// The reviewer's case: the request arrives 200 s late, the stop comes 150 s after the request (30 s after the
// run's own due time).
func TestAStopAfterTheDueTimeStillCallsOffALateShutdownRequest(t *testing.T) {
	for _, c := range stops {
		for _, late := range []time.Duration{200 * time.Second, 300 * time.Second, 410 * time.Second} {
			w, pc, r := lateShutdown(t, late)
			w.advance(150 * time.Second)
			c.stop(w, r)
			h := host(w.finish(), "pc-a")
			has(t, h, StLeftOn, c.why+": its shutdown countdown was called off. It came online after the wake and it is ON.")
			w.advance(15 * time.Minute)
			if !pc.on || pc.wentOff || pc.pending != nil || h.On != OnYes {
				t.Errorf("%s, %v late: the machine went off after the run was stopped (on: %v, pending %v, power %q): %s", c.name, late, pc.on, pc.pending, h.On, h.Text)
			}
			if w.limited != 0 {
				t.Errorf("%s: the agent refused %d requests for its limit", c.name, w.limited)
			}
		}
	}
}

// The request never arrives: the run asks until 5 minutes after its own due time, hears "nothing is pending"
// each time, and ends the machine as "not known" with what may still happen. Never "it is ON now", power "on".
func TestAStopAfterTheDueTimeOfARequestThatNeverArrivesIsNotKnown(t *testing.T) {
	for _, c := range stops {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.shutLost = "never"
		r := w.start(StartRequest{Hosts: []string{"pc-a"}})
		untilState(t, w, "pc-a", StShutting)
		asked := w.mono
		w.advance(150 * time.Second)
		c.stop(w, r)
		h := host(w.finish(), "pc-a")
		want := c.why + ": its shutdown request had got no answer, so its agent was asked to call off a countdown, and it answered: none is pending. " +
			"The request may still arrive: the machine then switches off 2 min later unless someone cancels that on its page. It is on now, and Fairy no longer watches it. " +
			"It was updated to 17-bbbbbbb."
		if h.State != StLeftOn || h.On != OnUnknown || h.Text != want || !pc.on {
			t.Errorf("%s: [%s] power %q, the machine on: %v: %s", c.name, h.State, h.On, pc.on, h.Text)
		}
		if d := w.mono - asked; d < 7*time.Minute || d > 7*time.Minute+5*time.Second {
			t.Errorf("%s: the run let go %v after the request, want the countdown (2 min) and 5 min", c.name, d)
		}
		if n := strings.Count(w.callsFor("pc-a"), "cancel"); n < 26 || n > 28 { // every 10 s from the stop on
			t.Errorf("%s: %d cancels: %s", c.name, n, w.callsFor("pc-a"))
		}
		if strings.Contains(strings.Join(logTexts(h), "\n"), "it is ON") || w.limited != 0 {
			t.Errorf("%s: the history says \"it is ON\", or the agent's limit was met (%d): %v", c.name, w.limited, logTexts(h))
		}
	}
}

// A request that was answered is as before: after its due time the run sends no cancel (an action a person
// asked for since would be called off), and says so.
func TestAStopAfterTheDueTimeOfAnAnsweredRequestSendsNoCancel(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	pc.shutMode = "ignore" // the countdown ends and the machine stays on
	r := w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	w.advance(150 * time.Second)
	_, _ = w.m.CancelRun(r.ID, "Bob", false)
	h := host(w.finish(), "pc-a")
	if strings.Contains(w.callsFor("pc-a"), "cancel") || !strings.Contains(h.Text, "after its shutdown countdown had run out") || !strings.Contains(h.Text, "so the run sent no cancel.") {
		t.Errorf("requests %q: %s", w.callsFor("pc-a"), h.Text)
	}
}
