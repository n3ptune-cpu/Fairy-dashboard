package web

import (
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// The last answers of a PC that switches itself off (final review of build 16, J1-A), through the real routes,
// the real run, the poller and Fairy-agent's own gate. Nobody confirms; the PC counts down. In the last two
// seconds its helper is stopped, as Windows does when it goes down: the agent's approval block is "unknown",
// enforced by the helper, with no "runs" and no "run". Then the PC is gone. The report says that it switched
// itself off; it said "the last answer of its agent ... said nothing about the run ... not confirmed that it
// is off", Left ON.
func TestAPCWhoseLastAnswersLackTheHelpersWordSwitchedItselfOff(t *testing.T) {
	r := newRunRig(t, "pc-a")
	_, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}})
	if res["ok"] != true {
		t.Fatal(res)
	}
	r.until(t, "pc-a waiting, armed", 300, func() bool { h := r.host("pc-a"); return h.State == wakerun.StApproval && h.Arm == wakerun.ArmArmed })
	stopped := 0 // answers the panel read after the helper was stopped
	r.until(t, "the run over", 3000, func() bool {
		r.b.Mu.Lock()
		at, counting := r.f.pending["pc-a"]
		now, on := r.f.now, r.f.on["pc-a"]
		r.b.Mu.Unlock()
		switch {
		case stopped > 0 && on:
			stopped++
		case stopped == 0 && counting && at.Sub(now) <= 2*time.Second && r.host("pc-a").State == wakerun.StSelfOff:
			stopped = 1
			r.gb.Stale("pc-a", jsonx.M{"state": "unknown", "enforced_by": "helper", "proto": 2.0, "error": "the helper does not answer"})
		}
		a, _ := r.s.WakeRuns.Snapshot()
		return a == nil
	})
	h := r.host("pc-a")
	r.b.Mu.Lock()
	on := r.f.on["pc-a"]
	r.b.Mu.Unlock()
	if stopped < 2 {
		t.Fatalf("the rig: the panel read no answer of the PC after its helper was stopped (%d)", stopped)
	}
	if on || h.State != wakerun.StOff || h.On != wakerun.OnNo || !strings.HasSuffix(h.Text, "not confirmed, not updated. It switched itself off again.") {
		t.Fatalf("the report: %s / %s %q; the PC is on: %v", h.State, h.On, h.Text, on)
	}
	r.noTokenAnywhere(t)
}
