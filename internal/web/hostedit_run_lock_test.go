package web

import (
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// Whether a machine's own run timing is stored ("a decision") or not ("what counts anyway") is decided
// against the panel's timing that state.json holds at the moment of the write, inside the write's own lock.
// Another admin changes "Run timings" in the instant after this admin's confirmed request was looked at and
// before it is written:
//
//   - the number typed was the panel's (nothing to change, the confirmation had no line) and is not any
//     more: the write would now store it as a decision;
//   - the number typed was another one (the confirmation said so) and is the panel's now: the write would
//     now store nothing.
//
// Either way the write would not do what was confirmed, and it is seen inside the lock: nothing is saved,
// and the admin is told to look again. A save that made its base before the lock stored the number that
// counted anyway in the one case and dropped the decision in the other, without a word.
func TestEditHostComparesARunTimingWithThePanelsAtTheMomentOfTheWrite(t *testing.T) {
	for _, c := range []struct {
		what        string
		typed, then int // the countdown typed for pc-a; what the other admin sets the panel's to meanwhile
		wantItems   int
	}{
		{"typed the panel's, which then changes", 90, 30, 0},
		{"typed another, which the panel's then becomes", 45, 45, 2},
	} {
		a := signedIn(t)
		e := a.e
		newFarm(t, e)
		e.rtSet(map[string]int{config.KeyShutdownDelay: 90})
		if row := a.ehRun("pc-a", config.KeyShutdownDelay); !ehRunIs(row, 90, "run", 90, "run") {
			t.Fatalf("%s: before: %v", c.what, row)
		}
		steps := 0
		e.s.hostEditStep = func(step string) {
			if step == "planned" {
				steps++
				e.rtSet(map[string]int{config.KeyShutdownDelay: c.then}) // the other admin's change, between the look and the write
			}
		}
		how, r := a.ehAsk("pc-a", map[string]any{"wake_update": map[string]any{"shutdown_delay_s": c.typed}})
		d, _ := r.body["detail"].(map[string]any)
		if how != "each" || steps != 0 || len(strs(d["items"])) != c.wantItems {
			t.Fatalf("%s: answered %q (%d writes begun): %s", c.what, how, steps, r.raw)
		}
		if cb := a.confirm(pendingID(r), false); cb.code != 303 || steps != 1 {
			t.Fatalf("%s: the confirmation: %d, %d writes begun", c.what, cb.code, steps)
		}
		if res := a.rtResult(); !hoRefused(res) {
			t.Errorf("%s: answered %v", c.what, res)
		}
		if got := e.storedRun("pc-a"); got != "" {
			t.Errorf("%s: stored %q, want nothing", c.what, got)
		}
		e.s.hostEditStep = nil
		if row := a.ehRun("pc-a", config.KeyShutdownDelay); !ehRunIs(row, c.then, "run", c.then, "run") {
			t.Errorf("%s: the dialog afterwards: %v", c.what, row)
		}
	}
}
