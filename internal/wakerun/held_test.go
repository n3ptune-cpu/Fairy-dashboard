package wakerun

import (
	"strings"
	"testing"
	"time"
)

// The run holds the timings it is handed to its own ranges, once, when it starts: a time after which a
// machine counts as off below five minutes, a countdown outside 10 s to an hour and a pacing outside 1 to
// 30 a minute are never used; the default counts instead. Everything in the range is taken as it is.
func TestARunHoldsItsTimingsToItsOwnRanges(t *testing.T) {
	for _, c := range []struct {
		offAfter, delay, pace             int // handed over
		wantOffAfter, wantDelay, wantPace int // what the run uses
	}{
		{360, 30, 2, 360, 30, 2},
		{300, 10, 1, 300, 10, 1},
		{3600, 3600, 30, 3600, 3600, 30},
		{299, 9, 0, 900, 120, 8},
		{5, 1, 100000, 900, 120, 8},
		{0, 0, -1, 900, 120, 8},
		{-60, -1, 31, 900, 120, 8},
		{900, 3601, 8, 900, 120, 8},
		{900, 120, 31, 900, 120, 8},
		{299, 45, 6, 900, 45, 6}, // each by itself
	} {
		in := Settings{OnlineTimeout: 300 * time.Second, OnlineTimeoutS: 300, Settle: time.Minute, SettleS: 60,
			ShutdownDelay: time.Duration(c.delay) * time.Second, ShutdownDelayS: c.delay,
			OffAfter: time.Duration(c.offAfter) * time.Second, OffAfterS: c.offAfter, PerAgentMin: c.pace}
		got := in.held()
		want := NewSettings(300, 60, c.wantDelay, c.wantPace).WithOffAfter(c.wantOffAfter)
		if got != want {
			t.Errorf("held(off after %d s, countdown %d s, %d a minute) = %+v, want %+v", c.offAfter, c.delay, c.pace, got, want)
		}
	}
}

// A run that is handed timings outside the ranges (a mistake somewhere between the files and the run) uses
// the defaults, and its report says the numbers it used: a machine silent for seven minutes is not taken for
// off, the shutdown has the countdown of two minutes, and the agents are asked at most 8 times a minute. A
// machine's own countdown still counts. The same holds for the manager's own settings.
func TestARunHandedTimingsOutsideTheRangesUsesAndReportsTheDefaults(t *testing.T) {
	bad := Settings{OnlineTimeout: 300 * time.Second, OnlineTimeoutS: 300, Settle: time.Minute, SettleS: 60,
		ShutdownDelay: time.Second, ShutdownDelayS: 1, OffAfter: 5 * time.Second, OffAfterS: 5, PerAgentMin: 100000}
	used := NewSettings(300, 60, 120, 8)
	for _, snapshot := range []bool{true, false} {
		w := newWorld(t)
		off7(w, "pc-seven")
		w.off("pc-off")
		w.off("pc-own").info.ShutdownDelay = 45 * time.Second
		req := StartRequest{Hosts: []string{"pc-off", "pc-own", "pc-seven"}}
		if snapshot {
			req.Settings = &bad
		} else {
			w.m.Settings = bad
		}
		r0 := w.start(req)
		w.runSet = &used // what the world's judge holds the run to
		if r0.Settings != used {
			t.Fatalf("snapshot %v: the run's settings at its start: %+v, want %+v", snapshot, r0.Settings, used)
		}
		if w.m.pace.limit != 8 {
			t.Fatalf("snapshot %v: the pacing of the run: %d a minute", snapshot, w.m.pace.limit)
		}
		r := w.finish()
		if r.Settings != used {
			t.Errorf("snapshot %v: the report's settings: %+v, want %+v", snapshot, r.Settings, used)
		}
		if h := host(r, "pc-seven"); h.State != StSkipped || w.callsFor("pc-seven") != "" {
			t.Errorf("snapshot %v: silent for seven minutes: %s: %s (calls %q)", snapshot, h.State, h.Text, w.callsFor("pc-seven"))
		}
		got := strings.Join(shutdowns(w), ", ")
		if !strings.Contains(got, "shutdown pc-off 120") || !strings.Contains(got, "shutdown pc-own 45") || len(shutdowns(w)) != 2 {
			t.Errorf("snapshot %v: the shutdown requests: %s", snapshot, got)
		}
		if h := host(r, "pc-off"); h.State != StOff || h.ShutdownDelayS != nil || h.OffAfterS != nil {
			t.Errorf("snapshot %v: pc-off: %s: %s (own in the report: %v %v)", snapshot, h.State, h.Text, h.ShutdownDelayS, h.OffAfterS)
		}
		if h := host(r, "pc-own"); h.State != StOff || h.ShutdownDelayS == nil || *h.ShutdownDelayS != 45 {
			t.Errorf("snapshot %v: pc-own: %s: %s (own countdown in the report: %v)", snapshot, h.State, h.Text, h.ShutdownDelayS)
		}
	}
}

