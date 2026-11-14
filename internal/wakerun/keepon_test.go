package wakerun

import (
	"strings"
	"testing"
	"time"
)

// Keep on for a machine whose arming got no answer, while its ticket can still arrive (independent review of
// 270e7ab, P4). One cancel that finds no run ends nothing: the ticket arrives a moment later and arms the
// machine. The run takes Keep on over: it repeats the cancel for as long as the ticket can arrive, and says
// what the machine's own word is in the end.
func TestKeepOnIsRepeatedWhileTheTicketCanStillArrive(t *testing.T) {
	for _, tc := range []struct {
		name, lost, how string
	}{
		{"the ticket arrives a minute later", "late", "its agent shows the run as cancelled"},
		{"the ticket never arrives", "never", "its agent shows no run any more"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			pc := w.checkedOff("pc-a")
			pc.chk.armLost, pc.chk.armLate = tc.lost, time.Minute
			run := w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
			w.waitFor(8*time.Minute, "it waits", func(r *Run) bool { return stateOf(r, "pc-a") == StApproval })
			a := named(w.m.MayGoOff(w.now), "pc-a")
			until := run.StartedAt.Add(run.Ticket.valid() + lateArm)
			if a == nil || a.Sure || a.TicketUntil == nil || a.TicketUntil.Sub(until).Abs() > 2*time.Second {
				t.Fatalf("the strip's list: %+v (the ticket can arrive until %s)", a, until)
			}
			if !w.m.KeepOn("pc-a", "Ann") {
				t.Fatal("the run did not take Keep on over")
			}
			if act, _ := w.m.Snapshot(); !w.m.KeepOn("pc-a", "Ann") || strings.Count(logOf(host(act, "pc-a")), "Keep on by Ann") != 1 {
				t.Fatalf("a second Keep on: the run is at it already, once:\n%s", logOf(host(act, "pc-a")))
			}
			if _, err := w.m.Waiting(run.ID); err == nil {
				t.Fatal("a machine that is being kept on is still offered for the confirmation")
			}
			// while the ticket can arrive, the machine's "no run" ends nothing: the run goes on cancelling
			for w.now.Before(until.Add(-5 * time.Second)) {
				w.tick()
				if act, _ := w.m.Snapshot(); act == nil || host(act, "pc-a").Final() {
					if tc.lost == "never" {
						t.Fatalf("the run stopped cancelling %s before the ticket's time was over: %q", until.Sub(w.now), host(act, "pc-a").Text)
					}
					break
				}
			}
			done := w.finish()
			h := host(done, "pc-a")
			has(t, h, StLeftOn, "Kept ON by Ann: its switch-off was called off ("+tc.how+").", "It is ON.")
			if n := strings.Count(" "+w.callsFor("pc-a")+" ", " cancel "); n < 2 {
				t.Fatalf("%d cancels were sent: %q", n, w.callsFor("pc-a"))
			}
			if a := named(w.m.MayGoOff(w.now), "pc-a"); a != nil {
				t.Fatalf("after it was kept on the strip still names it: %+v", a)
			}
			w.advance(40 * time.Minute)
			if !pc.on {
				t.Fatalf("the machine switched itself off after Keep on: %q", h.Text)
			}
		})
	}
}

// Keep on is not taken over for a machine that is not the run's to keep on: no run, another machine, one that
// is not armed, one that has ended.
func TestKeepOnIsOnlyForAMachineThatMayGoOff(t *testing.T) {
	w := newWorld(t)
	if w.m.KeepOn("pc-a", "Ann") {
		t.Fatal("no run is going")
	}
	pc := w.checkedOff("pc-a")
	pc.chk.armSays = "limits"
	w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
	w.waitFor(8*time.Minute, "it waits", func(r *Run) bool { return stateOf(r, "pc-a") == StApproval })
	w.advance(30 * time.Second)
	if w.m.KeepOn("pc-a", "Ann") || w.m.KeepOn("pc-x", "Ann") {
		t.Fatal("a machine that is not armed, or not in the run")
	}
	if got := w.callsFor("pc-a"); strings.Contains(got, "cancel") {
		t.Fatalf("a cancel was sent: %q", got)
	}
}
