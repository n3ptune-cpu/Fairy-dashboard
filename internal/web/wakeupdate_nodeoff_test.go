package web

import (
	"bytes"
	"context"
	"encoding/json"
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
type twoHosts struct {
	e     *env
	f     *farm
	up    bool   // PVE-B's Proxmox API answers
	noAPI bool   // ... never, also when the machine is on
	guest string // the state of PVE-B's guest
}

func setupTwoHosts(t *testing.T) *twoHosts {
	t.Helper()
	cfg := testutil.TwoHostsConfig(t, "")
	b := testutil.NewBackend()
	h := &twoHosts{guest: "stopped"}
	b.PVEOf = func(cluster string) ([]jsonx.M, error) { // (the backend's lock is held)
		switch {
		case cluster == "lab-a":
			return testutil.HostResources("PVE-A", "stopped"), nil
		case cluster == "lab-b" && h.up && !h.noAPI && h.f.on["lab-b-agent"] && !h.f.now.Before(h.f.ready["lab-b-agent"]):
			return testutil.HostResources("PVE-B", h.guest), nil
		}
		return nil, devices.Errf("connection failed")
	}
	b.Metrics["lab-a-agent"] = func() (jsonx.M, error) { m := testutil.AgentMetrics(); m["hostname"] = "pve-a"; return m, nil }
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
	h.f.version["lab-b-agent"] = "16-aaaaaaa"
	b.Mu.Lock()
	b.Metrics["lab-b-agent"] = func() (jsonx.M, error) { return h.f.metrics("lab-b-agent") }
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
	f.on["lab-b-agent"], f.booted["lab-b-agent"], f.ready["lab-b-agent"] = true, f.now.Add(5*time.Second), f.now.Add(20*time.Second)
	h.up = true
	h.e.b.Mu.Unlock()
}

// shown is the id PVE-B is shown under now.
func (h *twoHosts) shown() string {
	h.e.t.Helper()
	var ids []string
	for _, x := range h.e.s.Poller.Snapshot() {
		if x.ID == "lab-b-agent" || x.ID == "lab-b-node-pve-b" {
			ids = append(ids, x.ID)
		}
	}
	if len(ids) != 1 {
		h.e.t.Fatalf("PVE-B is shown as %v", ids)
	}
	return ids[0]
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
	// switched off on the node's row: off for the machine, under both ids
	put("lab-b-node-pve-b", false)
	if got := wakeUpdateHosts(t, e); got != `[]` || opted("lab-b-node-pve-b") != false {
		t.Fatalf("switched off under the node's id: file %s, opted in %v", got, opted("lab-b-node-pve-b"))
	}
	// switched on on the node's row: the file has the node's id, as build 12 writes and reads it
	put("lab-b-node-pve-b", true)
	if got := wakeUpdateHosts(t, e); got != `["lab-b-node-pve-b"]` || opted("lab-b-node-pve-b") != true {
		t.Fatalf("switched on under the node's id: file %s", got)
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
	if info, _ := (runOps{h2.e.s}).Host("lab-b-agent"); info.OptedIn {
		t.Fatal("still switched on for the run")
	}
}
