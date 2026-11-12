package web

import (
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/scheduler"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// "Which guests run on this Proxmox node" is asked before a run switches a node off again, and the cluster's
// list counts only when it is from after the node came online: the list from before shows the node's guests
// as they were while the node was off or starting. "After" was decided with two times of day (the Proxmox
// source's last good answer after the node's first answer), and the panel's clock can be set between the two.
// Set back before the node's first answer, a list from before it looked newer than the node's coming online:
// the panel counted no running guest on the node, and a run switched the node off under the guest that had
// started with it. Now the poller's steady clock is asked as well (poller.AnsweredSinceUp), and both must say
// "after".

// clockNode is nodeRun's node (SRV-PVEA with the agent "pve-agent" and Wake-on-LAN, off for longer than a
// quarter of an hour) with a time of day that a test sets apart from the steady clock (as in cutFarm). The
// cluster lists the node as online from 5 s before its agent answers, and its guest win11 as stopped until the
// agent answers; from then on as running when guestRuns says so (it starts with the node).
type clockNode struct {
	e         *env
	f         *farm
	skew      *time.Duration
	guestRuns bool
	job       *scheduler.Job
}

func newClockNode(t *testing.T, guestRuns bool) *clockNode {
	t.Helper()
	e := setup(t)
	f := newFarm(t, e)
	c := &clockNode{e: e, f: f, skew: new(time.Duration), guestRuns: guestRuns}
	began := f.now
	p := e.s.Poller
	p.Clock = func() time.Time { return f.now.Add(*c.skew) }
	p.Mono = func() time.Duration { return f.now.Sub(began) }
	e.s.WakeRuns.Now = p.Clock
	e.s.WakeRuns.Mono = p.Mono
	e.b.Mu.Lock()
	e.b.PVE = func() ([]jsonx.M, error) {
		rows := testutil.PVEResources()
		rows[2]["status"] = "stopped"
		ready := f.ready["pve-agent"]
		switch {
		case !f.on["pve-agent"] || f.now.Before(ready.Add(-5*time.Second)):
			rows[0]["status"] = "offline"
		case !f.now.Before(ready) && c.guestRuns:
			rows[2]["status"] = "running"
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
	e.b.Metrics["pve-agent"] = func() (jsonx.M, error) { return f.metrics("pve-agent") }
	e.b.Mu.Unlock()
	f.offline()
	f.optIn("node-srv-pvea")
	e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
		if path == "/power" {
			return 200, jsonx.M{"action": "shutdown", "pending": jsonx.M{"action": "shutdown", "at": float64(f.now.Add(120 * time.Second).Unix()), "delay_s": 120.0, "requested_by": "x"}}, nil
		}
		return 0, nil, nil
	}
	for _, j := range p.Scheduler.Jobs() {
		if j.Key == "proxmox:"+p.Clusters()[0].ID {
			c.job = j
		}
	}
	if c.job == nil {
		t.Fatal("no poll job for the Proxmox API")
	}
	return c
}

// start switches the node on: its agent answers 20 s from now, with an uptime that began 5 s from now.
func (c *clockNode) start() {
	c.e.b.Mu.Lock()
	c.f.on["pve-agent"], c.f.booted["pve-agent"], c.f.ready["pve-agent"] = true, c.f.now.Add(5*time.Second), c.f.now.Add(20*time.Second)
	c.e.b.Mu.Unlock()
}

// untilBeforeReady lets the farm run until d before the node's agent answers.
func (c *clockNode) untilBeforeReady(d time.Duration) {
	for c.f.now.Before(c.f.ready["pve-agent"].Add(-d)) {
		c.f.tick()
	}
}

// lastPoll: the Proxmox API is polled at a long interval (five minutes is the longest), and its latest poll
// was the one before this: the next is due when the test says so (nextPoll), or not within the test.
func (c *clockNode) lastPoll() { c.e.s.Poller.Scheduler.Remove(c.job.Key) }
func (c *clockNode) nextPoll() { c.e.s.Poller.Scheduler.Add(c.job) }

// The question itself, as the run asks it: since is the time of day of the node's first answer. The Proxmox
// API's last answer came 2 s before that answer, and then the panel's clock was set.
func TestWakeUpdateGuestsOfANodeWhenTheClockWasSet(t *testing.T) {
	const unseen = "its Proxmox API has not answered since it came online"
	for _, set := range []time.Duration{0, -time.Hour, -3 * time.Second, time.Hour} {
		for _, guestRuns := range []bool{true, false} {
			c := newClockNode(t, guestRuns)
			ops := runOps{c.e.s}
			c.start()
			c.untilBeforeReady(2 * time.Second)
			c.lastPoll()
			*c.skew = set
			c.untilBeforeReady(0)
			info, _, _ := c.e.s.Poller.AgentSeen("pve-agent")
			if !info.Online || info.SeenAgo != 0 {
				t.Fatalf("the set-up: the node's agent must have answered just now: %+v", info)
			}
			since := info.SeenAt
			if n, known, why := ops.Guests("node-srv-pvea", since); n != 0 || known || why != unseen {
				t.Errorf("the clock set by %v, the guest runs: %v. With the list from 2 s before the node's first answer: %d running, known %v, %q; want %q",
					set, guestRuns, n, known, why, unseen)
			}
			for i := 0; i < 30; i++ { // and it stays that for as long as the list is the old one
				c.f.tick()
			}
			if n, known, why := ops.Guests("node-srv-pvea", since); n != 0 || known || why != unseen {
				t.Errorf("the clock set by %v, the guest runs: %v. 30 s later, with the same list: %d running, known %v, %q; want %q", set, guestRuns, n, known, why, unseen)
			}
			// The Proxmox API's next answer is from after the node came online: it is current, and it counts.
			c.nextPoll()
			c.f.tick()
			n, known, names := ops.Guests("node-srv-pvea", since)
			if guestRuns && (n != 1 || !known || names != "win11") {
				t.Errorf("the clock set by %v. With a list from after the node came online: %d running, known %v, %q; want 1 (win11)", set, n, known, names)
			}
			if !guestRuns && (n != 0 || !known || names != "") {
				t.Errorf("the clock set by %v. With a list from after the node came online and no guest running: %d running, known %v, %q; want none, known", set, n, known, names)
			}
		}
	}
}

// nodeClockRun: a run wakes the node. Two seconds before the node's agent answers, the Proxmox API answers, and
// then the panel's time of day is set. slow: the Proxmox API is polled every five minutes, so its next answer
// comes five minutes after that one (and every second from then on, like everything in the farm).
func nodeClockRun(t *testing.T, set time.Duration, slow, guestRuns bool) (*wakerun.Host, *farm) {
	t.Helper()
	c := newClockNode(t, guestRuns)
	e, f := c.e, c.f
	if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"node-srv-pvea"}, By: "Alice", BySub: "u2"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		if a, _ := e.s.WakeRuns.Snapshot(); a != nil && hostOf(a, "node-srv-pvea").WakeSentAt != nil {
			break
		}
		f.tick()
	}
	c.start() // the wake packet starts the node
	c.untilBeforeReady(2 * time.Second)
	last := f.now
	if slow {
		c.lastPoll()
	}
	*c.skew = set
	for i := 0; i < 3600; i++ {
		if a, _ := e.s.WakeRuns.Snapshot(); a == nil {
			break
		}
		if slow && f.now.Sub(last) == 5*time.Minute-time.Second {
			c.nextPoll()
		}
		f.tick()
	}
	_, hist := e.s.WakeRuns.Snapshot()
	return hostOf(hist[0], "node-srv-pvea"), f
}

