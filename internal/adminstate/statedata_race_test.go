package adminstate

import (
	"os"
	"sync"
	"testing"
	"time"
)

// The version of the view of state.json is read while other requests write the file. StateData (the poller asks
// it at every poll, for the poll overrides) and the "said once per change of the file" notes read the field
// Version without the view's lock while a Refresh after a write changed it: a data race (second review of the
// fixes, its note on adminstate.StateData; the race detector reports it without the fix). The numbers StateData
// returns never go back, and they move with the writes.
func TestStateDataWhileStateJSONIsWritten(t *testing.T) {
	a := New(t.TempDir())
	if err := a.SetPowerDisabled("pc-a", true); err != nil {
		t.Fatal(err)
	}
	_, first := a.StateData()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	run := func(fn func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
					fn(i)
				}
			}
		}()
	}
	// two writers of the panel itself, and one write of another kind: a value that is not what Fairy writes
	// there (somebody's hand edit), which makes the view's readers say so once per change of the file
	run(func(i int) { _ = a.SetPowerDisabled("pc-a", i%2 == 0) })
	run(func(i int) { _ = a.SetControlDisabled(i%2 == 0, "test", "") })
	run(func(i int) {
		if i%50 == 0 {
			_ = os.WriteFile(a.StatePath, []byte(`{"power_control_disabled": "pc-a"}`), 0o600)
			a.StateWritten()
		}
		a.PowerDisabled("pc-a")
	})
	var back sync.Once
	last := make([]int, 2)
	for r := range last {
		last[r] = first
		run(func(int) {
			d, v := a.StateData()
			if v < last[r] || d == nil {
				back.Do(func() { t.Errorf("the version went from %d to %d (data %v)", last[r], v, d != nil) })
			}
			last[r] = v
		})
	}
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	if _, v := a.StateData(); v <= first {
		t.Errorf("the version is %d after the writes, and was %d before them", v, first)
	}
}
