package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// The report (build 12, the same in build 11): a standalone Proxmox host that is normally off, with an agent
// (node:, power_control, self_update) and its wol: written under the node's id. The node's row is made by an
// answer of its Proxmox API and kept in memory only, so after a restart of the panel while the machine is off
// the host is shown under its agent's id, and everything that was looked up by the shown id was not found:
// no Wake in the Power menu, and in "Update unpowered hosts" the machine said "no Wake-on-LAN is set up for it".
//
// twoHosts is that panel (testutil.TwoHostsConfig) right after such a start: PVE-A is on and relays the wake
// packets, PVE-B (agent lab-b-agent, node lab-b-node-pve-b) gives nothing. The farm's clock runs the poller
// and the run; start() is what the wake packet does to PVE-B: its agent answers 20 s later, and its Proxmox
// API answers from then on too (unless noAPI), showing its guest win11 in the state guest.
//
// The same panel with other names is setupNodeOff: the machine that is off (its agent, its node's id, its
// proxmox: entry and its node's name), and the hosts that are on (their proxmox: entry and node name, and
// their agents).
type twoHosts struct {
	e     *env
	f     *farm
	up    bool   // the Proxmox API of the machine that is off answers
	noAPI bool   // ... never, also when the machine is on
	guest string // the state of that machine's guest

	agent, node, cluster, name string            // the machine that is off
	others                     map[string]string // proxmox: entry -> node name, of the hosts that are on
}

func setupTwoHosts(t *testing.T) *twoHosts {
	t.Helper()
	return setupNodeOff(t, testutil.TwoHostsConfig(t, ""), &twoHosts{agent: "lab-b-agent", node: "lab-b-node-pve-b", cluster: "lab-b", name: "PVE-B",
		others: map[string]string{"lab-a": "PVE-A"}}, "lab-a-agent")
}

func setupNodeOff(t *testing.T, cfg *config.Panel, h *twoHosts, agentsOn ...string) *twoHosts {
	t.Helper()
	b := testutil.NewBackend()
	h.guest = "stopped"
	b.PVEOf = func(cluster string) ([]jsonx.M, error) { // (the backend's lock is held)
		switch {
		case h.others[cluster] != "":
			return testutil.HostResources(h.others[cluster], "stopped"), nil
		case cluster == h.cluster && h.up && !h.noAPI && h.f.on[h.agent] && !h.f.now.Before(h.f.ready[h.agent]):
			return testutil.HostResources(h.name, h.guest), nil
		}
		return nil, devices.Errf("connection failed")
	}
	for _, id := range agentsOn {
		b.Metrics[id] = func() (jsonx.M, error) { m := testutil.AgentMetrics(); m["hostname"] = id; return m, nil }
	}
	p, err := poller.New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, b)
	if err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	s := New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, p, nil, "test")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	h.e = &env{t: t, s: s, srv: srv, b: b}
	h.f = newFarm(t, h.e)
	h.f.version[h.agent] = "16-aaaaaaa"
	b.Mu.Lock()
	b.Metrics[h.agent] = func() (jsonx.M, error) { return h.f.metrics(h.agent) }
	b.Mu.Unlock()
	b.Reply = func(agent, path string) (int, jsonx.M, error) {
		if path == "/power" { // the agent's own answer: the countdown it scheduled
			return 200, jsonx.M{"action": "shutdown", "pending": jsonx.M{"action": "shutdown", "at": float64(h.f.now.Add(120 * time.Second).Unix()), "delay_s": 120.0, "requested_by": "x"}}, nil
		}
		return 0, nil, nil
	}
	return h
}

// start is the machine starting, as the wake packet starts it (the farm's own wake only knows pc-a and pc-b).
func (h *twoHosts) start() {
	f := h.f
	h.e.b.Mu.Lock()
	f.on[h.agent], f.booted[h.agent], f.ready[h.agent] = true, f.now.Add(5*time.Second), f.now.Add(20*time.Second)
	h.up = true
	h.e.b.Mu.Unlock()
}

