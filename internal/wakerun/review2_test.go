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
			// (second review of fix7a, R7A-04: the cancel is tried again until the run's own due time, here
			// against a link that stays down, and the last look begins then; it began at once)
			if d := done.EndedAt.Sub(at); d > before+lookFor+2*time.Second {
				t.Fatalf("the tries and the last look took %s, at most %s", d, before+lookFor)
			}
			if got := w.steps("pc-a"); !strings.Contains(got, "shutdown:ok cancel:agent unreachable: timeout") {
				t.Fatalf("audit steps: %s", got)
			}
		}
	}
}

// The same cancel while the machine can be reached, and no answer to any of the run's cancels comes (the first
// one arrived): it is ON, with the sentence that says the cancel got no answer.
//
// (Second review of fix7a, R7A-04: the cancel is tried again until the run's own due time, and this sentence is
// written only then. It was written after one cancel. A cancel that is answered at the second try:
// TestACancelWithoutAnAnswerIsTriedAgain.)
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
	pc.cancelLost = "always arrives"
	done := w.finish()
	h := host(done, "pc-a")
	has(t, h, StLeftOn, "The run was cancelled by Bob, and the request to call its shutdown countdown off got no answer (agent unreachable: timeout): it may switch off at "+clock(due)+
		". Cancel it on the machine's page.")
	if h.On != OnYes || !pc.on {
		t.Fatalf("power %q on %v: %s", h.On, pc.on, h.Text)
	}
	if n := strings.Count(w.callsFor("pc-a"), "cancel"); n != 9 || done.EndedAt.Before(due) {
		t.Fatalf("%d cancels, the run let go %v before the countdown's end: want one every %s for the 90 s that were left", n, due.Sub(*done.EndedAt), cancelRetry)
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
	// (r3-p2-06: the run that the kill switch stops now tries to call the countdown off that its request may
	// have started; the sentence held "... after its shutdown request got no answer (it would be due at ...).")
	// (Review of fix8, RF8-02: for a request without an answer the cancel is asked for until 5 minutes after the due
	// time, and the end does not say "its shutdown countdown (due at ...) could not be called off": the run
	// knows of no countdown. It held "..., but its shutdown countdown (due at ...) could not be called off (agent
	// unreachable: connection failed). It does not answer now: not confirmed that it is on.")
	want := "the run stopped (control is disabled by an administrator): its shutdown request had got no answer, and the request to call a countdown off did not get through either " +
		"(agent unreachable: connection failed). The request may still arrive: the machine then switches off 2 min later unless someone cancels that on its page. " +
		"Fairy no longer watches it. It was updated to 17-bbbbbbb."
	if h.State != StLeftOn || h.On != OnUnknown || h.Text != want || !pc.on {
		t.Fatalf("%s/%s, really on %v: %s", h.State, h.On, pc.on, h.Text)
	}
	// while the machine answers: the request did arrive, and the countdown it started is called off (r3-p2-06;
	// until then "... after its shutdown request got no answer (it would be due at ...). Fairy no longer watches
	// it: if the request arrived, it switches off by itself unless someone cancels that.", and it did go off)
	w = newWorld(t)
	pc = w.off("pc-a")
	pc.shutLost, pc.staleOnce = "arrives", true // the first answer after the request does not show the countdown yet
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	w.blocked = "control is disabled by an administrator"
	h = host(w.finish(), "pc-a")
	// (second review of fix7a, R7A-09: the run never saw a countdown, so it does not say that one was called
	// off; it held "...: its shutdown countdown was called off. It came online after the wake and it is ON.")
	// (Audit 6, r4-p2-02: nor does it say "it is ON" as the last word. It asked again until its own due time,
	// heard "nothing is pending" each time, and a request that has not arrived by then still can: the power is
	// "not known". It held "... none is pending. It came online after the wake and it is ON.", power "on".)
	has(t, h, StLeftOn, "the run stopped (control is disabled by an administrator): its shutdown request had got no answer, so its agent was asked to call off a countdown, "+
		"and it answered: none is pending. The request may still arrive: the machine then switches off 2 min later unless someone cancels that on its page. "+
		"It is on now, and Fairy no longer watches it.")
	w.advance(5 * time.Minute)
	if h.On != OnUnknown || !pc.on || pc.pending != nil {
		t.Fatalf("power %q, the machine on %v, pending %v: %s", h.On, pc.on, pc.pending, h.Text)
	}
	// a run that stops for another reason (its journal cannot be written) still sends nothing, and says what
	// may happen: TestTheReportAfterARestartNeverKnowsLessThanWhatWasSent
}

