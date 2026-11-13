package poller

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Build 14: when the agent's id begins with the id_prefix of several proxmox: entries, the entry with the
// longest id_prefix is the agent's (nodeChoice has what that can get wrong, and why it is said every time).
//
// The report: a site with a cluster and standalone hosts, id_prefixes beta-, alpha- and alpha-b-, and the
// agent alpha-b-box2-agent with node: BOX-2 on the standalone host. Its id begins with alpha- and with
// alpha-b-: "not decided", no Wake for the host while it was off after a restart of the panel, until
// links_to: was written.
func TestTheLongestIDPrefixWins(t *testing.T) {
	px := func(prefix string) *string { return &prefix }
	entries := func(prefixes ...string) (out []config.Proxmox) {
		for _, p := range prefixes {
			out = append(out, config.Proxmox{ID: strings.TrimSuffix("e-"+p, "-"), IDPrefix: px(p)})
		}
		return out
	}
	report := entries("beta-", "alpha-", "alpha-b-")
	for _, c := range []struct {
		name     string
		agents   []config.Agent
		clusters []config.Proxmox
		want     map[string]string // agent id -> node id
		inferred []string          // the sentences, whole, in order
	}{
		{name: "the report: three id_prefixes, two begin the agent's id", clusters: report,
			agents:   []config.Agent{{ID: "alpha-b-box2-agent", Node: "BOX-2"}},
			want:     map[string]string{"alpha-b-box2-agent": "alpha-b-node-box-2"},
			inferred: []string{"agent alpha-b-box2-agent (node: BOX-2) -> node alpha-b-node-box-2: inferred from id_prefix alpha-b- of the proxmox: entry e-alpha-b (the longest of alpha-, alpha-b-, which the agent's id begins with); set links_to in the agent's entry to override"}},
		{name: "the order of the entries plays no part", clusters: entries("alpha-b-", "alpha-", "beta-"),
			agents:   []config.Agent{{ID: "alpha-b-box2-agent", Node: "BOX-2"}},
			want:     map[string]string{"alpha-b-box2-agent": "alpha-b-node-box-2"},
			inferred: []string{"agent alpha-b-box2-agent (node: BOX-2) -> node alpha-b-node-box-2: inferred from id_prefix alpha-b- of the proxmox: entry e-alpha-b (the longest of alpha-b-, alpha-, which the agent's id begins with); set links_to in the agent's entry to override"}},
		{name: "nested three deep: each agent gets the deepest entry its id begins with", clusters: entries("alpha-", "alpha-b-", "alpha-b-c-"),
			agents: []config.Agent{{ID: "alpha-b-c-agent", Node: "n3"}, {ID: "alpha-b-agent", Node: "n2"}, {ID: "alpha-agent", Node: "n1"}, {ID: "alpha-bc-agent", Node: "n0"}},
			want:   map[string]string{"alpha-b-c-agent": "alpha-b-c-node-n3", "alpha-b-agent": "alpha-b-node-n2", "alpha-agent": "alpha-node-n1", "alpha-bc-agent": "alpha-node-n0"},
			inferred: []string{
				"agent alpha-b-c-agent (node: n3) -> node alpha-b-c-node-n3: inferred from id_prefix alpha-b-c- of the proxmox: entry e-alpha-b-c (the longest of alpha-, alpha-b-, alpha-b-c-, which the agent's id begins with); set links_to in the agent's entry to override",
				"agent alpha-b-agent (node: n2) -> node alpha-b-node-n2: inferred from id_prefix alpha-b- of the proxmox: entry e-alpha-b (the longest of alpha-, alpha-b-, which the agent's id begins with); set links_to in the agent's entry to override",
				"agent alpha-agent (node: n1) -> node alpha-node-n1: inferred from id_prefix alpha- of the proxmox: entry e-alpha, which the agent's id begins with; set links_to in the agent's entry to override",
				"agent alpha-bc-agent (node: n0) -> node alpha-node-n0: inferred from id_prefix alpha- of the proxmox: entry e-alpha, which the agent's id begins with; set links_to in the agent's entry to override"}},
		{name: "one id_prefix fits: that entry, as in build 13, and it is said", clusters: report,
			agents:   []config.Agent{{ID: "beta-agent", Node: "pve"}},
			want:     map[string]string{"beta-agent": "beta-node-pve"},
			inferred: []string{"agent beta-agent (node: pve) -> node beta-node-pve: inferred from id_prefix beta- of the proxmox: entry e-beta, which the agent's id begins with; set links_to in the agent's entry to override"}},
		{name: "no id_prefix fits: not decided, nothing inferred", clusters: report,
			agents: []config.Agent{{ID: "gamma-agent", Node: "pve"}}, want: map[string]string{}},
		{name: "an empty id_prefix is no shorter fit: it never counts", clusters: entries("", "alpha-", "alpha-b-"),
			agents:   []config.Agent{{ID: "alpha-b-x", Node: "pve"}, {ID: "x", Node: "pve2"}},
			want:     map[string]string{"alpha-b-x": "alpha-b-node-pve"},
			inferred: []string{"agent alpha-b-x (node: pve) -> node alpha-b-node-pve: inferred from id_prefix alpha-b- of the proxmox: entry e-alpha-b (the longest of alpha-, alpha-b-, which the agent's id begins with); set links_to in the agent's entry to override"}},
		{name: "links_to comes first, and nothing is inferred", clusters: report,
			agents: []config.Agent{{ID: "alpha-b-box2-agent", Node: "BOX-2", LinksTo: "alpha-node-box-2"}},
			want:   map[string]string{"alpha-b-box2-agent": "alpha-node-box-2"}},
		{name: "one proxmox: entry comes first, and nothing is inferred", clusters: entries("alpha-"),
			agents: []config.Agent{{ID: "alpha-b-agent", Node: "pve"}, {ID: "zeta", Node: "pve2"}},
			want:   map[string]string{"alpha-b-agent": "alpha-node-pve", "zeta": "alpha-node-pve2"}},
		{name: "the guards still hold: two agents that can mean the node the longest prefix names", clusters: report,
			agents: []config.Agent{{ID: "alpha-b-box2-agent", Node: "BOX-2"}, {ID: "alpha-b-other", Node: "box-2"}},
			want:   map[string]string{}},
		{name: "the guards still hold: the node's own agent has links_to, the other one is given nothing", clusters: report,
			agents: []config.Agent{{ID: "owner", LinksTo: "alpha-b-node-box-2"}, {ID: "alpha-b-2-agent", Node: "BOX-2"}},
			want:   map[string]string{}},
		// what is refused when the configuration is read (config.validateProxmox, managed.Check): if it ever
		// came here, two entries with one id_prefix decide nothing
		{name: "the same id_prefix twice: not decided", clusters: append(entries("alpha-", "alpha-b-"), config.Proxmox{ID: "twin", IDPrefix: px("alpha-b-")}),
			agents: []config.Agent{{ID: "alpha-b-agent", Node: "pve"}}, want: map[string]string{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			links, _, _ := nodeLinks(c.agents, c.clusters, nil)
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
			if inf := nodeInferred(c.agents, links); !slices.Equal(inf, c.inferred) {
				t.Errorf("inferred:\n%q\nwant\n%q", inf, c.inferred)
			}
		})
	}
}