// shown is the id the machine is shown under now.
func (h *twoHosts) shown() string {
	h.e.t.Helper()
	var ids []string
	for _, x := range h.e.s.Poller.Snapshot() {
		if x.ID == h.agent || x.ID == h.node {
			ids = append(ids, x.ID)
		}
	}
	if len(ids) != 1 {
		h.e.t.Fatalf("%s is shown as %v", h.name, ids)
	}
	return ids[0]
}

// wake starts a run for the machine under the id it is shown under while it is off (its agent's) and returns
// when the run's wake is out. The machine has not started yet.
func (h *twoHosts) wake() {
	t := h.e.t
	t.Helper()
	h.f.offline()
	if got := h.shown(); got != h.agent {
		t.Fatalf("%s is off and has no Proxmox answer since the start, but is shown as %s", h.name, got)
	}
	h.f.optIn(h.agent)
	if _, err := h.e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{h.agent}, By: "Alice", BySub: "u2"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		if a, _ := h.e.s.WakeRuns.Snapshot(); a == nil || hostOf(a, h.agent).WakeSentAt != nil {
			break
		}
		h.f.tick()
	}
}

// end lets the machine start, as the wake packet starts it, and the run finish. It returns the machine's entry
// of the run and what the run sent (without the farm's own machines).
func (h *twoHosts) end() (*wakerun.Host, []string) {
	h.e.t.Helper()
	h.start()
	r := h.f.finish()
	var sent []string
	for _, q := range h.f.requests() {
		if !strings.HasPrefix(q, "pc-") && !strings.HasPrefix(q, "nas ") {
			sent = append(sent, q)
		}
	}
	return hostOf(r, h.agent), sent
}

// run is wake and end: the whole run.
func (h *twoHosts) run() (*wakerun.Host, []string) {
	h.e.t.Helper()
	h.wake()
	return h.end()
}

// panel restart with the node off: wol under the node id is still found. The Updates page lists the machine
// with its relay, offers it as "off" for a run once it is switched on, and its Power menu offers the wake.
func TestPanelRestartWithTheNodeOffWolUnderTheNodeIDIsStillFoundOnThePages(t *testing.T) {
	h := setupTwoHosts(t)
	e := h.e
	admin := e.cookie(controller(), 0)
	h.f.offline()
	if got := h.shown(); got != "lab-b-agent" {
		t.Fatalf("shown as %s", got)
	}
	page := func() map[string]any {
		t.Helper()
		return wuHostRow(t, e.do("GET", "/api/admin/wake-update", nil, "Cookie", admin), "lab-b-agent")
	}
	if row := page(); row["can_join"] != true || row["can_wake"] != true || row["relay"] != "PVE-A" || row["no_wake"] != nil || row["power"] != "off" {
		t.Fatalf("the machine on the Updates page (build 12: can_wake false, \"no Wake-on-LAN is set up for it\"): %v", row)
	}
	h.f.optIn("lab-b-agent")
	if row := page(); row["choice"] != wakerun.ModeWake || row["why"] != nil {
		t.Fatalf("switched on for runs, it can be ticked as a machine that is off: %v", row)
	}
	caps := e.do("GET", "/api/systems/lab-b-agent/power", nil, "Cookie", admin)
	var wake map[string]any
	for _, a := range caps.body["actions"].([]any) {
		if m := a.(map[string]any); m["action"] == "wake" {
			wake = m
		}
	}
	if wake["available"] != true || wake["via"] != "wol:lab-a-agent" || wake["via_label"] != "PVE-A" {
		t.Fatalf("the Power menu (build 12: \"no wol: configured for this host\"): %v", wake)
	}
	if sys := e.do("GET", "/api/systems/lab-b-agent", nil, "Cookie", admin); !strings.Contains(string(sys.raw), `"wake"`) {
		t.Errorf("the host's power_actions do not list wake: %s", sys.raw)
	}
	// PVE-A relays, has no wol: of its own, and is given none
	if row := wuHostRow(t, e.do("GET", "/api/admin/wake-update", nil, "Cookie", admin), "lab-a-node-pve-a"); row["can_wake"] != false || row["no_wake"] != "no Wake-on-LAN is set up for it" {
		t.Errorf("PVE-A: %v", row)
	}
}

