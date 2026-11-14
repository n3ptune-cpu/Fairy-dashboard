package wakerun

import (
	"testing"
	"time"
)

// MayGoOff is what the panel's strip, Keep on and "Stop helper" go by: the machines that may still switch
// themselves off by a run's ticket, with the time as far as the panel knows it.

func mayGo(l []ArmedHost, id string) *ArmedHost {
	for i := range l {
		if l[i].ID == id {
			return &l[i]
		}
	}
	return nil
}

// A run that is going: the armed machine with its time, the machine whose arming got no answer with the
// latest time it can be, and not the machine that refused its ticket. After a restart of the panel the same
// two are still listed (no longer as machines of an active run), until their time, their notice and the
// allowance for an update are over.
func TestMayGoOffWhileARunWaitsAndAfterARestart(t *testing.T) {
	w := newWorld(t)
	w.checkedOff("pc-armed")
	w.checkedOff("pc-refused").chk.armSays = "not_fresh"
	w.checkedOff("pc-lost").chk.armLost = "never"
	if got := w.m.MayGoOff(w.now); len(got) != 0 {
		t.Fatalf("before any run: %+v", got)
	}
	w.start(StartRequest{Hosts: []string{"pc-armed", "pc-refused", "pc-lost"}, Latest: "17-bbbbbbb"})
	started := w.now
	act := w.waitFor(8*time.Minute, "three wait", func(r *Run) bool {
		return stateOf(r, "pc-armed") == StApproval && stateOf(r, "pc-refused") == StApproval && stateOf(r, "pc-lost") == StApproval
	})
	off := *host(act, "pc-armed").SelfOffAt
	check := func(when string, list []ArmedHost, active bool) {
		t.Helper()
		if len(list) != 2 || mayGo(list, "pc-refused") != nil {
			t.Fatalf("%s: %+v", when, list)
		}
		a, l := mayGo(list, "pc-armed"), mayGo(list, "pc-lost")
		if a == nil || !a.Sure || a.Latest || a.Active != active || a.At == nil || !a.At.Equal(off) || a.RunID != act.ID || a.Name != host(act, "pc-armed").Name {
			t.Fatalf("%s, the armed machine: %+v", when, a)
		}
		// the latest a ticket can arrive is valid_s and a minute after the start; the wait counts from there
		latest := started.Add(420*time.Second + time.Minute + 960*time.Second)
		if l == nil || l.Sure || !l.Latest || l.Active != active || l.At == nil || l.At.Sub(latest).Abs() > 2*time.Second {
			t.Fatalf("%s, the machine whose arming got no answer: %+v (latest %s)", when, l, clock(latest))
		}
	}
	check("while the run waits", w.m.MayGoOff(w.now), true)
	w.tick() // the journal is written at the end of a step
	// the panel starts again on the same journal
	m2 := New(w.m.path, w.m.Settings, w)
	m2.Now = w.m.Now
	if err := m2.Load(); err != nil {
		t.Fatal(err)
	}
	_, hist := m2.Snapshot()
	if r := hist[0]; r.State != RunInterrupted || !host(r, "pc-armed").MayGoOff || !host(r, "pc-lost").MayGoOff || host(r, "pc-refused").MayGoOff {
		t.Fatalf("after the restart: %s", describe(r))
	}
	w.m = m2
	check("after the restart", w.m.MayGoOff(w.now), false)
	// until its time, its notice (2 min), the minute it may be late and the quarter of an hour of an update
	gone := off.Add(2*time.Minute + time.Minute + 15*time.Minute)
	if list := w.m.MayGoOff(gone); mayGo(list, "pc-armed") == nil {
		t.Fatalf("at the end of its time: %+v", list)
	}
	if list := w.m.MayGoOff(gone.Add(time.Second)); mayGo(list, "pc-armed") != nil || mayGo(list, "pc-lost") == nil {
		t.Fatalf("a second later: %+v", list)
	}
	if list := w.m.MayGoOff(gone.Add(time.Hour)); len(list) != 0 {
		t.Fatalf("an hour later: %+v", list)
	}
	// a clock that was set far back does not bring them back for ever
	if list := w.m.MayGoOff(started.Add(-2 * time.Hour)); len(list) != 0 {
		t.Fatalf("with the clock set two hours back: %+v", list)
	}
}

// Nobody is listed who cannot go: a machine that was confirmed, one that was seen to switch itself off, and
// one whose switch-off was called off.
func TestMayGoOffNamesNobodyWhoCannotGo(t *testing.T) {
	t.Run("confirmed", func(t *testing.T) {
		w, _, _ := armedRun(t, nil)
		if len(w.m.MayGoOff(w.now)) != 1 {
			t.Fatal("armed and not listed")
		}
		if _, err := w.confirm("Ann"); err != nil {
			t.Fatal(err)
		}
		if got := w.m.MayGoOff(w.now); len(got) != 0 {
			t.Fatalf("confirmed: %+v", got)
		}
		done := w.finish()
		if got := w.m.MayGoOff(w.now); len(got) != 0 || host(done, "pc-a").MayGoOff {
			t.Fatalf("after the run: %+v", got)
		}
	})
	t.Run("switched itself off", func(t *testing.T) {
		w, _, _ := armedRun(t, nil)
		w.waitFor(20*time.Minute, "it is switching itself off", func(r *Run) bool { return stateOf(r, "pc-a") == StSelfOff })
		if got := w.m.MayGoOff(w.now); len(got) != 1 || !got[0].Active {
			t.Fatalf("while its notice counts: %+v", got)
		}
		done := w.finish()
		if got := w.m.MayGoOff(w.now); len(got) != 0 || host(done, "pc-a").MayGoOff || host(done, "pc-a").State != StOff {
			t.Fatalf("after it went off: %+v %s", got, describe(done))
		}
	})
	t.Run("kept on", func(t *testing.T) {
		w, pc, _ := armedRun(t, nil)
		pc.info.OptedIn = false // the run lets go of it and calls its switch-off off
		done := w.finish()
		if got := w.m.MayGoOff(w.now); len(got) != 0 || host(done, "pc-a").MayGoOff || host(done, "pc-a").State != StLeftOn {
			t.Fatalf("after its switch-off was called off: %+v %s", got, describe(done))
		}
	})
}