// The warning for the one case that is still "more than one": it names the prefixes and what to write.
func TestTheSameIDPrefixTwiceIsNotDecidedAndSaysSo(t *testing.T) {
	a, b := "alpha-b-", "alpha-b-"
	_, whys, _ := nodeLinks([]config.Agent{{ID: "alpha-b-agent", Node: "pve"}},
		[]config.Proxmox{{ID: "one", IDPrefix: &a}, {ID: "two", IDPrefix: &b}},
		[]config.SystemOverride{{ID: "alpha-b-node-pve", Wol: &config.Wol{MAC: "00:00:5e:00:53:03", Relay: "relay"}}})
	if len(whys) != 1 || !strings.Contains(whys[0], "its id begins with the id_prefix of more than one proxmox: entry, and none of them is the longest (alpha-b-, alpha-b-)") ||
		!strings.Contains(whys[0], "write links_to: <the id of its node> in the agent's entry") {
		t.Errorf("warnings: %q", whys)
	}
}

// The wrong pick the rule can make (nodeChoice): an agent of the entry with the shorter prefix whose id
// begins with the longer one. This test holds what follows from it, so that a change of the consequences is
// seen: the agent is given the node id under the longer prefix, the wol: written there is the one its row
// gets while its node has no row (a wake of the other machine), the link is said, and links_to puts it right.
func TestAnAgentOfTheShorterPrefixNamedLikeTheLongerOne(t *testing.T) {
	const line = "agent lab-b-2-agent (node: PVE-B) -> node lab-b-node-pve-b: inferred from id_prefix lab-b- of the proxmox: entry lab-b (the longest of lab-b-, lab-, which the agent's id begins with); set links_to in the agent's entry to override"
	poller := func(linksTo string) *Poller {
		t.Helper()
		// lab-a- and lab-b- are the fixture's standalone hosts, and lab-b-node-pve-b has a wol:. A third entry,
		// lab-, is a cluster with a node that is also called PVE-B. Its agent is named as if it were on the
		// standalone host lab-b, which has no agent in this configuration.
		cfg := testutil.TwoHostsConfig(t, "")
		short := "lab-"
		cfg.Proxmox = append(cfg.Proxmox, config.Proxmox{ID: "lab", IDPrefix: &short, ClusterName: "LAB", URLs: []string{"https://lab:8006"}})
		cfg.Agents = []config.Agent{cfg.Agents[0], {ID: "lab-b-2-agent", Name: "On the cluster", URL: "http://lab-b-2-agent", Node: "PVE-B", LinksTo: linksTo, Token: "read-word"}}
		p, err := New(cfg, config.Secrets{}, testutil.NewBackend())
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := poller("")
	if id, cluster, ok := p.NodeOf("lab-b-2-agent"); !ok || id != "lab-b-node-pve-b" || cluster != "lab-b" {
		t.Fatalf("the pick: %q %q %v", id, cluster, ok)
	}
	if o := p.PowerOverride("lab-b-2-agent"); o == nil || o.ID != "lab-b-node-pve-b" || o.Wol == nil {
		t.Fatalf("the consequence the rule's comment names (the other machine's wol: on this agent's row) is not there any more: %+v", o)
	}
	if inf := p.NodeLinksInferred(); !slices.Contains(inf, line) {
		t.Fatalf("the pick is not said:\n%q", inf)
	}
	if w := p.NodeLinkWarnings(); len(w) != 0 {
		t.Errorf("warnings: %q", w)
	}
	// links_to names the node the admin means: the pick is gone, the other machine's wol: with it, and the line
	p = poller("lab-node-pve-b")
	if id, cluster, ok := p.NodeOf("lab-b-2-agent"); !ok || id != "lab-node-pve-b" || cluster != "lab" {
		t.Fatalf("with links_to: %q %q %v", id, cluster, ok)
	}
	if o := p.PowerOverride("lab-b-2-agent"); o != nil {
		t.Fatalf("with links_to the agent's row still has a systems: entry: %+v", o)
	}
	for _, s := range p.NodeLinksInferred() {
		if strings.Contains(s, "lab-b-2-agent") {
			t.Errorf("a line for an agent with links_to: %s", s)
		}
	}
}

// The panel says every inferred link once in its log when it starts.
func TestTheStartLogsEveryInferredLink(t *testing.T) {
	var logs bytes.Buffer
	var logMu sync.Mutex
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&logMu, &logs}, nil)))
	defer slog.SetDefault(old)
	if _, err := New(testutil.TwoHostsConfig(t, ""), config.Secrets{}, testutil.NewBackend()); err != nil {
		t.Fatal(err)
	}
	logMu.Lock()
	buf := logs.String()
	logMu.Unlock()
	for _, want := range []string{
		"level=INFO msg=\"config: agent lab-a-agent (node: PVE-A) -> node lab-a-node-pve-a: inferred from id_prefix lab-a- of the proxmox: entry lab-a, which the agent's id begins with; set links_to in the agent's entry to override\"",
		"level=INFO msg=\"config: agent lab-b-agent (node: PVE-B) -> node lab-b-node-pve-b: inferred from id_prefix lab-b- of the proxmox: entry lab-b, which the agent's id begins with; set links_to in the agent's entry to override\"",
	} {
		if strings.Count(buf, want) != 1 {
			t.Errorf("the log lacks (or has twice) %s:\n%s", want, buf)
		}
	}
}
