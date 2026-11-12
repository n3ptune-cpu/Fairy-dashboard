package wakerun

import (
	"testing"
)

// The findings of audit 5 (round 1, auditor r1-p2) against this package, each as a run in the test world.

// r1-p2-05: a woken machine whose agent can no longer shut it down (its power control was switched off during
// the run) is left on, with the one sentence for it. The panel sends nothing (internal/web proves that the
// request never goes to the Proxmox API instead); the run does not try again and does not try another way.
func TestAWokenMachineWithoutItsAgentRouteIsLeftOn(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	pc.shutMode = "noroute"
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	r := w.finish()
	h := host(r, "pc-a")
	if h.State != StLeftOn || h.On != OnYes || h.Text != NoAgentRoute+" It was updated to 17-bbbbbbb." {
		t.Fatalf("%s (%s): %s", h.State, h.On, h.Text)
	}
	if got := w.callsFor("pc-a"); got != "wake update shutdown" || pc.shutdowns != 0 || !pc.on {
		t.Fatalf("asked once, nothing sent, the machine is on: %q, shutdowns sent %d, on=%v", got, pc.shutdowns, pc.on)
	}
	if got := w.steps("pc-a"); got != "wake:ok update:ok shutdown:its own agent can no longer take the request" {
		t.Fatalf("the audit steps: %s", got)
	}
}
