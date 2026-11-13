package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// Second review of fix7a (R7A-09), with the real poller and powerctl: the run's shutdown request got no answer
// (it may have arrived), and the panel's own view of the machine shows nothing pending: the polls since then
// failed, or have not shown the countdown yet. The kill switch stops the run, or an admin cancels it. The
// run's cancel was refused inside the panel ("nothing pending": powerctl offered the cancel only when the
// panel's view showed a pending action), nothing was sent, and a machine that did get the request went off.
// The run's cancel of its own shutdown is now sent whatever the panel's view shows: the agent answers 200
// when nothing is pending.
func TestWakeUpdateTheRunsCancelIsSentWhenThePanelSeesNothingPending(t *testing.T) {
	for _, c := range []struct {
		name    string
		kill    bool // the kill switch stops the run (false: Cancel run)
		arrived bool // the shutdown request reached the machine: its countdown runs
	}{
		{"the kill switch, the request arrived", true, true},
		{"the kill switch, the request did not arrive", true, false},
		{"Cancel run, the request arrived", false, true},
		{"Cancel run, the request did not arrive", false, false},
	} {
		e, f := lostFarm(t, "")
		asked := false // the shutdown was asked for: from then on the polls of the machine fail
		e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
			if agent != "pc-a" || path != "/power" {
				return 0, nil, nil
			}
			req := e.b.Requests[len(e.b.Requests)-1]
			if strings.Contains(req, "action:cancel") {
				return 200, jsonx.M{"action": "cancel", "pending": nil}, nil // what the agent answers, with or without a countdown
			}
			asked = true
			if !c.arrived { // the farm acts on every request it finds: this one never got there
				e.b.Requests[len(e.b.Requests)-1] = strings.Replace(req, "action:shutdown", "action:lost", 1)
			}
			return 0, nil, context.DeadlineExceeded
		}
		e.b.Mu.Lock()
		e.b.Metrics["pc-a"] = func() (jsonx.M, error) {
			if asked {
				return nil, devices.Errf("timeout")
			}
			return f.metrics("pc-a")
		}
		e.b.Mu.Unlock()
		r, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"})
		if err != nil {
			t.Fatal(err)
		}
		var run *wakerun.Run
		stopped := false
		for i := 0; i < 3600 && run == nil; i++ {
			f.tick()
			a, hist := e.s.WakeRuns.Snapshot()
			if a != nil && !stopped && hostOf(a, "pc-a").State == wakerun.StShutting && f.now.Sub(hostOf(a, "pc-a").Since) >= 10*time.Second {
				stopped = true
				for _, x := range e.s.Poller.Snapshot() {
					if x.ID == "pc-a" && x.PowerPending != nil {
						t.Fatalf("%s: set-up: the panel's view shows a pending action", c.name)
					}
				}
				if c.kill {
					if err := e.s.Poller.Admin.SetControlDisabled(true, "root (cli)", "stop everything"); err != nil {
						t.Fatal(err)
					}
				} else if _, err := e.s.WakeRuns.CancelRun(r.ID, "Bob", false); err != nil {
					t.Fatal(err)
				}
			}
			if a == nil && len(hist) > 0 {
				run = hist[0]
			}
		}
		if run == nil {
			t.Fatal(c.name, "the run did not end")
		}
		h := hostOf(run, "pc-a")
		for i := 0; i < 180; i++ { // past the time the countdown would have ended
			f.tick()
		}
		cancels := 0
		for _, q := range e.b.Requests {
			if strings.HasPrefix(q, "pc-a POST /power") && strings.Contains(q, "action:cancel") {
				cancels++
			}
		}
		// (audit 6, r4-p2-02: one cancel, until the request was seen to be able to arrive after it. The run never
		// saw a countdown, so it asks again every 10 s. Review of fix8, RF8-02: for as long as the request can
		// still arrive, 5 minutes after its own due time; it was until the due time, 11 or 12 cancels.)
		if cancels < 41 || cancels > 42 || !f.on["pc-a"] {
			t.Errorf("%s: %d cancel(s) reached the agent, want one every 10 s until 5 min after the due time; the machine is on: %v; [%s] %q: %s", c.name, cancels, f.on["pc-a"], h.State, h.On, h.Text)
		}
		why := map[bool]string{true: "the run stopped (control is disabled by an administrator)", false: "The run was cancelled by Bob"}[c.kill]
		// (Its agent answered the run's own request a moment ago: that is a machine that runs, whatever the polls
		// say. It held "... none is pending. It came online after the wake and it is ON.", power "on": the request
		// can still arrive after the last cancel, so the power is "not known" and the sentence says what may come.)
		want := why + ": its shutdown request had got no answer, so its agent was asked to call off a countdown, and it answered: none is pending. " +
			"The request may still arrive: the machine then switches off 2 min later unless someone cancels that on its page. It is on now, and Fairy no longer watches it. " +
			"It was updated to 17-bbbbbbb."
		if h.State != wakerun.StLeftOn || h.On != wakerun.OnUnknown || h.Text != want {
			t.Errorf("%s: [%s] %q: %s", c.name, h.State, h.On, h.Text)
		}
	}
}
