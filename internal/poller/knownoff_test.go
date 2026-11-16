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

// The mark has an age (KnownOffWithin), for whoever takes it as a sign that the machine is off right now: the
// switching of a plug's socket. The mark itself does not end with time: the row stays "offline" and KnownOff
// stays true until the machine gives a sign of life; and seeing the machine go off another time is a new mark.
func TestTheMarkOfAMachineSeenToGoOffHasAnAge(t *testing.T) {
	e := newSeenEnv(t, nil, seenStart)
	e.set(true)
	e.poll(0)
	if e.p.KnownOffWithin("laptop", time.Hour) {
		t.Fatal("a machine that answers counts as seen to go off")
	}
	e.set(false)
	e.poll(5 * time.Second)
	if !e.p.MarkOff("laptop") {
		t.Fatal("the fixture: not marked")
	}
	for _, c := range []struct {
		after  time.Duration // since the mark
		within time.Duration
		want   bool
	}{
		{0, time.Hour, true}, {59 * time.Minute, time.Hour, true}, {time.Hour, time.Hour, true}, {time.Hour + time.Second, time.Hour, false},
		{26 * time.Hour, time.Hour, false}, {26 * time.Hour, 0, false}, {26 * time.Hour, 27 * time.Hour, true},
	} {
		e.b.Mu.Lock()
		e.now = seenStart.Add(5*time.Second + c.after)
		e.b.Mu.Unlock()
		if got := e.p.KnownOffWithin("laptop", c.within); got != c.want {
			t.Errorf("%v after the mark, asked for %v: %v, want %v", c.after, c.within, got, c.want)
		}
		if !e.p.KnownOff("laptop") || snap(e.p)["laptop"].Status != "offline" {
			t.Errorf("%v after the mark: the mark is gone, or the row is %s", c.after, snap(e.p)["laptop"].Status)
		}
	}
	// a poll without an answer changes nothing; the machine is seen to go off again: the age starts again
	e.poll(0)
	if e.p.KnownOffWithin("laptop", time.Hour) || !e.p.KnownOff("laptop") {
		t.Error("a poll without an answer changed the mark")
	}
	e.p.MarkOff("laptop")
	if !e.p.KnownOffWithin("laptop", time.Hour) {
		t.Error("marked again, and the mark is old")
	}
	// an answer ends the mark, whatever its age; and ids without a mark have none
	e.set(true)
	e.poll(time.Second)
	if e.p.KnownOffWithin("laptop", time.Hour) || e.p.KnownOff("laptop") || e.p.KnownOffWithin("nobody", time.Hour) {
		t.Error("the mark of a machine that answers, or of an id without one")
	}
}
