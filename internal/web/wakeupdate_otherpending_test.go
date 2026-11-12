package web

import (
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// Recheck of fix7r, RC-04, with the real poller and powerctl. The run's shutdown of pc-a is accepted and its
// countdown runs. A person calls it off on the machine's page and asks for a reboot in half an hour: the agent
// shows that reboot as pending now. Then the kill switch stops the run, or an admin cancels it. The run's
// cancel (the agent's one cancel, sent whatever the panel sees as pending since R7A-09) took the person's
// reboot away, and the report said "its shutdown countdown was called off".
//
// And what stays: while the agent shows the run's own shutdown, the cancel goes out and the machine stays on.
func TestWakeUpdateTheRunsCancelLeavesAPersonsRebootAlone(t *testing.T) {
	for _, c := range []struct {
		name   string
		kill   bool // the kill switch stops the run (false: Cancel run)
		reboot bool // a person called the run's shutdown off and asked for a reboot, 20 s into the countdown
	}{
		{"the kill switch, a person's reboot is pending", true, true},
		{"Cancel run, a person's reboot is pending", false, true},
		{"the kill switch, the run's shutdown is pending", true, false},
		{"Cancel run, the run's shutdown is pending", false, false},
	} {
		e, f := lostFarm(t, "")
		var rebootAt time.Time // not zero: the agent's pending action is a reboot then
		e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
			if agent != "pc-a" || path != "/power" {
				return 0, nil, nil
			}
			if strings.Contains(e.b.Requests[len(e.b.Requests)-1], "action:cancel") {
				rebootAt = time.Time{} // the agent's cancel calls off whatever is pending
				return 200, jsonx.M{"action": "cancel", "pending": nil}, nil
			}
			return 200, jsonx.M{"action": "shutdown", "pending": jsonx.M{"action": "shutdown", "at": float64(f.now.Add(120 * time.Second).Unix()), "delay_s": 120.0, "requested_by": "x"}}, nil
		}
		e.b.Mu.Lock()
		e.b.Metrics["pc-a"] = func() (jsonx.M, error) {
			m, err := f.metrics("pc-a")
			if err == nil && !rebootAt.IsZero() {
				m["power_control"].(jsonx.M)["pending"] = jsonx.M{"action": "reboot", "at": float64(rebootAt.Unix()), "delay_s": 1800.0, "requested_by": "Carol"}
			}
			return m, err
		}
		e.b.Mu.Unlock()
		r, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"})
		if err != nil {
			t.Fatal(err)
		}
		var run *wakerun.Run
		asked, stopped := false, false
		for i := 0; i < 3600 && run == nil; i++ {
			f.tick()
			a, hist := e.s.WakeRuns.Snapshot()
			if a != nil && hostOf(a, "pc-a").State == wakerun.StShutting {
				since := f.now.Sub(hostOf(a, "pc-a").Since)
				if c.reboot && !asked && since >= 20*time.Second {
					asked = true
					e.b.Mu.Lock() // the person: Cancel on the machine's page, then Reboot with a delay
					delete(f.pending, "pc-a")
					rebootAt = f.now.Add(30 * time.Minute)
					e.b.Mu.Unlock()
				}
				if !stopped && since >= 30*time.Second {
					stopped = true
					if c.kill {
						if err := e.s.Poller.Admin.SetControlDisabled(true, "root (cli)", "stop everything"); err != nil {
							t.Fatal(err)
						}
					} else if _, err := e.s.WakeRuns.CancelRun(r.ID, "Bob", false); err != nil {
						t.Fatal(err)
					}
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
		e.b.Mu.Lock()
		at := rebootAt
		e.b.Mu.Unlock()
		for i := 0; i < 180; i++ { // past the time the run's countdown would have ended
			f.tick()
		}
		cancels := 0
		e.b.Mu.Lock()
		for _, q := range e.b.Requests {
			if strings.HasPrefix(q, "pc-a POST /power") && strings.Contains(q, "action:cancel") {
				cancels++
			}
		}
		still, on := !rebootAt.IsZero(), f.on["pc-a"]
		e.b.Mu.Unlock()
		why := map[bool]string{true: "the run stopped (control is disabled by an administrator)", false: "The run was cancelled by Bob"}[c.kill]
		if c.reboot {
			if cancels != 0 || !still || !on {
				t.Errorf("%s: %d cancel(s) reached the agent, want none; the person's reboot is still pending: %v; the machine is on: %v", c.name, cancels, still, on)
			}
			want := why + ": another power action is pending on it now (reboot at " + at.UTC().Format("15:04:05 UTC") + ", by the machine's own clock), not the shutdown the run asked for, " +
				"so the run sent no cancel: its agent's cancel would call that one off. It came online after the wake and it is ON now. Fairy no longer watches it. " +
				"It was updated to 17-bbbbbbb."
			if h.State != wakerun.StLeftOn || h.On != wakerun.OnYes || h.Text != want {
				t.Errorf("%s: [%s] %q: %s", c.name, h.State, h.On, h.Text)
			}
			continue
		}
		want := why + ": its shutdown countdown was called off. It came online after the wake and it is ON. It was updated to 17-bbbbbbb."
		if cancels != 1 || !on || h.State != wakerun.StLeftOn || h.On != wakerun.OnYes || h.Text != want {
			t.Errorf("%s: %d cancel(s) reached the agent, want 1; the machine is on: %v; [%s] %q: %s", c.name, cancels, on, h.State, h.On, h.Text)
		}
	}
}
