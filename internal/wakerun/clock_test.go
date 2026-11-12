package wakerun

import (
	"context"
	"testing"
	"time"
)

// Audit 5, round 3 (r3-p2-01, -02, -05): the run and the clocks. The agent says when its countdown ends by the
// machine's clock; the run used to compare that time with the panel's time of day. These are the auditor's
// proofs (TestR3P2...), unchanged in what they do and assert; each fails without the run's own steady clock
// for the countdown.

// skewOps is the world with a machine whose own clock is BEHIND the panel's by `behind`. The agent says when
// its countdown ends by its own clock (the answer to the shutdown request, and "pending.at" in every poll);
// the machine really goes off `delay` after the request, whatever its clock says (the world's truth is not
// touched). A freshly woken machine with a flat RTC battery, or one that keeps local time in its RTC (dual
// boot), is exactly this until its time is synchronised.
type skewOps struct {
	*world
	behind time.Duration
}

func (s skewOps) Observe(id string) Observation {
	o := s.world.Observe(id)
	if o.Pending != nil {
		p := *o.Pending
		p.At = p.At.Add(-s.behind)
		o.Pending = &p
	}
	return o
}

func (s skewOps) Shutdown(ctx context.Context, id string, delay time.Duration, by string, grant any) (time.Time, error) {
	at, err := s.world.Shutdown(ctx, id, delay, by, grant)
	if !at.IsZero() {
		at = at.Add(-s.behind)
	}
	return at, err
}

// over lets the world run until the run is over and returns it, without the checks of finish (the tests
// below make their own, after the countdown has run out).
func over(t *testing.T, w *world) *Run {
	t.Helper()
	for i := 0; i < 3600; i++ {
		if a, hist := w.m.Snapshot(); a == nil {
			return hist[0]
		}
		w.tick()
	}
	a, _ := w.m.Snapshot()
	t.Fatalf("the run did not end in an hour: %s", describe(a))
	return nil
}

// r3-p2-01: the machine's clock is behind (or ahead). Its countdown (120 s) runs and it goes off as asked.
// The run read the agent's "at" against the PANEL's clock: with ten minutes behind, "due" was 8 minutes ago
// at the first look, so "still on 5 min after its shutdown was due" was true in the first second of the
// countdown. With the clock ahead the run let go at once ("or the machine's clock is ahead of the panel's").
func TestAMachineWhoseClockIsWrongIsWatchedByTheRunsOwnClock(t *testing.T) {
	for _, behind := range []time.Duration{10 * time.Minute, 90 * time.Second, -10 * time.Minute, -26 * time.Hour} {
		w := newWorld(t)
		pc := w.off("pc-a")
		w.m.Ops = skewOps{w, behind}
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		untilState(t, w, "pc-a", StShutting)
		asked := w.now
		r := over(t, w)
		h := host(r, "pc-a")
		took := w.now.Sub(asked)
		w.advance(3 * time.Minute) // the countdown the agent took runs out
		if h.On == OnYes && !pc.on {
			t.Errorf("clock behind by %v: WRONG REPORT: the run says %q (%s) %v after the request, and the machine went off by the run's own shutdown: %s", behind, h.On, h.State, took, h.Text)
		}
		if h.State != StOff || h.On != OnNo || h.Text != "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again." {
			t.Errorf("clock behind by %v: [%s] %s: %s", behind, h.State, h.On, h.Text)
		}
		if took < 120*time.Second || took > 150*time.Second {
			t.Errorf("clock behind by %v: the run wrote its result %v after the request, want the countdown (120 s) and the 20 s of silence", behind, took)
		}
		w.wellReported(r)
	}
}

// r3-p2-02: a Proxmox node whose clock is 2 minutes behind. A guest starts 30 s into the countdown. Without
// the skew the run calls the countdown off (TestAGuestThatStartsDuringTheCountdownCallsItOff). With it, the
// "last 15 s" in which the guests are no longer asked about began before the countdown did: the run never
// asked, and the node went off under the guest.
func TestANodeWhoseClockIsBehindIsStillAskedAboutItsGuests(t *testing.T) {
	for _, behind := range []time.Duration{0, 2 * time.Minute, 20 * time.Minute, -20 * time.Minute} {
		w := newWorld(t)
		pc := w.off("pve1")
		w.m.Ops = skewOps{w, behind}
		w.start(StartRequest{Hosts: []string{"pve1"}})
		untilState(t, w, "pve1", StShutting)
		before := w.guestsAsked
		w.advance(30 * time.Second)
		pc.guests, pc.guestNames = 1, "web01" // a guest starts on the node, 90 s before it would go off
		r := over(t, w)
		h := host(r, "pve1")
		if !pc.on {
			t.Errorf("clock behind by %v: THE NODE WENT OFF with a guest running on it (started 90 s before the end of the countdown); guests asked about %d times; requests: %s; [%s] %s",
				behind, w.guestsAsked-before, w.callsFor("pve1"), h.State, h.Text)
		}
		has(t, h, StLeftOn, "Left ON: 1 guest is running on it (web01) since its shutdown was asked for: its shutdown countdown was called off. It came online after the wake and it is ON.")
		if got := w.callsFor("pve1"); got != "wake update shutdown cancel" {
			t.Errorf("clock behind by %v: requests: %s", behind, got)
		}
	}
}

