package web

import (
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// Audit 5, round 3 (r3-p2-04) with the real poller and powerctl: the agent took the shutdown request and shows
// its countdown; at the end of the countdown the system does not go down (blocked on the machine); the link to
// it is down from 5 s before the countdown's end for 40 s. The run wrote "switched off again" and ended; the
// machine was on. The auditor's proof (TestR3P2SilenceAroundTheEndOfTheCountdownWithTheRealPoller).
func TestWakeUpdateSilenceAroundTheEndOfTheCountdownIsNotOff(t *testing.T) {
	e, f := lostFarm(t, "")
	var due time.Time
	e.b.Mu.Lock()
	e.b.Metrics["pc-a"] = func() (jsonx.M, error) {
		if at, ok := f.pending["pc-a"]; ok {
			due = at
		}
		if !due.IsZero() && !f.now.Before(due.Add(-5*time.Second)) && f.now.Before(due.Add(35*time.Second)) {
			return nil, devices.Errf("connection failed") // the link is down
		}
		return f.metrics("pc-a")
	}
	e.b.Mu.Unlock()
	if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"}); err != nil {
		t.Fatal(err)
	}
	var run *wakerun.Run
	for i := 0; i < 3600 && run == nil; i++ {
		f.tick()
		e.b.Mu.Lock()
		if !due.IsZero() && !f.now.Before(due) {
			f.on["pc-a"] = true // the shutdown did not happen: the machine stays up
		}
		e.b.Mu.Unlock()
		if a, hist := e.s.WakeRuns.Snapshot(); a == nil && len(hist) > 0 {
			run = hist[0]
		}
	}
	h := hostOf(run, "pc-a")
	for i := 0; i < 120; i++ {
		f.tick()
	}
	status := ""
	for _, x := range e.s.Poller.Snapshot() {
		if x.ID == "pc-a" {
			status = x.Status
		}
	}
	if h.On == wakerun.OnNo && status == "online" {
		t.Errorf("REPORTED OFF (%v after the countdown's end), AND IT IS ON: %s", run.EndedAt.Sub(due), h.Text)
	}
	if h.State != wakerun.StLeftOn || h.On != wakerun.OnYes || !strings.Contains(h.Text, "It is still on 5 min after its shutdown was due: the shutdown did not work. It is ON.") {
		t.Errorf("[%s] power %q: %s", h.State, h.On, h.Text)
	}
}

// The usual end with the real poller: the machine goes off when its countdown ends, and "switched off again"
// is written once the poller's own polls have seen 20 s of nothing after the run's due time, not before.
func TestWakeUpdateOffIsSeenByPollsAfterTheCountdown(t *testing.T) {
	e, f := lostFarm(t, "")
	if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"}); err != nil {
		t.Fatal(err)
	}
	var due, gone time.Time
	var run *wakerun.Run
	for i := 0; i < 3600 && run == nil; i++ {
		e.b.Mu.Lock()
		if at, ok := f.pending["pc-a"]; ok {
			due = at
		}
		e.b.Mu.Unlock()
		f.tick()
		if !due.IsZero() && gone.IsZero() && !f.on["pc-a"] {
			gone = f.now
		}
		if a, hist := e.s.WakeRuns.Snapshot(); a == nil && len(hist) > 0 {
			run = hist[0]
		}
	}
	h := hostOf(run, "pc-a")
	if h.State != wakerun.StOff || h.On != wakerun.OnNo || f.on["pc-a"] {
		t.Fatalf("[%s] power %q, machine on %v: %s", h.State, h.On, f.on["pc-a"], h.Text)
	}
	if after := run.EndedAt.Sub(gone); after < 20*time.Second || after > 30*time.Second {
		t.Fatalf("\"switched off again\" %v after the machine went (countdown due at %s), want the 20 s of silence and a few more", after, due.Format("15:04:05"))
	}
}
