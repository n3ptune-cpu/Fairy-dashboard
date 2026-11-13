package wakerun

import (
	"strings"
	"testing"
	"time"
)

// Audit 6, r4-p2-02. The run's shutdown request gets no answer in the panel's time (the link is down for some
// seconds: the request sits in a send queue and is sent again), and reaches the agent AFTER the panel stopped
// waiting. The polls are answered meanwhile and show no countdown yet. After two of them the run wrote "The
// shutdown request got no answer, and its agent shows no countdown: the request did not arrive. It is ON." and
// let go. The request arrived, the agent counted down, and the machine went off with nobody watching.
//
// Now an answer without a countdown says only that the request has not arrived so far. The run goes on
// watching; a countdown that shows late is its own, counted from when it showed; and when none ever shows, the
// machine ends with its power "not known" and a sentence that says what may still happen.

// lateShutdown is one machine whose shutdown request reaches its agent late.
func lateShutdown(t *testing.T, late time.Duration) (*world, *machine, *Run) {
	t.Helper()
	w := newWorld(t)
	pc := w.off("pc-a")
	pc.shutLost, pc.shutLate = "late", late
	r := w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	return w, pc, r
}

func TestAShutdownRequestThatArrivesLateIsTheRunsShutdown(t *testing.T) {
	for _, late := range []time.Duration{time.Second, 3 * time.Second, 5 * time.Second, 20 * time.Second, 100 * time.Second, 150 * time.Second, 6 * time.Minute} {
		w, pc, _ := lateShutdown(t, late)
		asked := w.mono
		r := w.finish()
		h := host(r, "pc-a")
		has(t, h, StOff, "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.")
		if h.On != OnNo || pc.on || !pc.wentOff {
			t.Errorf("%v late: power %q, the machine on: %v: %s", late, h.On, pc.on, h.Text)
		}
		// "off" is said after the countdown that began when the request arrived, not after the one the run had
		// counted from its own request: the machine went off "late" plus two minutes after the request
		if off := asked + late + 120*time.Second; w.mono < off+20*time.Second || w.mono > off+30*time.Second {
			t.Errorf("%v late: the run ended %v after the machine went off, want 20 to 30 s", late, w.mono-off)
		}
		if got := w.callsFor("pc-a"); got != "wake update shutdown" {
			t.Errorf("%v late: requests %q", late, got)
		}
		if late > time.Second && !logHas(h, "the shutdown request arrived late. It switches off by about ") {
			t.Errorf("%v late: the history does not say that the request arrived late: %v", late, logTexts(h))
		}
		if strings.Contains(strings.Join(logTexts(h), "\n"), "did not arrive") {
			t.Errorf("%v late: the history says that the request did not arrive: %v", late, logTexts(h))
		}
	}
}

// The request never arrives, and the machine answers all along: the run watches until five minutes after the
// time the countdown would have ended, and then says what it knows. Not "it did not arrive", not "it is ON" as
// the last word: the power is "not known" in the record.
func TestAShutdownRequestThatNeverShowsEndsAsNotKnown(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	pc.shutLost = "never"
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	asked := w.mono
	h := host(w.finish(), "pc-a")
	want := "The shutdown request got no answer, and its agent has shown no countdown since (7 min). The request may still arrive: the machine then switches off 2 min later " +
		"unless someone cancels that on its page. It is on now, and Fairy no longer watches it. It was updated to 17-bbbbbbb."
	if h.State != StLeftOn || h.On != OnUnknown || h.Text != want || !pc.on {
		t.Errorf("[%s] power %q, the machine on: %v: %s", h.State, h.On, pc.on, h.Text)
	}
	if waited := w.mono - asked; waited < 7*time.Minute || waited > 7*time.Minute+5*time.Second {
		t.Errorf("the run let go %v after the request, want the countdown (2 min) and 5 min", waited)
	}
	if got := w.callsFor("pc-a"); got != "wake update shutdown" { // and the request is not sent again
		t.Errorf("requests %q", got)
	}
}

