package wakerun

import (
	"strings"
	"testing"
	"time"
)

// The second review of the fixes for audit 5, round 2 (reviews/panel-fixes-r2p), the reviewer's own tests,
// taken over with what they must show now. They use the test world like every other test here, and its
// judge: a shutdown request for a machine for which no wake was accepted fails the test.

const lostWakeLeftOn = "Left ON: the run's wake request for it got no answer, so Fairy cannot tell that the run woke it."

// personPowersOn: the machine starts now by a hand on the power button (or a firmware schedule, or another
// admin's "Power on"): its system is up 10 s later, its agent answers 40 s later.
func personPowersOn(w *world, mc *machine) {
	mc.on, mc.bootedAt, mc.agentReadyAt = true, w.now.Add(10*time.Second), w.now.Add(40*time.Second)
}

// MUST FIX 1 (P4): every wake request of the run gets no answer and no packet ever goes out (the relay died
// behind a kept connection, or a proxy answers for it), and a person switches the machine on 90 s into the
// wait. The run had no wake accepted; it switched the machine off all the same ("Came online after a wake
// request that got no answer, updated ..., and switched off again", wakes_accepted=0). Before the lost answer
// was handled at all, such a machine was never switched off. Now it is updated and left on.
func TestRVP4NoWakeWentOutAndAPersonSwitchesItOn(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(w *world, mc *machine)
		state string
		want  string
		calls string
	}{
		{"every wake request times out; no packet ever", func(w *world, mc *machine) { mc.wakeLost, mc.wakeBoots = "always", false },
			StLeftOn, lostWakeLeftOn + " It was updated to 17-bbbbbbb.", "wake wake wake update"},
		{"the first times out, every later one is refused by the relay; no packet ever", func(w *world, mc *machine) {
			mc.wakeLost, mc.wakeBoots = "never", false
			n := 0
			w.during = func(call string) {
				if call == "wake pc-a" {
					if n++; n == 2 {
						mc.relayDown = "agent unreachable: connection failed"
					}
				}
			}
		}, StLeftOn, lostWakeLeftOn + " It was updated to 17-bbbbbbb.", "wake wake wake update"},
		{"control: the relay refuses from the start", func(w *world, mc *machine) {
			mc.relayDown, mc.wakeBoots = "agent unreachable: connection failed", false
		},
			StGaveUp, "The wake could not be sent: agent unreachable: connection failed. It was not woken.", "wake"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			pc := w.off("pc-a")
			c.setup(w, pc)
			w.start(StartRequest{Hosts: []string{"pc-a"}})
			w.advance(90 * time.Second)
			personPowersOn(w, pc) // 90 s into the wait
			h := host(w.finish(), "pc-a")
			has(t, h, c.state, c.want)
			if pc.wakes != 0 {
				t.Fatal("set-up: a packet went out")
			}
			if pc.shutdowns != 0 || !pc.on || h.Wakes != 0 || w.callsFor("pc-a") != c.calls {
				t.Fatalf("shutdown requests %d, on %v, wakes accepted %d, requests %q: %s", pc.shutdowns, pc.on, h.Wakes, w.callsFor("pc-a"), h.Text)
			}
			if c.state == StLeftOn && (h.On != OnYes || !h.WakeUnsure || strings.Contains(h.Text, "It was not woken")) {
				t.Fatalf("power %q, wake unconfirmed %v: %s", h.On, h.WakeUnsure, h.Text)
			}
		})
	}
}

