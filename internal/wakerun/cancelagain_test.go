package wakerun

import (
	"strings"
	"testing"
	"time"
)

// Second review of fix7a (R7A-04): the run's cancel of its own countdown was sent once. When it got no answer
// the run ended the machine at once with "it may switch off at ... Cancel it on the machine's page.", with a
// minute and a half of countdown left, and a node went off under a guest that had started on it. A cancel is
// safe to repeat (the agent answers 200 when nothing is pending): the run now tries again every cancelRetry
// until its own due time, and only then writes that sentence.

// The reviewer's proof: a guest starts on the node 90 s before it would go off, and the answer to the run's
// cancel is lost.
func TestACancelWithoutAnAnswerIsTriedAgain(t *testing.T) {
	for _, c := range []struct {
		lost    string
		cancels int    // how many the run sends
		on      bool   // the node at the end
		want    string // its sentence
	}{
		// the first cancel does not arrive and gets no answer; the second, ten seconds later, is answered
		{"never", 2, true, "Left ON: 1 guest is running on it (vm-1) since its shutdown was asked for: its shutdown countdown was called off. It came online after the wake and it is ON."},
		// the first one arrives (the countdown is called off) and its answer is lost: the second is answered too
		{"arrives", 2, true, "Left ON: 1 guest is running on it (vm-1) since its shutdown was asked for: its shutdown countdown was called off. It came online after the wake and it is ON."},
		// no cancel ever arrives: tried again until the run's own due time, then the sentence of before
		{"always", 9, false, "Left ON: 1 guest is running on it (vm-1) since its shutdown was asked for, and the request to call its shutdown countdown (due at "},
	} {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.cancelLost = c.lost
		var due time.Time
		w.afterTick = func() {
			if pc.pending != nil {
				due = pc.pending.At
			}
			if !due.IsZero() && pc.guests == 0 && !w.now.Before(due.Add(-90*time.Second)) && w.now.Before(due) {
				pc.guests, pc.guestNames = 1, "vm-1" // a guest starts 90 s before the node would go off
			}
		}
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		r := over(t, w)
		h := host(r, "pc-a")
		w.advance(4 * time.Minute)
		n := strings.Count(w.callsFor("pc-a"), "cancel")
		if pc.wentOff && c.on {
			t.Errorf("the first cancel is lost (%s): the node went off under a guest that had started 90 s before; the run sent %d cancel(s) and ended %v before the countdown's end",
				c.lost, n, due.Sub(*r.EndedAt))
		}
		if n != c.cancels || pc.on != c.on || !strings.Contains(h.Text, c.want) {
			t.Errorf("cancel lost %q: %d cancel(s) sent, want %d; the node is on: %v; [%s] %q: %s\n  history: %s", c.lost, n, c.cancels, pc.on, h.State, h.On, h.Text, logOf(h))
		}
		if c.lost == "always" {
			// only when the run's own due time has passed, and then with the sentence of before
			if r.EndedAt.Before(due) || !strings.Contains(h.Text, "off got no answer (agent unreachable: timeout): it may have arrived.") || h.On != OnUnknown {
				t.Errorf("no cancel ever arrives: the run let go %v before the countdown's end: [%s] %q: %s", due.Sub(*r.EndedAt), h.State, h.On, h.Text)
			}
		}
	}
}

