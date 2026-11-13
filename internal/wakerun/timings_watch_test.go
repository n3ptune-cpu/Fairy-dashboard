package wakerun

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Two places where the review of build 14 changed the program and no test noticed.

// The machine's own countdown also decides which answers are "from after the run's request" while the run
// watches the countdown (stepShutting, answeredAfter). The run works the moment of its request out from the
// countdown's end minus the countdown: with the PANEL's countdown (2 minutes) for a machine whose own is 30
// seconds, that moment is a minute and a half too early, and an answer the poller got BEFORE the request
// counts as one from after it. Such an answer shows no countdown, and the run concludes from it that somebody
// called its countdown off: it reports the machine as left on while the machine counts down and goes off.
// That is what a machine with a short countdown of its own and a poll interval of some seconds gives.
//
// Here the poller's latest answer stays the one from 20 s before the request for ten seconds (it shows
// nothing pending). The run must conclude nothing from it, and report "switched off again" in the end.
func TestWatchingAMachinesOwnCountdownKnowsWhenItsRequestWent(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	pc.info.ShutdownDelay = 30 * time.Second
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	h := host(active(w), "pc-a")
	sent := h.dueMono - 30*time.Second // the steady clock when the request's answer came
	for i := 0; i < 10; i++ {
		// one second of the world, with the poller's latest answer still the old one
		w.clock.Lock()
		w.now = w.now.Add(time.Second)
		w.mono += time.Second
		w.clock.Unlock()
		w.sim()
		w.poll()
		pc.obs.Online, pc.obs.Pending, pc.seenMono = true, nil, sent-20*time.Second
		w.m.Tick(context.Background())
		a, hist := w.m.Snapshot()
		if a == nil || host(a, "pc-a").State != StShutting {
			if a == nil {
				a = hist[0]
			}
			t.Fatalf("%d s into its countdown of 30 s, from an answer that is older than the request, the run concluded: %s", i+1, describe(a))
		}
	}
	r := w.finish()
	has(t, host(r, "pc-a"), StOff, "switched off again.")
	for _, l := range host(r, "pc-a").Log {
		if strings.Contains(l.Text, "called off") {
			t.Fatalf("the run says its countdown was called off: %s", l.Text)
		}
	}
	if pc.on {
		t.Fatal("the machine is still on")
	}
}

// "Off" is asked twice: when the run starts (it was silent for its time then) and again when the machine's
// turn comes, with what the panel sees NOW. Between the two a machine can have given a sign of life (somebody
// pressed its power button; its agent's port refused a connection while it started) and be silent again: the
// count of its silence began anew, the row is "offline" as before, and nothing but that count says that it is
// not the machine that was off for a quarter of an hour. Without the second question the run wakes a machine
// that is starting, and switches it off as one it woke.
func TestAMachineThatGaveASignOfLifeAfterTheStartIsLeftAlone(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a") // off, and silent for a day
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	if h := host(active(w), "pc-a"); h.StartStatus != "offline" || !offAtStart(h) {
		t.Fatalf("the fixture: at the start the machine is not off for the run: %+v", h)
	}
	// before its first step: the count of its silence has begun again two minutes ago, and it is silent now
	pc.quietSince, pc.lastAnswer = w.now.Add(-2*time.Minute), w.now.Add(-2*time.Minute)
	r := w.finish()
	has(t, host(r, "pc-a"), StSkipped, "Not sure that it is off, so nothing was sent to it: Fairy takes a machine for off only after 15 min without an answer")
	if len(w.calls) != 0 || pc.wakes != 0 {
		t.Fatalf("requests: %v", w.calls)
	}
}