// Note N2 of the second review: the guests of a Proxmox node were asked about once, before the shutdown
// request. A guest that started on the node, or arrived on it, during the 120 s of the countdown was not
// noticed, and the node went off under it. The run now asks again while the countdown runs and calls it off.
func TestAGuestThatStartsDuringTheCountdownCallsItOff(t *testing.T) {
	type c struct {
		name   string
		after  time.Duration               // into the countdown
		change func(w *world, mc *machine) // what happens on the node then
		state  string
		want   string
		calls  string
	}
	for _, c := range []c{
		{"a guest starts 30 s into the countdown", 30 * time.Second, func(_ *world, mc *machine) { mc.guests, mc.guestNames = 1, "web01" }, StLeftOn,
			"Left ON: 1 guest is running on it (web01) since its shutdown was asked for: its shutdown countdown was called off. It came online after the wake and it is ON. It was updated to 17-bbbbbbb.",
			"wake update shutdown cancel"},
		{"two guests arrive 100 s into the countdown", 100 * time.Second, func(_ *world, mc *machine) { mc.guests = 2 }, StLeftOn,
			"Left ON: 2 guests are running on it since its shutdown was asked for: its shutdown countdown was called off.", "wake update shutdown cancel"},
		{"a guest that is not clearly stopped any more", 30 * time.Second, func(_ *world, mc *machine) { mc.guestsWhy = "the Proxmox API does not show as stopped: db (paused)" }, StLeftOn,
			"Left ON: Fairy cannot see whether guests run on it (the Proxmox API does not show as stopped: db (paused)) since its shutdown was asked for: its shutdown countdown was called off.",
			"wake update shutdown cancel"},
		{"the Proxmox API fails once and answers again: that is not a guest", 30 * time.Second, func(w *world, mc *machine) {
			mc.guestsWhy = "its Proxmox API has not answered since it came online"
			asked := w.guestsAsked
			w.afterTick = func() {
				if w.guestsAsked > asked { // one question of the run fell into it
					mc.guestsWhy = ""
				}
			}
		}, StOff, "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.", "wake update shutdown"},
		{"a guest starts in the last 10 s: too late for a cancel, as before", 110 * time.Second, func(_ *world, mc *machine) { mc.guests = 1 }, StOff,
			"Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.", "wake update shutdown"},
		// (second review of fix7a, R7A-04: the cancel is tried again until the run's own due time; here no answer
		// to any of them comes. With one lost answer the second try calls it off: TestACancelWithoutAnAnswerIsTriedAgain.)
		{"no cancel gets an answer: said, and not \"off\"", 30 * time.Second, func(_ *world, mc *machine) {
			mc.guests, mc.cancelLost = 1, "always arrives"
		}, StLeftOn, "Left ON: 1 guest is running on it since its shutdown was asked for, and the request to call its shutdown countdown off got no answer (agent unreachable: timeout): it may switch off at ",
			"wake update shutdown cancel"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			pc := w.off("pve1")
			w.start(StartRequest{Hosts: []string{"pve1"}})
			untilState(t, w, "pve1", StShutting)
			w.advance(c.after)
			c.change(w, pc)
			h := host(w.finish(), "pve1")
			has(t, h, c.state, c.want)
			if got := w.callsFor("pve1"); got != c.calls && (!strings.HasPrefix(c.name, "no cancel gets an answer") || oneCancel(got) != c.calls) {
				t.Fatalf("requests: %s", got)
			}
			if on := c.state == StLeftOn; pc.on != on || (on && h.On != OnYes) || pc.shutdowns != 1 {
				t.Fatalf("on %v, power %q, shutdown requests %d: %s", pc.on, h.On, pc.shutdowns, h.Text)
			}
		})
	}
	// a machine without guests is asked about just the same and switched off as ever; and the run asks at most
	// every guestsEvery, and only while the countdown runs
	w := newWorld(t)
	w.off("pc-a")
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	before := w.guestsAsked
	h := host(w.finish(), "pc-a")
	has(t, h, StOff, "switched off again")
	if n := w.guestsAsked - before; n < 15 || n > 22 { // 120 s less the last 15 s, every 5 s
		t.Fatalf("the guests were asked about %d times during the countdown", n)
	}
}
