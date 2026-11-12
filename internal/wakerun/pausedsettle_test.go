package wakerun

import (
	"testing"
	"time"
)

// Audit 6, r4-p2-07 (a note): polling is paused for a machine while the run waits out the settle time. From
// then on the run reads the last poll before the pause: "its agent answers, nothing pending, no update being
// installed". mayShutDown decided on that, and the shutdown was asked of a machine on which, meanwhile, someone
// had started an update. The run now leaves a machine on when it can no longer see it at that moment.
func TestPollingPausedBeforeTheShutdownRequestLeavesTheMachineOn(t *testing.T) {
	for _, installing := range []bool{false, true} {
		w := newWorld(t)
		pc := w.off("pc-a")
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		untilState(t, w, "pc-a", StSettling)
		w.advance(20 * time.Second)
		pc.paused = true
		if installing { // ... and someone starts an update on the machine, which the panel no longer sees
			pc.updating, pc.updDoneAt, pc.updState = true, w.now.Add(10*time.Minute), "downloading"
		}
		h := host(w.finish(), "pc-a")
		want := "Left ON: polling was paused for it during the run, so Fairy does not see how it is now and did not ask it to shut down. " +
			"It came online after the wake. It was updated to 17-bbbbbbb. Fairy can no longer look at it."
		if h.State != StLeftOn || h.On != OnUnknown || h.Text != want || !pc.on {
			t.Errorf("an update running: %v: [%s] power %q, the machine on: %v: %s", installing, h.State, h.On, pc.on, h.Text)
		}
		if got := w.callsFor("pc-a"); got != "wake update" {
			t.Errorf("an update running: %v: requests %q, want no shutdown", installing, got)
		}
	}
}
