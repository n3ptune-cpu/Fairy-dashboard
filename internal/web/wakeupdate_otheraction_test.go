package web

import (
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// Found with RC-04 of the recheck of fix7r, with the real poller and powerctl: the run's shutdown of pc-a is
// accepted and its countdown runs. 20 s into it a person calls it off on the machine's page and asks for a
// reboot, which starts after the run's own due time. The machine restarts: its agent gives nothing for 45 s,
// then answers again. The run took the silence after its due time for its own shutdown: "... and switched off
// again." The machine was on again a minute later.
func TestWakeUpdateARebootAfterTheDueTimeIsNotSwitchedOffAgain(t *testing.T) {
	for _, c := range []struct {
		name string
		// after: the person's reboot starts this long after the end of the run's countdown (negative: nobody
		// asks for anything, and the machine goes off at the end of the countdown)
		after time.Duration
	}{
		{"a reboot 10 s after the due time", 10 * time.Second},
		{"a reboot 40 s after the due time", 40 * time.Second},
		{"a reboot 2 min after the due time", 2 * time.Minute},
		{"what must stay: no other action, the machine goes off", -1},
	} {
		e, f := lostFarm(t, "")
		var rebootAt time.Time // not zero: the agent's pending action is a reboot, which starts then
		e.b.Mu.Lock()
		e.b.Metrics["pc-a"] = func() (jsonx.M, error) {
			if !rebootAt.IsZero() && !f.now.Before(rebootAt) && f.now.Before(rebootAt.Add(45*time.Second)) {
				return nil, devices.Errf("connection failed") // it restarts
			}
			m, err := f.metrics("pc-a")
			if err == nil && !rebootAt.IsZero() && f.now.Before(rebootAt) {
				m["power_control"].(jsonx.M)["pending"] = jsonx.M{"action": "reboot", "at": float64(rebootAt.Unix()), "delay_s": 1800.0, "requested_by": "Carol"}
			}
			return m, err
		}
		e.b.Mu.Unlock()
		if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"}); err != nil {
			t.Fatal(err)
		}
		var run *wakerun.Run
		var due time.Time
		for i := 0; i < 3600 && run == nil; i++ {
			f.tick()
			a, hist := e.s.WakeRuns.Snapshot()
			if a != nil && c.after >= 0 && rebootAt.IsZero() {
				if h := hostOf(a, "pc-a"); h.State == wakerun.StShutting && f.now.Sub(h.Since) >= 20*time.Second {
					e.b.Mu.Lock() // the person: Cancel on the machine's page, then Reboot with a delay
					due = f.pending["pc-a"]
					delete(f.pending, "pc-a")
					rebootAt = due.Add(c.after)
					e.b.Mu.Unlock()
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
		ended := f.now
		for i := 0; i < 360; i++ { // past the reboot
			f.tick()
		}
		status := ""
		for _, x := range e.s.Poller.Snapshot() {
			if x.ID == "pc-a" {
				status = x.Status
			}
		}
		if c.after < 0 {
			if h.State != wakerun.StOff || h.On != wakerun.OnNo || h.Text != cutOff || status == "online" {
				t.Errorf("%s: [%s] power %q, the machine is %q afterwards: %s", c.name, h.State, h.On, status, h.Text)
			}
			continue
		}
		if h.On == wakerun.OnNo && status == "online" {
			t.Errorf("%s: REPORTED OFF, AND IT IS ON (the run ended %v after the due time): %s", c.name, ended.Sub(due), h.Text)
		}
		want := "Its shutdown was called off, in the panel or on the machine, and another power action is pending on it now (reboot at " + rebootAt.UTC().Format("15:04:05 UTC") +
			", by the machine's own clock). It is ON now. Fairy no longer watches it. It was updated to 17-bbbbbbb."
		if h.State != wakerun.StLeftOn || h.On != wakerun.OnYes || h.Text != want || status != "online" {
			t.Errorf("%s: [%s] power %q, the machine is %q afterwards: %s", c.name, h.State, h.On, status, h.Text)
		}
		for _, q := range f.requests() {
			if strings.HasPrefix(q, "pc-a") && q != "pc-a /update" && q != "pc-a /power" {
				t.Errorf("%s: requests: %v", c.name, f.requests())
			}
		}
		if n := strings.Count(strings.Join(f.requests(), ","), "pc-a /power"); n != 1 {
			t.Errorf("%s: %d power requests reached pc-a, want the run's shutdown and nothing else (no cancel)", c.name, n)
		}
	}
}
