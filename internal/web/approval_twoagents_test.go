package web

import (
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Two agents that name one Proxmox node. The page pairs a row with the entries of the two lists by system_id:
// the Approval dialog takes the FIRST entry that names the row (apvMachine: find), the version chip took the
// LAST (upd.bySys, written in a loop), and the row itself showed the approval block of whichever agent was
// merged last. So the node's row could show one agent's check next to the other agent's record and buttons,
// and the version of the other.
//
// The node's row is one agent's now, the first of the configuration (poller: buildSnapshot, ShownSystem,
// AgentFor), and both lists say so: exactly one entry of each names the node's row, the same agent in both,
// which is the agent the row's requests go to. The second agent has a row of its own and is named with it, so
// its check and its version are on a page too.
func TestANodesRowIsOneAgentsInTheApprovalAndUpdatesLists(t *testing.T) {
	a, _ := nodeRowEnv(t) // pve-agent, links_to node-srv-pvea
	if err := a.s.Poller.AddAgent(config.Agent{ID: "second-agent", Name: "Second", LinksTo: "node-srv-pvea", Managed: true,
		Token: "read-word-2", ControlToken: "control-word-2", URL: "http://second-agent", PowerControl: true, SelfUpdate: true}, nil); err != nil {
		t.Fatal(err)
	}
	a.gb.Add(t, "second-agent")
	a.b.Mu.Lock()
	a.b.Metrics["second-agent"] = func() (jsonx.M, error) {
		x := testutil.AgentMetrics()
		x["agent_version"] = "40-bbbbbbb"
		x["features"].(jsonx.M)["self_update"] = true
		x["self_update"] = jsonx.M{"state": "idle", "since": 1.0}
		return x, nil
	}
	a.b.Mu.Unlock()
	for i := 0; i < 3; i++ { // the same poll after poll, whichever agent answered last
		a.poll()
		rows, machines, updates := a.everySystemIDIsARow(t, "two agents on one node")
		if rows["node-srv-pvea"] == nil || rows["second-agent"] == nil || rows["pve-agent"] != nil {
			t.Fatalf("the rows: node %v, second agent's own %v, first agent's own %v", rows["node-srv-pvea"] != nil, rows["second-agent"] != nil, rows["pve-agent"] != nil)
		}
		for name, list := range map[string]map[string]map[string]any{"/api/admin/approval": machines, "/api/admin/updates": updates} {
			var forNode []string
			for id, m := range list {
				if m["system_id"] == "node-srv-pvea" {
					forNode = append(forNode, id)
				}
			}
			if len(forNode) != 1 || forNode[0] != "pve-agent" {
				t.Errorf("%s: the node's row is named by %v, want pve-agent alone", name, forNode)
			}
			if list["second-agent"]["system_id"] != "second-agent" {
				t.Errorf("%s: second-agent is named with the row %v, want its own", name, list["second-agent"]["system_id"])
			}
		}
		if u := updates["pve-agent"]; u["version"] != "41-aaaaaaa" {
			t.Errorf("the version on the node's row: %v", u["version"])
		}
		if u := updates["second-agent"]; u["version"] != "40-bbbbbbb" {
			t.Errorf("the version on the second agent's row: %v", u["version"])
		}
		if own, ok := a.s.Poller.AgentFor("node-srv-pvea"); !ok || own.ID != "pve-agent" {
			t.Errorf("the requests of the node's row go to %q", own.ID)
		}
	}
}
