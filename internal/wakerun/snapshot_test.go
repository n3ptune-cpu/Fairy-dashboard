package wakerun

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// "Run timings" (build 16): the timings can be changed in the panel while it runs. A run gets the values in
// force at its start as one snapshot (StartRequest.Settings) and keeps them: whatever the manager's settings,
// or a machine's own, are changed to while the run is going changes nothing of what this run does, says or
// sends. The next run takes what is in force then.

// hostile are settings somebody would want a run to have who wants it to switch more off, faster: the least
// the configuration allows for every time, and the most for the pacing.
func hostile() Settings { return NewSettings(60, 10, 10, 30).WithOffAfter(300) }

func TestARunKeepsTheTimingsItStartedWith(t *testing.T) {
	w := newWorld(t)
	// the settings in force at the start: off after 6 minutes, a countdown of 30 s, the rest the defaults
	atStart := NewSettings(300, 60, 30, 8).WithOffAfter(360)
	woken := off7(w, "pc-woken") // silent for seven minutes: off by the run's six
	slow := off7(w, "pc-slow")   // needs four minutes to start: inside the run's five, outside the hostile one
	slow.bootTakes = 4 * time.Minute
	own := off7(w, "pc-own") // a machine with a countdown of its own, read once at the start
	own.info.ShutdownDelay = 45 * time.Second
	var settledAt, shutAt = map[string]time.Time{}, map[string]time.Time{}
	w.during = func(call string) {
		if id, ok := strings.CutPrefix(call, "shutdown "); ok {
			shutAt[strings.Fields(id)[0]] = w.now
		}
	}
	r0 := w.start(StartRequest{Hosts: []string{"pc-woken", "pc-slow", "pc-own"}, Settings: &atStart})
	if r0.Settings != atStart {
		t.Fatalf("the run's settings at the start: %+v", r0.Settings)
	}
	// the change, right after the start and before the run's first step: the panel's timings and every
	// machine's own
	w.m.Settings = hostile()
	for _, mc := range []*machine{woken, slow, own} {
		mc.info.OffAfter, mc.info.ShutdownDelay = 59*time.Minute, 10*time.Second
	}
	w.afterTick = func() {
		a, _ := w.m.Snapshot()
		if a == nil {
			return
		}
		if a.Settings != atStart {
			t.Fatalf("the run's settings changed while it was going: %+v", a.Settings)
		}
		for _, h := range a.Hosts {
			if h.State == StSettling && settledAt[h.ID].IsZero() {
				settledAt[h.ID] = w.now
			}
		}
	}
	r := w.finish()
	w.afterTick = nil
	for _, id := range []string{"pc-woken", "pc-slow", "pc-own"} {
		has(t, host(r, id), StOff, "Came online after the wake", "and switched off again.")
	}
	// the countdowns: the run's 30 s, and the 45 s the one machine had when the run started; never the 10 s
	if got := strings.Join(shutdowns(w), ", "); !strings.Contains(got, "shutdown pc-woken 30") || !strings.Contains(got, "shutdown pc-slow 30") ||
		!strings.Contains(got, "shutdown pc-own 45") || strings.Contains(got, " 10") {
		t.Fatalf("shutdown requests: %s", got)
	}
	// the pause before the shutdown: the run's minute, not ten seconds
	for id, at := range shutAt {
		if settledAt[id].IsZero() || at.Sub(settledAt[id]) < 60*time.Second {
			t.Errorf("%s: the shutdown was asked for %s after the update's result, the run's pause is 60 s", id, at.Sub(settledAt[id]))
		}
	}
	// the report: the run's numbers, and the one machine's own
	if r.Settings != atStart || r.Settings.OffAfterS != 360 || r.Settings.ShutdownDelayS != 30 || r.Settings.OnlineTimeoutS != 300 || r.Settings.SettleS != 60 || r.Settings.PerAgentMin != 8 {
		t.Fatalf("the finished run's settings: %+v", r.Settings)
	}
	if h := host(r, "pc-own"); h.ShutdownDelayS == nil || *h.ShutdownDelayS != 45 || h.OffAfterS != nil {
		t.Errorf("pc-own's own timings in the report: %v %v", h.ShutdownDelayS, h.OffAfterS)
	}
	if h := host(r, "pc-woken"); h.ShutdownDelayS != nil || h.OffAfterS != nil {
		t.Errorf("pc-woken has timings of its own in the report: %v %v", h.ShutdownDelayS, h.OffAfterS)
	}
	// The next run, started without a snapshot, has the manager's settings (what tests and an older caller
	// rely on); one started with a snapshot has the snapshot.
	w.advance(20 * time.Minute)
	woken.info.OffAfter, woken.info.ShutdownDelay = 0, 0
	r2 := w.start(StartRequest{Hosts: []string{"pc-woken"}})
	if r2.Settings != hostile() {
		t.Fatalf("a run without a snapshot: %+v", r2.Settings)
	}
	w.finish()
}

