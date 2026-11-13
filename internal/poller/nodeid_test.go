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
	// the relay's own host has no wol:, under either id, and is given none: PVE-B's entry is PVE-B's
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
		// PVE-A has no systems: entry: its agent's id begins with its entry's id_prefix, and that decides
		"lab-a-agent":      "lab-a-agent lab-a-node-pve-a",
		"lab-a-node-pve-a": "lab-a-node-pve-a lab-a-agent",
		"lab-a-node-pve-b": "lab-a-node-pve-b", // the other entry's prefix with PVE-B's name: no machine
		"lab-a-qemu-100":   "lab-a-qemu-100",
		"nobody":           "nobody",
	} {
		if got := strings.Join(p.SameMachine(id), " "); got != want {
			t.Errorf("SameMachine(%s) = %q, want %q", id, got, want)
		}
	}
}

// The switch `fairy admin host <id> power-control disable` is the machine's: set under the node's id it counts
// for the host while that is shown under its agent's id (the Wake that is found there now must not pass it),
// and set under the agent's id it counts for the node's row (review of fix13, must-fix 2: a run woke the
// machine under its agent's id, an admin switched its power control off under that id, the only one it had on
// the pages, the row became the node's, and the run's shutdown passed the switch).
func TestThePowerSwitchOfAMachineCountsUnderBothOfItsIDs(t *testing.T) {
	for _, set := range []string{"lab-b-node-pve-b", "lab-b-agent"} {
		t.Run("set under "+set, func(t *testing.T) {
			p, _, _ := twoHosts(t, "")
			for _, id := range []string{"lab-b-agent", "lab-b-node-pve-b"} {
				if as, off := p.PowerDisabledAs(id); off {
					t.Fatalf("%s: off without a switch (%s)", id, as)
				}
			}
			if err := p.Admin.SetPowerDisabled(set, true); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"lab-b-agent", "lab-b-node-pve-b"} {
				if as, off := p.PowerDisabledAs(id); !off || as != set {
					t.Errorf("the switch set under %s, asked for %s: %q %v", set, id, as, off)
				}
			}
			for _, id := range []string{"lab-a-agent", "lab-a-node-pve-a", "lab-a-node-pve-b", "lab-b-qemu-100"} {
				if _, off := p.PowerDisabledAs(id); off {
					t.Errorf("the switch of PVE-B counts for %s", id)
				}
			}
		})
	}
}

