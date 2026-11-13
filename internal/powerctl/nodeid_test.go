package powerctl

import (
	"context"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// twoHosts is the poller of testutil.TwoHostsConfig right after a start of the panel with PVE-B off: PVE-A
// answers (agent and Proxmox API), PVE-B gives nothing, so it has no node row and is shown under its agent's id.
func twoHosts(t *testing.T) (*poller.Poller, *testutil.Backend) {
	t.Helper()
	b := testutil.NewBackend()
	b.PVEOf = func(cluster string) ([]jsonx.M, error) {
		if cluster == "lab-a" {
			return testutil.HostResources("PVE-A", "stopped"), nil
		}
		return nil, devices.Errf("connection failed")
	}
	b.Metrics["lab-a-agent"] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
	p, err := poller.New(testutil.TwoHostsConfig(t, ""), config.Secrets{}, b)
	if err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	return p, b
}

func rowOf(t *testing.T, p *poller.Poller, id string) model.System {
	t.Helper()
	for _, s := range p.Snapshot() {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no system %s", id)
	return model.System{}
}

func capOf(p *poller.Poller, s model.System, action string) model.PowerActionCap {
	for _, c := range Capabilities(p, s).Actions {
		if c.Action == action {
			return c
		}
	}
	return model.PowerActionCap{}
}

// panel restart with the node off: wol under the node id is still found. The Power menu of the host, shown
// under its agent's id, offers "Power on (WoL via PVE-A)", and the packet that goes out is the one of the
// node's wol: entry, through its relay. (Build 12: "no wol: configured for this host".)
func TestPanelRestartWithTheNodeOffTheHostCanBeWoken(t *testing.T) {
	p, b := twoHosts(t)
	host := rowOf(t, p, "lab-b-agent")
	if host.Kind != "agent" || host.Status != "offline" {
		t.Fatalf("the fixture: %s %s", host.Kind, host.Status)
	}
	c := capOf(p, host, "wake")
	if !c.Available || model.Deref(c.Via) != "wol:lab-a-agent" || model.Deref(c.ViaLabel) != "PVE-A" {
		t.Fatalf("wake for the host under its agent's id: %+v (reason %q)", c, model.Deref(c.Reason))
	}
	res, err := Execute(context.Background(), p, host, Request{Action: "wake"}, "Ann")
	if err != nil || !strings.Contains(res.Message, "magic packet sent via PVE-A") {
		t.Fatalf("wake: %v %+v", err, res)
	}
	b.Mu.Lock()
	reqs := strings.Join(b.Requests, "\n")
	b.Mu.Unlock()
	if !strings.Contains(reqs, "lab-a-agent POST /wol map[broadcast:192.0.2.255 mac:00:00:5e:00:53:36 port:9]") || strings.Count(reqs, "/wol") != 1 {
		t.Fatalf("what was sent:\n%s", reqs)
	}
	// the other host has no wol: and is offered none: PVE-B's entry is PVE-B's
	if c := capOf(p, rowOf(t, p, "lab-a-node-pve-a"), "wake"); c.Available || !strings.Contains(model.Deref(c.Reason), "no wol: configured") {
		t.Errorf("PVE-A: %+v", c)
	}
}

// The power switch an administrator set for the node (`fairy admin host lab-b-node-pve-b power-control
// disable`) takes the wake away from the host under its agent's id too, and says which id to enable again.
func TestTheNodesPowerSwitchStopsTheWakeUnderTheAgentsID(t *testing.T) {
	p, b := twoHosts(t)
	if err := p.Admin.SetPowerDisabled("lab-b-node-pve-b", true); err != nil {
		t.Fatal(err)
	}
	host := rowOf(t, p, "lab-b-agent")
	c := capOf(p, host, "wake")
	if c.Available || !strings.Contains(model.Deref(c.Reason), "fairy admin host lab-b-node-pve-b power-control enable") {
		t.Fatalf("wake past the node's power switch: %+v (reason %q)", c, model.Deref(c.Reason))
	}
	if _, err := Execute(context.Background(), p, host, Request{Action: "wake"}, "Ann"); err == nil {
		t.Fatal("the wake was carried out")
	}
	b.Mu.Lock()
	defer b.Mu.Unlock()
	for _, r := range b.Requests {
		if strings.Contains(r, "/wol") {
			t.Errorf("sent: %s", r)
		}
	}
}

// Review of fix13, must-fix 1 (the reviewer's proof, as a test): two standalone Proxmox hosts whose node is
// called pve. Host 1 has the agent (node: pve) and no wol:; host 2 has its wol: under site2-node-pve. Both are
// off when the panel starts. On fix13's first rule ("the one candidate that has a systems: entry") host 1's
// row offered Power on and sent host 2's MAC. Now the configuration does not say which node is the agent's
// (its id begins with neither id_prefix): host 1 has no wol:, nothing is sent, and the start says what to add.
func TestPowerOnOfAHostNeverSendsTheMACOfAnotherHostWithTheSameNodeName(t *testing.T) {
	b := testutil.NewBackend()
	b.PVEOf = func(string) ([]jsonx.M, error) { return nil, devices.Errf("connection failed") }
	b.Metrics["relay"] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
	p, err := poller.New(testutil.SameNameConfig(t), config.Secrets{}, b)
	if err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	if id, _, ok := p.NodeOf("host1-agent"); ok {
		t.Fatalf("the configuration is taken to name %s as host 1's node", id)
	}
	host := rowOf(t, p, "host1-agent")
	if host.Kind != "agent" || host.Status != "offline" {
		t.Fatalf("the fixture: %s %s", host.Kind, host.Status)
	}
	for _, a := range host.PowerActions {
		if a == "wake" {
			t.Errorf("host 1's row lists wake: %v", host.PowerActions)
		}
	}
	if c := capOf(p, host, "wake"); c.Available || c.Via != nil || !strings.Contains(model.Deref(c.Reason), "no wol: configured for this host") {
		t.Errorf("wake on host 1's row: %+v (reason %q)", c, model.Deref(c.Reason))
	}
	if _, err := Execute(context.Background(), p, host, Request{Action: "wake"}, "Ann"); err == nil {
		t.Error("a wake of host 1 was carried out")
	}
	b.Mu.Lock()
	reqs := strings.Join(b.Requests, "\n")
	b.Mu.Unlock()
	if strings.Contains(reqs, "/wol") || strings.Contains(reqs, "00:00:5e:00:53:72") {
		t.Errorf("WRONG MACHINE: Power on of host 1 sent a wake packet:\n%s", reqs)
	}
	w := p.NodeLinkWarnings()
	if len(w) != 1 || !strings.Contains(w[0], "agent host1-agent (node: pve)") || !strings.Contains(w[0], "systems: has an entry for site2-node-pve") ||
		!strings.Contains(w[0], "write links_to: <the id of its node> in the agent's entry") {
		t.Errorf("the warning at the start: %q", w)
	}
}