// A start can hand the run the own two timings of its machines (StartRequest.Own): the numbers the panel
// read, showed and compared with the run's settings. A machine that is in it gets these, whatever the
// panel's configuration says for it a moment later (Ops.Host); 0 is "none of its own". A machine that is not
// in it gets Ops.Host's, as before. The run's own floors hold for both.
func TestARunTakesAMachinesOwnTimingsFromItsStart(t *testing.T) {
	val := func(p *int) int {
		if p == nil {
			return 0
		}
		return *p
	}
	set := NewSettings(300, 60, 120, 8)
	w := newWorld(t)
	for id, now := range map[string]HostTimings{ // what Ops.Host says when the run is made
		"pc-none": {OffAfter: 5 * time.Minute, ShutdownDelay: 10 * time.Second},
		"pc-own":  {},
		"pc-low":  {OffAfter: 20 * time.Minute, ShutdownDelay: 45 * time.Second},
		"pc-ops":  {OffAfter: 20 * time.Minute, ShutdownDelay: 45 * time.Second},
	} {
		mc := w.off(id)
		mc.info.OffAfter, mc.info.ShutdownDelay = now.OffAfter, now.ShutdownDelay
	}
	own := map[string]HostTimings{ // what the start read and compared
		"pc-none": {},
		"pc-own":  {OffAfter: 30 * time.Minute, ShutdownDelay: 5 * time.Minute},
		"pc-low":  {OffAfter: 5 * time.Second, ShutdownDelay: time.Second}, // below the floors: never used, the run's count
	}
	r0 := w.start(StartRequest{Hosts: []string{"pc-none", "pc-own", "pc-low", "pc-ops"}, Settings: &set, Own: own})
	for id, o := range own { // what the world's judge holds the run to
		if o.OffAfter < OffAfterFloor {
			o.OffAfter = 0
		}
		if o.ShutdownDelay < DelayFloor {
			o.ShutdownDelay = 0
		}
		w.ms[id].ownAtStart.offAfter, w.ms[id].ownAtStart.delay = o.OffAfter, o.ShutdownDelay
	}
	for id, want := range map[string][2]int{"pc-none": {0, 0}, "pc-own": {1800, 300}, "pc-low": {0, 0}, "pc-ops": {1200, 45}} {
		if h := host(r0, id); val(h.OffAfterS) != want[0] || val(h.ShutdownDelayS) != want[1] {
			t.Errorf("%s: its own in the run: %d s / %d s, want %d / %d", id, val(h.OffAfterS), val(h.ShutdownDelayS), want[0], want[1])
		}
	}
	w.finish()
	got := strings.Join(shutdowns(w), ", ")
	for _, want := range []string{"shutdown pc-none 120", "shutdown pc-own 300", "shutdown pc-low 120", "shutdown pc-ops 45"} {
		if !strings.Contains(got, want) {
			t.Errorf("the shutdown requests lack %q: %s", want, got)
		}
	}
}
