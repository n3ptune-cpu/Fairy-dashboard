package poller

import (
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Second review of build 14, R7. The mark "a run saw it go off" is the machine's under both of its ids, the
// agent's and its node's. But the node of ANY agent with that link was marked, and unmarked by any such
// agent's answer. A second agent that names the node (here lab-b-guest, an agent in a guest that was given its
// host's node:) has a row of its own since a9aa8ad: it is not the node's machine.
func TestTheMarkOfANodeIsItsOwnAgentsOnly(t *testing.T) {
	guestOn := true
	var e *offEnv
	e = newOffEnvWith(t, func(cfg *config.Panel) {
		cfg.Agents = append(cfg.Agents, config.Agent{ID: "lab-b-guest", Name: "Guest", URL: "http://lab-b-guest:9101", Node: "PVE-B", Token: testutil.ReadToken})
	})
	e.b.Metrics["lab-b-guest"] = func() (jsonx.M, error) {
		if guestOn {
			return testutil.AgentMetrics(), nil
		}
		return nil, devices.Errf("connection failed")
	}
	guest := func(on bool) {
		e.b.Mu.Lock()
		guestOn = on
		e.b.Mu.Unlock()
	}
	node := func() (status string, marked bool) {
		m, _ := e.marks()
		return snap(e.p)["lab-b-node-pve-b"].Status, m
	}
	e.poll(0)
	if a, ok := e.p.AgentFor("lab-b-node-pve-b"); !ok || a.ID != "lab-b-agent" || e.p.ShownSystem("lab-b-guest").ID != "lab-b-guest" {
		t.Fatalf("the fixture: the node's row is lab-b-agent's (%q), and lab-b-guest has its own", a.ID)
	}

	// The guest was switched off by a run, and one poll of the node's Proxmox API fails at that moment (the
	// node's row is "stale"); the host's own agent answers. The run marks the guest: the node is not marked.
	guest(false)
	e.set(true, "")
	e.poll(5 * time.Second)
	e.poll(5 * time.Second)
	e.p.MarkOff("lab-b-guest")
	if status, marked := node(); marked || status != "stale" {
		t.Errorf("the mark of the guest's agent went to the node its entry names: the node's row is %q (marked %v), want stale: its own agent answers", status, marked)
	}
	if got := snap(e.p)["lab-b-guest"].Status; got != "offline" {
		t.Errorf("the guest's own row after the mark: %s", got)
	}

	// The host itself goes off and a run marks it. An answer of the guest's agent is not the node's sign of
	// life (the entry may name the node wrongly: the agent runs elsewhere): the node's row stays "offline".
	e.set(false, "")
	e.poll(5 * time.Second)
	e.poll(5 * time.Second)
	if !e.p.MarkOff("lab-b-node-pve-b") {
		t.Fatal("the node was not marked")
	}
	if status, marked := node(); !marked || status != "offline" {
		t.Fatalf("the fixture: the node's row after its own mark: %q (marked %v)", status, marked)
	}
	guest(true)
	e.poll(5 * time.Second)
	if status, marked := node(); !marked || status != "offline" {
		t.Errorf("an answer of the second agent ended the node's mark: the row is %q (marked %v), want offline until the node or its own agent shows a sign of life", status, marked)
	}
	// its own agent answers: that ends it, as before (F8)
	e.set(true, "")
	e.poll(5 * time.Second)
	if status, marked := node(); marked || status != "stale" {
		t.Errorf("after an answer of the node's own agent: the row is %q (marked %v), want stale and no mark", status, marked)
	}
}
