package poller

import (
	"context"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Where the configuration does not decide which node is the agent's (two hosts whose node is "pve", and the
// agent's id begins with neither id_prefix), the rows are as they always were: the agent is a part of the
// node of that name that is there. A run for that row talks to this agent, so the timings written under the
// agent's own id are the row's: until this test they were looked up under the node's id alone, and the
// panel's 15 minutes and 2 minutes counted for a machine whose entry said otherwise.
func TestOwnRunTimingsUnderTheAgentsIDCountForTheNodeRowItIsShownAs(t *testing.T) {
	cfg := testutil.SameNameConfig(t)
	cfg.Systems = append(cfg.Systems, config.SystemOverride{ID: "host1-agent", WakeUpdate: &config.SystemWakeUpdate{OffAfterS: 3600, ShutdownDelayS: 600}})
	b := testutil.NewBackend()
	b.PVEOf = func(cluster string) ([]jsonx.M, error) {
		if cluster == "site1" {
			return testutil.HostResources("pve", "stopped"), nil
		}
		return nil, devices.Errf("connection failed")
	}
	b.Metrics["host1-agent"] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
	b.Metrics["relay"] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
	p, err := New(cfg, config.Secrets{}, b)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := p.NodeOf("host1-agent"); ok {
		t.Fatal("the fixture: the configuration decides the node of host1-agent")
	}
	// before anything answered: the agent's block under the agent's id, and under no node's id
	if off, delay := p.WakeUpdateOverride("host1-agent"); off != 3600 || delay != 600 {
		t.Errorf("under the agent's id: %d, %d", off, delay)
	}
	if off, delay := p.WakeUpdateOverride("site2-node-pve"); off != 0 || delay != 0 {
		t.Errorf("site2-node-pve, a node the agent is not shown as: %d, %d", off, delay)
	}
	p.PollOnce(context.Background())
	if s := p.ShownSystem("host1-agent"); s == nil || s.ID != "site1-node-pve" {
		t.Fatalf("the fixture: host1-agent is shown as %+v", s)
	}
	for _, id := range []string{"site1-node-pve", "host1-agent"} {
		if off, delay := p.WakeUpdateOverride(id); off != 3600 || delay != 600 {
			t.Errorf("shown as site1-node-pve, asked under %s: %d, %d, want the machine's own 3600, 600", id, off, delay)
		}
	}
	if off, delay := p.WakeUpdateOverride("site2-node-pve"); off != 0 || delay != 0 {
		t.Errorf("site2-node-pve, another machine: %d, %d", off, delay)
	}
}
