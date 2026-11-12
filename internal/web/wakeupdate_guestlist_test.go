package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/scheduler"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// Audit 6, r4-p2-01: a Proxmox node was switched off under a running guest when the Proxmox API is polled
// slowly. The guest list only had to be from after the node came online; nothing asked how old it was, and the
// run never asked the poller for a new one. Here the poller polls by its own schedule (Scheduler.Tick, as in
// Poller.Run: what is due, and what was asked for with Kick), with the Proxmox API at five minutes, the longest
// interval the panel takes. The run, the poller, powerctl and the scheduler are the real ones.

// slowAPINode is clockNode's node with a Proxmox API that says what is true whenever it is asked (the guest
// win11 runs from guestStarts on) and counts how often it is asked.
type slowAPINode struct {
	*clockNode
	guestStarts time.Time // zero: it never starts
	apiDown     bool
	asked       int
}

func newSlowAPINode(t *testing.T) *slowAPINode {
	t.Helper()
	n := &slowAPINode{clockNode: newClockNode(t, false)}
	e, f := n.e, n.f
	e.b.Mu.Lock()
	e.b.PVE = func() ([]jsonx.M, error) {
		n.asked++
		if n.apiDown {
			return nil, context.DeadlineExceeded
		}
		rows := testutil.PVEResources()
		rows[2]["status"] = "stopped"
		ready := f.ready["pve-agent"]
		switch {
		case !f.on["pve-agent"] || f.now.Before(ready.Add(-5*time.Second)):
			rows[0]["status"] = "offline"
		case !n.guestStarts.IsZero() && !f.now.Before(n.guestStarts):
			rows[2]["status"] = "running"
		}
		return rows, nil
	}
	e.b.Mu.Unlock()
	return n
}

// set changes what the API says, under the backend's lock.
func (n *slowAPINode) set(f func()) {
	n.e.b.Mu.Lock()
	f()
	n.e.b.Mu.Unlock()
}

// bySchedule: from here on the poller runs by its scheduler, on the farm's clock, and the Proxmox API's job has
// an interval of five minutes. Its first tick polls everything once (nothing was scheduled before).
func (n *slowAPINode) bySchedule() {
	p, f := n.e.s.Poller, n.f
	began := f.now
	p.Scheduler.Clock = func() float64 { return f.now.Sub(began).Seconds() }
	p.Scheduler.Add(&scheduler.Job{Key: n.job.Key, Fn: n.job.Fn, DefaultS: 300, TimeoutCap: n.job.TimeoutCap, Systems: n.job.Systems})
	f.poll = func() {
		p.Scheduler.Tick(context.Background())
		p.Scheduler.Wait()
		p.Scheduler.TakeDirty()
		p.Bump()
	}
}

// wakeAndComeUp starts a run for the node, lets the wake start it, and returns at the node's agent's first
// answer plus two seconds, in which the Proxmox API answered twice: the guest is stopped. From then on the
// poller runs by its schedule.
func (n *slowAPINode) wakeAndComeUp(t *testing.T) {
	t.Helper()
	e, f := n.e, n.f
	if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"node-srv-pvea"}, By: "Alice", BySub: "u2"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		if a, _ := e.s.WakeRuns.Snapshot(); a != nil && hostOf(a, "node-srv-pvea").WakeSentAt != nil {
			break
		}
		f.tick()
	}
	n.start()
	n.untilBeforeReady(0)
	f.tick()
	f.tick()
	n.bySchedule()
}

// end lets the run finish and returns the node's result.
func (n *slowAPINode) end(t *testing.T, each func(h *wakerun.Host)) *wakerun.Host {
	t.Helper()
	for i := 0; i < 3600; i++ {
		a, _ := n.e.s.WakeRuns.Snapshot()
		if a == nil {
			_, hist := n.e.s.WakeRuns.Snapshot()
			return hostOf(hist[0], "node-srv-pvea")
		}
		if each != nil {
			each(hostOf(a, "node-srv-pvea"))
		}
		n.f.tick()
	}
	t.Fatal("the run did not end in an hour")
	return nil
}

