package poller

import (
	"context"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Recheck of fix7r, RC-01: the agent of a Proxmox node, and the cluster's row for that node.
//
// The row is what the panel last read from the Proxmox API. While it says "online" the node is on for the
// panel: it is not "silent", and the quarter of an hour without an answer that makes a machine "off" for a run
// does not begin. That stays. But the row is not the node's own word, and it lags: a node whose link dropped
// is still listed as online for a while. "Since when has the panel had nothing at all from the machine"
// (SilentPolls.SinceAgo) is asked by the run to tell whether a machine was already out of reach before its
// shutdown countdown ended, and for that the row has no word: the first poll that got nothing is where it
// begins.

// clusterNode is a poller with the agent "pvea" on the Proxmox node SRV-PVEA. link, row and refuse are what the
// test sets (under the backend's lock): the link to the node, the cluster's row for it, and whether the node's
// address refuses the connection instead of giving nothing. at(s) sets the clock to s seconds and polls.
type clusterNode struct {
	p                 *Poller
	b                 *testutil.Backend
	link, row, refuse bool
	at                func(s int)
	start             time.Time
}

func newClusterNode(t *testing.T) *clusterNode {
	t.Helper()
	c := &clusterNode{b: testutil.NewBackend(), link: true, row: true, start: time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC)}
	var ck sync.Mutex
	now := c.start
	c.b.PVE = func() ([]jsonx.M, error) {
		rows := testutil.PVEResources()
		if !c.row {
			rows[0]["status"] = "offline"
		}
		return rows, nil
	}
	c.b.Metrics["pvea"] = func() (jsonx.M, error) {
		switch {
		case c.link:
			return testutil.AgentMetrics(), nil
		case c.refuse:
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
		}
		return nil, devices.Errf("connection failed")
	}
	cfg := testutil.Config(t, []string{"http://pve.invalid"}, "http://laptop.invalid", "http://tower.invalid")
	p, err := New(cfg, config.Secrets{}, c.b)
	if err != nil {
		t.Fatal(err)
	}
	p.Clock = func() time.Time {
		ck.Lock()
		defer ck.Unlock()
		return now
	}
	if err := p.AddAgent(config.Agent{ID: "pvea", Name: "pvea", URL: "http://pvea.invalid", Token: "read-word", Managed: true, Node: "SRV-PVEA"}, nil); err != nil {
		t.Fatal(err)
	}
	c.p = p
	c.at = func(s int) {
		ck.Lock()
		now = c.start.Add(time.Duration(s) * time.Second)
		ck.Unlock()
		p.PollOnce(context.Background())
	}
	return c
}

func (c *clusterNode) set(f func()) {
	c.b.Mu.Lock()
	f()
	c.b.Mu.Unlock()
}

// The reviewer's proof (reviews/fix7r-recheck, TestRecheckAStaleClusterRowMovesTheStartOfTheSilence). The agent
// gets nothing at all from 101 s on (the link to the node is down). The cluster still lists the node as online
// until 125 s. The silence was counted from the poll after the row turned: 130 s, after a countdown that could
// end at 120 s.
func TestAStaleClusterRowDoesNotMoveTheStartOfTheSilence(t *testing.T) {
	c := newClusterNode(t)
	c.at(0)
	c.at(100)
	if i, _, _ := c.p.AgentSeen("pvea"); !i.Online || i.Silent != (SilentPolls{}) {
		t.Fatalf("the node's agent must answer first: %+v", i)
	}
	c.set(func() { c.link = false }) // 101 s: the link to the node drops; the agent gets nothing at all from here on
	for s := 101; s <= 125; s += 4 {
		c.at(s)
		i, _, _ := c.p.AgentSeen("pvea")
		// no row while the cluster shows the node as online, and the beginning of "nothing" is said all the same
		if want := time.Duration(s-101) * time.Second; i.Silent.N != 0 || i.Silent.Row != 0 || i.Silent.SinceAgo != want {
			t.Fatalf("%d s, the cluster shows the node as online: %+v, want no row and \"nothing since %v ago\"", s, i.Silent, want)
		}
	}
	c.set(func() { c.row = false }) // 125 s: the panel's next read of the Proxmox API lists the node as offline
	for s := 126; s <= 150; s += 4 {
		c.at(s)
	}
	i, _, _ := c.p.AgentSeen("pvea")
	since := 150*time.Second - i.Silent.SinceAgo
	if i.Silent.N == 0 {
		t.Fatal("no row of silent polls")
	}
	if since != 101*time.Second {
		t.Errorf("the agent of a Proxmox node got nothing at all from 101 s on, and SinceAgo places the start of the silence at %v: "+
			"a countdown that could end at 120 s is then not \"out of reach before its end\", and two polls and 20 s later the run writes \"switched off again\"", since)
	}
	if i.Silent.SinceAgo < i.Silent.BeganAgo {
		t.Errorf("SinceAgo %v is shorter than BeganAgo %v", i.Silent.SinceAgo, i.Silent.BeganAgo)
	}
}