const guestsUnseen = "Left ON: Fairy cannot see whether guests run on it (its Proxmox API has not answered since it came online)"

// The whole run. No answer from the Proxmox API since the node came online, and a guest that started with the
// node: the node is left on, whatever the panel's clock was set to in between. With the clock set back three
// seconds the run wrote "switched off again", and the node was off, under the guest. (Three seconds: less than
// the five that the node takes from the wake to its system's start. With more, its uptime is longer than the
// time of day says has passed since the wake, and the run leaves it on for that: "it was already on".)
func TestWakeUpdateAGuestListFromBeforeTheNodeCameOnlineIsNotCurrentWhateverTheClockSays(t *testing.T) {
	for _, c := range []struct {
		name string
		set  time.Duration
	}{
		{"the control: the time of day is not set", 0},
		{"the time of day is set back three seconds", -3 * time.Second},
		{"the time of day is set forward two minutes", 2 * time.Minute},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, f := nodeClockRun(t, c.set, true, true)
			if h.State != wakerun.StLeftOn || h.On != wakerun.OnYes || !f.on["pve-agent"] || !strings.HasPrefix(h.Text, guestsUnseen) {
				t.Errorf("%s/%s, the node on: %v: %s", h.State, h.On, f.on["pve-agent"], h.Text)
			}
			if got := nodeRequests(f); got != "pve-agent /update" { // no shutdown was asked for
				t.Errorf("requests: %s", got)
			}
		})
	}
}