// MUST FIX 1, the second way it was wider: the corner the README states ("a machine whose agent answers
// within 8 seconds of the wake cannot have been off, and is left on") did not hold after a wake request that
// timed out. The request hangs for request_timeout + 8 s (12 s), the run looks again only then, so "answered
// sooner than a machine that was off can" was never true: a machine that was already starting, and whose
// agent answered 6 s after the wake, was "online after 13 s", updated and switched off.
func TestRVP4TooSoonCornerAfterALostWake(t *testing.T) {
	for _, lost := range []bool{false, true} {
		name := map[bool]string{false: "control: the wake is accepted at once", true: "the wake request times out after 12 s"}[lost]
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			pc := w.off("pc-a")
			pc.knownLimit = true // it runs (in its firmware) when the first wake request goes out
			pc.wakeBoots = false // the packet does nothing: the machine is already starting by a hand on its button
			if lost {
				pc.wakeLost = "always"
			}
			first := true
			w.during = func(call string) {
				if call != "wake pc-a" || !first {
					return
				}
				first = false
				// the person pressed the button a moment ago: the system starts 1 s after the wake, the agent answers 6 s after it
				pc.on, pc.bootedAt, pc.agentReadyAt = true, w.now.Add(1*time.Second), w.now.Add(6*time.Second)
				if lost { // the request hangs for 12 s; the world (machines, poller) goes on meanwhile
					for i := 0; i < 12; i++ {
						w.clock.Lock()
						w.now = w.now.Add(time.Second)
						w.mono += time.Second
						w.clock.Unlock()
						w.sim()
						w.poll()
					}
				}
			}
			w.start(StartRequest{Hosts: []string{"pc-a"}})
			h := host(w.finish(), "pc-a")
			if pc.shutdowns != 0 || !pc.on || h.State != StLeftOn || h.On != OnYes {
				t.Fatalf("shutdown requests %d, on %v, %s/%s: %s", pc.shutdowns, pc.on, h.State, h.On, h.Text)
			}
			if !lost && (!h.TooSoon || !strings.Contains(h.Text, "sooner than a machine that was off can start: it was already starting")) {
				t.Fatalf("the accepted wake: too_soon=%v: %s", h.TooSoon, h.Text)
			}
			if lost && (h.TooSoon || h.Wakes != 0 || !strings.HasPrefix(h.Text, lostWakeLeftOn)) {
				t.Fatalf("the lost wake: too_soon=%v wakes accepted %d: %s", h.TooSoon, h.Wakes, h.Text)
			}
		})
	}
}

// MUST FIX 2 (P5): the answer to the shutdown request is lost because the way to the machine went down BEFORE
// the request got there. The request never arrived; the machine is ON and out of reach. The report said
// state "off", power "off": "... switched off again (the shutdown request got no answer, but it went off when
// the countdown was due)", a green "done · off again" for a machine left on that nobody was told about.
func TestRVP5LostShutdownRequestNeverArrivedAndTheMachineIsOutOfReach(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	pc.shutLost = "never"
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	pc.unreachable = true // from the tick after the request on
	h := host(w.finish(), "pc-a")
	has(t, h, StLeftOn, "It came online after the wake. Its shutdown request got no answer, Fairy never saw its countdown, and it does not answer now: "+
		"not confirmed that it is off. It was updated to 17-bbbbbbb.")
	if !pc.on || pc.wentOff || pc.pending != nil {
		t.Fatalf("set-up: the request arrived (pending %v, on %v, went off %v)", pc.pending, pc.on, pc.wentOff)
	}
	if h.On != OnUnknown || w.callsFor("pc-a") != "wake update shutdown" { // not "off", and the request is not sent again
		t.Fatalf("power %q, requests %q: %s", h.On, w.callsFor("pc-a"), h.Text)
	}
}

// MUST FIX 2 (P5), the cancel: "Cancel run" (leave on) shortly before the countdown ends. The cancel REACHES
// the agent (the countdown is called off, the machine stays on), its answer is lost, and the machine is out of
// reach from then on. In three of these four timings the last look said "It is off (not answering)".
func TestRVP5CancelArrivedAnswerLostThenOutOfReach(t *testing.T) {
	for _, arrives := range []bool{true, false} {
		for _, before := range []time.Duration{60 * time.Second, 25 * time.Second, 10 * time.Second, 3 * time.Second} {
			w := newWorld(t)
			pc := w.off("pc-a")
			r := w.start(StartRequest{Hosts: []string{"pc-a"}})
			untilState(t, w, "pc-a", StShutting)
			due := *host(active(w), "pc-a").ShutdownAt
			for w.now.Before(due.Add(-before)) {
				w.tick()
			}
			if _, err := w.m.CancelRun(r.ID, "Bob", false); err != nil {
				t.Fatal(err)
			}
			// the cancel's answer does not come, and nothing answers after it
			pc.cancelLost, pc.unreachable = map[bool]string{true: "arrives", false: "never"}[arrives], true
			at := w.now
			done := w.finish() // its judge fails "off" for a machine that is on, and "on" for one that is off
			h := host(done, "pc-a")
			w.advance(before + 5*time.Second) // past the time the countdown ends, if it still runs
			if pc.on != arrives {
				t.Fatalf("set-up: the cancel arrived=%v, the machine is on=%v after the countdown's time", arrives, pc.on)
			}
			want := "The run was cancelled by Bob, and the request to call its shutdown countdown (due at " + clock(due) + ") off got no answer (agent unreachable: timeout): it may have arrived. " +
				"Asked to switch off, not confirmed: it does not answer now."
			if h.State != StLeftOn || h.On != OnUnknown || h.Text != want {
				t.Fatalf("cancel %s before the countdown ends, it arrived=%v: %s/%s: %s", before, arrives, h.State, h.On, h.Text)
			}
			if d := done.EndedAt.Sub(at); d > lookFor+2*time.Second {
				t.Fatalf("the last look took %s, at most %s", d, lookFor)
			}
			if got := w.steps("pc-a"); !strings.HasSuffix(got, "cancel:agent unreachable: timeout") {
				t.Fatalf("audit steps: %s", got)
			}
		}
	}
}

