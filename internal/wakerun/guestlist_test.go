package wakerun

import (
	"strings"
	"testing"
	"time"
)

// Audit 6, r4-p2-01. What the panel knows about the guests of a Proxmox node is the list of its latest poll of
// the Proxmox API, and that API may be polled as seldom as every five minutes. The run asked "which guests run
// on it" of whatever list was there: one from shortly after the node's start showed its guest as stopped, the
// guest started seconds later, and the run switched the node off under it. Now the run asks for a new poll
// before it decides, takes only a list that was asked for after its own question, and leaves the node on when
// none comes. The world's node here has such a list (machine.list): Guests answers from it, however old it is,
// so what is tested is the run's own rule.

// slowNode is a node whose Proxmox API is polled every five minutes.
func slowNode(w *world) *machine {
	pc := w.off("pve1")
	pc.list, pc.listEvery = true, 5*time.Minute
	return pc
}

// startsAt lets a guest start on the node at a moment of the run's own steady clock.
func guestStartsWhen(w *world, pc *machine, when func() bool) {
	w.afterTick = func() {
		if pc.guests == 0 && when() {
			pc.guests, pc.guestNames = 1, "win11"
		}
	}
}

// The auditor's case: the list of right after the node's start shows no guest, the guest starts ten seconds
// later, and the API's next poll of its own is minutes away.
func TestAGuestListFromMinutesAgoIsNotWhatTheRunDecidesOn(t *testing.T) {
	w := newWorld(t)
	pc := slowNode(w)
	w.start(StartRequest{Hosts: []string{"pve1"}})
	untilState(t, w, "pve1", StOnline)
	pc.listKick = true // the API's poll of its own falls right after the node's first answer: no guest runs yet
	w.tick()
	if !pc.listHas.ok || pc.listHas.guests != 0 {
		t.Fatalf("the set-up: the panel must have a list without a running guest: %+v", pc.listHas)
	}
	began := w.mono
	guestStartsWhen(w, pc, func() bool { return w.mono-began >= 10*time.Second })
	polls := pc.listPolls
	h := host(w.finish(), "pve1")
	has(t, h, StLeftOn, "Left ON: 1 guest is running on it (win11).", "It was updated to 17-bbbbbbb.")
	if got := w.callsFor("pve1"); got != "wake update" {
		t.Errorf("requests: %q, want no shutdown", got)
	}
	if !pc.on || pc.wentOff {
		t.Error("the node went off under its guest")
	}
	if pc.listPolls == polls {
		t.Error("the run did not ask for a new poll of the Proxmox API")
	}
}

// The normal case with a slowly polled API: no guest runs. The run asks for a new list, gets it at the next
// poll, and the node is updated and switched off again, a second later than without the question.
func TestANodeWithoutGuestsIsSwitchedOffAgainWithASlowlyPolledAPI(t *testing.T) {
	for _, takes := range []time.Duration{0, 3 * time.Second} {
		w := newWorld(t)
		pc := slowNode(w)
		pc.listTakes = takes
		w.start(StartRequest{Hosts: []string{"pve1"}})
		untilState(t, w, "pve1", StSettling)
		settling := w.mono
		untilState(t, w, "pve1", StShutting)
		if waited := w.mono - settling; waited < 60*time.Second || waited > 62*time.Second+takes {
			t.Errorf("a poll takes %v: the shutdown was asked for %v after the update, want the settle time (60 s) and one poll", takes, waited)
		}
		h := host(w.finish(), "pve1")
		has(t, h, StOff, "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.")
		if got := w.callsFor("pve1"); got != "wake update shutdown" {
			t.Errorf("a poll takes %v: requests %q", takes, got)
		}
	}
}

// No list comes: the API stops answering before the run asks, or answers too slowly. The run waits half a
// minute, and the node is left on. Nothing is sent.
func TestNoNewGuestListInTimeLeavesTheNodeOn(t *testing.T) {
	for _, c := range []struct {
		name string
		prep func(pc *machine)
		want string
	}{
		{"the API does not answer", func(pc *machine) { pc.listDown = true }, "Left ON: Fairy cannot see whether guests run on it (its Proxmox API does not answer)."},
		{"the API takes a minute to answer", func(pc *machine) { pc.listTakes = time.Minute }, "Left ON: Fairy cannot see whether guests run on it (the panel got no current list of its guests from its Proxmox API)."},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			pc := slowNode(w)
			w.start(StartRequest{Hosts: []string{"pve1"}})
			untilState(t, w, "pve1", StSettling)
			settling := w.mono
			c.prep(pc)
			h := host(w.finish(), "pve1")
			has(t, h, StLeftOn, c.want, "It was updated to 17-bbbbbbb.")
			if h.On != OnYes || !pc.on {
				t.Errorf("power %q, the node on: %v", h.On, pc.on)
			}
			if got := w.callsFor("pve1"); got != "wake update" {
				t.Errorf("requests: %q, want no shutdown", got)
			}
			if waited := w.mono - settling; waited < 90*time.Second || waited > 95*time.Second {
				t.Errorf("the run let go %v after the update, want the settle time (60 s) and a wait of 30 s", waited)
			}
		})
	}
}

