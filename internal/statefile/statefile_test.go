package statefile

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func TestLockedUpdatesNeverLoseWrites(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = Update(p, func(d Data) error { d[fmt.Sprint(i)] = i; return nil })
		}(i)
	}
	wg.Wait()
	if len(Read(p)) != 50 {
		t.Fatal(len(Read(p)))
	}
	w := NewWatchedEvery(p, 0)
	v := w.Version
	w.Get()
	_, _ = Update(p, func(d Data) error { d["x"] = 1; return nil })
	if w.Get()["x"] != 1.0 || w.Version <= v {
		t.Fatal("watched reload")
	}
}
