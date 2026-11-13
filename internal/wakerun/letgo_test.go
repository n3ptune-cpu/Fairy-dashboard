package wakerun

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Review of build 14 (F4). A run that lets go of a machine while the shutdown IT asked for is counting down
// (the machine was paired again, or switched off for these runs) only said "it switches off by itself unless
// someone cancels that" and went. Nobody watched the machine any more: a node went off under a guest that
// had started minutes before, with all of the countdown left in which the run could have called it off
// (TestRandomRunsWithLinkFlaps, seeds 50387, 52167, 1235914, 1237289 and 90002655).
//
// The run now calls its own countdown off before it goes, as when the run is cancelled.

// letGoWhile starts a run for pc-a, takes it to its countdown, and lets do change the machine.
func letGoWhile(t *testing.T, do func(pc *machine)) (*world, *machine, int) {
	t.Helper()
	w := newWorld(t)
	pc := w.off("pc-a")
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	if pc.pending == nil {
		t.Fatal("the fixture: no countdown on the machine")
	}
	before := len(w.calls)
	do(pc)
	return w, pc, before
}

var letGoCases = map[string]func(pc *machine){
	"It was paired again during the run (its entry in the panel was replaced), so the run let go of it": func(pc *machine) { pc.info.Entry++ },
	"It was switched off for \"Update unpowered hosts\" during the run, so the run let go of it":        func(pc *machine) { pc.info.OptedIn = false },
}

func TestARunCallsItsOwnCountdownOffBeforeItLetsGo(t *testing.T) {
	for why, do := range letGoCases {
		w, pc, before := letGoWhile(t, do)
		h := host(w.finish(), "pc-a")
		if got := strings.Join(w.calls[before:], ", "); got != "cancel pc-a" {
			t.Fatalf("%s: sent after it: %q, want one cancel", why, got)
		}
		has(t, h, StLeftOn, why+": its shutdown countdown was called off. It came online after the wake and it is ON.")
		if h.On != OnYes || !pc.on || pc.pending != nil || pc.wentOff {
			t.Fatalf("%s: reported %s; the machine is on: %v, a countdown is pending: %v", why, h.On, pc.on, pc.pending != nil)
		}
		// and it stays on: nothing switches it off after the run
		w.advance(10 * time.Minute)
		if !pc.on {
			t.Fatalf("%s: the machine went off after the run", why)
		}
	}
}

// The cancel does not get through: it is tried again until the run's own due time, the machine's row says so
// meanwhile, and the report does not say "it is ON" and nothing more. With the machine out of reach (no cancel
// arrives, the countdown runs out on the machine) it says that the countdown could not be called off and that
// the machine was asked to switch off and does not answer; with cancels whose answers are lost while the
// machine still answers, that it may switch off at the due time, and where to cancel that.
func TestALetGoWhoseCancelDoesNotGetThroughSaysWhatMayHappen(t *testing.T) {
	for why, do := range letGoCases {
		for _, c := range []struct {
			name   string
			break_ func(pc *machine)
			power  string
			want   []string
		}{
			{"out of reach", func(pc *machine) { pc.unreachable = true }, OnUnknown,
				[]string{why + ", but its shutdown countdown (due at ", ") could not be called off (", "Asked to switch off, not confirmed: it does not answer now."}},
			{"no answer to any cancel", func(pc *machine) { pc.cancelLost = "always arrives" }, OnYes,
				[]string{why + ", and the request to call its shutdown countdown off got no answer (", "): it may switch off at ", "Cancel it on the machine's page."}},
		} {
			w, _, before := letGoWhile(t, func(pc *machine) {
				do(pc)
				c.break_(pc)
			})
			due := *host(mustActive(t, w), "pc-a").ShutdownAt
			w.tick()
			if a, _ := w.m.Snapshot(); a == nil || !strings.Contains(strings.Join(logTexts(host(a, "pc-a")), "\n"), "could not be called off yet") {
				t.Fatalf("%s, %s: the row does not say that the cancel is being tried again", why, c.name)
			}
			h := host(w.finish(), "pc-a")
			cancels := 0
			for _, call := range w.calls[before:] {
				if call != "cancel pc-a" {
					t.Fatalf("%s, %s: sent after it: %v", why, c.name, w.calls[before:])
				}
				cancels++
			}
			if cancels < 2 {
				t.Fatalf("%s, %s: %d cancel(s), want it tried again until the due time", why, c.name, cancels)
			}
			if h.State != StLeftOn || h.On != c.power || !strings.Contains(h.Text, clock(due)) || strings.Contains(h.Text, "it is ON") {
				t.Fatalf("%s, %s: [%s/%s] %s", why, c.name, h.State, h.On, h.Text)
			}
			for _, part := range c.want {
				if !strings.Contains(h.Text, part) {
					t.Fatalf("%s, %s: the report lacks %q: %s", why, c.name, part, h.Text)
				}
			}
		}
	}
}

// A machine that is no longer in the panel at all cannot be asked anything: as before, nothing is sent, and
// the report says that its countdown was running and that Fairy can no longer look at it.
func TestAMachineRemovedDuringItsCountdownIsReportedAsBefore(t *testing.T) {
	w, _, before := letGoWhile(t, func(*machine) {})
	w.ms["gone"] = w.ms["pc-a"]
	delete(w.ms, "pc-a")
	w.m.Tick(context.Background())
	_, hist := w.m.Snapshot()
	if len(hist) != 1 {
		t.Fatal("the run should be over")
	}
	h := host(hist[0], "pc-a")
	if len(w.calls) != before || !strings.Contains(h.Text, "It is no longer in the panel, so the run let go of it while its shutdown was counting down (due at ") ||
		!strings.Contains(h.Text, "Fairy can no longer look at it.") {
		t.Fatalf("sent %v: [%s] %s", w.calls[before:], h.State, h.Text)
	}
}

func mustActive(t *testing.T, w *world) *Run {
	t.Helper()
	a, _ := w.m.Snapshot()
	if a == nil {
		t.Fatal("no run is going")
	}
	return a
}
