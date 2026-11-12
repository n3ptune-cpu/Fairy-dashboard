package wakerun

import (
	"strings"
	"testing"
	"time"
)

// Review of fix7r2, R2-01: "from after the run's request" was decided by two times of day.
//
// Whether an answer of the machine's agent is from after the run's shutdown request decides everything the run
// reads out of it during the countdown: its own countdown, "cancelled", another action in the shutdown's
// place. It was SeenAt after shutdownSent, both without a steady reading. After the panel's clock was set back
// during the countdown no answer was "from after the request" until the clock had caught up: the run did not
// see a person's reboot in the place of its shutdown, read the restart's silence as its own shutdown
// ("switched off again" for a machine that is on), and its cancel called the person's reboot off. It is
// decided on the steady clock now (Host.answeredAfter, Observation.SeenAgo).

// setPanelClock sets the time of day by d; the steady clock goes on. The machine's times are by the same
// clock in this world, so they move along (what its agent says "at" stays right by the machine's clock).
func setPanelClock(w *world, mc *machine, d time.Duration) {
	w.clock.Lock()
	w.now = w.now.Add(d)
	w.clock.Unlock()
	if mc.pending != nil {
		mc.pending.At = mc.pending.At.Add(d)
	}
	mc.bootedAt, mc.agentReadyAt = mc.bootedAt.Add(d), mc.agentReadyAt.Add(d)
}

var clockSteps = []struct {
	name string
	by   time.Duration
}{{"set back one hour", -time.Hour}, {"set forward one hour", time.Hour}, {"set back two seconds", -2 * time.Second}}

// The reviewer's sequence (TestReview7r2ARebootIsNotSwitchedOffAgainWhenThePanelsClockWasSetBack): the countdown
// is 120 s. 30 s in, the panel's time of day is set. 60 s in, a person calls the shutdown off on the machine's
// page and asks for a reboot; it starts 10 s after the run's due time, and the machine gives nothing for 45 s.
func TestARebootIsNotSwitchedOffAgainWhenThePanelsClockWasSet(t *testing.T) {
	for _, c := range clockSteps {
		w := newWorld(t)
		pc := w.off("pc-a")
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		untilState(t, w, "pc-a", StShutting)
		mono0 := w.mono
		var rebootMono time.Duration
		var rebootAt time.Time
		stepped := false
		w.afterTick = func() {
			since := w.mono - mono0
			if since >= 30*time.Second && !stepped {
				stepped = true
				setPanelClock(w, pc, c.by)
			}
			if since >= 60*time.Second && rebootMono == 0 {
				rebootMono, rebootAt = mono0+130*time.Second, w.now.Add(70*time.Second)
				pc.pending = &Pending{Action: "reboot", At: rebootAt} // a person: Cancel, then Reboot
			}
			if rebootMono != 0 {
				pc.unreachable = w.mono >= rebootMono && w.mono < rebootMono+45*time.Second // it restarts
			}
		}
		r := over(t, w)
		h := host(r, "pc-a")
		ended := w.mono - mono0
		for w.mono < mono0+240*time.Second {
			w.tick()
		}
		if (h.On == OnNo || strings.Contains(h.Text, "switched off again")) && w.answers(pc) {
			t.Errorf("the panel's clock %s: REPORTED OFF, AND IT IS ON (it restarted): [%s] power %q, %v after the request: %s", c.name, h.State, h.On, ended, h.Text)
		}
		if want := otherEarly + otherAction("reboot", rebootAt); h.State != StLeftOn || h.On != OnYes || h.Text != want || ended > 63*time.Second {
			t.Errorf("the panel's clock %s: [%s] power %q, %v after the request (want at the first answers that show the reboot): %s", c.name, h.State, h.On, ended, h.Text)
		}
		if got := w.callsFor("pc-a"); got != "wake update shutdown" {
			t.Errorf("the panel's clock %s: requests %q", c.name, got)
		}
	}
}

