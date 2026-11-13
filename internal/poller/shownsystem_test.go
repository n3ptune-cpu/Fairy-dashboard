package poller

import (
	"context"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// ShownSystem is the row an agent is shown as: its node's row while that row is there and the snapshot makes
// the agent a part of it, its own entry while it is not (the machine is off after a restart of the panel), and
// nothing before the agent was asked once. It is the snapshot's own answer, row for row.
func TestShownSystemIsTheRowOfTheSnapshot(t *testing.T) {
	p, _, up := twoHosts(t, "")
	shown := func(agent string) string {
		t.Helper()
		x := p.ShownSystem(agent)
		if x == nil {
			return ""
		}
		if _, ok := snap(p)[x.ID]; !ok {
			t.Errorf("ShownSystem(%s) = %s, which is no row of the snapshot", agent, x.ID)
		}
		return x.ID + "/" + x.Kind
	}
	// PVE-A is on: its agent is a part of its node's row. PVE-B is off: no node row, the agent's own row.
	if got := shown("lab-a-agent"); got != "lab-a-node-pve-a/node" {
		t.Errorf("the agent of a node that is there: %q", got)
	}
	if got := shown("lab-b-agent"); got != "lab-b-agent/agent" {
		t.Errorf("the agent of a node that has no row: %q", got)
	}
	if _, own := snap(p)["lab-a-agent"]; own {
		t.Error("the fixture: lab-a-agent has a row of its own")
	}
	// RelaySystem is the other question (whose status a relay has): the agent's own entry, in both cases.
	for _, id := range []string{"lab-a-agent", "lab-b-agent"} {
		if x := p.RelaySystem(id); x == nil || x.ID != id || x.Kind != "agent" {
			t.Errorf("RelaySystem(%s) = %+v, want the agent's own entry", id, x)
		}
	}
	// the node's Proxmox API answers: the row changes its id, and ShownSystem with it
	*up = true
	p.PollOnce(context.Background())
	if got := shown("lab-b-agent"); got != "lab-b-node-pve-b/node" {
		t.Errorf("after the node's row came: %q", got)
	}
	// no agent of that id; an agent that was never asked; an agent that names no node
	if x := p.ShownSystem("lab-b-node-pve-b"); x != nil {
		t.Errorf("a node's id is no agent's: %+v", x)
	}
	if x := p.ShownSystem("nobody"); x != nil {
		t.Errorf("an unknown id: %+v", x)
	}
	if err := p.AddAgent(config.Agent{ID: "plain", Name: "Plain", URL: "http://plain", Token: "read-word", Managed: true}, nil); err != nil {
		t.Fatal(err)
	}
	if x := p.ShownSystem("plain"); x != nil {
		t.Errorf("an agent that was never asked has no row: %+v", x)
	}
	p.PollOnce(context.Background())
	if got := shown("plain"); got != "plain/agent" {
		t.Errorf("an agent that names no node: %q", got)
	}
}

// An agent whose links_to names a guest is not merged into it (buildSnapshot merges into rows of kind "node"
// only), so it is shown as itself.
func TestShownSystemOfAnAgentLinkedToAGuestIsItsOwnRow(t *testing.T) {
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	p, err := New(cfg, config.Secrets{}, testutil.NewBackend())
	if err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	var guest string
	for id, x := range snap(p) {
		if x.Kind == "qemu" {
			guest = id
		}
	}
	if guest == "" {
		t.Fatal("the fixture has no guest")
	}
	if err := p.AddAgent(config.Agent{ID: "in-guest", Name: "In a guest", URL: "http://in-guest", Token: "read-word", LinksTo: guest, Managed: true}, nil); err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	if x := p.ShownSystem("in-guest"); x == nil || x.ID != "in-guest" {
		t.Fatalf("ShownSystem = %+v, want the agent's own row", x)
	}
	if _, ok := snap(p)["in-guest"]; !ok {
		t.Fatal("the snapshot has no row for the agent")
	}
}