// The same cancel when its answer comes while the machine can be reached: nothing changes, it is ON at once,
// with the sentence that says the cancel got no answer when it did not.
func TestACancelWithoutAnAnswerOnAMachineThatAnswers(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	r := w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	due := *host(active(w), "pc-a").ShutdownAt
	w.advance(30 * time.Second)
	if _, err := w.m.CancelRun(r.ID, "Bob", false); err != nil {
		t.Fatal(err)
	}
	pc.cancelLost = "arrives"
	h := host(w.finish(), "pc-a")
	has(t, h, StLeftOn, "The run was cancelled by Bob, and the request to call its shutdown countdown off got no answer (agent unreachable: timeout): it may switch off at "+clock(due)+
		". Cancel it on the machine's page.")
	if h.On != OnYes || !pc.on {
		t.Fatalf("power %q on %v: %s", h.On, pc.on, h.Text)
	}
}

// A cancel that the agent ANSWERS before the countdown is over, from a machine the panel's poller cannot reach
// at that moment and that gives nothing afterwards: its countdown is called off and it is on. The last look
// goes by the agent's answer to the run's own request; before, it waited, saw the silence run past the time
// the countdown would have ended, and said "It is off (not answering)".
func TestACancelTheAgentAnsweredIsNotOffWhenTheMachineGoesQuiet(t *testing.T) {
	for _, before := range []time.Duration{60 * time.Second, 25 * time.Second, 10 * time.Second, 3 * time.Second} {
		w := newWorld(t)
		pc := w.off("pc-a")
		r := w.start(StartRequest{Hosts: []string{"pc-a"}})
		untilState(t, w, "pc-a", StShutting)
		due := *host(active(w), "pc-a").ShutdownAt
		for w.now.Before(due.Add(-before)) {
			w.tick()
		}
		if _, err := w.m.CancelRun(r.ID, "Bob", false); err != nil {
			t.Fatal(err)
		}
		pc.unreachable, pc.cancelOK = true, true // the poll before the cancel is lost, the cancel gets through and is answered
		h := host(w.finish(), "pc-a")
		has(t, h, StLeftOn, "The run was cancelled by Bob: its shutdown countdown was called off. It came online after the wake and it is ON.")
		if h.On != OnYes || !pc.on || pc.pending != nil {
			t.Fatalf("cancel %s before the countdown ends: power %q on %v pending %v: %s", before, h.On, pc.on, pc.pending, h.Text)
		}
	}
}

// The last look and a shutdown request that got no answer (MUST FIX 2, the look's part): the run stops while
// it watches such a machine, which never got the request and is out of reach. The look ran past the time the
// countdown would have ended and said "It is off (not answering)". A request without an answer, of which no
// countdown was ever seen, is not a shutdown the run knows of: "not confirmed", and no word of a countdown.
func TestTheLastLookAfterAShutdownRequestWithoutAnAnswer(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	pc.shutLost = "never"
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	due := *host(active(w), "pc-a").ShutdownAt
	pc.unreachable = true
	for w.now.Before(due.Add(-5 * time.Second)) {
		w.tick()
	}
	w.blocked = "control is disabled by an administrator"
	r := w.finish()
	h := host(r, "pc-a")
	want := "the run stopped (control is disabled by an administrator) after its shutdown request got no answer (it would be due at " + clock(due) + "). " +
		"It does not answer now: not confirmed that it is on."
	if h.State != StLeftOn || h.On != OnUnknown || h.Text != want || !pc.on {
		t.Fatalf("%s/%s, really on %v: %s", h.State, h.On, pc.on, h.Text)
	}
	// while the machine answers, the same end says what may still happen, and that it is not known
	w = newWorld(t)
	pc = w.off("pc-a")
	pc.shutLost, pc.staleOnce = "arrives", true // the first answer after the request does not show the countdown yet
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	w.blocked = "control is disabled by an administrator"
	h = host(w.finish(), "pc-a")
	has(t, h, StLeftOn, "the run stopped (control is disabled by an administrator) after its shutdown request got no answer (it would be due at ",
		"). Fairy no longer watches it: if the request arrived, it switches off by itself unless someone cancels that.")
	if h.On != OnYes {
		t.Fatalf("power %q: %s", h.On, h.Text)
	}
}