// The same for "Cancel run" and for the kill switch, and for a cancel that cannot be sent at all for a while
// (the link is down for some seconds): tried again, and the machine stays on.
func TestTheRunsCancelIsTriedAgainUntilItsOwnDueTime(t *testing.T) {
	for _, c := range []struct {
		name    string
		kill    bool          // the kill switch stops the run (false: an admin cancels it)
		downFor time.Duration // the link is down for this long from the cancel on
		lost    string
		on      bool
		want    string
	}{
		{"Cancel run, the answer to the first cancel is lost", false, 0, "never", true, "The run was cancelled by Bob: its shutdown countdown was called off. It came online after the wake and it is ON."},
		{"the kill switch, the answer to the first cancel is lost", true, 0, "never", true, "the run stopped (control is disabled by an administrator): its shutdown countdown was called off. It came online after the wake and it is ON."},
		{"Cancel run, the link is down for 25 s", false, 25 * time.Second, "", true, "The run was cancelled by Bob: its shutdown countdown was called off. It came online after the wake and it is ON."},
		{"the kill switch, the link is down for 25 s", true, 25 * time.Second, "", true, "the run stopped (control is disabled by an administrator): its shutdown countdown was called off. It came online after the wake and it is ON."},
		{"the kill switch, the link stays down", true, time.Hour, "", false, "the run stopped (control is disabled by an administrator), but its shutdown countdown (due at "},
	} {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.cancelLost = c.lost
		r := w.start(StartRequest{Hosts: []string{"pc-a"}})
		untilState(t, w, "pc-a", StShutting)
		due := *host(active(w), "pc-a").ShutdownAt
		w.advance(30 * time.Second) // 90 s of the countdown are left
		at := w.now
		w.afterTick = func() { pc.unreachable = w.now.Sub(at) < c.downFor }
		pc.unreachable = c.downFor > 0
		if c.kill {
			w.blocked = "control is disabled by an administrator"
		} else if _, err := w.m.CancelRun(r.ID, "Bob", false); err != nil {
			t.Fatal(err)
		}
		done := over(t, w)
		h := host(done, "pc-a")
		w.afterTick = nil
		w.advance(3 * time.Minute)
		n := strings.Count(w.callsFor("pc-a"), "cancel")
		if pc.on != c.on || !strings.Contains(h.Text, c.want) || (c.on && (h.On != OnYes || n < 2)) {
			t.Errorf("%s: %d cancel(s) sent; the machine is on: %v; [%s] %q: %s\n  history: %s", c.name, n, pc.on, h.State, h.On, h.Text, logOf(h))
		}
		if !c.on && (done.EndedAt.Before(due) || n < 9 || h.On == OnYes) {
			t.Errorf("%s: %d cancel(s) sent, and the run let go %v before the countdown's end: [%s] %q: %s", c.name, n, due.Sub(*done.EndedAt), h.State, h.On, h.Text)
		}
		// never more often than every cancelRetry: an agent takes twelve cancels a minute, from everybody
		if max := int(90*time.Second/cancelRetry) + 1; n > max {
			t.Errorf("%s: %d cancels in 90 s, at most %d", c.name, n, max)
		}
	}
}

// A cancel that is being tried again keeps the reason it was first asked for, also when the kill switch stops
// the run meanwhile: the sentence names the guest, as the first try would have.
func TestACancelThatIsTriedAgainKeepsItsReason(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pve1")
	pc.cancelLost = "never"
	w.start(StartRequest{Hosts: []string{"pve1"}})
	untilState(t, w, "pve1", StShutting)
	w.advance(30 * time.Second)
	pc.guests, pc.guestNames = 1, "web01"
	for i := 0; i < 20 && !strings.Contains(w.callsFor("pve1"), "cancel"); i++ {
		w.tick()
	}
	w.blocked = "control is disabled by an administrator" // before the second try
	r := over(t, w)
	h := host(r, "pve1")
	want := "Left ON: 1 guest is running on it (web01) since its shutdown was asked for: its shutdown countdown was called off. It came online after the wake and it is ON. It was updated to 17-bbbbbbb."
	if h.Text != want || h.On != OnYes || !pc.on || strings.Count(w.callsFor("pve1"), "cancel") != 2 || r.State != RunStopped {
		t.Errorf("run %s, requests %s, the node is on: %v: [%s] %q: %s", r.State, w.callsFor("pve1"), pc.on, h.State, h.On, h.Text)
	}
}
