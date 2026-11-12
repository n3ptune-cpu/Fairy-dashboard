package wakerun

import (
	"strings"
	"testing"
	"time"
)

// Audit 5, round 2: requests of a run whose answer was lost (a timeout). The machine may have taken the
// request. The run must not take the missing answer for "nothing happened".

// r2-p2-03: the answer to the update request is lost while the agent installs. The run said "NOT updated
// (agent unreachable: timeout)", waited a minute and asked for the shutdown in the middle of the install.
// Now it looks at what the agent itself reports about its update, waits for an install that runs, reports
// what really happened, and never asks for a shutdown while the agent says an install is running.
func TestUpdateAnswerLost(t *testing.T) {
	t.Run("the agent took it: the install is waited for and reported", func(t *testing.T) {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.updLost, pc.updTakes = "arrives", 5*time.Minute // a slow download
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		var shutdownAt, updatedAt time.Time
		w.during = func(call string) {
			if call == "shutdown pc-a" {
				shutdownAt = w.now
			}
		}
		for i := 0; i < 3600 && pc.version != pc.newVersion; i++ {
			w.tick()
		}
		updatedAt = w.now
		r := w.finish()
		h := host(r, "pc-a")
		has(t, h, StOff, "updated from 16-aaaaaaa to 17-bbbbbbb", "switched off again")
		if h.Update != UpdOK || !h.UpdateUnsure || pc.on || pc.version != "17-bbbbbbb" {
			t.Fatalf("update %q unsure %v on %v version %s: %s", h.Update, h.UpdateUnsure, pc.on, pc.version, h.Text)
		}
		if got := w.callsFor("pc-a"); got != "wake update shutdown" { // the update is not asked for a second time
			t.Fatalf("requests: %s", got)
		}
		if got := w.steps("pc-a"); got != "wake:ok update:agent unreachable: timeout shutdown:ok" {
			t.Fatalf("audit steps: %s", got)
		}
		if !shutdownAt.After(updatedAt) {
			t.Fatalf("the shutdown was asked for at %s, the install ended at %s", clock(shutdownAt), clock(updatedAt))
		}
		if !logHas(h, "the update request got no answer (agent unreachable: timeout): it may have arrived") {
			t.Fatalf("the machine's history does not say that the answer was lost: %v", h.Log)
		}
	})

	t.Run("the agent took it and the install fails: the agent's own words", func(t *testing.T) {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.updLost, pc.updMode = "arrives", "fail"
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		h := host(w.finish(), "pc-a")
		has(t, h, StOff, "NOT updated (downloading the release: connection reset)", "switched off again")
	})

	t.Run("the agent never got it: said as that, no install runs, switched off again", func(t *testing.T) {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.updLost = "never"
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		h := host(w.finish(), "pc-a")
		has(t, h, StOff, "NOT updated (the update request got no answer (agent unreachable: timeout), and its agent reports no update running and no new result)",
			"switched off again")
		if h.Update != UpdFailed || pc.on || pc.version != "16-aaaaaaa" || w.callsFor("pc-a") != "wake update shutdown" {
			t.Fatalf("update %q on %v version %s requests %q", h.Update, pc.on, pc.version, w.callsFor("pc-a"))
		}
		// the agent was given a minute to show that it took the request
		var asked, decided time.Time
		for _, l := range h.Log {
			if strings.HasPrefix(l.Text, "the update request got no answer") {
				asked = l.T
			}
			if strings.HasPrefix(l.Text, "update failed") {
				decided = l.T
			}
		}
		if d := decided.Sub(asked); d < unsureIdleAfter || d > unsureIdleAfter+5*time.Second {
			t.Fatalf("decided %s after the lost answer, want about %s", d, unsureIdleAfter)
		}
	})

	t.Run("the run cannot find out: not confirmed, left on like an update without a result", func(t *testing.T) {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.updLost, pc.updMode = "arrives", "stuck"
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		for i := 0; i < 600 && !pc.updating; i++ {
			w.tick()
		}
		pc.unreachable = true // and from then on the panel hears nothing from its agent
		h := host(w.finish(), "pc-a")
		has(t, h, StLeftOn, "Its update did not finish (not confirmed: the update request got no answer (agent unreachable: timeout), and its agent did not say how it ended within 15 min)",
			"left it ON")
		if h.Update != UpdStuck || !pc.on || w.callsFor("pc-a") != "wake update" {
			t.Fatalf("update %q on %v requests %q", h.Update, pc.on, w.callsFor("pc-a"))
		}
	})

	t.Run("a machine that was on: the same, and it stays on", func(t *testing.T) {
		w := newWorld(t)
		pc := w.running("pc-a")
		pc.updLost = "arrives"
		w.start(StartRequest{UpdateOnly: []string{"pc-a"}})
		h := host(w.finish(), "pc-a")
		has(t, h, StStayedOn, "It was on and stays on.", "It was updated to 17-bbbbbbb.")
		if !pc.on || w.callsFor("pc-a") != "update" {
			t.Fatalf("on %v requests %q", pc.on, w.callsFor("pc-a"))
		}
	})
}

// Never a shutdown while the agent says an install is running, whoever started it: mayShutDown says no.
func TestNoShutdownWhileTheAgentSaysItInstalls(t *testing.T) {
	for _, state := range []string{"downloading", "applying"} {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.updMode = "current" // the run's own update finds nothing to do
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		for i := 0; i < 600 && host(active(w), "pc-a").State != StSettling; i++ {
			w.tick()
		}
		// someone starts an update on the machine during the pause before the shutdown
		pc.updating, pc.updMode, pc.updDoneAt, pc.updState, pc.updMsg = true, "stuck", w.now.Add(time.Hour), state, state+" 17-bbbbbbb"
		h := host(w.finish(), "pc-a")
		has(t, h, StLeftOn, "Left ON: its agent says that an update is being installed on it right now ("+state+").")
		if !pc.on || strings.Contains(w.callsFor("pc-a"), "shutdown") {
			t.Fatalf("%s: on %v requests %q", state, pc.on, w.callsFor("pc-a"))
		}
	}
}

func active(w *world) *Run {
	a, _ := w.m.Snapshot()
	return a
}

func logHas(h *Host, part string) bool {
	for _, l := range h.Log {
		if strings.Contains(l.Text, part) {
			return true
		}
	}
	return false
}
