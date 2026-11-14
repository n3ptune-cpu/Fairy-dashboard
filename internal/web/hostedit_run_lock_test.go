package web

import (
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// Whether a machine's own run timing is stored ("a decision") or not ("what counts anyway") is decided
// against the panel's timing that state.json holds at the moment of the write, inside the write's own lock.
// Another admin changes "Run timings" after this admin's request was looked at and confirmed, and before it
// is written:
//
//   - the number typed was the panel's and is not any more: it is stored, and it is what counts for the
//     machine afterwards ("the number you save is the number that counts");
//   - the number typed was another one and is the panel's now: nothing is stored, the machine follows the
//     panel, as it does for everybody who types a number that counts anyway.
func TestEditHostComparesARunTimingWithThePanelsAtTheMomentOfTheWrite(t *testing.T) {
	for _, c := range []struct {
		what        string
		typed, then int // the countdown typed for pc-a; what the other admin sets the panel's to meanwhile
		wantStored  string
		wantMessage string
	}{
		{"typed the panel's, which then changes", 90, 30, "shutdown_delay_s=90", "changes saved"},
		{"typed another, which the panel's then becomes", 45, 45, "", "nothing to change"},
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
		if how != "each" || steps != 0 {
			t.Fatalf("%s: answered %q (%d writes begun): %s", c.what, how, steps, r.raw)
		}
		if cb := a.confirm(pendingID(r), false); cb.code != 303 || steps != 1 {
			t.Fatalf("%s: the confirmation: %d, %d writes begun", c.what, cb.code, steps)
		}
		res, _ := e.do("GET", "/api/me", nil, "Cookie", a.sess).body["pending_result"].(map[string]any)
		if res["ok"] != true || res["message"] != c.wantMessage {
			t.Errorf("%s: answered %v", c.what, res)
		}
		if got := e.storedRun("pc-a"); got != c.wantStored {
			t.Errorf("%s: stored %q, want %q", c.what, got, c.wantStored)
		}
		// what counts for pc-a afterwards is the number that was typed, in both cases
		e.s.hostEditStep = nil
		if row := a.ehRun("pc-a", config.KeyShutdownDelay); row["value"] != float64(c.typed) || row["base"] != float64(c.then) {
			t.Errorf("%s: the dialog afterwards: %v", c.what, row)
		}
		if _, delay := e.s.Poller.WakeUpdateOverride("pc-a"); (c.wantStored != "") != (delay == c.typed) || (c.wantStored == "" && delay != 0) {
			t.Errorf("%s: pc-a's own countdown in force: %d", c.what, delay)
		}
	}
}
