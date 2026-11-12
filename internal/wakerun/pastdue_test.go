package wakerun

import (
	"strings"
	"testing"
	"time"
)

// Second review of fix7a (R7A-03): the run's cancel could call off an action the run did not start.
//
// The run "calls off the countdown it started" when the kill switch stops it, and when an admin cancels it.
// The request it sends is the agent's one cancel, which calls off whatever action of the panel is pending on
// the machine. So the run sends it only while its own countdown can still be running (its own due time, on its
// steady clock). After that it sends nothing, and the sentence does not say that a countdown was called off.

// The reviewer's proof: the kill switch goes on while the run still waits for a machine whose countdown is
// over (its shutdown did not happen). A person has meanwhile asked for a reboot of that machine on its page.
// (The kill switch came ten seconds after the reboot was asked for. Since the run lets go of a machine as soon
// as its agent shows another action than the run's shutdown, that is in the same second here: the run has not
// seen the reboot when it is stopped. TestAnotherPendingActionEndsTheWatch has the ten seconds.)
func TestTheRunsCancelIsNotSentAfterItsOwnCountdown(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	pc.shutMode = "ignore"
	var due time.Time
	personAsked := false
	w.afterTick = func() {
		if pc.pending != nil && !personAsked {
			due = pc.pending.At
		}
		if !due.IsZero() && !personAsked && !w.now.Before(due.Add(60*time.Second)) {
			personAsked = true
			pc.pending = &Pending{Action: "reboot", At: w.now.Add(30 * time.Minute)} // a person, on the machine's page
			w.blocked = "control is disabled by an administrator"
		}
	}
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	r := over(t, w)
	h := host(r, "pc-a")
	if strings.Contains(w.callsFor("pc-a"), "cancel") || pc.pending == nil {
		t.Errorf("the run sent a cancel 60 s after its own countdown was over (requests: %s); the reboot a person asked for is still pending: %v", w.callsFor("pc-a"), pc.pending != nil)
	}
	want := "the run stopped (control is disabled by an administrator) after its shutdown countdown had run out (it was due at " + clock(due) + "), so the run sent no cancel. " +
		"It came online after the wake and it is ON now. Fairy no longer watches it: if the shutdown is still under way, it switches off by itself. It was updated to 17-bbbbbbb."
	if h.State != StLeftOn || h.On != OnYes || h.Text != want {
		t.Errorf("[%s] power %q: %s", h.State, h.On, h.Text)
	}
	if strings.Contains(h.Text, "called off") {
		t.Errorf("the sentence says that a countdown was called off: %s", h.Text)
	}
}

// The same for "Cancel run": at every second around the end of the countdown. Up to the run's own due time the
// cancel is sent and the machine stays on; after it nothing is sent, and the report says what is seen.
func TestCancelRunAroundTheEndOfTheCountdown(t *testing.T) {
	for _, mode := range []string{"ok", "ignore"} { // the machine goes off at the end of its countdown; it stays up
		for k := -5; k <= 8; k++ {
			w := newWorld(t)
			pc := w.off("pc-a")
			pc.shutMode = mode
			r := w.start(StartRequest{Hosts: []string{"pc-a"}})
			untilState(t, w, "pc-a", StShutting)
			due := *host(active(w), "pc-a").ShutdownAt
			for w.now.Before(due.Add(time.Duration(k) * time.Second)) {
				w.tick()
			}
			if active(w) == nil {
				t.Fatalf("%s, %d s: the run is over already", mode, k)
			}
			if _, err := w.m.CancelRun(r.ID, "Bob", false); err != nil {
				t.Fatal(err)
			}
			h := host(w.finish(), "pc-a") // its judge fails "on" for a machine that is off, and the other way round
			sent := strings.Contains(w.callsFor("pc-a"), "cancel")
			// the run acts on a cancel at its next step, a second later: that step is past the due time from k = 0 on
			switch past := k >= 0; {
			case past && sent:
				t.Errorf("%s: cancel run %d s after the countdown's end: a cancel was sent (%s)", mode, k, w.callsFor("pc-a"))
			case past && (strings.Contains(h.Text, "called off") || !strings.Contains(h.Text, "The run was cancelled by Bob after its shutdown countdown had run out (it was due at "+clock(due)+"), so the run sent no cancel.")):
				t.Errorf("%s: cancel run %d s after the countdown's end: %s", mode, k, h.Text)
			case past && mode == "ok" && (h.State != StOff || h.On != OnNo || !strings.HasSuffix(h.Text, "so the run sent no cancel. It was updated to 17-bbbbbbb. It is off (not answering).")):
				t.Errorf("%s: cancel run %d s after the countdown's end: [%s] %q: %s", mode, k, h.State, h.On, h.Text)
			case past && mode == "ignore" && (h.State != StLeftOn || h.On != OnYes || !strings.Contains(h.Text, "it is ON now. Fairy no longer watches it: if the shutdown is still under way, it switches off by itself.")):
				t.Errorf("%s: cancel run %d s after the countdown's end: [%s] %q: %s", mode, k, h.State, h.On, h.Text)
			case !past && !sent:
				t.Errorf("%s: cancel run %d s before the countdown's end: no cancel was sent (%s)", mode, -k, w.callsFor("pc-a"))
			case k == -1:
				// the step that acts on it is the very second the countdown ends: the cancel is sent (the run's own
				// countdown can still be running), and in this world it comes too late. The judge has checked the report.
			case !past && (!pc.on || h.On != OnYes || !strings.Contains(h.Text, "The run was cancelled by Bob: its shutdown countdown was called off. It came online after the wake and it is ON.")):
				t.Errorf("%s: cancel run %d s before the countdown's end: on %v, [%s] %q: %s", mode, -k, pc.on, h.State, h.On, h.Text)
			}
		}
	}
}

// A shutdown request that got no answer, and "Cancel run" after the time it would have been due: nothing is
// sent, and the sentence claims neither a countdown nor that one was called off.
func TestCancelRunAfterTheDueTimeOfAShutdownRequestWithoutAnAnswer(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	pc.shutLost = "never"
	r := w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	due := *host(active(w), "pc-a").ShutdownAt
	pc.unreachable = true   // the link that lost the answer stays down
	for w.now.Before(due) { // the run acts on the cancel at its next step, one second past the due time
		w.tick()
	}
	if _, err := w.m.CancelRun(r.ID, "Bob", false); err != nil {
		t.Fatal(err)
	}
	h := host(w.finish(), "pc-a")
	want := "The run was cancelled by Bob after the time its shutdown would have been due (" + clock(due) + "; the request got no answer), so the run sent no cancel. " +
		"It was updated to 17-bbbbbbb. It does not answer now: not confirmed that it is on."
	if h.State != StLeftOn || h.On != OnUnknown || h.Text != want || strings.Contains(w.callsFor("pc-a"), "cancel") {
		t.Errorf("[%s] power %q, requests %s: %s", h.State, h.On, w.callsFor("pc-a"), h.Text)
	}
}