// The same for the run's cancel (RC-04): the run is cancelled in the second in which the person's reboot is
// asked for. Its cancel must not go out and call that reboot off, whatever was done to the panel's clock.
func TestTheRunsCancelLeavesAPersonsRebootAloneWhenThePanelsClockWasSet(t *testing.T) {
	want := func(at time.Time) string {
		return "The run was cancelled by Bob: another power action is pending on it now (reboot at " + clock(at) + ", by the machine's own clock), not the shutdown the run asked for, " +
			"so the run sent no cancel: its agent's cancel would call that one off. It came online after the wake and it is ON now. Fairy no longer watches it. It was updated to 17-bbbbbbb."
	}
	for _, c := range clockSteps {
		w := newWorld(t)
		pc := w.off("pc-a")
		r := w.start(StartRequest{Hosts: []string{"pc-a"}})
		untilState(t, w, "pc-a", StShutting)
		w.advance(30 * time.Second)
		setPanelClock(w, pc, c.by)
		w.advance(30 * time.Second)
		at := w.now.Add(30 * time.Minute)
		pc.pending = &Pending{Action: "reboot", At: at}
		if _, err := w.m.CancelRun(r.ID, "Bob", false); err != nil {
			t.Fatal(err)
		}
		h := host(over(t, w), "pc-a")
		if strings.Contains(w.callsFor("pc-a"), "cancel") || pc.pending == nil {
			t.Errorf("the panel's clock %s: the run sent a cancel (requests: %s); the reboot a person asked for is still pending: %v: %s", c.name, w.callsFor("pc-a"), pc.pending != nil, h.Text)
		}
		if h.State != StLeftOn || h.On != OnYes || h.Text != want(at) {
			t.Errorf("the panel's clock %s: [%s] power %q: %s", c.name, h.State, h.On, h.Text)
		}
	}
	// Two seconds are enough when the request is younger than that: the reboot is asked for in the second of the
	// run's request, the clock is set back two seconds a second later, and the run is cancelled then. The answer
	// of the next second carries the time of day of the request itself.
	w := newWorld(t)
	pc := w.off("pc-a")
	r := w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	at := w.now.Add(30 * time.Minute)
	pc.pending = &Pending{Action: "reboot", At: at}
	w.tick() // the first answer that shows it: one is not enough before the countdown was seen
	setPanelClock(w, pc, -2*time.Second)
	at = at.Add(-2 * time.Second)
	if _, err := w.m.CancelRun(r.ID, "Bob", false); err != nil {
		t.Fatal(err)
	}
	h := host(over(t, w), "pc-a")
	if strings.Contains(w.callsFor("pc-a"), "cancel") || pc.pending == nil || h.Text != want(at) {
		t.Errorf("set back two seconds, one second after the request: requests %s, the reboot is still pending: %v: %s", w.callsFor("pc-a"), pc.pending != nil, h.Text)
	}
}

// And the older rules that ask the same question: "cancelled" (an answer without a countdown, from after the
// request), and the run's own countdown. With the clock set back an hour, 30 s into the countdown.
func TestCancelledAndCountingAreSeenWhenThePanelsClockWasSetBack(t *testing.T) {
	// a person calls the countdown off on the machine's page, 60 s in
	w := newWorld(t)
	pc := w.off("pc-a")
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	mono0 := w.mono
	w.advance(30 * time.Second)
	setPanelClock(w, pc, -time.Hour)
	w.advance(30 * time.Second)
	pc.pending = nil
	h := host(over(t, w), "pc-a")
	if ended := w.mono - mono0; h.State != StLeftOn || h.On != OnYes || h.Text != "Its shutdown was cancelled, in the panel or on the machine, so it is ON. It was updated to 17-bbbbbbb." || ended > 63*time.Second {
		t.Errorf("cancelled on the machine's page after the clock was set back: [%s] power %q, %v after the request: %s", h.State, h.On, ended, h.Text)
	}

	// the clock is set back in the second of the request, before any answer showed the countdown; a guest starts
	// 90 s before the end: the countdown is seen, and called off (the cancel is sent while it counts)
	w = newWorld(t)
	pc = w.off("pc-a")
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	setPanelClock(w, pc, -time.Hour)
	w.advance(5 * time.Second)
	if a := host(active(w), "pc-a"); !a.pendingSeen {
		t.Errorf("the run's own countdown is not seen in the answers after its request when the clock was set back in the second of the request")
	}
	w.advance(25 * time.Second)
	pc.guests, pc.guestNames = 1, "vm-1"
	h = host(over(t, w), "pc-a")
	if w.callsFor("pc-a") != "wake update shutdown cancel" || !pc.on || !strings.Contains(h.Text, "its shutdown countdown was called off. It came online after the wake and it is ON.") {
		t.Errorf("a guest starts after the clock was set back: requests %s, on %v: %s", w.callsFor("pc-a"), pc.on, h.Text)
	}

	// what must stay: nothing else happens, the machine goes off at the end of its countdown
	w = newWorld(t)
	pc = w.off("pc-a")
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	w.advance(30 * time.Second)
	setPanelClock(w, pc, -time.Hour)
	has(t, host(over(t, w), "pc-a"), StOff, "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.")
	if pc.on {
		t.Error("set-up: the machine must have gone off")
	}
}

// The last look: "an answer from now" is by the steady clock too. A run is cancelled while a machine it woke
// does not answer for some seconds; the look waits for it. The panel's clock is set back an hour meanwhile, and
// then the machine answers. Every answer after the set looked an hour old.
func TestTheLastLookSeesAnAnswerAfterThePanelsClockWasSetBack(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	r := w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StSettling)
	pc.unreachable = true
	w.tick()
	if _, err := w.m.CancelRun(r.ID, "Bob", false); err != nil {
		t.Fatal(err)
	}
	w.tick()
	if a := active(w); a == nil || host(a, "pc-a").Text != lookingText {
		t.Fatalf("set-up: the last look must be waiting for the machine: %s", describe(a))
	}
	setPanelClock(w, pc, -time.Hour)
	w.advance(3 * time.Second)
	pc.unreachable = false
	h := host(over(t, w), "pc-a")
	if want := "The run was cancelled by Bob. It came online after the wake and it is ON. It was updated to 17-bbbbbbb."; h.State != StLeftOn || h.On != OnYes || h.Text != want {
		t.Errorf("[%s] power %q: %s", h.State, h.On, h.Text)
	}
}