func TestWakeUpdateASlowlyPolledProxmoxAPIIsAskedAnewBeforeANodeIsSwitchedOff(t *testing.T) {
	t.Run("a guest starts 8 s after the last list: the node is left on", func(t *testing.T) {
		n := newSlowAPINode(t)
		n.wakeAndComeUp(t)
		n.set(func() { n.guestStarts = n.f.now.Add(8 * time.Second) })
		h := n.end(t, nil)
		if h.State != wakerun.StLeftOn || h.On != wakerun.OnYes || !n.f.on["pve-agent"] || !strings.HasPrefix(h.Text, "Left ON: 1 guest is running on it (win11).") {
			t.Errorf("%s/%s, the node on: %v: %s", h.State, h.On, n.f.on["pve-agent"], h.Text)
		}
		if got := nodeRequests(n.f); got != "pve-agent /update" {
			t.Errorf("requests to the node's agent: %s; want no shutdown", got)
		}
	})
	t.Run("no guest runs: the node is updated and switched off again, on lists the run asked for", func(t *testing.T) {
		n := newSlowAPINode(t)
		n.wakeAndComeUp(t)
		before, atShutdown := 0, 0
		h := n.end(t, func(h *wakerun.Host) {
			n.set(func() {
				switch {
				case h.State == wakerun.StSettling:
					before = n.asked
				case h.State == wakerun.StShutting && atShutdown == 0:
					atShutdown = n.asked
				}
			})
		})
		if h.State != wakerun.StOff || h.On != wakerun.OnNo || n.f.on["pve-agent"] || h.Text != "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again." {
			t.Errorf("%s/%s, the node on: %v: %s", h.State, h.On, n.f.on["pve-agent"], h.Text)
		}
		if got := nodeRequests(n.f); got != "pve-agent /update, pve-agent /power" {
			t.Errorf("requests to the node's agent: %s", got)
		}
		if atShutdown <= before {
			t.Errorf("the Proxmox API was not asked between the end of the settle time and the shutdown request (%d polls before, %d at the request)", before, atShutdown)
		}
	})
	t.Run("a guest starts 30 s into the countdown: the countdown is called off", func(t *testing.T) {
		n := newSlowAPINode(t)
		n.wakeAndComeUp(t)
		h := n.end(t, func(h *wakerun.Host) {
			if h.State == wakerun.StShutting && n.guestStarts.IsZero() {
				n.set(func() { n.guestStarts = n.f.now.Add(30 * time.Second) })
			}
		})
		want := "Left ON: 1 guest is running on it (win11) since its shutdown was asked for: its shutdown countdown was called off."
		if h.State != wakerun.StLeftOn || h.On != wakerun.OnYes || !n.f.on["pve-agent"] || !strings.HasPrefix(h.Text, want) {
			t.Errorf("%s/%s, the node on: %v: %s", h.State, h.On, n.f.on["pve-agent"], h.Text)
		}
	})
	t.Run("the API stops answering after the last list, and a guest starts: the node is left on", func(t *testing.T) {
		n := newSlowAPINode(t)
		n.wakeAndComeUp(t)
		n.set(func() { n.apiDown, n.guestStarts = true, n.f.now.Add(8*time.Second) })
		h := n.end(t, nil)
		if h.State != wakerun.StLeftOn || h.On != wakerun.OnYes || !n.f.on["pve-agent"] || !strings.HasPrefix(h.Text, "Left ON: Fairy cannot see whether guests run on it (") {
			t.Errorf("%s/%s, the node on: %v: %s", h.State, h.On, n.f.on["pve-agent"], h.Text)
		}
		if got := nodeRequests(n.f); got != "pve-agent /update" {
			t.Errorf("requests to the node's agent: %s; want no shutdown", got)
		}
	})
}