// The rule itself (nodeLinks), from the configuration alone. A wol: is never given to a host that the
// configuration does not name as the one: a wrong MAC wakes somebody else's PC.
//
// Review of fix13, must-fix 1: with several proxmox: entries the first rule took "the one candidate that has a
// systems: entry". Two hosts whose node is called pve, the agent on the first, the wol: of the second under
// site2-node-pve: the agent's row offered Power on and sent the second host's MAC. Which ids have a systems:
// entry decides nothing any more: the proxmox: entry is the one whose id_prefix the agent's own id begins with.
func TestTheNodeOfAnAgentIsDecidedByTheConfigurationOrNotAtAll(t *testing.T) {
	px := func(prefix string) *string { return &prefix }
	one := []config.Proxmox{{ID: "main", IDPrefix: px("")}}
	two := []config.Proxmox{{ID: "site1", IDPrefix: px("site1-")}, {ID: "site2", IDPrefix: px("site2-")}}
	// the report's shape: a cluster without id_prefix and two standalone hosts with one each
	three := []config.Proxmox{{ID: "main", IDPrefix: px("")}, {ID: "yard-a", IDPrefix: px("yard-a-")}, {ID: "yard-b", IDPrefix: px("yard-b-")}}
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
		cluster  map[string]string // agent id -> the proxmox: entry of that node (where the case asks)
		warn     []string          // a part of each warning, in order
	}{
		// b. one proxmox: entry
		{name: "one proxmox: entry: node: names the node", agents: []config.Agent{{ID: "x", Node: "PVE 1"}}, clusters: one,
			want: map[string]string{"x": "node-pve-1"}},
		{name: "one proxmox: entry with a prefix the agent's id does not begin with: still that entry", agents: []config.Agent{{ID: "x", Node: "pve"}}, clusters: two[:1], systems: sys("site1-node-pve"),
			want: map[string]string{"x": "site1-node-pve"}},
		// c. several proxmox: entries: by the agent's own id
		{name: "the report: three entries, the agent's id begins with one id_prefix", clusters: three, systems: sys("yard-b-node-box-b"),
			agents:  []config.Agent{{ID: "yard-a-relay-agent", Node: "BOX-A"}, {ID: "yard-b-box-agent", Node: "BOX-B"}, {ID: "other-pc"}},
			want:    map[string]string{"yard-b-box-agent": "yard-b-node-box-b", "yard-a-relay-agent": "yard-a-node-box-a"},
			cluster: map[string]string{"yard-b-box-agent": "yard-b", "yard-a-relay-agent": "yard-a"}},
		{name: "the same without any systems: entry: the entries play no part", clusters: three,
			agents: []config.Agent{{ID: "yard-b-box-agent", Node: "BOX-B"}},
			want:   map[string]string{"yard-b-box-agent": "yard-b-node-box-b"}},
		{name: "the review's case: two nodes called pve, the wol: of the other host: not decided, a warning", clusters: two, systems: sys("site2-node-pve"),
			agents: []config.Agent{{ID: "relay"}, {ID: "host1-agent", Node: "pve"}},
			want:   map[string]string{},
			warn:   []string{"agent host1-agent (node: pve): its id begins with the id_prefix of none of the proxmox: entries"}},
		{name: "the same with an entry for each: not decided, one warning", clusters: two, systems: sys("site1-node-pve", "site2-node-pve"),
			agents: []config.Agent{{ID: "host1-agent", Node: "pve"}},
			want:   map[string]string{}, warn: []string{"systems: has an entry for site1-node-pve and site2-node-pve"}},
		{name: "the same without a systems: entry: not decided, and nothing to warn about", clusters: two,
			agents: []config.Agent{{ID: "host1-agent", Node: "pve"}}, want: map[string]string{}},
		// CHANGED (build 14): this was "not decided", with the warning "its id begins with the id_prefix of more
		// than one proxmox: entry (yard-, yard-b-)". The longest id_prefix decides (TestTheLongestIDPrefixWins).
		{name: "the agent's id begins with two id_prefixes (yard- and yard-b-): the longer one", systems: sys("yard-b-node-pve"),
			clusters: []config.Proxmox{{ID: "yard", IDPrefix: px("yard-")}, {ID: "yard-b", IDPrefix: px("yard-b-")}},
			agents:   []config.Agent{{ID: "yard-b-agent", Node: "pve"}},
			want:     map[string]string{"yard-b-agent": "yard-b-node-pve"}, cluster: map[string]string{"yard-b-agent": "yard-b"}},
		{name: "an empty id_prefix never fits: with it and one other, an id that begins with neither is not decided", systems: sys("node-pve"),
			clusters: []config.Proxmox{{ID: "main", IDPrefix: px("")}, {ID: "b", IDPrefix: px("b-")}},
			agents:   []config.Agent{{ID: "x", Node: "pve"}},
			want:     map[string]string{}, warn: []string{"agent x (node: pve): its id begins with the id_prefix of none of the proxmox: entries"}},
		{name: "an empty id_prefix never fits: it is no second fit for an id that begins with the other", systems: sys("node-pve", "b-node-pve"),
			clusters: []config.Proxmox{{ID: "main", IDPrefix: px("")}, {ID: "b", IDPrefix: px("b-")}},
			agents:   []config.Agent{{ID: "b-x", Node: "pve"}},
			want:     map[string]string{"b-x": "b-node-pve"}, cluster: map[string]string{"b-x": "b"}},
		// a. links_to
		{name: "links_to with a node's id", agents: []config.Agent{{ID: "x", LinksTo: "site2-node-pve"}}, clusters: two, systems: sys("site2-node-pve"),
			want: map[string]string{"x": "site2-node-pve"}, cluster: map[string]string{"x": "site2"}},
		{name: "links_to overrides node: and the agent's id", agents: []config.Agent{{ID: "site1-agent", LinksTo: "site2-node-pve", Node: "other"}}, clusters: two, systems: sys("site1-node-other"),
			want: map[string]string{"site1-agent": "site2-node-pve"}},
		{name: "links_to decides what the review's case left open", clusters: two, systems: sys("site2-node-pve"),
			agents: []config.Agent{{ID: "host1-agent", Node: "pve", LinksTo: "site1-node-pve"}},
			want:   map[string]string{"host1-agent": "site1-node-pve"}},
		{name: "links_to a guest: an agent in a VM is no node", agents: []config.Agent{{ID: "x", LinksTo: "qemu-100"}, {ID: "y", LinksTo: "site2-lxc-7"}}, clusters: append(one, two...), systems: sys("qemu-100", "site2-lxc-7"),
			want: map[string]string{}},
		// d. the guards
		{name: "two agents name the same node: none for either, a warning for each", agents: []config.Agent{{ID: "x", Node: "pve"}, {ID: "y", Node: "PVE"}}, clusters: one, systems: sys("node-pve"),
			want: map[string]string{}, warn: []string{"agent x (node: pve): the entry of y can mean the same node, node-pve", "agent y (node: PVE): the entry of x can mean the same node, node-pve"}},
		{name: "the same node name under two entries, an agent for each: none for either, although each id begins with a prefix", clusters: two, systems: sys("site1-node-pve"),
			agents: []config.Agent{{ID: "site1-agent", Node: "pve"}, {ID: "site2-agent", Node: "pve"}},
			want:   map[string]string{}, warn: []string{"agent site1-agent (node: pve): the entry of site2-agent can mean the same node, site1-node-pve", "agent site2-agent (node: pve): the entry of site1-agent can mean the same node, site2-node-pve"}},
		{name: "... and with links_to on both: each has its own", clusters: two, systems: sys("site1-node-pve"),
			agents: []config.Agent{{ID: "site1-agent", LinksTo: "site1-node-pve"}, {ID: "site2-agent", LinksTo: "site2-node-pve"}},
			want:   map[string]string{"site1-agent": "site1-node-pve", "site2-agent": "site2-node-pve"}},
		{name: "one linked to the node another names: none for either", agents: []config.Agent{{ID: "x", LinksTo: "node-pve"}, {ID: "y", Node: "pve"}}, clusters: one, systems: sys("node-pve"),
			want: map[string]string{}, warn: []string{"agent x (links_to: node-pve): the entry of y can mean the same node, node-pve", "agent y (node: pve): the entry of x can mean the same node, node-pve"}},
		{name: "the node's id is an agent's own id: that agent's entry stays its own", agents: []config.Agent{{ID: "x", Node: "pve"}, {ID: "site1-node-pve"}}, clusters: two[:1], systems: sys("site1-node-pve"),
			want: map[string]string{}, warn: []string{"agent x (node: pve): the id of its node, site1-node-pve, is also the id of an agent"}},
		{name: "no node:, no links_to, a node name without a letter or digit", agents: []config.Agent{{ID: "x"}, {ID: "y", Node: "--"}}, clusters: one,
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
			for id, want := range c.cluster {
				if links[id].Cluster != want {
					t.Errorf("agent %s: proxmox: entry %q, want %q", id, links[id].Cluster, want)
				}
			}
			if len(whys) != len(c.warn) {
				t.Fatalf("warnings %q, want %d", whys, len(c.warn))
			}
			for i, part := range c.warn {
				if !strings.Contains(whys[i], part) || !strings.Contains(whys[i], "links_to: <the id of its") && !strings.Contains(whys[i], "give that agent another id") {
					t.Errorf("warning %q lacks %q, or what to write", whys[i], part)
				}
			}
		})
	}
}

