package poller

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/normalise"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// The report (build 12): a standalone Proxmox host that is normally off, with an agent (node:) and its wol:
// written under the node's id. After a restart of the panel while it is off its Proxmox API does not answer,
// so the panel has no row for the node and shows the host under its agent's id: badge AGENT, "last seen
// never", no Wake in the Power menu, and "no Wake-on-LAN is set up for it" for "Update unpowered hosts".

// twoHosts is the poller of testutil.TwoHostsConfig: PVE-A is on, PVE-B is off (neither its agent nor its
// Proxmox API answers) until up is set.
func twoHosts(t *testing.T, more string) (p *Poller, b *testutil.Backend, up *bool) {
	t.Helper()
	cfg := testutil.TwoHostsConfig(t, more)
	b = testutil.NewBackend()
	up = new(bool)
	b.PVEOf = func(cluster string) ([]jsonx.M, error) {
		switch {
		case cluster == "lab-a":
			return testutil.HostResources("PVE-A", "stopped"), nil
		case cluster == "lab-b" && *up:
			return testutil.HostResources("PVE-B", "stopped"), nil
		}
		return nil, devices.Errf("connection failed")
	}
	b.Metrics["lab-a-agent"] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
	b.Metrics["lab-b-agent"] = func() (jsonx.M, error) {
		if *up {
			return testutil.AgentMetrics(), nil
		}
		return nil, devices.Errf("connection failed")
	}
	p, err := New(cfg, config.Secrets{}, b)
	if err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	return p, b, up
}

// panel restart with the node off: wol under the node id is still found
func TestPanelRestartWithTheNodeOffWolUnderTheNodeIDIsStillFound(t *testing.T) {
	p, b, up := twoHosts(t, "")
	rows := snap(p)
	host, ok := rows["lab-b-agent"]
	if _, node := rows["lab-b-node-pve-b"]; !ok || node || host.Kind != "agent" || host.Status != "offline" {
		t.Fatalf("the fixture: PVE-B is off, so it is shown under its agent's id: %+v", rows)
	}
	if p.Override("lab-b-agent") != nil {
		t.Fatal("the fixture: nothing is written under the agent's id")
	}
	o := p.PowerOverride("lab-b-agent")
	if o == nil || o.ID != "lab-b-node-pve-b" || o.Wol == nil || o.Wol.MAC != "00:00:5e:00:53:36" || o.Wol.Relay != "lab-a-agent" {
		t.Fatalf("the wol: under the node's id is not found for the host while it is shown under its agent's id: %+v", o)
	}
	if !slices.Contains(host.PowerActions, "wake") {
		t.Errorf("the host's row does not list wake: %v", host.PowerActions)
	}
	if id, cluster, ok := p.NodeOf("lab-b-agent"); !ok || id != "lab-b-node-pve-b" || cluster != "lab-b" {
		t.Errorf("NodeOf = %q %q %v", id, cluster, ok)
	}
	// the relay's own host has no wol:, under either id, and is given none
	if o := p.PowerOverride("lab-a-agent"); o != nil {
		t.Errorf("PVE-A was given a systems: entry: %+v", o)
	}
	if o := p.PowerOverride("lab-a-node-pve-a"); o != nil {
		t.Errorf("PVE-A's node was given a systems: entry: %+v", o)
	}
	if w := p.NodeLinkWarnings(); len(w) != 0 {
		t.Errorf("warnings for a configuration that decides everything: %v", w)
	}

	// the machine starts: its Proxmox API answers, the row is the node's from here on, and the entry is the same
	b.Mu.Lock()
	*up = true
	b.Mu.Unlock()
	p.PollOnce(context.Background())
	rows = snap(p)
	node, ok := rows["lab-b-node-pve-b"]
	if _, agent := rows["lab-b-agent"]; !ok || agent || node.Kind != "node" || !slices.Contains(node.PowerActions, "wake") {
		t.Fatalf("with its Proxmox API answering the host is the node's row: %+v", rows)
	}
	if o := p.PowerOverride("lab-b-node-pve-b"); o == nil || o.ID != "lab-b-node-pve-b" || o.Wol == nil {
		t.Errorf("the node's own entry: %+v", o)
	}
}

// The ids one machine is shown under, both ways round; every other id is alone.
func TestSameMachine(t *testing.T) {
	p, _, _ := twoHosts(t, "")
	for id, want := range map[string]string{
		"lab-b-agent":      "lab-b-agent lab-b-node-pve-b",
		"lab-b-node-pve-b": "lab-b-node-pve-b lab-b-agent",
		// PVE-A has no systems: entry, and with two proxmox: entries node: alone does not say which has it
		"lab-a-agent":      "lab-a-agent",
		"lab-a-node-pve-a": "lab-a-node-pve-a",
		"lab-a-node-pve-b": "lab-a-node-pve-b", // the other entry's prefix with PVE-B's name: no machine
		"lab-a-qemu-100":   "lab-a-qemu-100",
		"nobody":           "nobody",
	} {
		if got := strings.Join(p.SameMachine(id), " "); got != want {
			t.Errorf("SameMachine(%s) = %q, want %q", id, got, want)
		}
	}
}

