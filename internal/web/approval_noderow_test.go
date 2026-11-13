package web

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// The Approval dialog on the row of a Proxmox node (build 13): "Checking whether it can be switched on…" for
// good. GET /api/admin/approval has one entry per agent and names the page's row in system_id; for the agent
// of a node that was the agent's own id (Poller.RelaySystem never answers with the node), and the page looks
// for the entry of the row it draws. The Accept buttons, the rows "Machine id", "old key" and "other trust",
// and on the host's page the version chip and "Update agent" (GET /api/admin/updates, the same field) were
// missing on a node's row for the same reason.
//
// Both lists name the row the agent is shown as (Poller.ShownSystem).

// nodeRowEnv is newApprovalEnv plus the agent "pve-agent" of the Proxmox node SRV-PVEA (links_to), with a
// control token, power_control and self_update, and no trust file (its check is off).
func nodeRowEnv(t *testing.T) (*approvalEnv, *testutil.Machine) {
	t.Helper()
	a := newApprovalEnv(t)
	if err := a.s.Poller.AddAgent(config.Agent{ID: "pve-agent", Name: "PVE agent", LinksTo: "node-srv-pvea", Managed: true,
		Token: "read-word", ControlToken: "control-word", URL: "http://pve-agent", PowerControl: true, SelfUpdate: true}, nil); err != nil {
		t.Fatal(err)
	}
	m := a.gb.Add(t, "pve-agent")
	a.b.Mu.Lock()
	a.b.Metrics["pve-agent"] = func() (jsonx.M, error) {
		x := testutil.AgentMetrics()
		x["hostname"] = "srv-pvea"
		x["agent_version"] = "41-aaaaaaa"
		x["features"].(jsonx.M)["self_update"] = true
		x["self_update"] = jsonx.M{"state": "idle", "since": 1.0}
		return x, nil
	}
	a.b.Mu.Unlock()
	a.poll()
	a.poll()
	return a, m
}

// rows are the rows the browser has (GET /api/systems as the admin), by id.
func (a *approvalEnv) rows(t *testing.T) map[string]map[string]any {
	t.Helper()
	r := a.do("GET", "/api/systems", nil, "Cookie", a.admin)
	list, ok := r.body["systems"].([]any)
	if r.code != 200 || !ok {
		t.Fatalf("systems: %d %s", r.code, r.raw)
	}
	out := map[string]map[string]any{}
	for _, x := range list {
		out[x.(map[string]any)["id"].(string)] = x.(map[string]any)
	}
	return out
}

// updateRows are the rows of GET /api/admin/updates, by agent id.
func (a *approvalEnv) updateRows(t *testing.T) map[string]map[string]any {
	t.Helper()
	r := a.do("GET", "/api/admin/updates", nil, "Cookie", a.admin)
	list, ok := r.body["agents"].([]any)
	if r.code != 200 || !ok {
		t.Fatalf("updates: %d %s", r.code, r.raw)
	}
	out := map[string]map[string]any{}
	for _, x := range list {
		out[x.(map[string]any)["id"].(string)] = x.(map[string]any)
	}
	return out
}

// pageMachine is the page's own lookup of a row's machine in the approval list (app.js, apvMachine):
// (apv.data?.machines || []).find((m) => m.system_id === s.id || m.id === s.id)
func pageMachine(machines map[string]map[string]any, row string) map[string]any {
	for _, m := range machines {
		if m["system_id"] == row || m["id"] == row {
			return m
		}
	}
	return nil
}

// everySystemIDIsARow holds the rule for both lists: the system_id of every entry is the id of a row the
// browser has, and the row of every agent is found by the page's own lookup.
func (a *approvalEnv) everySystemIDIsARow(t *testing.T, when string) (rows, machines, updates map[string]map[string]any) {
	t.Helper()
	rows = a.rows(t)
	_, machines = a.page(t)
	updates = a.updateRows(t)
	if len(machines) != len(a.s.Poller.Agents()) || len(updates) != len(machines) {
		t.Fatalf("%s: %d agents, %d machines on the approval page, %d rows on the Updates page", when, len(a.s.Poller.Agents()), len(machines), len(updates))
	}
	for name, list := range map[string]map[string]map[string]any{"/api/admin/approval": machines, "/api/admin/updates": updates} {
		for id, m := range list {
			sid, _ := m["system_id"].(string)
			if rows[sid] == nil {
				t.Errorf("%s: %s names the row %q for the agent %s, and the browser has no such row", when, name, sid, id)
			}
		}
	}
	return rows, machines, updates
}