// wakeUpdateHosts is state.json's wake_update_hosts as it is in the file.
func wakeUpdateHosts(t *testing.T, e *env) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.s.Cfg.DataDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Hosts json.RawMessage `json:"wake_update_hosts"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := json.Compact(&out, d.Hosts); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// run switch set under the agent id applies to the node row, and back. The file keeps the id the switch was
// pressed under, in the list an older panel reads; switching off takes the machine out under both ids.
func TestRunSwitchSetUnderTheAgentIDAppliesToTheNodeRowAndBack(t *testing.T) {
	h := setupTwoHosts(t)
	e := h.e
	admin := e.cookie(controller(), 0)
	hdr := []string{"Cookie", admin, "X-Requested-With", "fairy"}
	opted := func(id string) any {
		t.Helper()
		return wuHostRow(t, e.do("GET", "/api/admin/wake-update", nil, "Cookie", admin), id)["opted_in"]
	}
	put := func(id string, on bool) {
		t.Helper()
		if r := e.do("PUT", "/api/systems/"+id+"/wake-update", map[string]any{"enabled": on}, hdr...); r.code != 200 {
			t.Fatal(id, on, r.code, string(r.raw))
		}
	}
	// the machine is off, shown under its agent's id: switched on there
	if h.shown() != "lab-b-agent" || opted("lab-b-agent") != false {
		t.Fatal("the fixture")
	}
	put("lab-b-agent", true)
	if got := wakeUpdateHosts(t, e); got != `["lab-b-agent"]` || opted("lab-b-agent") != true {
		t.Fatalf("switched on under the agent's id: file %s", got)
	}
	// it starts, its Proxmox API answers: the row is the node's, and the switch is still on
	h.start()
	for i := 0; i < 30; i++ {
		h.f.tick()
	}
	if h.shown() != "lab-b-node-pve-b" || opted("lab-b-node-pve-b") != true {
		t.Fatalf("the switch did not follow the machine to its node's row: shown as %s, opted in %v", h.shown(), opted("lab-b-node-pve-b"))
	}
	if opted("lab-a-node-pve-a") != false {
		t.Fatal("PVE-A was switched on by PVE-B's switch")
	}
	if got := wakeUpdateHosts(t, e); got != `["lab-b-agent"]` {
		t.Fatalf("the file was rewritten by reading it: %s", got)
	}
	// switched off on the node's row: off for the machine, under both ids, and the audit entry names both
	put("lab-b-node-pve-b", false)
	if got := wakeUpdateHosts(t, e); got != `[]` || opted("lab-b-node-pve-b") != false {
		t.Fatalf("switched off under the node's id: file %s, opted in %v", got, opted("lab-b-node-pve-b"))
	}
	lastSwitch := func(e *env) (string, map[string]any) {
		t.Helper()
		all := auditOf(e, "wakeupdate.switch") // newest first
		if len(all) == 0 {
			t.Fatal("no audit entry")
		}
		return fmt.Sprint(all[0]["target"]), all[0]["params"].(map[string]any)
	}
	if target, params := lastSwitch(e); target != "lab-b-node-pve-b" || params["enabled"] != false || params["also"] != "lab-b-agent" {
		t.Fatalf("the audit entry of a switch that was taken out under the machine's other id: %s %v", target, params)
	}
	// switched on on the node's row: the file has the node's id, as build 12 writes and reads it
	put("lab-b-node-pve-b", true)
	if got := wakeUpdateHosts(t, e); got != `["lab-b-node-pve-b"]` || opted("lab-b-node-pve-b") != true {
		t.Fatalf("switched on under the node's id: file %s", got)
	}
	// ... and off again there: nothing was kept under the other id, and the entry names none
	put("lab-b-node-pve-b", false)
	if _, params := lastSwitch(e); params["enabled"] != false || len(params) != 1 {
		t.Fatalf("the audit entry of a plain switch off: %v", params)
	}

	// and back: the panel restarts while the machine is off. The switch under the node's id counts for the host
	// that is shown under its agent's id.
	h2 := setupTwoHosts(t)
	if err := h2.e.s.Poller.Admin.SetWakeUpdate("lab-b-node-pve-b", true); err != nil {
		t.Fatal(err)
	}
	admin2 := h2.e.cookie(controller(), 0)
	row := wuHostRow(t, h2.e.do("GET", "/api/admin/wake-update", nil, "Cookie", admin2), "lab-b-agent")
	if h2.shown() != "lab-b-agent" || row["opted_in"] != true {
		t.Fatalf("the switch set under the node's id does not count under the agent's: %v", row)
	}
	if info, ok := (runOps{h2.e.s}).Host("lab-b-agent"); !ok || !info.OptedIn || info.RelayID != "lab-a-agent" || info.MACs != 1 || info.AgentID != "lab-b-agent" {
		t.Fatalf("the run's view of the machine: %+v %v", info, ok)
	}
	if r := h2.e.do("PUT", "/api/systems/lab-b-agent/wake-update", map[string]any{"enabled": false}, "Cookie", admin2, "X-Requested-With", "fairy"); r.code != 200 {
		t.Fatal(r.code, string(r.raw))
	}
	if got := wakeUpdateHosts(t, h2.e); got != `[]` {
		t.Fatalf("switched off under the agent's id: file %s", got)
	}
	if target, params := lastSwitch(h2.e); target != "lab-b-agent" || params["also"] != "lab-b-node-pve-b" {
		t.Fatalf("the audit entry: %s %v", target, params)
	}
	if info, _ := (runOps{h2.e.s}).Host("lab-b-agent"); info.OptedIn {
		t.Fatal("still switched on for the run")
	}
}

// run survives the id change: a run wakes the machine under its agent's id, and while it starts its Proxmox
// API answers and its row becomes the node's. The run sees it come online, updates it and switches it off
// again, exactly as a machine whose id stays. (Before: "It is no longer in the panel, so the run let go of
// it", and the machine it had just woken stayed on.)
func TestRunSurvivesTheIDChange(t *testing.T) {
	h := setupTwoHosts(t)
	host, sent := h.run()
	if got := h.shown(); got != "lab-b-node-pve-b" {
		t.Fatalf("after the run PVE-B is shown as %s", got)
	}
	if host.State != wakerun.StOff || host.On != wakerun.OnNo || !strings.Contains(host.Text, "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.") {
		t.Fatalf("the machine ended as %s (%s): %s\n%+v", host.State, host.On, host.Text, host.Log)
	}
	if got := strings.Join(sent, ", "); got != "lab-a-agent /wol, lab-b-agent /update, lab-b-agent /power" {
		t.Fatalf("what the run sent: %s", got)
	}
	if host.ID != "lab-b-agent" || host.Name != "PVE-B" {
		t.Errorf("the run's entry: %s %s", host.ID, host.Name)
	}
}

// The rules of a run hold across the id change. A Proxmox node with a running guest is left on; so is one
// whose guest's state the panel cannot say, and one whose Proxmox API does not answer at all (the host then
// stays under its agent's id, and its entry says that it is a node: nobody can say which guests run on it).
func TestRunAcrossTheIDChangeLeavesANodeWithGuestsOn(t *testing.T) {
	for _, c := range []struct {
		name  string
		guest string
		noAPI bool
		want  string
		shown string
	}{
		{name: "a guest runs", guest: "running", want: "1 guest is running on it (win11)", shown: "lab-b-node-pve-b"},
		{name: "a guest whose state is not known", guest: "unknown", want: "Fairy cannot see whether guests run on it (the Proxmox API does not show as stopped: win11 (unknown))", shown: "lab-b-node-pve-b"},
		{name: "its Proxmox API never answers", noAPI: true, want: "Fairy cannot see whether guests run on it (its entry says that it is a Proxmox node, and no Proxmox API has shown the panel that node)", shown: "lab-b-agent"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := setupTwoHosts(t)
			h.guest, h.noAPI = c.guest, c.noAPI
			host, sent := h.run()
			if got := h.shown(); got != c.shown {
				t.Fatalf("after the run PVE-B is shown as %s", got)
			}
			if host.State != wakerun.StLeftOn || host.On != wakerun.OnYes || !strings.Contains(host.Text, c.want) {
				t.Errorf("the node ended as %s (%s): %s", host.State, host.On, host.Text)
			}
			for _, q := range sent {
				if q == "lab-b-agent /power" {
					t.Errorf("RULE BROKEN: the node was asked to shut down: %v", sent)
				}
			}
		})
	}
}

// A run never switches off a machine it did not wake, across the id change too. With two agents linked to one
// node (a configuration mistake) the configuration names that node for neither of them (the rule's guard: no
// other agent's entry may mean the same node), so the node's row stands for neither agent's machine: a run
// that knows one of them by its agent's id lets go of it and sends nothing. (That the row's requests must go
// to the run's own agent is another check: TestARunDoesNotFollowItsMachineToANodeTheConfigurationDoesNotName.)
func TestTheNodesRowStandsForARunsMachineOnlyWithItsOwnAgent(t *testing.T) {
	e := setupSystems(t, nil) // pve-agent is linked to node-srv-pvea and is part of that row
	ops := runOps{e.s}
	if x, ok := ops.system("pve-agent"); !ok || x.ID != "node-srv-pvea" || x.Kind != "node" {
		t.Fatalf("the row of the node's own agent: %s %v", x.ID, ok)
	}
	if _, ok := ops.Host("pve-agent"); !ok {
		t.Fatal("the run does not know the node's own agent")
	}
	if err := e.s.Poller.AddAgent(config.Agent{ID: "second-agent", Name: "Second", LinksTo: "node-srv-pvea", Managed: true, Token: "read-word", ControlToken: "control-word",
		URL: "http://second-agent", PowerControl: true, SelfUpdate: true}, nil); err != nil {
		t.Fatal(err)
	}
	e.b.Mu.Lock()
	e.b.Metrics["second-agent"] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
	e.b.Mu.Unlock()
	e.s.Poller.PollOnce(context.Background())
	if a, ok := e.s.Poller.AgentFor("node-srv-pvea"); !ok || a.ID != "pve-agent" {
		t.Fatalf("the fixture: the node's requests go to %s", a.ID)
	}
	for _, id := range []string{"pve-agent", "second-agent"} {
		if x, ok := ops.system(id); ok {
			t.Fatalf("a row for %s: %s", id, x.ID)
		}
		if _, ok := ops.Host(id); ok {
			t.Fatalf("the run takes the node's row for the machine of %s", id)
		}
	}
	// an id that is no agent's and no row's is nobody
	if _, ok := ops.system("nobody"); ok {
		t.Fatal("a row for nobody")
	}
}

// The node's row stands for the run's machine only when the configuration names it too. Here the other
// Proxmox entry lists a node with the same name, PVE-B: the panel's link by name makes the agent a part of
// that row (the first that is there), and the configuration names lab-b-node-pve-b (the systems: entry). The
// two do not agree, so the run does not take that row for its machine: it lets go and sends nothing.
func TestARunDoesNotFollowItsMachineToANodeTheConfigurationDoesNotName(t *testing.T) {
	h := setupTwoHosts(t)
	e := h.e
	ops := runOps{e.s}
	if _, ok := ops.Host("lab-b-agent"); !ok {
		t.Fatal("the fixture: the run knows the machine under its agent's id")
	}
	e.b.Mu.Lock()
	e.b.PVEOf = func(cluster string) ([]jsonx.M, error) {
		if cluster == "lab-a" {
			return testutil.HostResources("PVE-B", "stopped"), nil // a node of that name in the other entry
		}
		return nil, devices.Errf("connection failed")
	}
	h.f.on["lab-b-agent"], h.f.booted["lab-b-agent"], h.f.ready["lab-b-agent"] = true, h.f.now, h.f.now
	e.b.Mu.Unlock()
	h.f.tick()
	rows := map[string]string{}
	for _, x := range e.s.Poller.Snapshot() {
		rows[x.ID] = x.Kind
	}
	if a, ok := e.s.Poller.AgentFor("lab-a-node-pve-b"); rows["lab-a-node-pve-b"] != "node" || rows["lab-b-agent"] != "" || !ok || a.ID != "lab-b-agent" {
		t.Fatalf("the fixture: the agent is part of the other entry's node of the same name: %v", rows)
	}
	if x, ok := ops.system("lab-b-agent"); ok {
		t.Fatalf("the run takes %s for its machine", x.ID)
	}
	if _, ok := ops.Host("lab-b-agent"); ok {
		t.Fatal("the run still knows the machine")
	}
	// Now the node the configuration names answers as well. Its row is there, but the agent is a part of the
	// other one, and the requests for this row go to nobody: it is not the run's machine either.
	e.b.Mu.Lock()
	e.b.PVEOf = func(string) ([]jsonx.M, error) { return testutil.HostResources("PVE-B", "stopped"), nil }
	e.b.Mu.Unlock()
	h.f.tick()
	rows = map[string]string{}
	for _, x := range e.s.Poller.Snapshot() {
		rows[x.ID] = x.Kind
	}
	if _, ok := e.s.Poller.AgentFor("lab-b-node-pve-b"); rows["lab-a-node-pve-b"] != "node" || rows["lab-b-node-pve-b"] != "node" || rows["lab-b-agent"] != "" || ok {
		t.Fatalf("the fixture: both nodes called PVE-B are there, the agent a part of the first: %v", rows)
	}
	if x, ok := ops.system("lab-b-agent"); ok {
		t.Fatalf("the run takes %s, whose requests go to nobody, for its machine", x.ID)
	}
	if _, ok := ops.Host("lab-b-agent"); ok {
		t.Fatal("the run still knows the machine")
	}
}

// The dialog says in one sentence why Start cannot be pressed when no machine can be ticked.
func TestUIWakeUpdateDialogSaysWhenNoMachineCanBeTicked(t *testing.T) {
	js := appJS(t)
	want := `${joined.length && !(off.length || asleep.length || on.length) ? '<div class="warnbox">No machine can be ticked right now, so no run can be started: the reason is next to each machine below.</div>' : ""}`
	if strings.Count(js, want) != 1 {
		t.Errorf("app.js lacks (or has twice) %q", want)
	}
	// the three lists it speaks of are the ones that have tick boxes, and no other has
	for _, box := range []string{`off.map((h) => tick(h, "wake", true))`, `asleep.map((h) => tick(h, "wake", false))`, `on.map((h) => tick(h, "update_only", false))`} {
		if !strings.Contains(js, box) {
			t.Errorf("app.js lacks the tick boxes %q", box)
		}
	}
	if n := strings.Count(js, "tick(h, "); n != 3 {
		t.Errorf("tick boxes are made in %d places, the sentence knows 3", n)
	}
}
