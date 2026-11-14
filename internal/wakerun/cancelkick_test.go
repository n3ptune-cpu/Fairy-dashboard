package wakerun

import (
	"strings"
	"testing"
	"time"
)

// Fourth review of build 14, F2. A cancel that is tried again decides at every try by what the machine's agent
// shows as pending: no cancel while another action than the run's shutdown shows (callOff). What it shows is
// the answer of the panel's latest poll, and while the run only repeats its cancel nobody asked the poller to
// poll: the watch does that every 5 s (stepShutting), a run that was stopped never comes there again. A machine
// that is polled once a minute, or whose failed polls were put off, was looked at by an answer up to a minute
// old, and the cancel went out every 10 s: whether a person's own shutdown was seen in time was a matter of
// when the poll happened to run. The poller is now asked to poll the machine every 5 s for as long as a cancel
// is being tried again.

// untilCancel ticks the world until the run has sent one more cancel.
func untilCancel(t *testing.T, w *world) {
	t.Helper()
	for n, i := cancels(w), 0; cancels(w) == n; i++ {
		if i > 60 {
			t.Fatalf("no further cancel in a minute (%d so far): %s", n, describe(active(w)))
		}
		w.tick()
	}
}

func TestWhileACancelIsRepeatedThePollerIsAskedToPoll(t *testing.T) {
	w, pc := lostAndStopped(t, stops[0].stop)
	pc.pollEvery = time.Minute
	w.advance(12 * time.Second)
	asked, sent := w.kicks["pc-a"], cancels(w)
	w.advance(21 * time.Second)
	if n := w.kicks["pc-a"] - asked; n < 4 || n > 6 || cancels(w) <= sent { // every kickEvery (5 s)
		t.Errorf("in 21 s of repeating its cancel the run asked the poller %d times to poll the machine (%d cancels, %d before)", n, cancels(w), sent)
	}
}

// The request never arrives, the run is stopped and repeats its cancel, and the machine is polled once a
// minute. A person asks for a shutdown in 1 minute right after one of the cancels. The next cancel came 10 s
// later, decided by an answer from before the person asked, and called their shutdown off.
func TestTheRepeatedCancelSeesAPersonsShutdownOnAMachineThatIsPolledSeldom(t *testing.T) {
	for _, c := range stops {
		w, pc := lostAndStopped(t, c.stop)
		pc.pollEvery = time.Minute
		w.advance(12 * time.Second)
		untilCancel(t, w)
		sent := cancels(w)
		mine := &Pending{Action: "shutdown", At: w.now.Add(60 * time.Second), DelayS: 60}
		pc.pending = mine // a person, on the machine's page
		for i := 0; i < 58; i++ {
			w.tick()
			if pc.pending != mine || cancels(w) != sent {
				t.Fatalf("%s: %d s after the person asked: their shutdown is pending: %v, %d cancel(s) (%d before): A PERSON'S SHUTDOWN WAS CALLED OFF", c.name, i+1, pc.pending == mine, cancels(w), sent)
			}
		}
		h := host(w.finish(), "pc-a")
		if !pc.wentOff || h.On != OnUnknown || strings.Contains(h.Text, "none is pending") ||
			!strings.Contains(h.Text, "A shutdown was pending on the machine meanwhile that is not the one the run asked for (a countdown of 60 s, due at "+clock(mine.At)) {
			t.Errorf("%s: the machine went off by the person's shutdown: %v; power %q: %s", c.name, pc.wentOff, h.On, h.Text)
		}
	}
}

// The same for a cancel that did not get through, inside a countdown the run has seen: the first cancel gets
// no answer and does not arrive, and a person calls the run's shutdown off on the machine's page and asks for
// their own, 15 minutes on. The second try came 10 s later, decided by an answer that still showed the run's
// countdown, and called the person's shutdown off; the report said "its shutdown countdown was called off".
func TestACancelThatIsTriedAgainSeesAPersonsShutdownOnAMachineThatIsPolledSeldom(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	pc.cancelLost = "never"
	r := w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	w.advance(5 * time.Second)
	pc.pollEvery = time.Minute
	w.advance(5 * time.Second)
	if _, err := w.m.CancelRun(r.ID, "Bob", false); err != nil {
		t.Fatal(err)
	}
	untilCancel(t, w) // the one that gets no answer
	mine := &Pending{Action: "shutdown", At: w.now.Add(15 * time.Minute), DelayS: 900}
	pc.pending = mine // the person: the run's shutdown called off, and their own asked for
	h := host(w.finish(), "pc-a")
	if pc.pending != mine || cancels(w) != 1 {
		t.Errorf("A PERSON'S SHUTDOWN WAS CALLED OFF: it is pending: %v, %d cancel(s): %s", pc.pending == mine, cancels(w), h.Text)
	}
	has(t, h, StLeftOn, "The run was cancelled by Bob: a shutdown is pending on it now that is not the one the run asked for (a countdown of 15 min that ends at "+clock(mine.At)+
		", by the machine's own clock; the run asked for one of 2 min). Someone asked for it in the place of the run's, so the run sent no further cancel (1 did not get through)")
}
