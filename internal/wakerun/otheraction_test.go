package wakerun

import (
	"strings"
	"testing"
	"time"
)

// Found with RC-04 of the recheck of fix7r: "switched off again" for a machine that is restarting.
//
// A person calls the run's shutdown off on the machine's page and asks for a reboot. The agent shows "reboot
// pending": that is neither the run's countdown nor "nothing pending", so the run went on watching. The reboot
// starts after the run's own due time, the machine gives nothing for longer than 20 s while it restarts, two
// polls see that: "... and switched off again." It is on again a minute later. The same for a sleep (a machine
// that sleeps is not off), and for a shutdown of the person's own, whose end is not the run's result.
//
// The run now lets go of the machine as soon as its agent shows, in an answer that is surely from after it took
// the run's shutdown, that another action is pending in its place: it is ON, the sentence names the action, and
// what comes after is never read as the run's shutdown.

const (
	otherEarly = "Its shutdown was called off, in the panel or on the machine, and "
	otherLate  = "Its shutdown is no longer pending (it was called off, or it did not switch the machine off), and "
)

func otherAction(action string, at time.Time) string {
	return "another power action is pending on it now (" + action + " at " + clock(at) + ", by the machine's own clock). It is ON now. Fairy no longer watches it. It was updated to 17-bbbbbbb."
}

// The reboot is asked for 60 s before the end of the run's countdown, and starts 10 s, 40 s and 2 min after it.
// The machine is silent for 45 s while it restarts.
func TestAnotherPendingActionEndsTheWatch(t *testing.T) {
	for _, after := range []time.Duration{10 * time.Second, 40 * time.Second, 2 * time.Minute} {
		w := newWorld(t)
		pc := w.off("pc-a")
		var due, rebootAt time.Time
		w.afterTick = func() {
			if pc.pending != nil && pc.pending.Action == "shutdown" && due.IsZero() {
				due = pc.pending.At
			}
			if !due.IsZero() && rebootAt.IsZero() && !w.now.Before(due.Add(-60*time.Second)) {
				rebootAt = due.Add(after)
				pc.pending = &Pending{Action: "reboot", At: rebootAt} // a person: Cancel on the machine's page, then Reboot
			}
			if !rebootAt.IsZero() {
				pc.unreachable = !w.now.Before(rebootAt) && w.now.Before(rebootAt.Add(45*time.Second)) // it restarts
			}
		}
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		r := over(t, w)
		h := host(r, "pc-a")
		w.advance(after + 4*time.Minute) // the reboot, and well past it
		if h.On == OnNo && w.answers(pc) {
			t.Errorf("a reboot %v after the due time: REPORTED OFF, AND IT IS ON (%v after the due time): %s", after, r.EndedAt.Sub(due), h.Text)
		}
		if want := otherEarly + otherAction("reboot", rebootAt); h.State != StLeftOn || h.On != OnYes || h.Text != want {
			t.Errorf("a reboot %v after the due time: [%s] power %q, ended %v after the due time: %s", after, h.State, h.On, r.EndedAt.Sub(due), h.Text)
		}
		if !r.EndedAt.Before(due.Add(-55 * time.Second)) {
			t.Errorf("a reboot %v after the due time: the run let go %v after the due time, want at the first answer that shows the reboot", after, r.EndedAt.Sub(due))
		}
		if got := w.callsFor("pc-a"); got != "wake update shutdown" {
			t.Errorf("a reboot %v after the due time: requests %q, want no cancel and nothing else", after, got)
		}
		w.wellReported(r)
	}
	// what must stay: with no other action, a machine that goes off at the end of its countdown is switched off again
	w := newWorld(t)
	w.off("pc-a")
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	has(t, host(w.finish(), "pc-a"), StOff, "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.")
}

