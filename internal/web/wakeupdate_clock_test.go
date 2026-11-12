package web

import (
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// Audit 5, round 3 (r3-p2-01) with the real poller and powerctl: the woken machine's clock is behind the
// panel's by 10 minutes, or ahead (the agent gives "pending.at" by its own clock, in its answer and in every
// poll). The countdown runs and the machine goes off as asked; the run said "the shutdown did not work. It is
// ON." one second in. The auditor's proof (TestR3P2AgentClockBehindWithTheRealPoller), with a clock that is
// ahead as well.
func TestWakeUpdateAMachineWhoseClockIsWrong(t *testing.T) {
	for _, behind := range []time.Duration{10 * time.Minute, -10 * time.Minute} {
		e, f := lostFarm(t, "")
		e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
			if path == "/power" {
				return 200, jsonx.M{"action": "shutdown", "pending": jsonx.M{"action": "shutdown", "at": float64(f.now.Add(120*time.Second - behind).Unix()), "delay_s": 120.0, "requested_by": "x"}}, nil
			}
			return 0, nil, nil
		}
		e.b.Mu.Lock()
		e.b.Metrics["pc-a"] = func() (jsonx.M, error) {
			m, err := f.metrics("pc-a")
			if at, ok := f.pending["pc-a"]; ok && err == nil {
				m["power_control"].(jsonx.M)["pending"] = jsonx.M{"action": "shutdown", "at": float64(at.Add(-behind).Unix()), "delay_s": 120.0, "requested_by": "run"}
			}
			return m, err
		}
		e.b.Mu.Unlock()
		if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"}); err != nil {
			t.Fatal(err)
		}
		h := hostOf(f.finish(), "pc-a")
		for i := 0; i < 180; i++ {
			f.tick()
		}
		if h.On == wakerun.OnYes && !f.on["pc-a"] {
			t.Errorf("clock behind by %v: WRONG REPORT: the run says %q (%s) and the machine went off by the run's own shutdown: %s", behind, h.On, h.State, h.Text)
		}
		if h.State != wakerun.StOff || h.On != wakerun.OnNo || f.on["pc-a"] {
			t.Errorf("clock behind by %v: [%s] power %q, machine on %v: %s", behind, h.State, h.On, f.on["pc-a"], h.Text)
		}
	}
}
