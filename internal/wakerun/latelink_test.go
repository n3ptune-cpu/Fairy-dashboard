package wakerun

import (
	"strings"
	"testing"
	"time"
)

// Review of fix8, RF8-01 (must-fix). A shutdown request that got no answer arrives late, the run sees its
// countdown and moves its due time. The earliest end of the countdown stayed where it was, at "the request plus
// the delay": a link that dropped DURING the late countdown, after that moment, was not "out of reach before
// the end". When the shutdown then did not happen (blocked, hanging) and the link stayed down, two silent polls
// after the due time were "switched off again" for a machine that was on. For a request that was answered the
// same drop is "It was already out of reach before its countdown ended ... Not confirmed that it is off."
//
// The run saw an answer without a countdown after its request: the countdown began after that, so its earliest
// end is that answer plus the delay (seeCountdown). The reviewer's cases: 20 s, 100 s and 200 s late, the link
// cut 10 s before the real end.
func TestALateCountdownWithALinkThatDropsDuringItIsNotOff(t *testing.T) {
	for _, late := range []time.Duration{20 * time.Second, 100 * time.Second, 200 * time.Second} {
		for _, before := range []time.Duration{10 * time.Second, 60 * time.Second, 115 * time.Second} { // how long before the real end the link drops
			w, pc, _ := lateShutdown(t, late)
			pc.shutMode = "ignore" // the shutdown does not happen
			drop := w.mono + late + 120*time.Second - before
			w.afterTick = func() {
				if w.mono >= drop {
					pc.unreachable = true
				}
			}
			h := host(w.finish(), "pc-a")
			if !pc.on || pc.wentOff {
				t.Fatalf("the set-up: the machine must stay on")
			}
			want := "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and asked to switch off. It was already out of reach before its countdown ended, " +
				"and it does not answer now: Fairy cannot tell a machine that went off from a connection that dropped. Not confirmed that it is off."
			if h.State != StLeftOn || h.On != OnUnknown || h.Text != want {
				t.Errorf("%v late, the link drops %v before the end: [%s] power %q for a machine that is on: %s", late, before, h.State, h.On, h.Text)
			}
		}
	}
}

// The other side must hold: a late countdown whose machine answers to its end and then goes off is "switched
// off again" (TestAShutdownRequestThatArrivesLateIsTheRunsShutdown), also when the link drops right AFTER the
// real end, and a machine that goes off at the end with the link up is not "out of reach before the end".
func TestALateCountdownThatEndsIsStillOff(t *testing.T) {
	for _, late := range []time.Duration{20 * time.Second, 100 * time.Second, 200 * time.Second} {
		w, pc, _ := lateShutdown(t, late)
		h := host(w.finish(), "pc-a")
		if h.State != StOff || h.On != OnNo || pc.on || !strings.HasSuffix(h.Text, "and switched off again.") {
			t.Errorf("%v late: [%s] power %q, the machine on: %v: %s", late, h.State, h.On, pc.on, h.Text)
		}
	}
}

// RF8-06 of the same review: a late countdown that a person calls off on the machine, after the moment the
// run's first earliest end. It read "It is still on 5 min after its shutdown was due: the shutdown did not
// work. It is ON." six minutes later; with the earliest end where it belongs it is what it is: cancelled.
func TestALateCountdownThatAPersonCallsOffIsCancelled(t *testing.T) {
	w, pc, _ := lateShutdown(t, 100*time.Second)
	w.advance(160 * time.Second) // 60 s into the real countdown, 40 s after "the request plus the delay"
	pc.pending = nil
	began := w.mono
	h := host(w.finish(), "pc-a")
	has(t, h, StLeftOn, "Its shutdown was cancelled, in the panel or on the machine, so it is ON.")
	if took := w.mono - began; h.On != OnYes || !pc.on || took > 10*time.Second {
		t.Errorf("power %q, the machine on: %v, said %v after the cancel", h.On, pc.on, took)
	}
}
