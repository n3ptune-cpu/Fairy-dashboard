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

// r2-p2-04: the answer to the wake request is lost and the packet went out. The run said "The wake could not
// be sent ... It was not woken", power "untouched", and never looked at the machine again; the machine started
// and stayed on. Now a lost answer is "a wake may have gone out": the run waits for the machine exactly as
// after an accepted wake, and what it then sees decides through mayShutDown.
func TestWakeAnswerLost(t *testing.T) {
	t.Run("the packet went out: the machine comes up fresh, is updated and switched off again", func(t *testing.T) {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.wakeLost = "arrives"
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		h := host(w.finish(), "pc-a")
		has(t, h, StOff, "Came online after a wake request that got no answer, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.")
		if pc.on || h.On != OnNo || !h.WakeUnsure || h.Wakes != 0 || h.WakeSentAt == nil || !h.OnlineAt.After(*h.WakeSentAt) {
			t.Fatalf("on %v power %q unsure %v wakes %d: %s", pc.on, h.On, h.WakeUnsure, h.Wakes, h.Text)
		}
		if got := w.callsFor("pc-a"); got != "wake update shutdown" {
			t.Fatalf("requests: %s", got)
		}
		if got := w.steps("pc-a"); got != "wake:agent unreachable: timeout update:ok shutdown:ok" {
			t.Fatalf("audit steps: %s", got)
		}
		if !logHas(h, "the wake request got no answer (agent unreachable: timeout): the wake may have gone out. Waiting for it to come online, up to 5 min") {
			t.Fatalf("the machine's history: %v", h.Log)
		}
	})

	t.Run("no packet and no machine: not \"it was not woken\", but \"Fairy does not know\"", func(t *testing.T) {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.wakeLost = "always"
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		h := host(w.finish(), "pc-a")
		has(t, h, StGaveUp, "The wake request got no answer, so Fairy does not know whether the wake went out. It did not come online within 5 min.",
			"if it did switch on, it is ON")
		if pc.on || h.On != OnUnknown || strings.Contains(h.Text, "It was not woken") || strings.Contains(w.callsFor("pc-a"), "update") {
			t.Fatalf("on %v power %q requests %q: %s", pc.on, h.On, w.callsFor("pc-a"), h.Text)
		}
	})

	t.Run("the first answer is lost, the next wake is accepted", func(t *testing.T) {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.wakeLost = "never"
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		h := host(w.finish(), "pc-a")
		has(t, h, StOff, "Woken, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.")
		if pc.on || h.Wakes != 1 || !h.WakeUnsure || w.callsFor("pc-a") != "wake wake update shutdown" {
			t.Fatalf("on %v wakes %d requests %q: %s", pc.on, h.Wakes, w.callsFor("pc-a"), h.Text)
		}
	})

	t.Run("the packet went out and the machine is slow: it is seen coming online later", func(t *testing.T) {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.wakeLost, pc.bootTakes = "arrives", 6*time.Minute
		other := w.off("pc-b") // keeps the run going long enough to see it
		other.updTakes = 4 * time.Minute
		w.start(StartRequest{Hosts: []string{"pc-a", "pc-b"}})
		h := host(w.finish(), "pc-a")
		has(t, h, StGaveUp, "It did not come online within 5 min after the wake.", "It came online later", "it is ON") // later wakes were accepted
		if !pc.on || h.On != OnYes {
			t.Fatalf("on %v power %q: %s", pc.on, h.On, h.Text)
		}
	})

	t.Run("a machine that was on and out of reach is still never switched off", func(t *testing.T) {
		w := newWorld(t)
		pc := w.running("pc-a")
		pc.unreachable = true
		w.silentFor(pc, 20*time.Minute)
		pc.wakeLost = "arrives"
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		for i := 0; i < 30; i++ {
			w.tick()
		}
		pc.unreachable = false // back in reach during the wait, up for three days
		h := host(w.finish(), "pc-a")
		has(t, h, StLeftOn, "did not look freshly started")
		if !pc.on || strings.Contains(w.callsFor("pc-a"), "shutdown") {
			t.Fatalf("on %v requests %q: %s", pc.on, w.callsFor("pc-a"), h.Text)
		}
	})

	t.Run("cancelled, and the panel restarted, while the machine is waited for", func(t *testing.T) {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.wakeLost, pc.wakeBoots = "always", false
		r := w.start(StartRequest{Hosts: []string{"pc-a"}})
		for i := 0; i < 20; i++ {
			w.tick()
		}
		old, _ := w.restart()
		if oh := host(old, "pc-a"); oh.On != OnUnknown || !strings.Contains(oh.Text, "A wake request for it went out at") || !strings.Contains(oh.Text, "got no answer") {
			t.Fatalf("after a restart (%s): %s", oh.On, oh.Text)
		}
		w2 := newWorld(t)
		pc2 := w2.off("pc-a")
		pc2.wakeLost, pc2.wakeBoots = "always", false
		r = w2.start(StartRequest{Hosts: []string{"pc-a"}})
		for i := 0; i < 20; i++ {
			w2.tick()
		}
		if _, err := w2.m.CancelRun(r.ID, "Bob", false); err != nil {
			t.Fatal(err)
		}
		h := host(w2.finish(), "pc-a")
		has(t, h, StGaveUp, "The run was cancelled by Bob. A wake may have gone out to it (the request got no answer): if it switches on, it stays ON.")
		if h.On != OnUnknown {
			t.Fatalf("power %q", h.On)
		}
	})
}