// The switch `fairy admin host <node id> power-control disable` counts for the host while it is shown under
// its agent's id: the Wake that is found there now must not pass it.
func TestThePowerSwitchOfANodeCountsUnderItsAgentsID(t *testing.T) {
	p, _, _ := twoHosts(t, "")
	if as, off := p.PowerDisabledAs("lab-b-agent"); off {
		t.Fatalf("off without a switch (%s)", as)
	}
	if err := p.Admin.SetPowerDisabled("lab-b-node-pve-b", true); err != nil {
		t.Fatal(err)
	}
	if as, off := p.PowerDisabledAs("lab-b-agent"); !off || as != "lab-b-node-pve-b" {
		t.Errorf("the node's switch for the host under its agent's id: %q %v", as, off)
	}
	if as, off := p.PowerDisabledAs("lab-b-node-pve-b"); !off || as != "lab-b-node-pve-b" {
		t.Errorf("the node's switch for the node: %q %v", as, off)
	}
	if _, off := p.PowerDisabledAs("lab-a-agent"); off {
		t.Error("the switch of PVE-B counts for PVE-A")
	}
}

// The rule itself (nodeLinks), from the configuration alone. A wol: is never given to a host that the
// configuration does not name as the one: a wrong MAC wakes somebody else's PC.
func TestTheNodeOfAnAgentIsDecidedByTheConfigurationOrNotAtAll(t *testing.T) {
	px := func(prefix string) *string { return &prefix }
	one := []config.Proxmox{{ID: "main", IDPrefix: px("")}}
	two := []config.Proxmox{{ID: "a", IDPrefix: px("a-")}, {ID: "b", IDPrefix: px("b-")}}
	sys := func(ids ...string) (out []config.SystemOverride) {
		for _, id := range ids {
			out = append(out, config.SystemOverride{ID: id, Wol: &config.Wol{MAC: "00:00:5e:00:53:01", Relay: "relay"}})
		}
		return out
	}
	for _, c := range []struct {
		name     string
		agents   []config.Agent
		clusters []config.Proxmox
		systems  []config.SystemOverride
		want     map[string]string // agent id -> node id
		warn     []string          // a part of each warning, in order
	}{
		{name: "one proxmox: entry: node: names the node", agents: []config.Agent{{ID: "x", Node: "PVE 1"}}, clusters: one,
			want: map[string]string{"x": "node-pve-1"}},
		{name: "one proxmox: entry with a prefix, and an entry under the node's id", agents: []config.Agent{{ID: "x", Node: "pve"}}, clusters: two[:1], systems: sys("a-node-pve"),
			want: map[string]string{"x": "a-node-pve"}},
		{name: "two proxmox: entries, one systems: entry of that form: that one", agents: []config.Agent{{ID: "x", Node: "pve"}}, clusters: two, systems: sys("b-node-pve", "x-node-pve", "b-node-other"),
			want: map[string]string{"x": "b-node-pve"}},
		{name: "two proxmox: entries, no systems: entry: not decided, and nothing to warn about", agents: []config.Agent{{ID: "x", Node: "pve"}}, clusters: two,
			want: map[string]string{}},
		{name: "two proxmox: entries, an entry for each: none, and a warning", agents: []config.Agent{{ID: "x", Node: "pve"}}, clusters: two, systems: sys("a-node-pve", "b-node-pve"),
			want: map[string]string{}, warn: []string{"agent x (node: pve): systems: has entries for a-node-pve and b-node-pve"}},
		{name: "two agents name the same node: none for either, one warning", agents: []config.Agent{{ID: "x", Node: "pve"}, {ID: "y", Node: "PVE"}}, clusters: one, systems: sys("node-pve"),
			want: map[string]string{}, warn: []string{"agents x and y: their entries can all mean the node node-pve"}},
		{name: "the same node name in two clusters, an agent for each, one entry: none", agents: []config.Agent{{ID: "x", Node: "pve"}, {ID: "y", Node: "pve"}}, clusters: two, systems: sys("a-node-pve"),
			want: map[string]string{}, warn: []string{"agents x and y: their entries can all mean the node a-node-pve"}},
		{name: "links_to with a node's id", agents: []config.Agent{{ID: "x", LinksTo: "b-node-pve"}}, clusters: two, systems: sys("b-node-pve"),
			want: map[string]string{"x": "b-node-pve"}},
		{name: "links_to wins over node:", agents: []config.Agent{{ID: "x", LinksTo: "b-node-pve", Node: "other"}}, clusters: two, systems: sys("a-node-other"),
			want: map[string]string{"x": "b-node-pve"}},
		{name: "links_to a guest: an agent in a VM is no node", agents: []config.Agent{{ID: "x", LinksTo: "qemu-100"}, {ID: "y", LinksTo: "b-lxc-7"}}, clusters: append(one, two...), systems: sys("qemu-100", "b-lxc-7"),
			want: map[string]string{}},
		{name: "one linked to the node another names: none for either", agents: []config.Agent{{ID: "x", LinksTo: "node-pve"}, {ID: "y", Node: "pve"}}, clusters: one, systems: sys("node-pve"),
			want: map[string]string{}, warn: []string{"agents x and y: their entries can all mean the node node-pve"}},
		{name: "the node's id is an agent's own id: that agent's entry stays its own", agents: []config.Agent{{ID: "x", Node: "pve"}, {ID: "a-node-pve"}}, clusters: two[:1], systems: sys("a-node-pve"),
			want: map[string]string{}, warn: []string{"agent x: the id of its node, a-node-pve, is also the id of an agent"}},
		{name: "no node:, no links_to, a node name without a letter or digit, no proxmox: entry", agents: []config.Agent{{ID: "x"}, {ID: "y", Node: "--"}}, clusters: one,
			want: map[string]string{}},
		{name: "node: without any proxmox: entry", agents: []config.Agent{{ID: "x", Node: "pve"}}, systems: sys("node-pve"),
			want: map[string]string{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			links, whys := nodeLinks(c.agents, c.clusters, c.systems)
			got := map[string]string{}
			for id, l := range links {
				got[id] = l.ID
			}
			if len(got) != len(c.want) {
				t.Errorf("links %v, want %v", got, c.want)
			}
			for id, want := range c.want {
				if got[id] != want {
					t.Errorf("agent %s: node %q, want %q", id, got[id], want)
				}
			}
			if len(whys) != len(c.warn) {
				t.Fatalf("warnings %q, want %d", whys, len(c.warn))
			}
			for i, part := range c.warn {
				if !strings.Contains(whys[i], part) {
					t.Errorf("warning %q lacks %q", whys[i], part)
				}
			}
		})
	}
}

