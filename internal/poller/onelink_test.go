package poller

import (
	"context"
	"fmt"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// linkPanel is a panel with the given proxmox: entries (id, id_prefix), agents and systems: lines, whose
// Proxmox APIs answer with the nodes of `nodes` (proxmox id -> node name; changed under b.Mu) and whose agents
// all answer.
func linkPanel(t *testing.T, entries [][2]string, agents, systems string, nodes map[string]string) (*Poller, *testutil.Backend) {
	t.Helper()
	t.Setenv("TOK_A", testutil.ReadToken)
	t.Setenv("CTL_LAB", testutil.ControlToken)
	t.Setenv("PVE_SECRET", "pve-secret-uuid")
	dir := t.TempDir()
	y := `
public_url: https://panel.example.com
request_timeout: 1
offline_after: 60
audit_log: ` + fmt.Sprintf("%q", dir+"/audit.log") + `
data_dir: ` + fmt.Sprintf("%q", dir+"/data") + `
oidc: {issuer: "https://id.example.com", client_id: cid, allowed_groups: [viewers], control_groups: [controllers]}
proxmox:
`
	for i, e := range entries {
		y += fmt.Sprintf("  - {id: %s, id_prefix: %s, cluster_name: C%d, standalone: true, urls: [\"https://pve%d:8006\"], token_id: \"fairy@pve!panel\", token_secret_env: PVE_SECRET, verify_tls: false}\n", e[0], e[1], i, i)
	}
	y += "agents:\n" + agents
	if systems != "" {
		y += "systems:\n" + systems
	}
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	b := testutil.NewBackend()
	b.PVEOf = func(cluster string) ([]jsonx.M, error) {
		if n := nodes[cluster]; n != "" {
			return testutil.HostResources(n, "stopped"), nil
		}
		return nil, devices.Errf("connection failed")
	}
	for _, a := range cfg.Agents {
		b.Metrics[a.ID] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
	}
	p, err := New(cfg, config.Secrets{}, b)
	if err != nil {
		t.Fatal(err)
	}
	return p, b
}

const linkAgent = "  - {id: %s, name: %s, url: \"http://%s\", token_env: TOK_A, control_token_env: CTL_LAB, %s, power_control: true, self_update: true}\n"

// The rows and the configuration's link are one decision. Entries alpha- and alpha-b-, a node "pve" in both
// (the installer's name), the agent alpha-b-agent with node: pve. The configuration gives it alpha-b-node-pve
// (the longest id_prefix its id begins with), and that is where its wol:, its switches and its timings are
// looked up. Until build 13 the rows made it a part of alpha-node-pve, the first node of that name in the
// order of the file: the row of one machine showed the sensors, the approval block and the buttons of
// another, and the timings written for the machine were not found under the row it was shown as.
func TestTheRowsFollowTheLinkOfTheConfiguration(t *testing.T) {
	nodes := map[string]string{"alpha": "pve", "alphab": "pve"}
	p, _ := linkPanel(t, [][2]string{{"alpha", "alpha-"}, {"alphab", "alpha-b-"}},
		fmt.Sprintf(linkAgent, "alpha-b-agent", "B", "b", "node: pve"),
		"  - {id: alpha-b-node-pve, wake_update: {off_after_s: 3600}}\n", nodes)
	p.PollOnce(context.Background())
	nodeID, _, ok := p.NodeOf("alpha-b-agent")
	if !ok || nodeID != "alpha-b-node-pve" {
		t.Fatalf("the configuration's link: %q %v", nodeID, ok)
	}
	rows := snap(p)
	if _, own := rows["alpha-b-agent"]; own {
		t.Error("the agent has a row of its own although its node's row is there")
	}
	if len(rows["alpha-b-node-pve"].Temps) == 0 {
		t.Errorf("the agent is not a part of its node's row: %+v", rows["alpha-b-node-pve"])
	}
	if len(rows["alpha-node-pve"].Temps) != 0 {
		t.Error("the agent's sensors are in the row of alpha-node-pve, another machine")
	}
	if s := p.ShownSystem("alpha-b-agent"); s == nil || s.ID != nodeID {
		t.Errorf("ShownSystem = %+v, want the row of %s", s, nodeID)
	}
	if a, ok := p.AgentFor("alpha-b-node-pve"); !ok || a.ID != "alpha-b-agent" {
		t.Errorf("AgentFor(its node) = %q %v", a.ID, ok)
	}
	if a, ok := p.AgentFor("alpha-node-pve"); ok {
		t.Errorf("AgentFor(alpha-node-pve) = %q: an action on that row would go to another machine's agent", a.ID)
	}
	if off, _ := p.WakeUpdateOverride(p.ShownSystem("alpha-b-agent").ID); off != 3600 {
		t.Errorf("the machine's own off_after_s under the row it is shown as: %d", off)
	}

	// its own host's API does not answer (the other one does): the agent keeps its own row. It is not made
	// a part of the node of the same name that happens to be there.
	p2, _ := linkPanel(t, [][2]string{{"alpha", "alpha-"}, {"alphab", "alpha-b-"}},
		fmt.Sprintf(linkAgent, "alpha-b-agent", "B", "b", "node: pve"), "", map[string]string{"alpha": "pve"})
	p2.PollOnce(context.Background())
	rows = snap(p2)
	if _, own := rows["alpha-b-agent"]; !own || len(rows["alpha-node-pve"].Temps) != 0 {
		t.Errorf("with only alpha's node there: own row %v, alpha-node-pve has the agent's sensors %v", own, len(rows["alpha-node-pve"].Temps) != 0)
	}
	if s := p2.ShownSystem("alpha-b-agent"); s == nil || s.ID != "alpha-b-agent" {
		t.Errorf("ShownSystem = %+v, want the agent's own row", s)
	}
}

// The shape the longest-prefix rule was made for: a site with a cluster and a standalone host next to it
// (id_prefix b- and b-c-), another site (a-), and the agent of the standalone host, b-c-x-agent, on its node
// X. With node: and with links_to, with its node's row there and without it, in every order of the entries:
// one machine under its two ids, with its wol: and its timings, and the row it is shown as is the node the
// configuration names.
func TestAnAgentOfTheLongerPrefixIsOneMachineAtRunTime(t *testing.T) {
	all := [][2]string{{"sitea", "a-"}, {"siteb", "b-"}, {"sitebc", "b-c-"}}
	for _, order := range [][]int{{0, 1, 2}, {2, 1, 0}, {1, 2, 0}} {
		for _, link := range []string{"node: X", "links_to: b-c-node-x"} {
			var entries [][2]string
			for _, i := range order {
				entries = append(entries, all[i])
			}
			// the other two sites have a node of the same name, and theirs are always there
			nodes := map[string]string{"sitea": "X", "siteb": "X"}
			p, b := linkPanel(t, entries,
				"  - {id: relay, name: R, url: \"http://relay\", token_env: TOK_A, control_token_env: CTL_LAB, wol_relay: true}\n"+
					fmt.Sprintf(linkAgent, "b-c-x-agent", "X", "x", link),
				"  - {id: b-c-node-x, wol: {mac: \"00:00:5e:00:53:36\", relay: relay}, wake_update: {off_after_s: 1800}}\n", nodes)
			for _, there := range []bool{false, true} {
				when := fmt.Sprintf("order %v, %s, its node's row there: %v", order, link, there)
				b.Mu.Lock()
				if there {
					nodes["sitebc"] = "X"
				}
				b.Mu.Unlock()
				p.PollOnce(context.Background())
				const agent, node = "b-c-x-agent", "b-c-node-x"
				if id, cl, ok := p.NodeOf(agent); !ok || id != node || cl != "sitebc" {
					t.Errorf("%s: NodeOf = %q %q %v", when, id, cl, ok)
				}
				shown := p.ShownSystem(agent)
				want := agent
				if there {
					want = node
				}
				if shown == nil || shown.ID != want {
					t.Fatalf("%s: shown as %+v, want %s", when, shown, want)
				}
				rows := snap(p)
				if len(rows["a-node-x"].Temps) != 0 || len(rows["b-node-x"].Temps) != 0 {
					t.Errorf("%s: the agent's sensors are in the row of another site's node X", when)
				}
				for _, id := range []string{agent, node} {
					if o := p.PowerOverride(id); o == nil || o.Wol == nil {
						t.Errorf("%s: no wol: found under %s", when, id)
					}
					if off, _ := p.WakeUpdateOverride(id); off != 1800 {
						t.Errorf("%s: off_after_s under %s = %d", when, id, off)
					}
				}
				if a, ok := p.AgentFor(shown.ID); !ok || a.ID != agent {
					t.Errorf("%s: AgentFor(%s) = %q", when, shown.ID, a.ID)
				}
			}
		}
	}
}
