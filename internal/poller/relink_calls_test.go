package poller

import (
	"fmt"
	"maps"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// The links linkOfLocked goes by are worked out when the poller is made and again after every change of the
// hosts and the Proxmox nodes (relink). Each of the four changes that can be made in a running panel has its
// own call, and each call is needed: here every change alone makes another answer, and what is kept after it
// must be the answer to the configuration as it is then. (With one of the four calls taken out, the rows and
// every lookup by a machine's two ids go by the configuration from before that change until the next one.)
func TestEachChangeOfTheHostsAndTheNodesIsFollowedByTheKeptLinks(t *testing.T) {
	// one proxmox: entry and an agent whose id begins with no id_prefix: decided, there is only one entry
	p, _ := linkPanel(t, [][2]string{{"alpha", "alpha-"}}, fmt.Sprintf(linkAgent, "gamma-z", "Z", "z", "node: pve"), "", map[string]string{"alpha": "pve"})
	pfx := "beta-"
	beta := config.Proxmox{ID: "beta", IDPrefix: &pfx, ClusterName: "B", Standalone: true, URLs: []string{"https://b:8006"}, TokenID: "fairy@pve!panel", Managed: true}
	x := config.Agent{ID: "alpha-x", Name: "X", URL: "http://x", Node: "pve", Managed: true, Token: testutil.ReadToken}
	kept := func(when string, want map[string]string) {
		t.Helper()
		got := *p.decided.Load()
		if !maps.Equal(got, want) {
			t.Errorf("%s: the links that are kept are %v, want %v", when, got, want)
		}
		links, _ := p.links()
		now := map[string]string{}
		for id, l := range links {
			now[id] = l.ID
		}
		if !maps.Equal(now, want) {
			t.Fatalf("%s: the fixture: the configuration says %v, the test expects %v", when, now, want)
		}
		// and what asks linkOfLocked sees it: an agent that is decided is linked to its node whether the node's
		// row is there or not, and one that is not decided is linked to none that the configuration names
		for _, a := range p.Agents() {
			if id, ok := want[a.ID]; ok && p.LinkOf(a) != id {
				t.Errorf("%s: %s is linked to %s, want %s", when, a.ID, p.LinkOf(a), id)
			}
		}
	}
	kept("at the start", map[string]string{"gamma-z": "alpha-node-pve"})
	// a second proxmox: entry: the id of gamma-z begins with neither id_prefix, so it is not decided any more
	if err := p.AddProxmox(beta); err != nil {
		t.Fatal(err)
	}
	kept("after a Proxmox node was added", map[string]string{})
	// a host whose id begins with alpha-: it would be decided, but gamma-z can mean the same node
	if err := p.AddAgent(x, nil); err != nil {
		t.Fatal(err)
	}
	kept("after a host was added", map[string]string{})
	// the Proxmox node is removed again: one entry, two agents that name its node pve: none for either
	p.RemoveProxmox("beta")
	kept("after the Proxmox node was removed", map[string]string{})
	if err := p.AddProxmox(beta); err != nil {
		t.Fatal(err)
	}
	// With two entries and gamma-z taken out of the configuration by hand (it is config.yaml's, Remove is for
	// hosts added in the panel), alpha-x alone is decided by its id.
	p.cfgMu.Lock()
	delete(p.agents, "gamma-z")
	p.Cfg.Agents = []config.Agent{x}
	p.cfgMu.Unlock()
	p.relink()
	kept("the fixture: alpha-x alone, two entries", map[string]string{"alpha-x": "alpha-node-pve"})
	// each of the two removals alone changes the answer from here
	p.RemoveAgent("alpha-x")
	kept("after the host was removed", map[string]string{})
	if err := p.AddAgent(x, nil); err != nil {
		t.Fatal(err)
	}
	kept("after the host was added again", map[string]string{"alpha-x": "alpha-node-pve"})
	other := config.Agent{ID: "spare-y", Name: "Y", URL: "http://y", Node: "pve", Managed: true, Token: testutil.ReadToken}
	if err := p.AddAgent(other, nil); err != nil {
		t.Fatal(err)
	}
	kept("after a second host that can mean the same node was added", map[string]string{})
	p.RemoveProxmox("beta") // one entry: still two agents for one node
	kept("after the Proxmox node was removed with two hosts", map[string]string{})
	p.RemoveAgent("spare-y")
	kept("after the second host was removed", map[string]string{"alpha-x": "alpha-node-pve"})
	if err := p.AddProxmox(beta); err != nil {
		t.Fatal(err)
	}
	if err := p.AddAgent(config.Agent{ID: "beta-w", Name: "W", URL: "http://w", Node: "pve", Managed: true, Token: testutil.ReadToken}, nil); err != nil {
		t.Fatal(err)
	}
	kept("two sites with a node pve and an agent each: neither is decided", map[string]string{})
	p.RemoveAgent("beta-w")
	kept("after the other site's host was removed", map[string]string{"alpha-x": "alpha-node-pve"})
	p.RemoveAgent("alpha-x")
	if err := p.AddAgent(config.Agent{ID: "plain-v", Name: "V", URL: "http://v", Node: "pve", Managed: true, Token: testutil.ReadToken}, nil); err != nil {
		t.Fatal(err)
	}
	kept("an id without a prefix and two entries", map[string]string{})
	p.RemoveProxmox("beta")
	kept("after the second entry was removed: the one entry decides", map[string]string{"plain-v": "alpha-node-pve"})
}