// A poll that was on its way when the run asked does not count, although its answer comes after the question:
// it read the cluster before. Here its request left two seconds before the settle time was over, the guest
// started one second after that, and the answer (no guest) arrives two seconds into the run's wait.
func TestAPollThatLeftBeforeTheRunAskedIsNotTheNewList(t *testing.T) {
	w := newWorld(t)
	pc := slowNode(w)
	pc.listTakes = 4 * time.Second
	w.start(StartRequest{Hosts: []string{"pve1"}})
	untilState(t, w, "pve1", StSettling)
	w.advance(57 * time.Second)
	pc.listKick = true // the API's own poll: its request leaves at the next second, 58 s into the settle time
	w.tick()
	pc.guests, pc.guestNames = 1, "win11" // the guest starts right after that request left
	if pc.listFly == nil || pc.listFly.guests != 0 {
		t.Fatalf("the set-up: a poll without the guest must be on its way: %+v", pc.listFly)
	}
	h := host(w.finish(), "pve1")
	has(t, h, StLeftOn, "Left ON: 1 guest is running on it (win11).")
	if got := w.callsFor("pve1"); got != "wake update" {
		t.Errorf("requests: %q, want no shutdown", got)
	}
}

// During the countdown the run asks about the guests every five seconds: each of those looks asks for a new
// poll, so a guest that starts on a node whose API is polled every five minutes is seen, and the countdown is
// called off. And a list that stops coming is "cannot see its guests" after two looks.
func TestTheGuestListIsReadAnewDuringTheCountdown(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(pc *machine)
		want   string
	}{
		{"a guest starts 30 s into the countdown", func(pc *machine) { pc.guests, pc.guestNames = 1, "win11" },
			"Left ON: 1 guest is running on it (win11) since its shutdown was asked for: its shutdown countdown was called off."},
		{"the API stops answering 30 s into the countdown, and a guest starts", func(pc *machine) { pc.listDown, pc.guests = true, 1 },
			"Left ON: Fairy cannot see whether guests run on it (its Proxmox API does not answer) since its shutdown was asked for: its shutdown countdown was called off."},
		{"the API gets slow 30 s into the countdown (a minute for an answer), and a guest starts", func(pc *machine) { pc.listTakes, pc.guests = time.Minute, 1 },
			"Left ON: Fairy cannot see whether guests run on it (the panel got no current list of its guests from its Proxmox API) since its shutdown was asked for: its shutdown countdown was called off."},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			pc := slowNode(w)
			w.start(StartRequest{Hosts: []string{"pve1"}})
			untilState(t, w, "pve1", StShutting)
			w.advance(30 * time.Second)
			c.change(pc)
			h := host(w.finish(), "pve1")
			has(t, h, StLeftOn, c.want)
			if got := oneCancel(w.callsFor("pve1")); got != "wake update shutdown cancel" {
				t.Errorf("requests: %q", got)
			}
			if !pc.on || pc.wentOff {
				t.Error("the node went off under its guest")
			}
		})
	}
}

// One failed poll of the API during the countdown is still not a guest (as before): the next list is there
// again, and the node goes off.
func TestOneFailedPollOfTheAPIDuringTheCountdownIsNotAGuest(t *testing.T) {
	w := newWorld(t)
	pc := slowNode(w)
	w.start(StartRequest{Hosts: []string{"pve1"}})
	untilState(t, w, "pve1", StShutting)
	w.advance(30 * time.Second)
	pc.listDown = true
	polls := pc.listPolls
	w.afterTick = func() {
		if pc.listPolls > polls {
			pc.listDown = false
		}
	}
	h := host(w.finish(), "pve1")
	has(t, h, StOff, "and switched off again.")
	if got := w.callsFor("pve1"); got != "wake update shutdown" {
		t.Errorf("requests: %q", got)
	}
}

// A machine that is no node of a Proxmox API is not kept waiting, and nothing is asked for it: its shutdown is
// asked for when the settle time is over, as before.
func TestAnOrdinaryMachineDoesNotWaitForAGuestList(t *testing.T) {
	w := newWorld(t)
	w.off("pc-a")
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StSettling)
	settling := w.mono
	untilState(t, w, "pc-a", StShutting)
	if waited := w.mono - settling; waited != 60*time.Second {
		t.Errorf("the shutdown was asked for %v after the update, want the settle time (60 s)", waited)
	}
	if h := host(active(w), "pc-a"); strings.Contains(strings.Join(logTexts(h), "\n"), "Proxmox") {
		t.Errorf("the history of an ordinary machine speaks of a Proxmox API: %v", logTexts(h))
	}
}

func logTexts(h *Host) []string {
	var out []string
	for _, l := range h.Log {
		out = append(out, l.Text)
	}
	return out
}
