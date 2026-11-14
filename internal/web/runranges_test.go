package web

import (
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// The run engine holds the timings it is handed to its own ranges (wakerun: Settings.held). They are
// config.yaml's ranges and defaults, so a value the panel can save is never one the run would replace.
func TestTheRunsOwnRangesAreConfigYAMLs(t *testing.T) {
	of := func(key string) config.RunTiming {
		tm, ok := config.RunTimingOf(key)
		if !ok {
			t.Fatal("no timing " + key)
		}
		return tm
	}
	if d := of(config.KeyShutdownDelay); time.Duration(d.Min)*time.Second != wakerun.DelayFloor || time.Duration(d.Max)*time.Second != wakerun.DelayMax ||
		time.Duration(d.Default)*time.Second != wakerun.DefaultDelay {
		t.Errorf("the countdown: config.yaml %+v, the run %s to %s, default %s", d, wakerun.DelayFloor, wakerun.DelayMax, wakerun.DefaultDelay)
	}
	if p := of(config.KeyRequestsPerMin); p.Min != 1 || p.Max != wakerun.PaceMax || p.Default != wakerun.DefaultPace {
		t.Errorf("the pacing: config.yaml %+v, the run 1 to %d, default %d", p, wakerun.PaceMax, wakerun.DefaultPace)
	}
	if o := of(config.KeyOffAfter); time.Duration(o.Min)*time.Second != wakerun.OffAfterFloor || time.Duration(o.Default)*time.Second != wakerun.MinOff {
		t.Errorf("counts as off after: config.yaml %+v, the run from %s, default %s", o, wakerun.OffAfterFloor, wakerun.MinOff)
	}
}
