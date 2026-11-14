package wakerun

import (
	"testing"
	"time"
)

// What the panel says a short "counts as off after" means ("A machine that is silent for longer than 6
// minutes counts as off. If it was restarting and comes up during the run, the run switches it off after its
// update.", internal/web: rtConsequence) is what a run does. Two machines that were silent for seven
// minutes in a run that takes six for off:
//
//   - one that has been on for three days and was out of reach: it counts as off and is sent the wake; it
//     answers with its old uptime, so it does not look freshly started: it is only updated (the admin ticked
//     that) and LEFT ON, with no shutdown request at all;
//   - one that was in a restart and comes up by itself while the run waits for it, freshly started: the run
//     cannot tell it from a machine it woke, updates it and switches it off.
func TestARunSwitchesOffAMachineThatRestartedNotOneThatWasOnlySilent(t *testing.T) {
	set := NewSettings(300, 60, 120, 8).WithOffAfter(360)

	w := newWorld(t)
	pc := w.running("pc-a")
	pc.unreachable = true
	w.advance(7 * time.Minute)
	w.start(StartRequest{Hosts: []string{"pc-a"}, UpdateIfOn: true, Settings: &set})
	w.advance(30 * time.Second)
	pc.unreachable = false
	r := w.finish()
	if h := host(r, "pc-a"); !pc.on || len(shutdowns(w)) != 0 || !h.NotFresh || h.State == StOff || w.callsFor("pc-a") != "wake update" {
		t.Fatalf("on for three days, out of reach for seven minutes: %s: %s (on %v, shutdown requests %v, calls %s)", h.State, h.Text, pc.on, shutdowns(w), w.callsFor("pc-a"))
	}

	w = newWorld(t)
	pc = off7(w, "pc-b")
	pc.wakeBoots = false // the wake packet does nothing: the machine is in its restart
	w.start(StartRequest{Hosts: []string{"pc-b"}, UpdateIfOn: true, Settings: &set})
	w.advance(40 * time.Second)
	pc.on, pc.bootedAt, pc.agentReadyAt = true, w.now, w.now.Add(5*time.Second) // its restart is over
	r = w.finish()
	if h := host(r, "pc-b"); h.State != StOff || pc.on || len(shutdowns(w)) != 1 || h.NotFresh {
		t.Fatalf("in a restart for seven minutes, up again during the run: %s: %s (on %v, shutdown requests %v)", h.State, h.Text, pc.on, shutdowns(w))
	}
	// with the default, 15 minutes, neither is touched: seven minutes of silence is "not sure"
	def := NewSettings(300, 60, 120, 8)
	w = newWorld(t)
	off7(w, "pc-c")
	w.start(StartRequest{Hosts: []string{"pc-c"}, UpdateIfOn: true, Settings: &def})
	r = w.finish()
	if h := host(r, "pc-c"); h.State != StSkipped || len(w.calls) != 0 {
		t.Fatalf("silent for seven minutes with fifteen in force: %s: %s (calls %v)", h.State, h.Text, w.calls)
	}
}
