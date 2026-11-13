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

// A machine that a run saw go off is "offline" at once, not "stale" for the rest of offline_after (MarkOff).
// It never contradicts an answer: nothing is marked for a machine that answers, an answer ends it, and the
// next time the machine stops answering it is "stale" first, as every machine.
func TestMarkOffMakesAStaleMachineOffline(t *testing.T) {
	e := newSeenEnv(t, nil, seenStart)
	status := func() string { return snap(e.p)["laptop"].Status }
	e.set(true)
	e.poll(0)
	if e.p.MarkOff("laptop") || status() != "online" {
		t.Fatalf("a machine that answers was marked: %s", status())
	}
	e.set(false)
	e.poll(5 * time.Second)
	e.poll(5 * time.Second)
	if status() != "stale" {
		t.Fatalf("10 s after its last answer (offline_after is %v s): %s", e.cfg.OfflineAfter, status())
	}
	if !e.p.MarkOff("laptop") || status() != "offline" {
		t.Fatalf("after MarkOff: %s", status())
	}
	if s := snap(e.p)["laptop"]; s.CPUPercent != nil || s.PowerPending != nil || s.LastSeen == nil {
		t.Fatalf("the row of a machine that is off: %+v", s)
	}
	if e.p.MarkOff("laptop") { // again: nothing changes
		t.Fatal("MarkOff twice changed the row twice")
	}
	for i := 0; i < 4; i++ { // it stays offline through the polls that used to say "stale"
		e.poll(5 * time.Second)
		if status() != "offline" {
			t.Fatalf("%d s after its last answer: %s", 15+5*i, status())
		}
	}
	// it answers again: online, and the mark is gone with the answer
	e.set(true)
	e.poll(5 * time.Second)
	if status() != "online" {
		t.Fatalf("back on: %s", status())
	}
	e.set(false)
	e.poll(5 * time.Second)
	if status() != "stale" {
		t.Fatalf("it stops answering another time, and nobody saw it go off: %s", status())
	}
	e.poll(time.Duration(e.cfg.OfflineAfter) * time.Second)
	if status() != "offline" {
		t.Fatalf("after offline_after: %s", status())
	}
	// ids that are no machine with an agent
	if e.p.MarkOff("nobody") || e.p.MarkOff("qemu-100") {
		t.Fatal("MarkOff for an id without an agent")
	}
}

// A standalone Proxmox host that is off has no API that answers: its node's row is "stale" like its agent's.
// MarkOff, under either of the machine's ids, makes the row the page shows "offline"; the node's API
// answering again ends it. A node its cluster shows as online is never marked.
func TestMarkOffIsForTheRowOfTheNodeToo(t *testing.T) {
	for _, id := range []string{"lab-b-node-pve-b", "lab-b-agent"} {
		t.Run(id, func(t *testing.T) {
			cfg := testutil.TwoHostsConfig(t, "")
			b := testutil.NewBackend()
			on := true
			b.PVEOf = func(cluster string) ([]jsonx.M, error) {
				switch {
				case cluster == "lab-a":
					return testutil.HostResources("PVE-A", "stopped"), nil
				case on:
					return testutil.HostResources("PVE-B", "stopped"), nil
				}
				return nil, devices.Errf("connection failed")
			}
			b.Metrics["lab-a-agent"] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
			b.Metrics["lab-b-agent"] = func() (jsonx.M, error) {
				if on {
					return testutil.AgentMetrics(), nil
				}
				return nil, devices.Errf("connection failed")
			}
			p, err := New(cfg, config.Secrets{}, b)
			if err != nil {
				t.Fatal(err)
			}
			now := seenStart
			p.Clock = func() time.Time { return now }
			poll := func(d time.Duration) string {
				b.Mu.Lock()
				now = now.Add(d)
				b.Mu.Unlock()
				p.PollOnce(context.Background())
				return snap(p)["lab-b-node-pve-b"].Status
			}
			if got := poll(0); got != "online" {
				t.Fatalf("the fixture: PVE-B is on, shown as its node: %q (%v)", got, snap(p))
			}
			if p.MarkOff(id) || p.MarkOff("lab-a-agent") {
				t.Fatal("a machine that is on was marked")
			}
			b.Mu.Lock()
			on = false
			b.Mu.Unlock()
			poll(5 * time.Second)
			if got := poll(5 * time.Second); got != "stale" {
				t.Fatalf("10 s after it went off: %q", got)
			}
			if !p.MarkOff(id) || snap(p)["lab-b-node-pve-b"].Status != "offline" {
				t.Fatalf("after MarkOff(%s): %q", id, snap(p)["lab-b-node-pve-b"].Status)
			}
			if _, own := snap(p)["lab-b-agent"]; own {
				t.Fatal("the fixture: the agent has a row of its own")
			}
			if got := poll(5 * time.Second); got != "offline" {
				t.Fatalf("the next poll: %q", got)
			}
			if s := snap(p)["lab-a-node-pve-a"]; s.Status != "online" { // the other host is not touched
				t.Fatalf("PVE-A: %s", s.Status)
			}
			b.Mu.Lock()
			on = true
			b.Mu.Unlock()
			if got := poll(5 * time.Second); got != "online" {
				t.Fatalf("back on: %q", got)
			}
			b.Mu.Lock()
			on = false
			b.Mu.Unlock()
			if got := poll(5 * time.Second); got != "stale" {
				t.Fatalf("off another time, unseen: %q", got)
			}
		})
	}
}
