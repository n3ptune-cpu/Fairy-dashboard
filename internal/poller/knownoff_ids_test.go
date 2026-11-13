package poller

import (
	"context"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// offEnv is the two standalone hosts with switches for what answers of PVE-B: its agent, and its Proxmox API
// with the node shown as nodeStatus ("": the API does not answer).
type offEnv struct {
	p          *Poller
	b          *testutil.Backend
	now        time.Time
	agentOn    bool
	nodeStatus string
}

func newOffEnv(t *testing.T) *offEnv { return newOffEnvWith(t, nil) }

// newOffEnvWith lets prepare change the configuration, or write into its data folder, before the poller is made.
func newOffEnvWith(t *testing.T, prepare func(cfg *config.Panel)) *offEnv {
	t.Helper()
	e := &offEnv{b: testutil.NewBackend(), now: seenStart, agentOn: true, nodeStatus: "online"}
	e.b.PVEOf = func(cluster string) ([]jsonx.M, error) {
		switch {
		case cluster == "lab-a":
			return testutil.HostResources("PVE-A", "stopped"), nil
		case e.nodeStatus != "":
			r := testutil.HostResources("PVE-B", "stopped")
			r[0]["status"] = e.nodeStatus
			return r, nil
		}
		return nil, devices.Errf("connection failed")
	}
	e.b.Metrics["lab-a-agent"] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
	e.b.Metrics["lab-b-agent"] = func() (jsonx.M, error) {
		if e.agentOn {
			return testutil.AgentMetrics(), nil
		}
		return nil, devices.Errf("connection failed")
	}
	cfg := testutil.TwoHostsConfig(t, "")
	if prepare != nil {
		prepare(cfg)
	}
	p, err := New(cfg, config.Secrets{}, e.b)
	if err != nil {
		t.Fatal(err)
	}
	p.Clock = func() time.Time { return e.now }
	e.p = p
	return e
}

// set changes what answers, under the backend's lock (the answers are read under it).
func (e *offEnv) set(agentOn bool, nodeStatus string) {
	e.b.Mu.Lock()
	e.agentOn, e.nodeStatus = agentOn, nodeStatus
	e.b.Mu.Unlock()
}

func (e *offEnv) poll(d time.Duration) {
	e.b.Mu.Lock()
	e.now = e.now.Add(d)
	e.b.Mu.Unlock()
	e.p.PollOnce(context.Background())
}

func (e *offEnv) marks() (node, agent bool) {
	e.p.mu.Lock()
	defer e.p.mu.Unlock()
	return e.p.knownOff["lab-b-node-pve-b"], e.p.knownOff["lab-b-agent"]
}

// The mark is the machine's, under both of its ids, and any sign of life ends it under both (review of build
// 14, F8). A run saw PVE-B go off and marked it; the machine is switched on again, its agent answers first and
// its Proxmox API (pveproxy) some polls later. Until this the agent's answer ended the mark of the agent's own
// entry only: the row the page shows is the node's, and it said "offline" for a machine that answered.
func TestAnAnswerOfTheAgentEndsTheMarkOfItsNodeToo(t *testing.T) {
	e := newOffEnv(t)
	e.poll(0)
	e.set(false, "")
	e.poll(5 * time.Second)
	e.poll(5 * time.Second)
	before, _, _ := e.p.AgentSeen("lab-b-agent")
	if !e.p.MarkOff("lab-b-node-pve-b") {
		t.Fatal("not marked")
	}
	if n, a := e.marks(); !n || !a {
		t.Fatalf("the fixture: marks node %v, agent %v", n, a)
	}
	// the mark does not move the clock that "off for 15 minutes" is counted with
	after, _, _ := e.p.AgentSeen("lab-b-agent")
	if !before.QuietSince.Equal(after.QuietSince) || before.Silent != after.Silent {
		t.Fatalf("MarkOff moved the count of the machine's silence: %v %+v -> %v %+v", before.QuietSince, before.Silent, after.QuietSince, after.Silent)
	}
	e.set(true, "") // the agent is back, the node's API is not
	e.poll(5 * time.Second)
	if n, a := e.marks(); n || a {
		t.Fatalf("after the agent's answer: marks node %v, agent %v", n, a)
	}
	// the row is what it is without the mark: its node's API has been silent for 15 s, less than offline_after
	if got := snap(e.p)["lab-b-node-pve-b"].Status; got != "stale" {
		t.Fatalf("the machine's row, 15 s after its node's last answer and with its agent answering: %s", got)
	}
}

// The other way round: the node's Proxmox API shows the node online while the agent has not answered yet.
// That is a sign of life too, and it ends the mark of the agent's entry. A cluster that lists the node as
// OFFLINE is none: the agent's mark stays (the machine is off, and its cluster says so).
func TestANodeThatItsClusterShowsOnlineEndsTheMarkOfItsAgent(t *testing.T) {
	for status, wantAgentMark := range map[string]bool{"online": false, "offline": true} {
		e := newOffEnv(t)
		e.poll(0)
		e.set(false, "")
		e.poll(5 * time.Second)
		e.poll(5 * time.Second)
		if !e.p.MarkOff("lab-b-agent") {
			t.Fatal("not marked")
		}
		e.set(false, status)
		e.poll(5 * time.Second)
		if n, a := e.marks(); n || a != wantAgentMark {
			t.Errorf("the node's API answers with the node %s: marks node %v, agent %v (want agent %v)", status, n, a, wantAgentMark)
		}
	}
}

// MarkOff only ever turns "stale" into "offline" for a machine that gives nothing.
//
// A node its cluster shows as online is never marked, whatever its agent does (a cluster's other nodes answer
// for it; the agent may just have been stopped): the node's row stays "online", and nothing is kept that
// would make it "offline" at the next look.
func TestMarkOffNeverMarksANodeItsClusterShowsOnline(t *testing.T) {
	e := newOffEnv(t)
	e.poll(0)
	e.set(false, "online") // the agent stops answering, the node is still shown as online
	e.poll(5 * time.Second)
	e.poll(5 * time.Second)
	before, _, _ := e.p.AgentSeen("lab-b-agent")
	e.p.MarkOff("lab-b-node-pve-b")
	if n, _ := e.marks(); n {
		t.Fatal("a node that its cluster shows as online was marked")
	}
	if got := snap(e.p)["lab-b-node-pve-b"].Status; got != "online" {
		t.Fatalf("the node's row after MarkOff: %s", got)
	}
	after, _, _ := e.p.AgentSeen("lab-b-agent")
	if !before.QuietSince.Equal(after.QuietSince) || before.Silent != after.Silent {
		t.Fatal("MarkOff moved the count of the agent's silence")
	}
}

// And nothing is marked while the machine's agent answers: with the agent on and only the node's Proxmox API
// silent (pveproxy restarts, the API token was taken away), the node's entry is "stale", and it is not this
// function's to make it "offline": the machine runs.
func TestMarkOffMarksNothingWhileTheAgentAnswers(t *testing.T) {
	e := newOffEnv(t)
	e.poll(0)
	e.set(true, "") // the agent answers, the node's API does not
	e.poll(5 * time.Second)
	e.poll(5 * time.Second)
	e.p.mu.Lock()
	own := e.p.systems["lab-b-node-pve-b"].Status
	e.p.mu.Unlock()
	if own != "stale" {
		t.Fatalf("the fixture: the node's own entry is %q", own)
	}
	for _, id := range []string{"lab-b-node-pve-b", "lab-b-agent"} {
		if e.p.MarkOff(id) {
			t.Errorf("MarkOff(%s) changed a row of a machine whose agent answers", id)
		}
	}
	if n, a := e.marks(); n || a {
		t.Fatalf("marks node %v, agent %v", n, a)
	}
	e.p.mu.Lock()
	own = e.p.systems["lab-b-node-pve-b"].Status
	e.p.mu.Unlock()
	if own != "stale" {
		t.Fatalf("the node's own entry after MarkOff: %q", own)
	}
}
