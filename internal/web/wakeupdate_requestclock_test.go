package web

import (
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// Review of fix7r2, R2-01, with the real poller and powerctl. The run's shutdown of pc-a is accepted and its
// countdown runs. 30 s in, the panel's time of day is set (the steady clock goes on). 60 s in, a person calls
// the shutdown off on the machine's page and asks for a reboot.
//
// watch: nothing else happens. The reboot starts 10 s after the end of the run's countdown and the machine
// gives nothing for 45 s. With the clock set back an hour no answer was "from after the run's request" any
// more: the run did not see the reboot and wrote "switched off again" for a machine that was restarting.
// cancel: the run is cancelled in the second of the person's reboot. Its cancel went out and called the reboot off.
func TestWakeUpdateAPersonsRebootIsSeenWhenThePanelsClockWasSet(t *testing.T) {
	for _, c := range []struct {
		name   string
		set    time.Duration
		cancel bool
	}{
		{"watch, the clock set back one hour", -time.Hour, false},
		{"watch, the clock set forward one hour", time.Hour, false},
		{"watch, the clock set back two seconds", -2 * time.Second, false},
		{"cancel, the clock set back one hour", -time.Hour, true},
		{"cancel, the clock set forward one hour", time.Hour, true},
		{"cancel, the clock set back two seconds", -2 * time.Second, true},
	} {
		e, f, skew := cutFarm(t)
		var rebootAt time.Time // not zero: the agent's pending action is a reboot, which starts then (by the machine's clock)
		e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
			if path != "/power" {
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
			if !rebootAt.IsZero() && !f.now.Before(rebootAt) && f.now.Before(rebootAt.Add(45*time.Second)) {
				return nil, devices.Errf("connection failed") // it restarts
			}
			m, err := f.metrics("pc-a")
			if err == nil && !rebootAt.IsZero() && f.now.Before(rebootAt) {
				m["power_control"].(jsonx.M)["pending"] = jsonx.M{"action": "reboot", "at": float64(rebootAt.Unix()), "delay_s": 70.0, "requested_by": "Carol"}
			}
			return m, err
		}
		e.b.Mu.Unlock()
		r, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"})
		if err != nil {
			t.Fatal(err)
		}
		var run *wakerun.Run
		var at time.Time
		set := false
		for i := 0; i < 3600 && run == nil; i++ {
			f.tick()
			a, hist := e.s.WakeRuns.Snapshot()
			if a != nil {
				// (the farm's own clock is the steady one here; the panel's time of day is f.now plus the skew)
				if h := hostOf(a, "pc-a"); h.State == wakerun.StShutting {
					e.b.Mu.Lock()
					due, counting := f.pending["pc-a"]
					e.b.Mu.Unlock()
					if counting && !set && !f.now.Before(due.Add(-90*time.Second)) {
						set, *skew = true, c.set
					}
					if counting && set && !f.now.Before(due.Add(-60*time.Second)) {
						e.b.Mu.Lock() // the person: Cancel on the machine's page, then Reboot
						delete(f.pending, "pc-a")
						rebootAt = due.Add(10 * time.Second)
						at = rebootAt
						e.b.Mu.Unlock()
						if c.cancel {
							if _, err := e.s.WakeRuns.CancelRun(r.ID, "Bob", false); err != nil {
								t.Fatal(err)
							}
						}
					}
				}
			}
			if a == nil && len(hist) > 0 {
				run = hist[0]
			}
		}
		if run == nil || at.IsZero() {
			t.Fatal(c.name, "the run did not end, or never got to its countdown")
		}
		h := hostOf(run, "pc-a")
		for i := 0; i < 300; i++ { // past the reboot
			f.tick()
		}
		status := ""
		for _, x := range e.s.Poller.Snapshot() {
			if x.ID == "pc-a" {
				status = x.Status
			}
		}
		cancels := 0
		e.b.Mu.Lock()
		for _, q := range e.b.Requests {
			if strings.HasPrefix(q, "pc-a POST /power") && strings.Contains(q, "action:cancel") {
				cancels++
			}
		}
		e.b.Mu.Unlock()
		if h.On == wakerun.OnNo && status == "online" {
			t.Errorf("%s: REPORTED OFF, AND IT IS ON: %s", c.name, h.Text)
		}
		action := "another power action is pending on it now (reboot at " + at.UTC().Format("15:04:05 UTC") + ", by the machine's own clock)"
		want := "Its shutdown was called off, in the panel or on the machine, and " + action + ". It is ON now. Fairy no longer watches it. It was updated to 17-bbbbbbb."
		if c.cancel {
			want = "The run was cancelled by Bob: " + action + ", not the shutdown the run asked for, so the run sent no cancel: its agent's cancel would call that one off. " +
				"It came online after the wake and it is ON now. Fairy no longer watches it. It was updated to 17-bbbbbbb."
		}
		if cancels != 0 || h.State != wakerun.StLeftOn || h.On != wakerun.OnYes || h.Text != want || status != "online" {
			t.Errorf("%s: %d cancel(s) reached the agent, want none; [%s] power %q, the machine is %q afterwards: %s", c.name, cancels, h.State, h.On, status, h.Text)
		}
	}
}