// The other ways to the same end, and what stays as it was.
func TestWhoseShutdownItIsWhenAnotherActionWasAskedFor(t *testing.T) {
	type world1 struct {
		w   *world
		pc  *machine
		due time.Time
		at  time.Time // when the person's action happens
	}
	// run lets a run get to its countdown and calls step at the end of every second from then on, with the time
	// since the end of the run's countdown (negative before it).
	run := func(shutMode, shutLost string, step func(x *world1, since time.Duration)) (*world1, *Run, *Host) {
		x := &world1{w: newWorld(t)}
		x.pc = x.w.off("pc-a")
		x.pc.shutMode, x.pc.shutLost = shutMode, shutLost
		x.w.start(StartRequest{Hosts: []string{"pc-a"}})
		untilState(t, x.w, "pc-a", StShutting)
		x.due = *host(active(x.w), "pc-a").ShutdownAt
		x.w.afterTick = func() { step(x, x.w.now.Sub(x.due)) }
		r := over(t, x.w)
		return x, r, host(r, "pc-a")
	}
	never := func(what string, h *Host) {
		t.Helper()
		if strings.Contains(h.Text, "switched off again") || h.State == StOff || h.On == OnNo {
			t.Errorf("%s: the run reports it as switched off again: [%s] power %q: %s", what, h.State, h.On, h.Text)
		}
	}

	// The run's shutdown did not happen (the countdown ran out, the system stayed up). 30 s after the due time a
	// person asks for a reboot, which starts a minute later; the machine is silent for 45 s.
	x, r, h := run("ignore", "", func(x *world1, since time.Duration) {
		if since >= 30*time.Second && x.at.IsZero() {
			x.at = x.w.now.Add(time.Minute)
			x.pc.pending = &Pending{Action: "reboot", At: x.at}
		}
		if !x.at.IsZero() {
			x.pc.unreachable = !x.w.now.Before(x.at) && x.w.now.Before(x.at.Add(45*time.Second))
		}
	})
	x.w.advance(5 * time.Minute)
	never("a reboot asked for after the run's shutdown did not happen", h)
	if want := otherLate + otherAction("reboot", x.at); h.State != StLeftOn || h.On != OnYes || h.Text != want || !x.w.answers(x.pc) {
		t.Errorf("a reboot asked for after the run's shutdown did not happen: [%s] power %q, ended %v after the due time: %s", h.State, h.On, r.EndedAt.Sub(x.due), h.Text)
	}

	// A sleep, asked for 60 s before the end of the run's countdown, for 20 s after it. The machine sleeps from
	// then on: it gives nothing, and it is not off.
	x, r, h = run("ok", "", func(x *world1, since time.Duration) {
		if since >= -60*time.Second && x.at.IsZero() {
			x.at = x.due.Add(20 * time.Second)
			x.pc.pending = &Pending{Action: "sleep", At: x.at}
		}
		if !x.at.IsZero() && !x.w.now.Before(x.at) {
			x.pc.unreachable = true
		}
	})
	x.w.advance(5 * time.Minute)
	never("a sleep in place of the run's shutdown", h)
	if want := otherEarly + otherAction("sleep", x.at); h.State != StLeftOn || h.On != OnYes || h.Text != want || !x.pc.on {
		t.Errorf("a sleep in place of the run's shutdown: [%s] power %q, ended %v after the due time: %s", h.State, h.On, r.EndedAt.Sub(x.due), h.Text)
	}

	// A shutdown of a person's own, asked for after the run's did not happen: the agent showed nothing pending
	// after the run's countdown, and then a shutdown again. The run asks for one shutdown only, so this one is
	// not the run's, and the machine going off by it is not the run's "switched off again".
	x, r, h = run("ignore", "", func(x *world1, since time.Duration) {
		if since >= 30*time.Second && x.at.IsZero() {
			x.at = x.w.now.Add(time.Minute)
			x.pc.shutMode, x.pc.pending = "ok", &Pending{Action: "shutdown", At: x.at}
		}
	})
	never("a person's own shutdown after the run's did not happen", h)
	want := otherLate + "another shutdown was asked for after that (it ends at " + clock(x.at) + ", by the machine's own clock), which is not the run's. " +
		"It is ON now. Fairy no longer watches it: it switches off then unless someone cancels that on its page. It was updated to 17-bbbbbbb."
	if h.State != StLeftOn || h.On != OnYes || h.Text != want || !x.pc.on {
		t.Errorf("a person's own shutdown after the run's did not happen: [%s] power %q, ended %v after the due time: %s", h.State, h.On, r.EndedAt.Sub(x.due), h.Text)
	}
	x.w.advance(3 * time.Minute)
	if x.pc.on {
		t.Error("set-up: the person's shutdown must switch the machine off")
	}

	// Holding before and now: the run saw its countdown called off (an answer without one, well before the end)
	// and let go there. The person's own shutdown comes after that and is nothing the run reports.
	x, _, h = run("ok", "", func(x *world1, since time.Duration) {
		switch {
		case since >= -90*time.Second && x.at.IsZero():
			x.at = x.due.Add(time.Minute)
			x.pc.pending = nil // Cancel, on the machine's page
		case since >= -80*time.Second && x.pc.pending == nil && x.pc.on:
			x.pc.pending = &Pending{Action: "shutdown", At: x.at} // and ten seconds later a shutdown of their own
		}
	})
	never("a person's own shutdown after the run's was called off", h)
	if h.State != StLeftOn || h.On != OnYes || h.Text != "Its shutdown was cancelled, in the panel or on the machine, so it is ON. It was updated to 17-bbbbbbb." {
		t.Errorf("a person's own shutdown after the run's was called off: [%s] power %q: %s", h.State, h.On, h.Text)
	}

	// Holding before and now, the limit that stays: the run's shutdown is called off and a shutdown of the
	// person's own asked for inside one second, so no answer of the agent shows "nothing pending" in between.
	// The agent's answer does not say whose a shutdown is: the run takes it for its own, and when the machine
	// goes off, a minute after the run's due time, it is "switched off again". The machine is off.
	x, _, h = run("ok", "", func(x *world1, since time.Duration) {
		if since >= -60*time.Second && x.at.IsZero() {
			x.at = x.due.Add(time.Minute)
			x.pc.pending = &Pending{Action: "shutdown", At: x.at}
		}
	})
	if h.State != StOff || h.On != OnNo || x.pc.on || !x.pc.wentOff {
		t.Errorf("the limit (a shutdown in place of the run's, never an answer without one): [%s] power %q, the machine is on: %v: %s", h.State, h.On, x.pc.on, h.Text)
	}

	// Holding before and now: a shutdown request that got no answer, and a reboot shows as pending. The run
	// never knew of a countdown of its own, so silence was never its "off"; nothing changes here.
	x, _, h = run("ok", "never", func(x *world1, since time.Duration) {
		if x.at.IsZero() {
			x.at = x.due.Add(30 * time.Second)
			x.pc.pending = &Pending{Action: "reboot", At: x.at}
		}
		x.pc.unreachable = !x.w.now.Before(x.at) && x.w.now.Before(x.at.Add(45*time.Second))
	})
	never("a reboot after a shutdown request without an answer", h)
	if h.On != OnUnknown || !strings.Contains(h.Text, "Its shutdown request got no answer, Fairy never saw its countdown, and it does not answer now: not confirmed that it is off.") {
		t.Errorf("a reboot after a shutdown request without an answer: [%s] power %q: %s", h.State, h.On, h.Text)
	}
}