// jumpOps is the world seen from a panel whose time of day is `*ahead` in front of the machines' (a clock that
// was set while the run was going: a time server's correction, a person, a VM host that resumed). The world's
// own time is the truth and the machines' clock; what the panel's poller stamps (SeenAt, QuietSince) moves
// with the panel's clock; what an agent says (pending.at) does not. The steady clock is not touched.
type jumpOps struct {
	*world
	ahead *time.Duration
}

func (j jumpOps) Observe(id string) Observation {
	o := j.world.Observe(id)
	if !o.SeenAt.IsZero() {
		o.SeenAt = o.SeenAt.Add(*j.ahead)
	}
	if !o.QuietSince.IsZero() {
		o.QuietSince = o.QuietSince.Add(*j.ahead)
	}
	return o
}

func jumpWorld(t *testing.T) (*world, *time.Duration) {
	w := newWorld(t)
	ahead := new(time.Duration)
	w.m.Ops = jumpOps{w, ahead}
	w.m.Now = func() time.Time {
		w.clock.Lock()
		defer w.clock.Unlock()
		return w.now.Add(*ahead)
	}
	return w, ahead
}

// r3-p2-01, second way in: the PANEL's clock is set, forward or back, while a countdown runs. Set forward by
// ten minutes, "due" was long ago at the next look: "the shutdown did not work. It is ON.", and the machine
// went off as asked.
func TestThePanelsClockSetDuringACountdown(t *testing.T) {
	for _, jump := range []time.Duration{10 * time.Minute, -10 * time.Minute, 26 * time.Hour} {
		w, ahead := jumpWorld(t)
		pc := w.off("pc-a")
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		untilState(t, w, "pc-a", StShutting)
		asked := w.mono
		w.advance(10 * time.Second)
		*ahead = jump
		r := over(t, w)
		h := host(r, "pc-a")
		took := w.mono - asked
		w.advance(3 * time.Minute)
		if h.On == OnYes && !pc.on {
			t.Errorf("clock set by %v: WRONG REPORT: the run says %q and the machine went off by the run's own countdown: %s", jump, h.On, h.Text)
		}
		if h.State != StOff || h.On != OnNo || pc.on || took < 120*time.Second || took > 150*time.Second {
			t.Errorf("clock set by %v: [%s] %s after %v (machine on: %v): %s", jump, h.State, h.On, took, pc.on, h.Text)
		}
	}
}

// r3-p2-05: the panel's clock is set BACK by two hours while the run waits for a machine that never comes
// online. The wait for a machine, the pacer's minute and the run's own limit were differences of two times of
// day, so the run, and the one run slot, waited for the two hours too.
func TestThePanelsClockSetBackDoesNotHoldTheRun(t *testing.T) {
	w, ahead := jumpWorld(t)
	pc := w.off("pc-a")
	pc.wakeBoots = false
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StWaking)
	*ahead = -2 * time.Hour
	began := w.mono
	for i := 0; i < 3*3600; i++ {
		if a, _ := w.m.Snapshot(); a == nil {
			break
		}
		w.tick()
	}
	took := w.mono - began
	a, hist := w.m.Snapshot()
	if a != nil || took > 6*time.Minute {
		t.Fatalf("the run (online timeout 5 min) held its slot for %v of real time after a wake that should be given up on after 5 minutes (still going: %v)", took, a != nil)
	}
	has(t, host(hist[0], "pc-a"), StGaveUp, "It did not come online within 5 min after the wake.")
	// the run's own limit holds on the steady clock too: a run that is stuck in a later wait ends by it
	w, ahead = jumpWorld(t)
	pc = w.off("pc-a")
	pc.updMode = "stuck"
	r := w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StUpdating)
	*ahead = -6 * time.Hour
	began = w.mono
	for i := 0; i < 5*3600; i++ {
		if a, _ := w.m.Snapshot(); a == nil {
			break
		}
		w.tick()
	}
	if a, _ := w.m.Snapshot(); a != nil || w.mono-began > w.m.maxRun(r)+time.Minute {
		t.Fatalf("a run whose clock was set back 6 h was not over after its own limit (%v): still going %v after %v", w.m.maxRun(r), a != nil, w.mono-began)
	}
	if !pc.on || pc.shutdowns != 0 {
		t.Fatalf("the machine: on %v, shutdown requests %d", pc.on, pc.shutdowns)
	}
}
