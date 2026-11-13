package poller

import (
	"context"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// plugRow is one device as an agent relays it in /metrics (power_sources).
func plugRow(id string, watts float64, hint string) map[string]any {
	row := map[string]any{"id": id, "type": "homeassistant", "name": id, "ok": true, "watts": watts}
	if hint != "" {
		row["system_hint"] = hint
	}
	return row
}

// The watts of a plug land on the row its machine is shown as, whichever of the machine's two ids the link was
// written with (build 14). One agent, PVE-A's, reads the plugs (through Home Assistant, on its network) and
// relays them; two of them feed ANOTHER machine, PVE-B, which is a Proxmox node with an agent and is normally
// off: one is linked with PVE-B's agent id, one with its node's id. A third is the relaying machine's own
// (system_hint: self), and that machine is a node with an agent too.
//
// Until now the watts were looked up by the shown row's id alone: with PVE-B's node row there, the plug linked
// with the agent's id showed on no row; with PVE-B off after a restart of the panel (shown under its agent's
// id), the one linked with the node's id showed on no row; and system_hint: self of a node's agent never
// showed anywhere.
func TestPlugWattsLandOnTheRowOfTheMachineUnderEitherOfItsIDs(t *testing.T) {
	cfg := testutil.TwoHostsConfig(t, "")
	for i := range cfg.Agents {
		if cfg.Agents[i].ID == "lab-a-agent" {
			cfg.Agents[i].PowerLinks = map[string]string{"plug-b-1": "lab-b-agent", "plug-b-2": "lab-b-node-pve-b"}
		}
	}
	b := testutil.NewBackend()
	up := false
	b.PVEOf = func(cluster string) ([]jsonx.M, error) {
		switch {
		case cluster == "lab-a":
			return testutil.HostResources("PVE-A", "stopped"), nil
		case up:
			return testutil.HostResources("PVE-B", "stopped"), nil
		}
		return nil, devices.Errf("connection failed")
	}
	b.Metrics["lab-a-agent"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["power_sources"] = []any{plugRow("plug-b-1", 10, ""), plugRow("plug-b-2", 20, ""), plugRow("plug-a", 5, "self"), plugRow("plug-loose", 7, "")}
		return m, nil
	}
	b.Metrics["lab-b-agent"] = func() (jsonx.M, error) {
		if up {
			m := testutil.AgentMetrics()
			m["power_sources"] = []any{} // it relays nothing itself
			return m, nil
		}
		return nil, devices.Errf("connection failed")
	}
	p, err := New(cfg, config.Secrets{}, b)
	if err != nil {
		t.Fatal(err)
	}
	check := func(when, rowB string) {
		t.Helper()
		rows := snap(p)
		watts := func(id string) float64 {
			if r, ok := rows[id]; ok && r.PowerW != nil {
				return *r.PowerW
			}
			return -1
		}
		if got := watts(rowB); got != 30 {
			t.Errorf("%s: the row of PVE-B (%s) has %v W, want the 30 W of its two plugs", when, rowB, got)
		}
		if got := watts("lab-a-node-pve-a"); got != 5 {
			t.Errorf("%s: the row of PVE-A has %v W, want the 5 W of its own plug (system_hint: self of the node's agent)", when, got)
		}
		// what the page gets: each reading names the row, so the host's page and the Power page find it
		linked := map[string]string{}
		for _, s := range p.Power.Overview().Sources {
			for _, r := range s.Readings {
				if r.SystemID != nil {
					linked[s.ID] = *r.SystemID
				} else {
					linked[s.ID] = ""
				}
			}
		}
		want := map[string]string{"lab-a-agent/plug-b-1": rowB, "lab-a-agent/plug-b-2": rowB, "lab-a-agent/plug-a": "lab-a-node-pve-a", "lab-a-agent/plug-loose": ""}
		for id, row := range want {
			if got, ok := linked[id]; !ok || got != row {
				t.Errorf("%s: %s is linked to %q, want %q", when, id, got, row)
			}
		}
		for id := range rows {
			if id != rowB && id != "lab-a-node-pve-a" && rows[id].PowerW != nil {
				t.Errorf("%s: the row %s has %v W", when, id, *rows[id].PowerW)
			}
		}
	}
	p.PollOnce(context.Background())
	if _, node := snap(p)["lab-b-node-pve-b"]; node {
		t.Fatal("the fixture: PVE-B is off after the panel's start and has no node row")
	}
	check("PVE-B off, shown under its agent's id", "lab-b-agent")
	b.Mu.Lock()
	up = true
	b.Mu.Unlock()
	p.PollOnce(context.Background())
	if _, own := snap(p)["lab-b-agent"]; own {
		t.Fatal("the fixture: PVE-B is on and is shown as its node's row")
	}
	check("PVE-B on, shown under its node's id", "lab-b-node-pve-b")
	// the watts are still the sources' own: the total counts each plug once
	if tot := p.Power.Overview().TotalW; tot == nil || *tot != 42 {
		t.Errorf("the total: %v, want 42", tot)
	}
}

// ShownID: the row a machine is shown as, by either of its ids; nothing is guessed where the configuration
// does not decide whose node an id is.
func TestShownID(t *testing.T) {
	p, b, up := twoHosts(t, "")
	_ = b
	p.PollOnce(context.Background())
	for id, want := range map[string]string{
		"lab-b-agent": "lab-b-agent", "lab-b-node-pve-b": "lab-b-agent", // off: its agent's row is the one that is there
		"lab-a-agent": "lab-a-node-pve-a", "lab-a-node-pve-a": "lab-a-node-pve-a",
		"lab-a-qemu-100": "lab-a-qemu-100", "nobody": "nobody", "": "", "lab-c-node-x": "lab-c-node-x",
	} {
		if got := p.ShownID(id); got != want {
			t.Errorf("PVE-B off: ShownID(%q) = %q, want %q", id, got, want)
		}
	}
	*up = true
	p.PollOnce(context.Background())
	for id, want := range map[string]string{"lab-b-agent": "lab-b-node-pve-b", "lab-b-node-pve-b": "lab-b-node-pve-b"} {
		if got := p.ShownID(id); got != want {
			t.Errorf("PVE-B on: ShownID(%q) = %q, want %q", id, got, want)
		}
	}
	// not decided: two hosts whose node is "pve", an agent whose id says nothing. A link with the node's id of
	// the host that is off lands on no row; the agent's own id is its own row.
	u, err := New(testutil.SameNameConfig(t), config.Secrets{}, testutil.NewBackend())
	if err != nil {
		t.Fatal(err)
	}
	u.PollOnce(context.Background())
	for id, want := range map[string]string{"site1-node-pve": "site1-node-pve", "site2-node-pve": "site2-node-pve", "host1-agent": "host1-agent"} {
		if got := u.ShownID(id); got != want {
			t.Errorf("not decided: ShownID(%q) = %q, want %q", id, got, want)
		}
	}
}