// The normal case stays as it was, with the clock set or not: a list that came after the node came online is
// current. A node without a running guest is updated and switched off again; a guest that runs keeps it on.
func TestWakeUpdateAGuestListFromAfterTheNodeCameOnlineIsCurrentWhateverTheClockSays(t *testing.T) {
	for _, c := range []struct {
		name string
		set  time.Duration
	}{
		{"the control: the time of day is not set", 0},
		{"the time of day is set back three seconds", -3 * time.Second},
		{"the time of day is set forward two minutes", 2 * time.Minute},
	} {
		t.Run(c.name+", no guest runs", func(t *testing.T) {
			h, f := nodeClockRun(t, c.set, false, false)
			if h.State != wakerun.StOff || h.On != wakerun.OnNo || f.on["pve-agent"] || nodeRequests(f) != "pve-agent /update, pve-agent /power" {
				t.Errorf("%s/%s, the node on: %v, requests %q: %s", h.State, h.On, f.on["pve-agent"], nodeRequests(f), h.Text)
			}
		})
		t.Run(c.name+", a guest runs", func(t *testing.T) {
			h, f := nodeClockRun(t, c.set, false, true)
			if h.State != wakerun.StLeftOn || h.On != wakerun.OnYes || !f.on["pve-agent"] || !strings.HasPrefix(h.Text, "Left ON: 1 guest is running on it (win11)") {
				t.Errorf("%s/%s, the node on: %v: %s", h.State, h.On, f.on["pve-agent"], h.Text)
			}
			if got := nodeRequests(f); got != "pve-agent /update" {
				t.Errorf("requests: %s", got)
			}
		})
	}
}

// A setting by an hour between the wake and the node's first answer does not get as far as the question about
// the guests in a run (so it is asked directly above): the run takes an answer for "after the wake" by the
// time of day too, and it waits for that for five minutes by both clocks. It gives the node up, and says that
// it may be on. Nothing is sent to it.
func TestWakeUpdateTheClockSetByAnHourBeforeTheNodesFirstAnswer(t *testing.T) {
	for _, set := range []time.Duration{-time.Hour, time.Hour} {
		h, f := nodeClockRun(t, set, false, true)
		want := "It did not come online within 5 min after the wake. It may still be starting, or be on without its network connection (Tailscale): " +
			"if it did switch on, it is ON. Fairy has not switched it off and will not."
		if h.State != wakerun.StGaveUp || h.On != wakerun.OnUnknown || h.Text != want || !f.on["pve-agent"] || nodeRequests(f) != "" {
			t.Errorf("the clock set by %v: %s/%s, the node on: %v, requests %q: %s", set, h.State, h.On, f.on["pve-agent"], nodeRequests(f), h.Text)
		}
	}
}
