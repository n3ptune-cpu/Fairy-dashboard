package poller

import (
	"context"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Build 14: a machine's own timings for "Update unpowered hosts" (the wake_update: block of its systems:
// entry) are the machine's, under both of its ids. PVE-B is a standalone Proxmox host that is normally off:
// while it is off after a restart of the panel it is shown under its agent's id, and under its node's id once
// its Proxmox API answers. Its systems: entry is written under the node's id (where its wol: is). The block
// must be found whichever id the machine is shown under, and whichever of the two ids the entry was written
// under: looking it up by the shown id alone was the mistake of build 12 with wol:.
func TestAMachinesOwnRunTimingsAreFoundUnderBothOfItsIDs(t *testing.T) {
	both := func(p *Poller, wantOff, wantDelay int, when string) {
		t.Helper()
		for _, id := range []string{"lab-b-agent", "lab-b-node-pve-b"} {
			if off, delay := p.WakeUpdateOverride(id); off != wantOff || delay != wantDelay {
				t.Errorf("%s: WakeUpdateOverride(%s) = %d, %d, want %d, %d", when, id, off, delay, wantOff, wantDelay)
			}
		}
		// the other host has none, under either id
		for _, id := range []string{"lab-a-agent", "lab-a-node-pve-a", "nobody", ""} {
			if off, delay := p.WakeUpdateOverride(id); off != 0 || delay != 0 {
				t.Errorf("%s: WakeUpdateOverride(%s) = %d, %d for a machine without a block", when, id, off, delay)
			}
		}
	}
	// under the node's id, in the entry that has the wol: (the block continues the fixture's last entry)
	p, _, up := twoHosts(t, "    wake_update: {off_after_s: 360, shutdown_delay_s: 30}\n")
	if _, node := snap(p)["lab-b-node-pve-b"]; node {
		t.Fatal("the fixture: PVE-B is off, so it has no node row")
	}
	both(p, 360, 30, "under the node's id, shown under the agent's id")
	*up = true
	p.PollOnce(context.Background())
	if _, agent := snap(p)["lab-b-agent"]; agent {
		t.Fatal("the fixture: PVE-B is on, so it is shown as its node")
	}
	both(p, 360, 30, "under the node's id, shown under the node's id")

	// under the agent's id
	p, _, up = twoHosts(t, "  - {id: lab-b-agent, wake_update: {off_after_s: 420}}\n")
	both(p, 420, 0, "under the agent's id, shown under the agent's id")
	*up = true
	p.PollOnce(context.Background())
	both(p, 420, 0, "under the agent's id, shown under the node's id")

	// under both, with different values: one answer for the machine whichever id is asked (its timings must
	// not change with the id its row has), and it is the larger, careful one of each key
	p, _, up = twoHosts(t, "    wake_update: {off_after_s: 360, shutdown_delay_s: 90}\n  - {id: lab-b-agent, wake_update: {off_after_s: 420, shutdown_delay_s: 45}}\n")
	both(p, 420, 90, "under both ids, shown under the agent's id")
	*up = true
	p.PollOnce(context.Background())
	both(p, 420, 90, "under both ids, shown under the node's id")
	p, _, _ = twoHosts(t, "    wake_update: {off_after_s: 360}\n  - {id: lab-b-agent, wake_update: {shutdown_delay_s: 45}}\n")
	both(p, 360, 45, "one key under each id")

	// no block at all: nothing, and the wol: of the entry is found as before
	p, _, _ = twoHosts(t, "")
	both(p, 0, 0, "no block")
	if o := p.PowerOverride("lab-b-agent"); o == nil || o.Wol == nil {
		t.Errorf("the wol: of the entry: %+v", o)
	}
}

// Where the configuration does not say which node is the agent's (two hosts whose node has the same name,
// nothing in the agent's id), a block under a node's id is that node's and nobody else's: the agent's row does
// not get the timings of what may be another machine. A block under the agent's own id is the agent's.
func TestRunTimingsAreNotGivenToAnAgentWhoseNodeIsNotDecided(t *testing.T) {
	cfg := testutil.SameNameConfig(t)
	for i := range cfg.Systems {
		if cfg.Systems[i].ID == "site2-node-pve" {
			cfg.Systems[i].WakeUpdate = &config.SystemWakeUpdate{OffAfterS: 300, ShutdownDelayS: 10}
		}
	}
	cfg.Systems = append(cfg.Systems, config.SystemOverride{ID: "relay", WakeUpdate: &config.SystemWakeUpdate{ShutdownDelayS: 60}})
	p, err := New(cfg, config.Secrets{}, testutil.NewBackend())
	if err != nil {
		t.Fatal(err)
	}
	if off, delay := p.WakeUpdateOverride("host1-agent"); off != 0 || delay != 0 {
		t.Errorf("host1-agent was given the timings of site2-node-pve: %d, %d", off, delay)
	}
	if off, delay := p.WakeUpdateOverride("site2-node-pve"); off != 300 || delay != 10 {
		t.Errorf("site2-node-pve: %d, %d", off, delay)
	}
	if off, delay := p.WakeUpdateOverride("site1-node-pve"); off != 0 || delay != 0 {
		t.Errorf("site1-node-pve: %d, %d", off, delay)
	}
	if off, delay := p.WakeUpdateOverride("relay"); off != 0 || delay != 60 { // an agent that names no node: its own id
		t.Errorf("relay: %d, %d", off, delay)
	}
}