// The id worked out for node: is the id the Proxmox poll gives the node's row (normalise.NodeID), letter for letter.
func TestTheNodeIDOfAnAgentIsTheIDOfTheNodesRow(t *testing.T) {
	prefix := "lab-b-"
	for _, name := range []string{"PVE-B", "pve1", "Srv PVE.A", "BOX_B2", "ÜBER-node"} {
		got := nodeCandidates(config.Agent{ID: "x", Node: name}, []config.Proxmox{{ID: "c", IDPrefix: &prefix}})
		if len(got) != 1 || got[0].ID != normalise.NodeID(prefix, name) || got[0].Cluster != "c" {
			t.Errorf("node: %s: %+v, want %s", name, got, normalise.NodeID(prefix, name))
		}
	}
}

// Two candidates in the real poller: an entry under each prefix for the node's name. The host is given neither
// wol:, the start says so once, and with its Proxmox API answering the node's own entry is its own as before.
func TestAWolIsNotGuessedBetweenTwoNodeIDs(t *testing.T) {
	p, b, up := twoHosts(t, `  - id: lab-a-node-pve-b
    wol: {mac: "00:00:5e:00:53:99", relay: lab-a-agent}
`)
	if o := p.PowerOverride("lab-b-agent"); o != nil {
		t.Fatalf("a wol: was picked from two candidates: %+v", o)
	}
	if got := snap(p)["lab-b-agent"].PowerActions; slices.Contains(got, "wake") {
		t.Errorf("the row lists wake: %v", got)
	}
	w := p.NodeLinkWarnings()
	if len(w) != 1 || !strings.Contains(w[0], "agent lab-b-agent (node: PVE-B): systems: has entries for lab-a-node-pve-b and lab-b-node-pve-b") ||
		!strings.Contains(w[0], "links_to: <the node's id>") {
		t.Errorf("warnings: %q", w)
	}
	if got := strings.Join(p.SameMachine("lab-b-agent"), " "); got != "lab-b-agent" {
		t.Errorf("SameMachine: %q", got)
	}
	b.Mu.Lock()
	*up = true
	b.Mu.Unlock()
	p.PollOnce(context.Background())
	if o := p.PowerOverride("lab-b-node-pve-b"); o == nil || o.Wol == nil || o.Wol.MAC != "00:00:5e:00:53:36" {
		t.Errorf("the node's own entry: %+v", o)
	}
}
