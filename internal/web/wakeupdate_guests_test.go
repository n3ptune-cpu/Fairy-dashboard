package web

import (
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// A Proxmox node in a run, with the real poller, powerctl and run manager (the second review's own set-up,
// reviews/panel-fixes-r2p): SRV-PVEA has an agent and Wake-on-LAN through the NAS, is off, and a run wakes it.
// pve is the cluster's answer at every poll while the node runs; h is the node in the run (nil before it has
// one). Before that the fixture's guest on the node is stopped.
func nodeRun(t *testing.T, pve func(h *wakerun.Host, rows []jsonx.M) ([]jsonx.M, error)) (*wakerun.Host, *farm) {
	t.Helper()
	e := setup(t)
	f := newFarm(t, e)
	p := e.s.Poller
	nodeUp := false
	var cur *wakerun.Host
	e.b.Mu.Lock()
	e.b.PVE = func() ([]jsonx.M, error) {
		rows := testutil.PVEResources()
		rows[2]["status"] = "stopped"
		if !nodeUp || !f.on["pve-agent"] { // not woken yet, or switched off again
			rows[0]["status"] = "offline"
			return rows, nil
		}
		return pve(cur, rows)
	}
	e.b.Mu.Unlock()
	if err := p.AddAgent(config.Agent{ID: "pve-agent", Name: "PVE agent", LinksTo: "node-srv-pvea", Managed: true, Token: "read-word", ControlToken: "control-word",
		URL: "http://pve-agent", PowerControl: true, SelfUpdate: true},
		&config.SystemOverride{ID: "node-srv-pvea", Name: "Big server", Wol: &config.Wol{MAC: "00:00:5e:00:53:09", Relay: "nas"}}); err != nil {
		t.Fatal(err)
	}
	f.version["pve-agent"] = "16-aaaaaaa"
	e.b.Mu.Lock()
	e.b.Metrics["pve-agent"] = func() (jsonx.M, error) { return f.metrics("pve-agent") }
	e.b.Mu.Unlock()
	f.offline()
	f.optIn("node-srv-pvea")
	e.b.Reply = func(agent, path string) (int, jsonx.M, error) { // the agent's answer to a shutdown: its countdown, by the farm's clock
		if path == "/power" {
			return 200, jsonx.M{"action": "shutdown", "pending": jsonx.M{"action": "shutdown", "at": float64(f.now.Add(120 * time.Second).Unix()), "delay_s": 120.0, "requested_by": "x"}}, nil
		}
		return 0, nil, nil
	}
	if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"node-srv-pvea"}, By: "Alice", BySub: "u2"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		if a, _ := e.s.WakeRuns.Snapshot(); a != nil && hostOf(a, "node-srv-pvea").WakeSentAt != nil {
			break
		}
		f.tick()
	}
	e.b.Mu.Lock() // the wake packet starts the node
	f.on["pve-agent"], f.booted["pve-agent"], f.ready["pve-agent"] = true, f.now.Add(5*time.Second), f.now.Add(20*time.Second)
	nodeUp = true
	e.b.Mu.Unlock()
	for i := 0; i < 3600; i++ {
		a, _ := e.s.WakeRuns.Snapshot()
		if a == nil {
			break
		}
		e.b.Mu.Lock()
		cur = hostOf(a, "node-srv-pvea")
		e.b.Mu.Unlock()
		f.tick()
	}
	_, hist := e.s.WakeRuns.Snapshot()
	return hostOf(hist[0], "node-srv-pvea"), f
}

func nodeRequests(f *farm) string {
	var out []string
	for _, r := range f.requests() {
		if strings.HasPrefix(r, "pve-agent ") {
			out = append(out, r)
		}
	}
	return strings.Join(out, ", ")
}

// The set-up itself: every guest of the node is stopped, so the node is updated and switched off again.
func TestWakeUpdateANodeWhoseGuestsAreStoppedIsSwitchedOffAgain(t *testing.T) {
	h, f := nodeRun(t, func(_ *wakerun.Host, rows []jsonx.M) ([]jsonx.M, error) { return rows, nil })
	if h.State != wakerun.StOff || h.On != wakerun.OnNo || f.on["pve-agent"] || nodeRequests(f) != "pve-agent /update, pve-agent /power" {
		t.Fatalf("%s/%s, on %v, requests %q: %s", h.State, h.On, f.on["pve-agent"], nodeRequests(f), h.Text)
	}
}

// Second review, note N2, with the real stack: a guest starts on the node during the node's shutdown
// countdown. The node went off under it. Now the run sees it in the Proxmox API's next answers and calls its
// countdown off.
func TestWakeUpdateAGuestThatStartsDuringTheNodesCountdown(t *testing.T) {
	h, f := nodeRun(t, func(h *wakerun.Host, rows []jsonx.M) ([]jsonx.M, error) {
		if h != nil && h.State == wakerun.StShutting {
			rows[2]["status"] = "running"
		}
		return rows, nil
	})
	want := "Left ON: 1 guest is running on it (win11) since its shutdown was asked for: its shutdown countdown was called off. It came online after the wake and it is ON. It was updated to 17-bbbbbbb."
	if h.State != wakerun.StLeftOn || h.On != wakerun.OnYes || h.Text != want || !f.on["pve-agent"] {
		t.Fatalf("%s/%s, on %v: %s", h.State, h.On, f.on["pve-agent"], h.Text)
	}
	if got := nodeRequests(f); got != "pve-agent /update, pve-agent /power, pve-agent /power" { // the shutdown, and its cancel
		t.Fatalf("requests: %s", got)
	}
	f.tick() // the farm acts on the requests of the run's last step
	if _, counting := f.pending["pve-agent"]; counting {
		t.Fatal("the node's countdown still runs")
	}
}