// The warning of the review's case, whole: it names the agent, the ids its node can have, the entry that is
// not used, and what to add.
func TestTheWarningForAnAgentWhoseNodeIsNotDecided(t *testing.T) {
	site1, site2 := "site1-", "site2-"
	_, whys := nodeLinks([]config.Agent{{ID: "host1-agent", Node: "pve"}},
		[]config.Proxmox{{ID: "site1", IDPrefix: &site1}, {ID: "site2", IDPrefix: &site2}},
		[]config.SystemOverride{{ID: "site2-node-pve", Wol: &config.Wol{MAC: "00:00:5e:00:53:02", Relay: "relay"}}})
	want := "agent host1-agent (node: pve): its id begins with the id_prefix of none of the proxmox: entries, so the configuration does not say which node is the agent's " +
		"(it can be site1-node-pve or site2-node-pve). systems: has an entry for site2-node-pve: it is not used for this agent while it is shown under its own id " +
		"(after a restart of the panel with the machine off, until the node's Proxmox API answers). To say which node it is, write links_to: <the id of its node> " +
		"in the agent's entry (links_to: site2-node-pve if that entry is this machine's)"
	if len(whys) != 1 || whys[0] != want {
		t.Errorf("warnings:\n%q\nwant\n%q", whys, want)
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

// An entry under the other proxmox: entry's prefix for the same node name is another machine's (review of
// fix13, must-fix 1). PVE-B's agent, lab-b-agent, is given the wol: under lab-b-node-pve-b, its own entry's
// prefix, and never the one under lab-a-node-pve-b, whatever is written there.
func TestAWolUnderAnotherEntrysPrefixIsNeverTaken(t *testing.T) {
	p, b, up := twoHosts(t, `  - id: lab-a-node-pve-b
    wol: {mac: "00:00:5e:00:53:99", relay: lab-a-agent}
`)
	if o := p.PowerOverride("lab-b-agent"); o == nil || o.ID != "lab-b-node-pve-b" || o.Wol == nil || o.Wol.MAC != "00:00:5e:00:53:36" {
		t.Fatalf("the wol: of PVE-B's agent: %+v", o)
	}
	if w := p.NodeLinkWarnings(); len(w) != 0 {
		t.Errorf("warnings: %q", w)
	}
	if got := strings.Join(p.SameMachine("lab-a-node-pve-b"), " "); got != "lab-a-node-pve-b" {
		t.Errorf("SameMachine of the other entry's id: %q", got)
	}
	b.Mu.Lock()
	*up = true
	b.Mu.Unlock()
	p.PollOnce(context.Background())
	if o := p.PowerOverride("lab-b-node-pve-b"); o == nil || o.Wol == nil || o.Wol.MAC != "00:00:5e:00:53:36" {
		t.Errorf("the node's own entry: %+v", o)
	}
}
