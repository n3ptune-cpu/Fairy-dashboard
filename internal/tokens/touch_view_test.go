package tokens

import "testing"

// Touch writes tokens.json (the time of the last use, at most once a minute per token). Like every other
// write of this store it has the store's view read the file, so that the view never depends on telling the
// new file from the one before by looking at it.
func TestTouchTellsTheViewOfItsWrite(t *testing.T) {
	s := NewStore(t.TempDir())
	raw, r, err := s.Create("ha", "system", "read", "cli:root", 0)
	if err != nil || s.Verify(raw) == nil {
		t.Fatal(err)
	}
	reads := s.watch.Reads
	s.Touch(&r)
	if s.watch.Reads != reads+1 {
		t.Errorf("Touch wrote tokens.json; the view read it %d times (want 1)", s.watch.Reads-reads)
	}
	if l, err := s.List(); err != nil || len(l) != 1 || l[0].LastUsed == nil {
		t.Errorf("the last use is not in the list: %+v, %v", l, err)
	}
	s.Touch(&r) // inside the minute: no write, and nothing to read
	if s.watch.Reads != reads+1 {
		t.Errorf("a Touch that writes nothing had the view read the file (%d reads, want 1)", s.watch.Reads-reads)
	}
}