// What moves the start: the node's own word. A poll that the node's address refuses (it runs, its agent does
// not listen) ends "nothing at all", with the cluster's row saying either thing, and so does an answer.
func TestOnlyTheNodeItselfMovesTheStartOfTheSilence(t *testing.T) {
	for _, life := range []string{"a refused connection", "an answer"} {
		c := newClusterNode(t)
		c.at(0)
		c.set(func() { c.link = false })
		c.at(10)
		c.at(20)
		if i, _, _ := c.p.AgentSeen("pvea"); i.Silent.N != 0 || i.Silent.SinceAgo != 10*time.Second {
			t.Fatalf("%s: two polls without anything, the cluster shows the node as online: %+v", life, i.Silent)
		}
		c.set(func() { c.link, c.refuse = life == "an answer", true })
		c.at(30)
		if i, _, _ := c.p.AgentSeen("pvea"); i.Silent != (SilentPolls{}) {
			t.Fatalf("after %s: %+v", life, i.Silent)
		}
		c.set(func() { c.link, c.refuse = false, false })
		c.at(40)
		c.at(50)
		if i, _, _ := c.p.AgentSeen("pvea"); i.Silent.N != 0 || i.Silent.SinceAgo != 10*time.Second {
			t.Fatalf("after %s the silence begins with the next poll that gets nothing (40 s): %+v", life, i.Silent)
		}
		c.set(func() { c.row = false })
		c.at(60)
		c.at(70)
		if i, _, _ := c.p.AgentSeen("pvea"); i.Silent.N == 0 || i.Silent.SinceAgo != 30*time.Second {
			t.Fatalf("after %s, the row has turned: %+v, want nothing since 40 s", life, i.Silent)
		}
	}
}

// And what the cluster's row goes on deciding, exactly as before: whether the node is "silent" at all, and so
// the quarter of an hour that makes it "off" for a run. While the row says "online" there is no QuietSince,
// however long the agent's polls get nothing; the silence begins with the first poll after the row turned.
// (This test holds before and after the fix of RC-01.)
func TestTheClustersRowStillDecidesSinceWhenANodeIsSilent(t *testing.T) {
	c := newClusterNode(t)
	c.at(0)
	c.set(func() { c.link = false })
	for s := 10; s <= 1200; s += 10 { // twenty minutes without anything from its agent; the cluster shows it as online
		c.at(s)
		if i, _, _ := c.p.AgentSeen("pvea"); i.Online || !i.QuietSince.IsZero() || i.Sign != "" || i.Refused || i.Silent.N != 0 {
			t.Fatalf("%d s: a node that the cluster shows as online is not silent: %+v", s, i)
		}
	}
	c.set(func() { c.row = false })
	var began time.Time
	for s := 1210; s <= 1300; s += 10 {
		c.at(s)
		i, _, _ := c.p.AgentSeen("pvea")
		if began.IsZero() && !i.QuietSince.IsZero() {
			began = i.QuietSince
		}
		if s >= 1220 && (i.QuietSince.IsZero() || !i.QuietSince.Equal(began) || i.Silent.N == 0) {
			t.Fatalf("%d s: the row has turned, the node is silent since %v: %+v", s, began, i)
		}
	}
	// the agent's poll and the cluster's run side by side: the first poll after the row turned, or the one after it
	if d := began.Sub(c.start); d != 1210*time.Second && d != 1220*time.Second {
		t.Fatalf("the node's silence began %v after the start, want the first or second poll after the cluster showed it as offline (1210 s, 1220 s): "+
			"the twenty minutes before do not count", d)
	}
	// the row says "online" again: no silence any more
	c.set(func() { c.row = true })
	c.at(1310)
	c.at(1320)
	if i, _, _ := c.p.AgentSeen("pvea"); !i.QuietSince.IsZero() || i.Silent.N != 0 {
		t.Fatalf("the cluster shows the node as online again: %+v", i)
	}
}