// The last look. A machine that the run let go of because another action is pending in its shutdown's place,
// and that gives nothing at the look (the answer that showed the action was some seconds old, and the reboot
// began): silence that would prove "off" after the run's own shutdown proves nothing here.
func TestSilenceAfterAnotherActionIsNeverTheRunsOff(t *testing.T) {
	for _, notPending := range []bool{false, true} {
		w := newWorld(t)
		pc := w.off("pc-a") // it gives nothing at all
		w.advance(100 * time.Second)
		due := w.now.Add(-60 * time.Second)
		// what the watch of a countdown has when "off" is proven: polls of the row after the due time, the first 30 s ago
		h := &Host{ID: "pc-a", ShutdownAt: &due, endMin: -time.Second, silRow: pc.silRow, silN: pc.silN, silAfter: 3, silSince: w.mono - 30*time.Second, notPending: notPending}
		h.verdict = &verdict{state: StLeftOn, on: OnYes, text: "It is ON now.", neutral: "Another action is pending.", since: w.now.Add(-40 * time.Second), sinceMono: w.mono - 40*time.Second}
		if !h.seenOff(w.mono) {
			t.Fatal("set-up: the silence must be proof of \"off\" for the watch of a countdown")
		}
		w.m.look(&Run{}, h, w.now)
		switch {
		case !notPending && (h.State != StOff || h.On != OnNo || h.Text != "Another action is pending. It is off (not answering)."):
			t.Errorf("set-up: after the run's own shutdown this silence is \"off\": [%s] power %q: %s", h.State, h.On, h.Text)
		case notPending && (h.State != StLeftOn || h.On != OnUnknown || h.Text != "Another action is pending. It does not answer now: not confirmed that it is on."):
			t.Errorf("another action was pending in the shutdown's place: [%s] power %q: %s", h.State, h.On, h.Text)
		}
	}
}
