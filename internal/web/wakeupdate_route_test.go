package web

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// r1-p2-05 (audit 5; suspected by reading, proven by this test on the code before the fix: "a shutdown went
// to the Proxmox API: POST /nodes/SRV-PVEA/status command=shutdown").
//
// A run's shutdown is the agent's: it counts down and can be cancelled. The run's Shutdown asked powerctl to
// "shut down", let it pick the route, and looked at the route afterwards. For a Proxmox node whose cluster has
// node_power: true, powerctl falls back to the Proxmox API when the node's agent has no power control (any
// more: a Re-pair with other options during the run takes it away). That shutdown starts at once, has no
// countdown and cannot be cancelled, and the run then reported the node as "Left ON".
//
// Now the route is part of the request (powerctl.Request.OnlyVia): powerctl refuses before anything is sent
// when the route it would take is another one, and the run leaves the machine on with one fixed sentence.
func TestWakeUpdateShutdownNeverFallsBackToTheProxmoxAPI(t *testing.T) {
	e := noGrantSetup(t, true, "", true) // node_power: true, and the node SRV-PVEA has an agent of its own with power control
	ops := runOps{e.s}
	ctx := context.Background()
	calls := func() (agent, pve string) {
		e.b.Mu.Lock()
		defer e.b.Mu.Unlock()
		return strings.Join(e.b.Requests, "; "), strings.Join(e.b.PVECalls, "; ")
	}
	reset := func() {
		e.b.Mu.Lock()
		e.b.Requests, e.b.PVECalls = nil, nil
		e.b.Mu.Unlock()
	}
	e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
		if path == "/power" {
			return 200, jsonx.M{"action": "shutdown", "pending": jsonx.M{"action": "shutdown", "at": float64(time.Now().Add(120 * time.Second).Unix()), "delay_s": 120.0, "requested_by": "x"}}, nil
		}
		return 0, nil, nil
	}
	// the agent route, as the run confirmed it at its start: the shutdown goes to the node's own agent
	reset()
	at, err := ops.Shutdown(ctx, "node-srv-pvea", 120*time.Second, "Alice", nil)
	agent, pve := calls()
	if err != nil || at.IsZero() || !strings.Contains(agent, "own POST /power") || strings.Contains(pve, "command=") {
		t.Fatalf("with the agent route: %v, at %v; agent: %s; Proxmox: %s", err, at, agent, pve)
	}
	// during the run the agent loses power control (a Re-pair with other options): the agent route is gone, and
	// the cluster would take a node shutdown
	a, _ := e.s.Poller.AgentByID("own")
	e.s.Poller.RemoveAgent("own")
	if err := e.s.Poller.AddAgent(config.Agent{ID: "own", URL: a.URL, Node: a.Node, Managed: true, Token: testutil.ReadToken, ControlToken: testutil.ControlToken,
		PowerControl: false, SelfUpdate: true}, nil); err != nil {
		t.Fatal(err)
	}
	e.s.Poller.PollOnce(ctx)
	e.s.Poller.PollOnce(ctx)
	reset()
	at, err = ops.Shutdown(ctx, "node-srv-pvea", 120*time.Second, "Alice", nil)
	agent, pve = calls()
	if strings.Contains(pve, "command=shutdown") || strings.Contains(pve, "/status") {
		t.Fatalf("a shutdown went to the Proxmox API: %s", pve)
	}
	if strings.Contains(agent, "/power") {
		t.Fatalf("a power request went to an agent without power control: %s", agent)
	}
	var oe *wakerun.OpError
	if err == nil || !at.IsZero() || !errors.As(err, &oe) || oe.Code != wakerun.CodeNoAgentRoute {
		t.Fatalf("the run must be told that the agent route is gone: %v (at %v)", err, at)
	}
	// the same request from the power menu still takes the Proxmox route: that is an admin's own click, with
	// the typed name, outside any run
	admin := append([]string{"Cookie", e.cookie(controller(), 0)}, "X-Requested-With", "fairy")
	e.s.control = newRateLimiter(1000)
	reset()
	pw := e.do("POST", "/api/systems/node-srv-pvea/power", map[string]any{"action": "shutdown", "delay_s": 60, "confirm": "SRV-PVEA"}, admin...)
	if _, pve := calls(); pw.code != 200 || pw.body["via"] != "proxmox-api" || !strings.Contains(pve, "command=shutdown") {
		t.Fatalf("the power menu: %d via=%v; Proxmox: %s", pw.code, pw.body["via"], pve)
	}
	// the route cannot be set from outside: a client that sends the field gets the strict body's refusal
	if r := e.do("POST", "/api/systems/node-srv-pvea/power", map[string]any{"action": "shutdown", "confirm": "SRV-PVEA", "only_via": "agent"}, admin...); r.code != 422 && r.code != 400 {
		t.Fatalf("only_via in a request body: %d %s", r.code, r.raw)
	}
}
