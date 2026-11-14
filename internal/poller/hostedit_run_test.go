package poller

import (
	"context"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
)

// "Edit host", a machine's own run timings: what an admin decided in the panel lies over what config.yaml's
// systems: entry sets, and is found under both ids of a machine, whichever of them the decision is kept
// under and whichever the machine is shown under. Without a decision nothing changes.
func TestPanelRunTimingsLieOverTheConfigurations(t *testing.T) {
	both := func(p *Poller, wantOff, wantDelay int, when string) {
		t.Helper()
		for _, id := range []string{"lab-b-agent", "lab-b-node-pve-b"} {
			if off, delay := p.WakeUpdateOverride(id); off != wantOff || delay != wantDelay {
				t.Errorf("%s: WakeUpdateOverride(%s) = %d, %d, want %d, %d", when, id, off, delay, wantOff, wantDelay)
			}
		}
		for _, id := range []string{"lab-a-agent", "lab-a-node-pve-a", "nobody", ""} { // the other host: never
			if off, delay := p.WakeUpdateOverride(id); off != 0 || delay != 0 {
				t.Errorf("%s: WakeUpdateOverride(%s) = %d, %d for a machine nothing was decided about", when, id, off, delay)
			}
		}
	}
	for _, key := range []string{"lab-b-agent", "lab-b-node-pve-b"} {
		p, _, up := twoHosts(t, "    wake_update: {off_after_s: 360, shutdown_delay_s: 30}\n")
		both(p, 360, 30, "kept under "+key+": nothing decided")
		setHost(t, p, key, adminstate.HostEdit{Run: adminstate.RunEdit{OffAfterS: 1200}})
		both(p, 1200, 30, "kept under "+key+": one decision, the other key is config.yaml's")
		if off, delay := p.ConfigWakeUpdate("lab-b-agent"); off != 360 || delay != 30 {
			t.Errorf("config.yaml's own values changed with a decision: %d, %d", off, delay)
		}
		*up = true
		p.PollOnce(context.Background())
		if _, agent := snap(p)["lab-b-agent"]; agent {
			t.Fatal("the fixture: PVE-B is on, so it is shown as its node")
		}
		both(p, 1200, 30, "kept under "+key+": shown under the node's id")
		setHost(t, p, key, adminstate.HostEdit{Name: "Rack B", Run: adminstate.RunEdit{OffAfterS: 300, ShutdownDelayS: 10}})
		both(p, 300, 10, "kept under "+key+": both decided, at their floors, next to a name")
		setHost(t, p, key, adminstate.HostEdit{Name: "Rack B"})
		both(p, 360, 30, "kept under "+key+": the decisions are gone")
	}
	// a machine without a block: the decision is its own timing; the other key stays "the panel's" (0)
	p, _, _ := twoHosts(t, "")
	setHost(t, p, "lab-b-agent", adminstate.HostEdit{Run: adminstate.RunEdit{ShutdownDelayS: 45}})
	both(p, 0, 45, "no block in config.yaml")
}

// A decision counts only when it is a value its key can have: whatever reaches this point, a number below a
// floor (or above a range) never comes into force; the configuration's value counts for that key.
func TestPanelRunTimingsNeverLowerAFloor(t *testing.T) {
	for _, c := range []struct {
		e                  adminstate.RunEdit
		off, delay         int
		wantOff, wantDelay int
	}{
		{adminstate.RunEdit{}, 360, 30, 360, 30},
		{adminstate.RunEdit{OffAfterS: 600, ShutdownDelayS: 45}, 360, 30, 600, 45},
		{adminstate.RunEdit{OffAfterS: 300, ShutdownDelayS: 10}, 0, 0, 300, 10},
		{adminstate.RunEdit{OffAfterS: 3600, ShutdownDelayS: 3600}, 0, 0, 3600, 3600},
		{adminstate.RunEdit{OffAfterS: 299, ShutdownDelayS: 9}, 360, 30, 360, 30},
		{adminstate.RunEdit{OffAfterS: 60, ShutdownDelayS: 1}, 0, 0, 0, 0},
		{adminstate.RunEdit{OffAfterS: -1, ShutdownDelayS: -1}, 900, 120, 900, 120},
		{adminstate.RunEdit{OffAfterS: 3601, ShutdownDelayS: 3601}, 900, 120, 900, 120},
		{adminstate.RunEdit{OffAfterS: 1, ShutdownDelayS: 45}, 900, 120, 900, 45},
	} {
		if off, delay := runWith(c.e, c.off, c.delay); off != c.wantOff || delay != c.wantDelay {
			t.Errorf("runWith(%+v, %d, %d) = %d, %d, want %d, %d", c.e, c.off, c.delay, off, delay, c.wantOff, c.wantDelay)
		}
	}
}
