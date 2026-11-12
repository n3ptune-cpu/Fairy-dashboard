package wakerun

import (
	"strings"
	"testing"
	"time"
)

// Second review of fix7a (R7A-01, R7A-02): "switched off again" for a machine that is on.
//
// A machine that was out of reach before its countdown can have ended is never called "off" by its silence
// afterwards (cutEarly). That must hold until the machine answers or shows a sign of life, and only until
// then: the run forgot it whenever the poller started a new row of silent polls, had no row at a look, or
// polling was paused for a moment. None of these is an answer.

// Polling of the machine is paused for a moment around the end of the countdown and taken up again. The link
// had dropped 30 s before the end, and the shutdown did not happen (the reviewer's proof).
func TestAPauseDoesNotForgetThatTheLinkDroppedBeforeTheEnd(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	pc.shutMode = "ignore" // accepted, the countdown runs out, the system stays up
	var due time.Time
	w.afterTick = func() {
		if pc.pending != nil {
			due = pc.pending.At
		}
		if !due.IsZero() {
			pc.unreachable = !w.now.Before(due.Add(-30 * time.Second))
			pc.paused = !w.now.Before(due.Add(-5*time.Second)) && w.now.Before(due.Add(5*time.Second))
		}
	}
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	r := over(t, w)
	h := host(r, "pc-a")
	if h.On == OnNo && pc.on {
		t.Errorf("REPORTED OFF, AND IT IS ON: %s", h.Text)
	}
	if h.State != StLeftOn || h.On != OnUnknown || !strings.Contains(h.Text, "It was already out of reach before its countdown ended") ||
		!strings.Contains(h.Text, "Not confirmed that it is off.") {
		t.Errorf("[%s] power %q, %v after the countdown's end: %s", h.State, h.On, r.EndedAt.Sub(due), h.Text)
	}
}
