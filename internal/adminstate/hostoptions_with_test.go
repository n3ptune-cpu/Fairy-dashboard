package adminstate

import (
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// EditHostOptionsWith hands its function the run timings of the same reading of state.json it writes from:
// what another writer of the file stored a moment ago, whatever this store had read before. A function that
// reads them changes nothing in them.
func TestEditHostOptionsWithGivesTheRunTimingsOfTheSameReading(t *testing.T) {
	dir := t.TempDir()
	a, other := New(dir), New(dir)
	set := func(v int) {
		t.Helper()
		if _, _, err := other.EditRunTimings("nora", func(o *RunTimings) error { o.Set(config.KeyShutdownDelay, v); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	_ = a.RunTimings() // this store has read the file while it held nothing
	for i, v := range []int{30, 45} {
		set(v)
		got, calls := -1, 0
		_, changed, err := a.EditHostOptionsWith("ann", func(o *HostOptions, panel RunTimings) error {
			calls++
			got, _ = panel.Value(config.KeyShutdownDelay)
			panel.Set(config.KeyShutdownDelay, 10) // a copy: never written
			o.SetHost("pc-a", HostEdit{Run: RunEdit{ShutdownDelayS: 60 + i}})
			return nil
		})
		if err != nil || !changed || calls != 1 || got != v {
			t.Fatalf("the panel's countdown in the file is %d; the function got %d (%d calls, changed %v, %v)", v, got, calls, changed, err)
		}
		if now, _ := other.RunTimings().Value(config.KeyShutdownDelay); now != v {
			t.Fatalf("the run timings were written by a change of the host options: %d", now)
		}
	}
	if e := a.HostOptions().Hosts["pc-a"]; e.Run.ShutdownDelayS != 61 || e.By != "ann" {
		t.Fatalf("stored: %+v", e)
	}
}
