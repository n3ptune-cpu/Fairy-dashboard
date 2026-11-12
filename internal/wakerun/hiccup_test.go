package wakerun

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Review of fix7r2, R2-02: one answer of the agent without the run's countdown, and then the same shutdown
// again, made the run call its OWN shutdown "not the run's" and let go.
//
// The test world shows the run what the machine's agent holds. Here the agent's ANSWER differs from what it
// holds for a poll, so the tests move the world themselves: tickShown is world.tick with a hook between the
// poll and the run's step (the reviewer's tickShowing).
func tickShown(w *world, show func()) {
	w.clock.Lock()
	w.now = w.now.Add(time.Second)
	w.mono += time.Second
	w.clock.Unlock()
	w.sim()
	w.poll()
	if show != nil {
		show()
	}
	w.m.Tick(context.Background())
	if w.afterTick != nil {
		w.afterTick()
	}
}

// The reviewer's proof (TestReview7r2AHiccupOfTheAgentIsNotSomeoneElsesShutdown). The agent holds the run's
// shutdown the whole time. In one answer, 3 s or 2 s before the end of the countdown, it shows nothing pending
// (Windows: the agent's process restarted since, and its helper gave no status for that poll); in the next one
// it shows the same shutdown again. The run goes on watching its own countdown, and reports the machine that
// goes off by it as that.
func TestAHiccupOfTheAgentIsNotSomeoneElsesShutdown(t *testing.T) {
	for _, before := range []time.Duration{3 * time.Second, 2 * time.Second, time.Second} {
		w := newWorld(t)
		pc := w.off("pc-a")
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		untilState(t, w, "pc-a", StShutting)
		due := *host(active(w), "pc-a").ShutdownAt
		var r *Run
		for i := 0; i < 900 && r == nil; i++ {
			tickShown(w, func() {
				if w.now.Equal(due.Add(-before)) && pc.obs.Online { // the hiccup: one answer without the countdown
					pc.obs.Pending = nil
				}
			})
			if a, hist := w.m.Snapshot(); a == nil {
				r = hist[0]
			}
		}
		if r == nil {
			t.Fatalf("the run did not end")
		}
		h := host(r, "pc-a")
		if strings.Contains(h.Text, "which is not the run's") && pc.wentOff {
			t.Errorf("a hiccup %v before the end: the run called its OWN shutdown \"not the run's\" and let go %v before the due time; the machine went off by it: %s",
				before, due.Sub(*r.EndedAt), h.Text)
		}
		if h.State != StOff || h.On != OnNo || h.Text != "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again." || !pc.wentOff {
			t.Errorf("a hiccup %v before the end: [%s] power %q, ended %v after the due time, the machine went off: %v: %s", before, h.State, h.On, r.EndedAt.Sub(due), pc.wentOff, h.Text)
		}
		w.wellReported(r)
	}
}

// The second belt: after the run's due time. The run's shutdown did not switch the machine off (the system
// stayed up). Its agent shows the shutdown for a while after its time, with one answer without it in between.
// A shutdown with the end the run saw is the run's own: it is not "another shutdown, which is not the run's".
// One with another end, asked for after the agent showed none, is.
func TestTheSameShutdownAfterTheDueTimeIsTheRunsOwn(t *testing.T) {
	for _, c := range []struct {
		name  string
		shift time.Duration // the end the agent gives when the shutdown shows again, against the one the run saw
		other bool
	}{
		{"the same end", 0, false},
		{"an end one second off (the agent's helper reads its clock a little later)", time.Second, false},
		{"an end a minute later", time.Minute, true},
	} {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.shutMode = "ignore"
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		untilState(t, w, "pc-a", StShutting)
		due := *host(active(w), "pc-a").ShutdownAt
		var r *Run
		for i := 0; i < 900 && r == nil; i++ {
			tickShown(w, func() {
				since := w.now.Sub(due)
				switch {
				case !pc.obs.Online:
				case since >= 0 && since < 10*time.Second && since != 3*time.Second: // still shown after its time; not in one answer, 3 s after it
					pc.obs.Pending = &Pending{Action: "shutdown", At: due}
				case since >= 12*time.Second && since < 20*time.Second && c.shift != 0: // after two answers without one
					pc.obs.Pending = &Pending{Action: "shutdown", At: due.Add(c.shift)}
				}
			})
			if a, hist := w.m.Snapshot(); a == nil {
				r = hist[0]
			}
		}
		if r == nil {
			t.Fatalf("the run did not end")
		}
		h := host(r, "pc-a")
		if got := strings.Contains(h.Text, "which is not the run's"); got != c.other {
			t.Errorf("%s: said \"not the run's\": %v, want %v (ended %v after the due time): %s", c.name, got, c.other, r.EndedAt.Sub(due), h.Text)
		}
		if !c.other && (h.State != StLeftOn || h.On != OnYes || !strings.HasPrefix(h.Text, "It is still on 5 min after its shutdown was due: the shutdown did not work. It is ON.")) {
			t.Errorf("%s: [%s] power %q, ended %v after the due time: %s", c.name, h.State, h.On, r.EndedAt.Sub(due), h.Text)
		}
		if strings.Contains(h.Text, "switched off again") || h.On == OnNo {
			t.Errorf("%s: reported off for a machine that is on: %s", c.name, h.Text)
		}
	}
}
