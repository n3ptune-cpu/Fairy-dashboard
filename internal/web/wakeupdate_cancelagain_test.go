package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/audit"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// Second review of fix7a (R7A-04), with the real poller and powerctl: the kill switch stops a run while its
// countdown runs, and the answer to the run's cancel is lost. The run sent that one cancel, wrote "it may
// switch off at ... Cancel it on the machine's page." and let go. It now tries again every ten seconds until
// its own due time, and every try is in the audit log.
func TestWakeUpdateACancelWithoutAnAnswerIsTriedAgain(t *testing.T) {
	for _, arrives := range []bool{false, true} { // the cancel whose answer is lost: did it reach the machine?
		e, f := lostFarm(t, "")
		lose := 1 // answers to cancels that are lost
		e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
			if path != "/power" {
				return 0, nil, nil
			}
			if req := e.b.Requests[len(e.b.Requests)-1]; strings.Contains(req, "action:cancel") {
				if lose > 0 {
					lose--
					if !arrives { // the farm acts on every cancel it finds: this one never got there
						e.b.Requests[len(e.b.Requests)-1] = strings.Replace(req, "action:cancel", "action:lost", 1)
					}
					return 0, nil, context.DeadlineExceeded
				}
				return 200, jsonx.M{"action": "cancel", "pending": nil}, nil
			}
			return 200, jsonx.M{"action": "shutdown", "pending": jsonx.M{"action": "shutdown", "at": float64(f.now.Add(120 * time.Second).Unix()), "delay_s": 120.0, "requested_by": "x"}}, nil
		}
		if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"}); err != nil {
			t.Fatal(err)
		}
		var run *wakerun.Run
		stopped := false
		for i := 0; i < 3600 && run == nil; i++ {
			f.tick()
			a, hist := e.s.WakeRuns.Snapshot()
			if a != nil && !stopped && hostOf(a, "pc-a").State == wakerun.StShutting && f.now.Sub(hostOf(a, "pc-a").Since) >= 30*time.Second {
				stopped = true
				if err := e.s.Poller.Admin.SetControlDisabled(true, "root (cli)", "stop everything"); err != nil {
					t.Fatal(err)
				}
			}
			if a == nil && len(hist) > 0 {
				run = hist[0]
			}
		}
		if run == nil {
			t.Fatal("the run did not end")
		}
		h := hostOf(run, "pc-a")
		for i := 0; i < 180; i++ { // past the time the countdown would have ended
			f.tick()
		}
		var cancels []string
		for _, r := range f.e.b.Requests {
			if strings.HasPrefix(r, "pc-a POST /power") && (strings.Contains(r, "action:cancel") || strings.Contains(r, "action:lost")) {
				cancels = append(cancels, r)
			}
		}
		want := "the run stopped (control is disabled by an administrator): its shutdown countdown was called off. It came online after the wake and it is ON. It was updated to 17-bbbbbbb."
		if len(cancels) != 2 || !f.on["pc-a"] || h.State != wakerun.StLeftOn || h.On != wakerun.OnYes || h.Text != want {
			t.Errorf("the lost cancel arrived=%v: %d cancels sent, the machine is on: %v; [%s] %q: %s", arrives, len(cancels), f.on["pc-a"], h.State, h.On, h.Text)
		}
		// both tries are in the audit log, each says that control was off, the second that it is a try again
		entries, _ := audit.Read(e.s.Cfg.AuditLog, "", "", "", nil, 500)
		var got []string
		for i := len(entries) - 1; i >= 0; i-- { // oldest first
			if en := entries[i]; en["action"] == "power.cancel" {
				p, _ := en["params"].(map[string]any)
				line := en["outcome"].(string)
				if off, _ := p["control_off"].(string); off != "" {
					line += " | control off"
				}
				if again, _ := p["again"].(bool); again {
					line += " | again"
				}
				got = append(got, line)
			}
		}
		if len(got) != 2 || got[0] != "error: agent unreachable: timeout | control off" || got[1] != "ok | control off | again" {
			t.Errorf("the lost cancel arrived=%v: audit entries of the cancels: %q", arrives, got)
		}
	}
}
