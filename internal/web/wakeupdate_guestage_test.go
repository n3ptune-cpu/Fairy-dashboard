package web

import (
	"testing"
	"time"
)

// The question itself (runOps.Guests): a list whose request left more than 20 s ago is not what runs on the node
// now, whoever asks. The Proxmox API's job is taken out of the schedule ("polled every five minutes"), as in the
// tests of the clock.
func TestWakeUpdateAGuestListOlderThanTwentySecondsIsNotCurrent(t *testing.T) {
	c := newClockNode(t, false)
	ops := runOps{c.e.s}
	c.start()
	c.untilBeforeReady(0)
	c.f.tick()
	info, _, _ := c.e.s.Poller.AgentSeen("pve-agent")
	since := info.SeenAt.Add(-time.Second)
	c.lastPoll()
	for i := 0; i <= 20; i++ {
		if n, known, why := ops.Guests("node-srv-pvea", since); n != 0 || !known || why != "" {
			t.Fatalf("%d s after the list was asked for: %d running, known %v, %q; want none, known", i, n, known, why)
		}
		if listed, ok, ago := ops.AskGuests("node-srv-pvea"); !listed || !ok || ago != time.Duration(i)*time.Second {
			t.Fatalf("%d s after the list was asked for: AskGuests says listed %v, ok %v, %v ago", i, listed, ok, ago)
		}
		c.f.tick()
	}
	const stale = "the panel has no current list of its guests from its Proxmox API"
	if n, known, why := ops.Guests("node-srv-pvea", since); n != 0 || known || why != stale {
		t.Errorf("21 s after the list was asked for: %d running, known %v, %q; want %q", n, known, why, stale)
	}
	c.nextPoll()
	c.f.tick()
	if n, known, why := ops.Guests("node-srv-pvea", since); n != 0 || !known || why != "" {
		t.Errorf("with a new list: %d running, known %v, %q; want none, known", n, known, why)
	}
	// a machine that is no node of a Proxmox API has no list to wait for
	if listed, _, _ := ops.AskGuests("pc-a"); listed {
		t.Error("an ordinary machine has a guest list to wait for")
	}
	if listed, _, _ := ops.AskGuests("no-such-machine"); listed {
		t.Error("a machine the panel does not have has a guest list to wait for")
	}
}