// A request that arrives after the run has let go (ten minutes late): the report had said that it may, and what
// happens then. It did not say "ON".
func TestAShutdownRequestThatArrivesAfterTheRunLetGo(t *testing.T) {
	w, pc, _ := lateShutdown(t, 10*time.Minute)
	h := host(w.finish(), "pc-a")
	if h.State != StLeftOn || h.On != OnUnknown || !strings.Contains(h.Text, "The request may still arrive: the machine then switches off 2 min later") {
		t.Errorf("[%s] power %q: %s", h.State, h.On, h.Text)
	}
	w.advance(10 * time.Minute)
	if pc.on {
		t.Fatal("the set-up: the request must have arrived and switched the machine off")
	}
}

// The link is down when the request leaves and for 40 s after: no answer shows anything meanwhile, and the
// request arrives when the link is back. The first answer already shows the countdown; it ends two minutes
// after that, and the run's own count (from its request) would be 28 s short.
func TestAShutdownRequestThatArrivesWhenTheLinkIsBack(t *testing.T) {
	w, pc, _ := lateShutdown(t, time.Second)
	pc.unreachable = true
	w.advance(40 * time.Second)
	pc.unreachable = false
	r := w.finish()
	h := host(r, "pc-a")
	has(t, h, StOff, "and switched off again.")
	if h.On != OnNo || pc.on {
		t.Errorf("power %q, the machine on: %v: %s", h.On, pc.on, h.Text)
	}
}

// A node, and a guest that starts on it after the late request arrived: the run still asks about the guests,
// until 15 s before the countdown that began with the arrival ends, and calls it off. (The run had let go
// before the request arrived.)
func TestAGuestStartsOnANodeWhoseShutdownRequestArrivedLate(t *testing.T) {
	for _, late := range []time.Duration{20 * time.Second, 100 * time.Second} {
		w, pc, _ := lateShutdown(t, late)
		w.advance(late + 60*time.Second) // a minute into the countdown that the late request started
		pc.guests, pc.guestNames = 1, "win11"
		h := host(w.finish(), "pc-a")
		has(t, h, StLeftOn, "Left ON: 1 guest is running on it (win11) since its shutdown was asked for: its shutdown countdown was called off.")
		w.advance(10 * time.Minute)
		if !pc.on || pc.wentOff || h.On != OnYes {
			t.Errorf("%v late: the node went off under its guest (on: %v, power %q): %s", late, pc.on, h.On, h.Text)
		}
	}
}

