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
