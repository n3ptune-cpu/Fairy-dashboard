package web

import (
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// Recheck of fix7r, RC-01, end to end with the real poller, powerctl and run: a Proxmox node in a run, and
// the cluster's row for it.
//
// The run's shutdown of the node is accepted and its countdown is seen. 19 s before the countdown's end the
// link to the node drops and stays down, and the shutdown does not happen on the node. The agent's polls get
// nothing from then on. But the panel still holds the cluster's row "online" for the node (what it last read
// from the Proxmox API; a cluster takes its time to notice that a node dropped out), until 5 s after the due
// time. The run took that row for the node's own sign of life: every look cleared "out of reach before the
// end", and the poller counted the silence from when the row turned. Two polls and 20 s later: "switched off
// again". The node was on.

type clusterRowCase struct {
	name string
	// drop: the link to the node goes down this long before the countdown's end, and stays down (0: it stays up)
	drop time.Duration
	// goesOff: the shutdown happens on the node at the end of its countdown
	goesOff bool
	// rowTurns: the cluster's row for the node says "online" until this long after the countdown's end, then
	// "offline" (negative: it never turns)
	rowTurns time.Duration
	want     string
}

const (
	rowNotConfirmed = "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and asked to switch off. " + cutNotConfirmed
	rowStillOn      = "It still shows as on 5 min after its shutdown was due (the Proxmox API shows it as online), and its agent does not answer: the shutdown did not finish. It is ON. " +
		"It was updated to 17-bbbbbbb."
)

// clusterRowRun runs one case and returns the node in the ended run, how long after the countdown's end the
// run ended it, and the node's status in the panel once the link is back.
func clusterRowRun(t *testing.T, c clusterRowCase) (*wakerun.Host, time.Duration, string) {
	t.Helper()
	e := setup(t)
	f := newFarm(t, e)
	p := e.s.Poller
	var due time.Time
	nodeUp, linkDown := false, false
	e.b.Mu.Lock()
	e.b.PVE = func() ([]jsonx.M, error) {
		rows := testutil.PVEResources()
		rows[2]["status"] = "stopped" // no guest runs on the node
		if !nodeUp || (c.rowTurns >= 0 && !due.IsZero() && !f.now.Before(due.Add(c.rowTurns))) {
			rows[0]["status"] = "offline"
		}
		return rows, nil
	}
	e.b.Mu.Unlock()
	if err := p.AddAgent(config.Agent{ID: "pve-agent", Name: "PVE agent", LinksTo: "node-srv-pvea", Managed: true, Token: "read-word", ControlToken: "control-word",
		URL: "http://pve-agent", PowerControl: true, SelfUpdate: true},
		&config.SystemOverride{ID: "node-srv-pvea", Name: "Big server", Wol: &config.Wol{MAC: "00:00:5e:00:53:09", Relay: "nas"}}); err != nil {
		t.Fatal(err)
	}
	f.version["pve-agent"] = "16-aaaaaaa"
	e.b.Mu.Lock()
	e.b.Metrics["pve-agent"] = func() (jsonx.M, error) {
		if at, ok := f.pending["pve-agent"]; ok {
			due = at
		}
		if linkDown || (c.drop > 0 && !due.IsZero() && !f.now.Before(due.Add(-c.drop))) {
			linkDown = true
			return nil, devices.Errf("connection failed") // the link is down, and stays down
		}
		return f.metrics("pve-agent")
	}
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
		t.Fatal(c.name, err)
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
	var run *wakerun.Run
	for i := 0; i < 3600 && run == nil; i++ {
		f.tick()
		e.b.Mu.Lock()
		if !c.goesOff && !due.IsZero() && !f.now.Before(due) {
			f.on["pve-agent"] = true // the shutdown did not happen: the node stays up
		}
		e.b.Mu.Unlock()
		if a, hist := e.s.WakeRuns.Snapshot(); a == nil && len(hist) > 0 {
			run = hist[0]
		}
	}
	if run == nil {
		t.Fatal(c.name, "the run did not end")
	}
	h := hostOf(run, "node-srv-pvea")
	if h.ShutdownAt == nil || due.IsZero() {
		t.Fatalf("%s: the run did not get to its shutdown: [%s] %s", c.name, h.State, h.Text)
	}
	after := f.now.Sub(due)
	// the link comes back, and the cluster sees the node as it is: is it on?
	e.b.Mu.Lock()
	linkDown = false
	on := f.on["pve-agent"]
	e.b.Metrics["pve-agent"] = func() (jsonx.M, error) { return f.metrics("pve-agent") }
	e.b.PVE = func() ([]jsonx.M, error) {
		rows := testutil.PVEResources()
		rows[2]["status"] = "stopped"
		if !on {
			rows[0]["status"] = "offline"
		}
		return rows, nil
	}
	e.b.Mu.Unlock()
	for i := 0; i < 60; i++ {
		f.tick()
		e.b.Mu.Lock()
		f.on["pve-agent"] = on
		e.b.Mu.Unlock()
	}
	status := ""
	for _, x := range p.Snapshot() {
		if x.ID == "node-srv-pvea" {
			status = x.Status
		}
	}
	return h, after, status
}

func TestWakeUpdateAStaleClusterRowIsNotTheNodesSignOfLife(t *testing.T) {
	for _, c := range []clusterRowCase{
		// the reviewer's sequence
		{name: "the link drops 19 s before the end, the shutdown does not happen, the row turns 5 s after the end", drop: 19 * time.Second, rowTurns: 5 * time.Second, want: rowNotConfirmed},
		{name: "the same, the row turns 60 s after the end", drop: 19 * time.Second, rowTurns: 60 * time.Second, want: rowNotConfirmed},
		{name: "the same, the row turns in the second of the end", drop: 19 * time.Second, rowTurns: 0, want: rowNotConfirmed},
		// the run cannot tell this one from the first, and says so (it erred to "off" by luck before)
		{name: "the link drops 19 s before the end, the node does go off, the row turns 5 s after the end", drop: 19 * time.Second, goesOff: true, rowTurns: 5 * time.Second, want: rowNotConfirmed},
	} {
		h, after, status := clusterRowRun(t, c)
		if h.On == wakerun.OnNo && status == "online" {
			t.Errorf("%s: REPORTED OFF, AND IT IS ON (%v after the countdown's end): %s", c.name, after, h.Text)
		}
		if h.State != wakerun.StLeftOn || h.On != wakerun.OnUnknown || h.Text != c.want {
			t.Errorf("%s: [%s] power %q, %v after the countdown's end, the node is %q afterwards: %s", c.name, h.State, h.On, after, status, h.Text)
		}
		if strings.Contains(h.Text, "switched off again") || strings.Contains(h.Text, "It is off") {
			t.Errorf("%s: the run says that the node is off: %s", c.name, h.Text)
		}
	}
}

// What must stay (these hold before and after the fix): a node that goes off at the end of an undisturbed
// countdown is "switched off again", also when the cluster's row turns some seconds later; and while the row
// says "online" the node is ON for the run, whatever its agent's polls get.
func TestWakeUpdateANodeThatGoesOffIsConfirmedWhenTheClusterRowTurnsLater(t *testing.T) {
	for _, c := range []clusterRowCase{
		{name: "it goes off, the row turns in the same second", goesOff: true, rowTurns: 0, want: cutOff},
		{name: "it goes off, the row turns 8 s later", goesOff: true, rowTurns: 8 * time.Second, want: cutOff},
		{name: "it goes off, the row turns 90 s later", goesOff: true, rowTurns: 90 * time.Second, want: cutOff},
	} {
		h, after, status := clusterRowRun(t, c)
		if h.State != wakerun.StOff || h.On != wakerun.OnNo || h.Text != c.want || status == "online" {
			t.Errorf("%s: [%s] power %q, %v after the countdown's end, the node is %q afterwards: %s", c.name, h.State, h.On, after, status, h.Text)
		}
		// two polls after the row turned, the first of them 20 s old: no sooner, and not much later
		if min, max := c.rowTurns+20*time.Second, c.rowTurns+32*time.Second; after < min || after > max {
			t.Errorf("%s: confirmed %v after the countdown's end, want between %v and %v", c.name, after, min, max)
		}
	}
	for _, c := range []clusterRowCase{
		{name: "the link drops 19 s before the end, the shutdown does not happen, the row goes on saying online", drop: 19 * time.Second, rowTurns: -1, want: rowStillOn},
		{name: "it goes off, and the row goes on saying online", goesOff: true, rowTurns: -1, want: rowStillOn},
	} {
		h, after, status := clusterRowRun(t, c)
		if h.State != wakerun.StLeftOn || h.On != wakerun.OnYes || h.Text != c.want {
			t.Errorf("%s: [%s] power %q, %v after the countdown's end, the node is %q afterwards: %s", c.name, h.State, h.On, after, status, h.Text)
		}
	}
}