// untilState ticks the world until the machine is in the state.
func untilState(t *testing.T, w *world, id, state string) {
	t.Helper()
	for i := 0; i < 3600; i++ {
		if host(active(w), id).State == state {
			return
		}
		w.tick()
	}
	t.Fatalf("%s never got to %q: %s", id, state, describe(active(w)))
}

// r2-p2-05 a): the answer to the shutdown request is lost. The run said "Left ON: the shutdown was not
// accepted (agent unreachable: timeout)", power "on", while the agent counted down and the machine went off.
func TestShutdownAnswerLost(t *testing.T) {
	t.Run("the agent took it and shows its countdown: off again", func(t *testing.T) {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.shutLost = "arrives"
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		h := host(w.finish(), "pc-a")
		has(t, h, StOff, "Woken, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.")
		if pc.on || h.On != OnNo || !h.ShutdownUnsure || w.callsFor("pc-a") != "wake update shutdown" {
			t.Fatalf("on %v power %q unsure %v requests %q: %s", pc.on, h.On, h.ShutdownUnsure, w.callsFor("pc-a"), h.Text)
		}
		if got := w.steps("pc-a"); got != "wake:ok update:ok shutdown:agent unreachable: timeout" {
			t.Fatalf("audit steps: %s", got)
		}
		if !logHas(h, "the shutdown request got no answer (agent unreachable: timeout): it may have arrived. If it did, it switches off at about ") {
			t.Fatalf("the machine's history: %v", h.Log)
		}
	})

	t.Run("the agent took it and is not heard of again: off when the countdown was due", func(t *testing.T) {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.shutLost = "arrives"
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		untilState(t, w, "pc-a", StShutting)
		pc.unreachable = true // the link that lost the answer stays down
		h := host(w.finish(), "pc-a")
		has(t, h, StOff, "Woken, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again (the shutdown request got no answer, but it went off when the countdown was due).")
		if pc.on || h.On != OnNo {
			t.Fatalf("on %v power %q: %s", pc.on, h.On, h.Text)
		}
	})

	t.Run("the agent never got it: its own word says so, and it is ON", func(t *testing.T) {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.shutLost = "never"
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		h := host(w.finish(), "pc-a")
		has(t, h, StLeftOn, "The shutdown request got no answer, and its agent shows no countdown: the request did not arrive. It is ON. It was updated to 17-bbbbbbb.")
		if !pc.on || h.On != OnYes || w.callsFor("pc-a") != "wake update shutdown" {
			t.Fatalf("on %v power %q requests %q: %s", pc.on, h.On, w.callsFor("pc-a"), h.Text)
		}
	})

	t.Run("the panel restarts meanwhile", func(t *testing.T) {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.shutLost = "arrives"
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		untilState(t, w, "pc-a", StShutting)
		old, _ := w.restart()
		if oh := host(old, "pc-a"); oh.On != OnUnknown || !strings.Contains(oh.Text, "The panel restarted after a shutdown request for it got no answer (it would be due at ") {
			t.Fatalf("after a restart (%s): %s", oh.On, oh.Text)
		}
	})
}

