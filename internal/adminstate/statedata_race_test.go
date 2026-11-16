package adminstate

import (
	"os"
	"strings"
	"sync"
	"sync/atomic"
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

// The same for the notes that are said once per change of the file about what an Edit host override, and a run
// timing, hold that the panel does not use (hostOptionsWarn, runTimingsWarn): each looks at the version and
// then stores it, as the note about a value of the wrong kind does (badShape). The test above makes only that
// last one speak, so a read of the field without the lock in one of the other two passed it, and the whole
// suite, with the race detector on (third review, its change M27). Here state.json holds a key under
// host_options and one under run_timings that this build does not know (what a newer build leaves behind), so
// every look at the two has something to say, while the file is written. Each write keeps the two keys, so
// they are still said at the end.
//
// It runs for a second, with four lookers at each of the two: a store is reached once per change of the file
// and a write has to come within the next few lines, so a shorter run with fewer lookers did not always meet
// it (measured in the commit that added this test).
func TestSaidOnceNotesWhileStateJSONIsWritten(t *testing.T) {
	a := New(t.TempDir())
	const held = `{"host_options": {"rev": 1, "left_by_a_newer_build": 1}, "run_timings": {"rev": 1, "left_by_a_newer_build": 1}}`
	if err := os.WriteFile(a.StatePath, []byte(held), 0o600); err != nil {
		t.Fatal(err)
	}
	a.StateWritten()
	said := func() (host, run int) {
		_, h := a.HostOptionsRead()
		_, r := a.RunTimingsRead()
		return len(h), len(r)
	}
	if h, r := said(); h != 1 || r != 1 {
		t.Fatalf("the fixture: %d lines about host_options and %d about run_timings, want one of each", h, r)
	}
	if err := a.SetPowerDisabled("pc-b", true); err != nil {
		t.Fatalf("the fixture: the panel cannot write the file: %v", err)
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
	run(func(i int) { _ = a.SetPowerDisabled("pc-b", i%2 == 0) }) // each write is a new version of the file
	run(func(i int) { _ = a.SetControlDisabled(i%2 == 0, "test", "") })
	// And a writer that is not the panel, as fast as the disk lets it. A note stores the version two lines after
	// it looked at it, and only when the file changed: the panel's own writes are too few to come between the
	// two (with them alone a store without the lock passed this test).
	byHand := a.StatePath + ".by-hand"
	run(func(i int) {
		body := held
		if i%2 == 1 {
			body = strings.ReplaceAll(held, `"rev": 1`, `"rev": 2`)
		}
		if os.WriteFile(byHand, []byte(body), 0o600) == nil && os.Rename(byHand, a.StatePath) == nil {
			a.StateWritten()
		}
	})
	var looks [2]atomic.Int64
	for range 4 {
		run(func(int) { a.HostOptions(); looks[0].Add(1) }) // hostOptionsWarn
		run(func(int) { a.RunTimings(); looks[1].Add(1) })  // runTimingsWarn
	}
	time.Sleep(time.Second)
	close(stop)
	wg.Wait()
	for i, what := range []string{"host_options", "run_timings"} {
		if looks[i].Load() == 0 {
			t.Errorf("nobody looked at %s", what)
		}
	}
	if _, v := a.StateData(); v <= first {
		t.Errorf("the version is %d after the writes, and was %d before them", v, first)
	}
	if h, r := said(); h != 1 || r != 1 {
		t.Errorf("after the writes: %d lines about host_options and %d about run_timings, want one of each (a write dropped the keys)", h, r)
	}
}