// "Cancel run" and the kill switch while the run watches a request that has not arrived yet. The agent answers
// the cancel with "nothing is pending", and the run used to let go on that: "... none is pending. It came
// online after the wake and it is ON." The request arrived after it, and the machine went off. Now the cancel
// is asked again every 10 s for as long as the request can still arrive, and the countdown that shows is called
// off.
func TestTheCancelIsAskedAgainWhileALostShutdownRequestCanStillArrive(t *testing.T) {
	for _, c := range []struct {
		name string
		stop func(w *world, r *Run)
		why  string
	}{
		{"Cancel run", func(w *world, r *Run) { _, _ = w.m.CancelRun(r.ID, "Bob", false) }, "The run was cancelled by Bob"},
		{"the kill switch", func(w *world, _ *Run) { w.blocked = "control is disabled by an administrator" }, "the run stopped (control is disabled by an administrator)"},
	} {
		t.Run(c.name+", the request arrives 39 s later", func(t *testing.T) {
			w, pc, r := lateShutdown(t, 40*time.Second)
			w.tick() // one answer without a countdown, and then the stop
			c.stop(w, r)
			h := host(w.finish(), "pc-a")
			has(t, h, StLeftOn, c.why+": its shutdown countdown was called off. It came online after the wake and it is ON.")
			w.advance(10 * time.Minute)
			if !pc.on || pc.wentOff || pc.pending != nil || h.On != OnYes {
				t.Errorf("the machine went off after the run was stopped (on: %v, pending %v, power %q): %s", pc.on, pc.pending, h.On, h.Text)
			}
			if n := strings.Count(w.callsFor("pc-a"), "cancel"); n < 4 || n > 6 { // every 10 s; the one after the arrival calls it off
				t.Errorf("%d cancels: %s", n, w.callsFor("pc-a"))
			}
		})
		t.Run(c.name+", the request never arrives", func(t *testing.T) {
			w := newWorld(t)
			pc := w.off("pc-a")
			pc.shutLost = "never"
			r := w.start(StartRequest{Hosts: []string{"pc-a"}})
			untilState(t, w, "pc-a", StShutting)
			asked := w.mono
			w.advance(10 * time.Second)
			c.stop(w, r)
			h := host(w.finish(), "pc-a")
			want := c.why + ": its shutdown request had got no answer, so its agent was asked to call off a countdown, and it answered: none is pending. " +
				"The request may still arrive: the machine then switches off 2 min later unless someone cancels that on its page. It is on now, and Fairy no longer watches it. " +
				"It was updated to 17-bbbbbbb."
			if h.State != StLeftOn || h.On != OnUnknown || h.Text != want || !pc.on {
				t.Errorf("[%s] power %q, the machine on: %v: %s", h.State, h.On, pc.on, h.Text)
			}
			// (Review of fix8, RF8-02: for as long as the request can still arrive, which is as long as the watch
			// itself waits for it: 5 minutes after the run's own due time. It was until the due time, 11 or 12 cancels.)
			if d := w.mono - asked; d < 7*time.Minute || d > 7*time.Minute+5*time.Second {
				t.Errorf("the run let go %v after the request, want the countdown's time (2 min) and 5 min", d)
			}
			if n := strings.Count(w.callsFor("pc-a"), "cancel"); n < 41 || n > 42 { // every 10 s, six a minute
				t.Errorf("%d cancels: %s", n, w.callsFor("pc-a"))
			}
			if w.limited != 0 {
				t.Errorf("the agent refused %d requests for its limit", w.limited)
			}
		})
	}
}

// The update request: its answer is lost too, and the agent gets it late. The run waits a minute for the agent
// to show an update and then goes on ("... its agent reports no update running and no new result"); mayShutDown
// looks at the agent's update state once more right before the shutdown request. What was open is a request
// that arrives after that, during the countdown: the machine went off in the middle of the install.
func TestAnUpdateRequestThatArrivesLate(t *testing.T) {
	for _, c := range []struct {
		name  string
		late  time.Duration
		takes time.Duration
		state string
		want  string
		calls string
	}{
		{"30 s late: the run sees the update and waits for its result", 30 * time.Second, 30 * time.Second, StOff,
			"Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.", "wake update shutdown"},
		{"110 s late, during the settle time: no shutdown is asked for while it installs", 110 * time.Second, 5 * time.Minute, StLeftOn,
			"Left ON: its agent says that an update is being installed on it right now (downloading).", "wake update"},
		{"150 s late, 30 s into the countdown: the countdown is called off", 150 * time.Second, 5 * time.Minute, StLeftOn,
			"Left ON: its update request, which had got no answer, arrived after all, and its agent says that an update is being installed on it right now (downloading): " +
				"its shutdown countdown was called off. It came online after the wake and it is ON. Its update was asked for and may still be running; the agent finishes it by itself.",
			"wake update shutdown cancel"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			pc := w.off("pc-a")
			pc.updLost, pc.updLate, pc.updTakes = "late", c.late, c.takes
			w.start(StartRequest{Hosts: []string{"pc-a"}})
			h := host(w.finish(), "pc-a")
			has(t, h, c.state, c.want)
			if got := w.callsFor("pc-a"); got != c.calls {
				t.Errorf("requests %q, want %q", got, c.calls)
			}
			if c.state == StLeftOn && (!pc.on || pc.wentOff || h.On != OnYes) {
				t.Errorf("the machine went off while it installed (on: %v, power %q): %s", pc.on, h.On, h.Text)
			}
		})
	}
}
