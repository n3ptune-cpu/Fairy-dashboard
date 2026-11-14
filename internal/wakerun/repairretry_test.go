package wakerun

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Fourth review of build 14: two changed copies of the program that the tests let through, both in what
// callOff does when the wait after a Re-pair ends (the agent that has the id now has shown the run's own
// countdown: it is the machine the run woke). A cancel that was being tried again before the Re-pair goes on
// from where it was: what was sent before still counts, and the next try is made at once. Each change was
// applied by hand, seen to fail the test named for it, and taken out. No change of the program.

// retriedThenPairedAgain is a machine whose countdown the run tries to call off after "Cancel run", without
// success, and that is paired again (the same machine) right after one of the tries; its agent does not
// answer under the new entry yet. It returns how many cancels went out before the Re-pair.
func retriedThenPairedAgain(t *testing.T, cancelLost string) (*world, *machine, int) {
	t.Helper()
	w := newWorld(t)
	pc := w.off("pc-a")
	pc.cancelLost = cancelLost
	r := w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	w.advance(3 * time.Second)
	pc.unreachable = true
	if _, err := w.m.CancelRun(r.ID, "Bob", false); err != nil {
		t.Fatal(err)
	}
	untilCancel(t, w)
	untilCancel(t, w) // right after the second try
	tries := cancels(w)
	pc.info.Entry++ // Re-pair, the same machine
	w.tick()
	row := host(mustActive(t, w), "pc-a")
	if cancels(w) != tries || row.calloff == nil || !row.calloff.idWait || row.calloff.tries != tries ||
		!logHas(row, fmt.Sprintf("no further cancel is sent (%d requests went out before) until the agent that has this id now shows that countdown", tries)) {
		t.Fatalf("the fixture: %d cancels (%d before the Re-pair), waiting: %v: %v", cancels(w), tries, row.calloff != nil && row.calloff.idWait, logTexts(row))
	}
	return w, pc, tries
}

// steps.go, "c.tries == 0" made "true": after the wait the cancel started over as if nothing had been sent.
// What was sent before the Re-pair is in the sentence at the end: here a person's own shutdown shows in the
// place of the run's after one more try, and the sentence says how many cancels did not get through, all of
// them. The history says once that the cancel is being tried again, not a second time after the wait.
func TestAfterTheWaitForARePairTheCancelsSentBeforeItStillCount(t *testing.T) {
	w, pc, tries := retriedThenPairedAgain(t, "always") // no cancel ever arrives, none is answered
	pc.unreachable = false
	w.tick() // its agent answers under the new entry and shows the run's countdown: the wait is over
	if cancels(w) != tries+1 || pc.pending == nil {
		t.Fatalf("the fixture: %d cancels after the wait (%d before), a countdown is pending: %v", cancels(w), tries, pc.pending != nil)
	}
	mine := &Pending{Action: "shutdown", At: w.now.Add(15 * time.Minute), DelayS: 900}
	pc.pending = mine // a person: the run's shutdown called off, and their own asked for
	h := host(w.finish(), "pc-a")
	if pc.pending != mine || cancels(w) != tries+1 {
		t.Errorf("the person's shutdown is pending: %v, %d cancels (want %d)", pc.pending == mine, cancels(w), tries+1)
	}
	has(t, h, StLeftOn, fmt.Sprintf("Someone asked for it in the place of the run's, so the run sent no further cancel (%d did not get through): its agent's cancel would call that one off.", tries+1))
	if n := strings.Count(strings.Join(logTexts(h), "\n"), "could not be called off yet"); n != 1 {
		t.Errorf("the history says %d times that the cancel is being tried again, want once: %v", n, logTexts(h))
	}
}

// steps.go, "c.lastMono = mono - cancelRetry" taken out: after the wait the next try came when the 10 s
// since the last one were over, up to 10 s later, while the countdown ran on. It is made in the step in which
// the agent of the new entry shows the run's countdown.
func TestAfterTheWaitForARePairTheCancelIsTriedAgainAtOnce(t *testing.T) {
	w, pc, tries := retriedThenPairedAgain(t, "")
	pc.unreachable = false
	w.tick() // its agent answers under the new entry and shows the run's countdown
	if cancels(w) != tries+1 || pc.pending != nil {
		t.Fatalf("one second after the agent of the new entry showed the run's countdown: %d cancels (%d before the Re-pair), the countdown is still pending: %v", cancels(w), tries, pc.pending != nil)
	}
	h := host(w.finish(), "pc-a")
	has(t, h, StLeftOn, "The run was cancelled by Bob: its shutdown countdown was called off. It came online after the wake and it is ON.")
	w.advance(5 * time.Minute)
	if h.On != OnYes || !pc.on || pc.wentOff || cancels(w) != tries+1 {
		t.Errorf("power %q, the machine on: %v, %d cancels (want %d): %s", h.On, pc.on, cancels(w), tries+1, h.Text)
	}
}