// r2-p2-05 b): after the countdown the machine does not go off: its agent is gone and the machine refuses
// the connection (the system hangs in its shutdown). The run said "switched off again", power "off", after
// 20 s. A refused connection is a machine that is on, here as everywhere else.
func TestRefusingAfterTheCountdownIsNotOff(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	pc.shutMode = "hang"
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	due := *host(active(w), "pc-a").ShutdownAt
	r := w.finish()
	h := host(r, "pc-a")
	has(t, h, StLeftOn, "It still shows as on 5 min after its shutdown was due (the machine refused the connection), and its agent does not answer: the shutdown did not finish. It is ON.",
		"It was updated to 17-bbbbbbb.")
	if !pc.on || h.On != OnYes {
		t.Fatalf("on %v power %q: %s", pc.on, h.On, h.Text)
	}
	if d := r.EndedAt.Sub(due); d < offGrace || d > offGrace+5*time.Second {
		t.Fatalf("the run let go %s after the countdown, want %s", d, offGrace)
	}
}

// r2-p2-05 c) and the last look in general: what the report says about a machine's power is what the run
// sees when it lets go of the machine, never what it assumes.
func TestTheReportSaysWhatIsSeen(t *testing.T) {
	// A cancel with "leave on" at every second from the shutdown request until the run would be over. Before:
	// in the 20 s after the countdown the report said "its shutdown countdown was called off. The run woke it
	// and it is ON." about a machine that was off.
	t.Run("cancel at every second of the countdown and after it", func(t *testing.T) {
		for _, ok := range []bool{false, true} { // false: the agent of a machine that is gone does not answer the cancel
			seen := map[string]int{}
			for k := 0; k <= 150; k++ {
				w := newWorld(t)
				pc := w.off("pc-a")
				pc.cancelOK = ok
				r := w.start(StartRequest{Hosts: []string{"pc-a"}})
				untilState(t, w, "pc-a", StShutting)
				due := *host(active(w), "pc-a").ShutdownAt
				w.advance(time.Duration(k) * time.Second)
				if active(w) == nil {
					break
				}
				late := !w.now.Add(time.Second).Before(due) // the run acts on a cancel at its next step, a second later
				if _, err := w.m.CancelRun(r.ID, "Bob", false); err != nil {
					t.Fatal(err)
				}
				at := w.now
				done := w.finish() // its judge fails "on" for a machine that is off, and the other way round
				h := host(done, "pc-a")
				seen[h.State+"/"+h.On]++
				switch {
				case !late && (h.State != StLeftOn || h.On != OnYes || !pc.on || !strings.Contains(h.Text, "its shutdown countdown was called off. The run woke it and it is ON.")):
					t.Fatalf("cancel %d s into the countdown: %s/%s on=%v: %s", k, h.State, h.On, pc.on, h.Text)
				case late && (h.State != StOff || h.On != OnNo || pc.on || !strings.HasSuffix(h.Text, " It is off (not answering).") || strings.Contains(h.Text, "it is ON")):
					t.Fatalf("cancel %d s into the countdown, after it ended: %s/%s on=%v: %s", k, h.State, h.On, pc.on, h.Text)
				}
				if d := done.EndedAt.Sub(at); d > lookFor+2*time.Second {
					t.Fatalf("cancel %d s into the countdown: the last look took %s, at most %s", k, d, lookFor)
				}
			}
			if seen[StLeftOn+"/"+OnYes] == 0 || seen[StOff+"/"+OnNo] == 0 {
				t.Fatalf("cancel answered %v: the sweep did not see both sides: %v", ok, seen)
			}
		}
	})

	t.Run("the run stops while a machine it cannot reach counts down: not confirmed", func(t *testing.T) {
		w := newWorld(t)
		pc := w.off("pc-a")
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		untilState(t, w, "pc-a", StShutting)
		w.advance(5 * time.Second)
		pc.unreachable = true
		w.advance(3 * time.Second)
		w.blocked = "control is disabled by an administrator"
		at := w.now
		r := w.finish()
		h := host(r, "pc-a")
		has(t, h, StLeftOn, "the run stopped (control is disabled by an administrator) while its shutdown was counting down (due at ",
			"). Asked to switch off, not confirmed: it does not answer now.")
		if h.On != OnUnknown || r.State != RunStopped || strings.Contains(h.Text, "it switches off by itself") {
			t.Fatalf("power %q run %s: %s", h.On, r.State, h.Text)
		}
		if d := r.EndedAt.Sub(at); d < lookFor-2*time.Second || d > lookFor+2*time.Second {
			t.Fatalf("the last look took %s, want %s", d, lookFor)
		}
		if strings.Contains(w.callsFor("pc-a"), "cancel") {
			t.Fatalf("a stopped run sends nothing: %s", w.callsFor("pc-a"))
		}
	})

	t.Run("the run stops while a machine that answers counts down: as before, at once", func(t *testing.T) {
		w := newWorld(t)
		w.off("pc-a")
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		untilState(t, w, "pc-a", StShutting)
		w.advance(5 * time.Second)
		w.blocked = "control is disabled by an administrator"
		at := w.now
		r := w.finish()
		h := host(r, "pc-a")
		has(t, h, StLeftOn, "while its shutdown was counting down (it ends at ", "it switches off by itself unless someone cancels that.")
		if h.On != OnYes || r.EndedAt.Sub(at) > 2*time.Second {
			t.Fatalf("power %q after %s: %s", h.On, r.EndedAt.Sub(at), h.Text)
		}
	})

	t.Run("its agent stops answering before the update", func(t *testing.T) {
		for _, c := range []struct {
			name  string
			gone  func(pc *machine)
			power string
			want  string
		}{
			{"nothing at all from it", func(pc *machine) { pc.unreachable = true }, OnUnknown,
				"Its agent stopped answering before the update. The run woke it. It does not answer now: not confirmed that it is on."},
			{"it refuses the connection", func(pc *machine) { pc.agentDown, pc.onBy = true, OnByRefused }, OnYes,
				"Its agent stopped answering before the update. The run woke it and it is ON."},
		} {
			w := newWorld(t)
			pc := w.off("pc-a")
			w.start(StartRequest{Hosts: []string{"pc-a"}})
			untilState(t, w, "pc-a", StOnline)
			c.gone(pc)
			h := host(w.finish(), "pc-a")
			has(t, h, StLeftOn, c.want)
			if h.On != c.power || !pc.on {
				t.Fatalf("%s: power %q on %v: %s", c.name, h.On, pc.on, h.Text)
			}
		}
	})

	t.Run("its agent does not answer again after the update", func(t *testing.T) {
		w := newWorld(t)
		pc := w.off("pc-a")
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		untilState(t, w, "pc-a", StSettling)
		pc.unreachable = true
		h := host(w.finish(), "pc-a")
		has(t, h, StLeftOn, "Its agent did not answer again after the update, so Fairy could not ask it to shut down. The run woke it. It was updated to 17-bbbbbbb. "+
			"It does not answer now: not confirmed that it is on.")
		if h.On != OnUnknown || strings.Contains(h.Text, "probably") {
			t.Fatalf("power %q: %s", h.On, h.Text)
		}
	})

	t.Run("a machine that was on and stops answering during its update", func(t *testing.T) {
		w := newWorld(t)
		pc := w.running("pc-a")
		pc.updMode = "stuck"
		w.start(StartRequest{UpdateOnly: []string{"pc-a"}})
		untilState(t, w, "pc-a", StUpdating)
		w.advance(20 * time.Second)
		pc.unreachable = true
		h := host(w.finish(), "pc-a")
		has(t, h, StStayedOn, "It was on and stays on. Its update did not work: no result 15 min after it was asked for. It does not answer now, so that is not confirmed.")
		if h.On != OnUnknown {
			t.Fatalf("power %q: %s", h.On, h.Text)
		}
	})
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