func shutdowns(w *world) []string {
	var out []string
	for _, c := range w.calls {
		if strings.HasPrefix(c, "shutdown ") {
			out = append(out, c)
		}
	}
	return out
}

// The direction that matters most: a run that started when a machine counted as off after 15 minutes does not
// take a machine for off that has been silent for seven, whatever off_after is lowered to after the start; and
// a machine's own time that is lowered after the start does not count for this run either.
func TestLoweringOffAfterDuringARunDoesNotMakeItsMachinesCountAsOff(t *testing.T) {
	w := newWorld(t)
	atStart := NewSettings(300, 60, 120, 8) // off after the default 15 minutes
	off7(w, "pc-seven")
	mine := off7(w, "pc-mine")
	mine.info.OffAfter = 20 * time.Minute // its own, longer time at the start
	w.off("pc-off")                       // off for a day
	w.start(StartRequest{Hosts: []string{"pc-off", "pc-seven", "pc-mine"}, Settings: &atStart})
	w.m.Settings = hostile()             // the panel's: 5 minutes
	mine.info.OffAfter = 5 * time.Minute // the machine's own: 5 minutes
	r := w.finish()
	has(t, host(r, "pc-off"), StOff, "switched off again.")
	has(t, host(r, "pc-seven"), StSkipped, "Not sure that it is off, so nothing was sent to it: Fairy takes a machine for off only after 15 min without an answer")
	has(t, host(r, "pc-mine"), StSkipped, "Fairy takes a machine for off only after 20 min without an answer")
	if got := strings.Join(w.calls, ", "); strings.Contains(got, "pc-seven") || strings.Contains(got, "pc-mine") {
		t.Fatalf("something was sent to a machine the run could not take for off: %s", got)
	}
	// the same machines in a run that starts NOW, with the lowered values in force: they are off for it
	w.advance(time.Minute)
	low := hostile()
	w.start(StartRequest{Hosts: []string{"pc-seven", "pc-mine"}, Settings: &low})
	r = w.finish()
	has(t, host(r, "pc-seven"), StOff, "switched off again.")
	has(t, host(r, "pc-mine"), StOff, "switched off again.")
	if r.Settings.OffAfterS != 300 || r.Settings.ShutdownDelayS != 10 {
		t.Fatalf("the second run's settings: %+v", r.Settings)
	}
}

// The pacing is the run's too. Six machines behind one relay, two requests a minute at the start: the wakes
// go out two a minute to the end, also after the panel's pacing was raised to 30 during the run; the next run
// has the pacing in force at its own start, and counts what the run before it sent in the last minute.
func TestARunKeepsThePacingItStartedWith(t *testing.T) {
	w := newWorld(t)
	var ids []string
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("pc-%d", i)
		w.off(id)
		ids = append(ids, id)
	}
	var wakes []time.Time
	w.during = func(call string) {
		if strings.HasPrefix(call, "wake ") {
			wakes = append(wakes, w.now)
		}
	}
	perMinute := func() int { // the most wakes in any window the pacer counts as a minute
		most := 0
		for i := range wakes {
			n := 0
			for j := i; j < len(wakes) && wakes[j].Sub(wakes[i]) < paceWindow; j++ {
				n++
			}
			most = max(most, n)
		}
		return most
	}
	slow := NewSettings(300, 60, 120, 2)
	w.start(StartRequest{Hosts: ids, Settings: &slow})
	w.m.Settings = hostile() // 30 a minute
	w.advance(10 * time.Second)
	if len(wakes) != 2 {
		t.Fatalf("%d wakes in the first seconds of a run with two requests a minute", len(wakes))
	}
	r := w.finish()
	if len(wakes) < 6 || perMinute() != 2 || r.Settings.PerAgentMin != 2 {
		t.Fatalf("%d wakes, at most %d in a minute, the run's pacing %d (want two a minute to the end)", len(wakes), perMinute(), r.Settings.PerAgentMin)
	}
	for _, id := range ids {
		has(t, host(r, id), StOff, "switched off again.")
	}
	// the next run: the pacing in force at its start
	w.advance(30 * time.Minute)
	wakes = nil
	fast := NewSettings(300, 60, 120, 6)
	w.start(StartRequest{Hosts: ids, Settings: &fast})
	w.advance(10 * time.Second)
	if len(wakes) != 6 {
		t.Fatalf("%d wakes in the first seconds of a run with six requests a minute", len(wakes))
	}
	w.finish()
	if w.limited != 0 {
		t.Fatalf("an agent refused %d requests as too many", w.limited)
	}
	// setLimit never goes below one request a minute
	p := newPacer(8)
	p.setLimit(0)
	if p.limit != 1 {
		t.Fatalf("a limit of 0: %d", p.limit)
	}
}
