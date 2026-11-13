package wakerun

import (
	"strings"
	"testing"
	"time"
)

// Review of fix8, RF8-05. An install that begins while the shutdown countdown runs: the run calls its
// countdown off, so the machine does not go off in the middle of it. That look was there only after an update
// request that had got no answer, and it always said "its update request, which had got no answer, arrived
// after all", also when the run had seen its own update finish: the result "updated to 17-bbbbbbb" was lost
// from the row. After a request that was answered nothing looked, and the machine went off in the install.

// anotherUpdate starts an update on the machine that is not the run's, 30 s into the countdown.
func anotherUpdate(t *testing.T, w *world, pc *machine) *Host {
	t.Helper()
	untilState(t, w, "pc-a", StShutting)
	w.advance(30 * time.Second)
	pc.updating, pc.updDoneAt, pc.updState, pc.updMsg = true, w.now.Add(5*time.Minute), "downloading", "downloading 18-ccccccc"
	return host(w.finish(), "pc-a")
}

func TestAnUpdateDuringTheCountdownCallsItOffAndKeepsTheRunsOwnResult(t *testing.T) {
	for name, lost := range map[string]string{"the run's update request was answered": "", "its answer was lost, and the run saw the update finish": "arrives"} {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.updLost = lost
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		h := anotherUpdate(t, w, pc)
		want := "Left ON: its agent says that an update is being installed on it right now (downloading): its shutdown countdown was called off. " +
			"It came online after the wake and it is ON. It was updated to 17-bbbbbbb."
		if h.State != StLeftOn || h.On != OnYes || h.Text != want {
			t.Errorf("%s: [%s] power %q: %s", name, h.State, h.On, h.Text)
		}
		if strings.Contains(h.Text, "arrived after all") || h.Update != UpdOK {
			t.Errorf("%s: the row says that the run's update request arrived late, or lost its result (%q): %s", name, h.Update, h.Text)
		}
		w.advance(10 * time.Minute)
		if !pc.on || pc.wentOff || oneCancel(w.callsFor("pc-a")) != "wake update shutdown cancel" {
			t.Errorf("%s: the machine went off while it installed (on: %v), requests %q", name, pc.on, w.callsFor("pc-a"))
		}
	}
}

// The words "arrived after all" are for the one case they are true of: the request got no answer and the run
// went on without having seen an update (TestAnUpdateRequestThatArrivesLate holds that sentence).
func TestArrivedAfterAllIsSaidOnlyOfARequestTheRunNeverSawArrive(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	pc.updLost, pc.updLate, pc.updTakes = "late", 150*time.Second, 5*time.Minute
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	h := host(w.finish(), "pc-a")
	if !strings.Contains(h.Text, "Left ON: its update request, which had got no answer, arrived after all, and its agent says that an update is being installed on it right now (downloading)") ||
		!strings.Contains(h.Text, "Its update was asked for and may still be running") || !pc.on {
		t.Errorf("[%s]: %s", h.State, h.Text)
	}
}