func TestTheApprovalAndUpdatesListsNameTheRowOfANodesAgent(t *testing.T) {
	a, _ := nodeRowEnv(t)
	rows, machines, updates := a.everySystemIDIsARow(t, "the node is there")
	if rows["pve-agent"] != nil || rows["node-srv-pvea"] == nil || rows["node-srv-pvea"]["kind"] != "node" {
		t.Fatalf("the fixture: the agent is a part of its node's row: %v", rows)
	}
	m := machines["pve-agent"]
	if m["system_id"] != "node-srv-pvea" || m["name"] != "SRV-PVEA" || m["ready"] != true {
		t.Fatalf("the approval page's entry of the node's agent: %v", m)
	}
	if got := pageMachine(machines, "node-srv-pvea"); got == nil || got["id"] != "pve-agent" {
		t.Fatalf("the page finds no machine for the node's row: %v", got)
	}
	if u := updates["pve-agent"]; u["system_id"] != "node-srv-pvea" || u["version"] != "41-aaaaaaa" {
		t.Fatalf("the Updates page's row of the node's agent: %v", u)
	}
	// an agent that is a row of its own is named by its own id, as before
	for _, id := range []string{"laptop", "tower"} {
		if machines[id]["system_id"] != id || updates[id]["system_id"] != id {
			t.Errorf("%s: %v / %v", id, machines[id]["system_id"], updates[id]["system_id"])
		}
	}

	// The panel starts again while the node's Proxmox API does not answer: there is no row for the node, and
	// the machine is shown under its agent's id. The lists name that row.
	a.b.Mu.Lock()
	a.b.PVE = func() ([]jsonx.M, error) { return nil, devices.Errf("connection failed") }
	a.b.Mu.Unlock()
	p, err := poller.New(a.s.Cfg, a.s.Secrets, a.gb)
	if err != nil {
		t.Fatal(err)
	}
	s := New(a.s.Cfg, a.s.Secrets, p, nil, "test")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	p.PollOnce(context.Background())
	p.PollOnce(context.Background())
	b := &approvalEnv{env: &env{t: t, s: s, srv: srv, b: a.b}, iss: a.iss, gb: a.gb}
	b.admin = b.cookie(controller(), time.Hour)
	rows, machines, updates = b.everySystemIDIsARow(t, "after a restart without the node")
	if rows["node-srv-pvea"] != nil || rows["pve-agent"] == nil || rows["pve-agent"]["kind"] != "agent" {
		t.Fatalf("the fixture: without its node's row the machine is shown under its agent's id: %v", rows)
	}
	if machines["pve-agent"]["system_id"] != "pve-agent" || updates["pve-agent"]["system_id"] != "pve-agent" {
		t.Fatalf("without the node's row: %v / %v", machines["pve-agent"]["system_id"], updates["pve-agent"]["system_id"])
	}
	if got := pageMachine(machines, "pve-agent"); got == nil || got["id"] != "pve-agent" {
		t.Fatalf("the page finds no machine for the agent's row: %v", got)
	}
}

// What an admin does from a node's row, through the machine's real gate: Turn on (the dialog's button carries
// the id of the machine the page found for the row), then a shutdown of the node's row and an update of its
// agent, each with the machine's own approval.
func TestTurnOnAndApprovedActionsFromANodesRow(t *testing.T) {
	a, m := nodeRowEnv(t)
	_, machines := a.page(t)
	found := pageMachine(machines, "node-srv-pvea")
	if found == nil || found["ready"] != true {
		t.Fatalf("the dialog of the node's row has no machine that is ready: %v", found)
	}
	id, _ := found["id"].(string) // data-id of the dialog's Turn on
	// the route knows machines by their agent's id: the row's id is none, and the dialog never sends it
	if r := a.turnOn("node-srv-pvea"); r.code != 400 {
		t.Fatalf("Turn on by the node's id: %d %s", r.code, r.raw)
	}
	pending, _, ok := approvalAsked(a.turnOn(id))
	if !ok {
		t.Fatal("Turn on did not ask for the passkey")
	}
	a.approve(t, pending)
	if res := a.result(t); res["ok"] != true {
		t.Fatalf("Turn on: %v", res)
	}
	a.poll()
	if ap := a.s.Poller.ApprovalOf("pve-agent"); ap == nil || ap.State != api.ApprovalRequired {
		t.Fatalf("its check is not on after Turn on: %+v", ap)
	}
	if b := a.s.Poller.BindingOf("pve-agent"); !b.OK() || b.Record.MachineID == "" {
		t.Fatalf("its machine id is not on record after Turn on: %+v", b)
	}
	// the dialog's rows of an admin come from the same entry: the id on record is there for the node's row
	_, machines, _ = a.everySystemIDIsARow(t, "after Turn on")
	if found = pageMachine(machines, "node-srv-pvea"); found == nil || found["state"] != "required" || found["machine_id"] == nil || found["machine_id"] == "" {
		t.Fatalf("the node's row after Turn on: %v", found)
	}

	// a shutdown of the node's row: the machine's approval is asked for, and its gate lets the request through
	name, _ := a.do("GET", "/api/systems/node-srv-pvea", nil, "Cookie", a.admin).body["name"].(string)
	pending, _, ok = approvalAsked(a.post("/api/systems/node-srv-pvea/power", map[string]any{"action": "shutdown", "delay_s": 60, "confirm": name}))
	if !ok {
		t.Fatal("the shutdown of the node's row did not ask for the machine's approval")
	}
	a.approve(t, pending)
	if res := a.result(t); res["ok"] != true {
		t.Fatalf("the approved shutdown of the node's row: %v", res)
	}
	if r := a.post("/api/systems/node-srv-pvea/power", map[string]any{"action": "cancel"}); r.code != 200 {
		t.Fatalf("cancel: %d %s", r.code, r.raw)
	}

	// "Update agent" on the node's page: the page finds the row by the node's id and sends the agent's
	u := a.updateRows(t)["pve-agent"]
	if u["system_id"] != "node-srv-pvea" {
		t.Fatalf("the Updates row: %v", u)
	}
	pending, _, ok = approvalAsked(a.post("/api/admin/updates/"+u["id"].(string), map[string]any{}))
	if !ok {
		t.Fatal("the update did not ask for the machine's approval")
	}
	a.approve(t, pending)
	if res := a.result(t); res["ok"] != true {
		t.Fatalf("the approved update: %v", res)
	}
	var approved []string
	for _, p := range m.Passed() {
		if p.Approved {
			approved = append(approved, p.Path)
		}
	}
	if len(approved) != 2 || approved[0] != "/power" || approved[1] != "/update" {
		t.Fatalf("what the machine's gate let through with an approval: %v (all: %+v)", approved, m.Passed())
	}
}
