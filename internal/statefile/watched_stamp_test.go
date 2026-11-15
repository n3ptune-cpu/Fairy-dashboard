package statefile

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// ownClock gives the views a clock that only the test moves (pass), until the test ends.
func ownClock(t *testing.T) (pass func(time.Duration)) {
	t.Helper()
	now, real := time.Now(), timeNow
	timeNow = func() time.Time { return now }
	t.Cleanup(func() { timeNow = real })
	return func(d time.Duration) { now = now.Add(d) }
}

// settle lets the time pass after which a view goes by the file's stamp alone, and has the view look once.
func settle(t *testing.T, w *Watched, pass func(time.Duration)) {
	t.Helper()
	pass(YoungFor)
	w.Look()
	if !w.settled {
		t.Fatalf("the rig: %v after its first look the view still reads the file at every look", YoungFor)
	}
}

// holdTheLook makes every look at a file say what the file is right now, until the returned function is
// called (or the test ends): the case a stamp cannot tell (see stamp), made on purpose.
func holdTheLook(t *testing.T, path string) (release func()) {
	t.Helper()
	held, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	real := statFile
	statFile = func(string) (fs.FileInfo, error) { return held, nil }
	t.Cleanup(func() { statFile = real }) // also when the test stops before it lets go
	return func() { statFile = real }
}

// Refresh reads the file. It does not ask a look at the file whether that is needed: after a write of this
// process the answer can be "no" and be wrong. And it changes nothing, the version included, when the file
// holds what the view holds.
func TestRefreshReadsTheFileWhateverALookAtItSays(t *testing.T) {
	pass := ownClock(t)
	path := filepath.Join(t.TempDir(), "state.json")
	w := NewWatchedEvery(path, 0)
	writeSite(t, path, "site-a")
	before := w.Refresh()
	if before["site"] != "site-a" {
		t.Fatalf("the first write: %v", before)
	}
	settle(t, w, pass) // from here on a look alone decides whether Get and Look read
	release := holdTheLook(t, path)
	defer release()
	v := w.Version

	// nothing was written: the same data (the same map), the same version, however often
	for i := 0; i < 3; i++ {
		if got := w.Refresh(); reflect.ValueOf(got).Pointer() != reflect.ValueOf(before).Pointer() || w.Version != v {
			t.Fatalf("Refresh of a file that did not change: version %d, was %d; another map: %v", w.Version, v, reflect.ValueOf(got).Pointer() != reflect.ValueOf(before).Pointer())
		}
	}

	// a write that no look can see
	writeSite(t, path, "site-b")
	if got := w.Look()["site"]; got != "site-a" {
		t.Fatalf("the rig: a look sees the write (%v), so this test shows nothing", got)
	}
	if got := w.Refresh()["site"]; got != "site-b" {
		t.Errorf("Refresh after a write: the view says %v; the file says site-b", got)
	}
	if w.Version != v+1 {
		t.Errorf("the version moved from %d to %d with one change of the file", v, w.Version)
	}
	if got := w.Get()["site"]; got != "site-b" {
		t.Errorf("a Get after it: %v", got)
	}

	// a file that is damaged in that way: the view is empty and says why, once
	if err := os.WriteFile(path, []byte(`{"site": "site-`), 0o600); err != nil {
		t.Fatal(err)
	}
	if d := w.Refresh(); len(d) != 0 || !IsDamaged(w.Err()) || w.Version != v+2 {
		t.Errorf("a damaged file: data %v, error %v, version %d (want %d)", d, w.Err(), w.Version, v+2)
	}
	if d := w.Refresh(); len(d) != 0 || !IsDamaged(w.Err()) || w.Version != v+2 {
		t.Errorf("the same damaged file again: data %v, error %v, version %d (want %d)", d, w.Err(), w.Version, v+2)
	}
	// and whole again
	if err := os.WriteFile(path, []byte(`{"site": "site-c"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if d := w.Refresh(); d["site"] != "site-c" || w.Err() != nil || w.Version != v+3 {
		t.Errorf("the file whole again: data %v, error %v, version %d (want %d)", d, w.Err(), w.Version, v+3)
	}
}

// Get and Look read the file only when a look at it finds another file, once the view has known the file for
// YoungFor: the switches are asked at every request, and a request does not read state.json for it.
func TestALookThatFindsTheSameFileDoesNotReadIt(t *testing.T) {
	pass := ownClock(t)
	path := filepath.Join(t.TempDir(), "state.json")
	w := NewWatchedEvery(path, 0)
	writeSite(t, path, "site-a")
	if got := w.Get()["site"]; got != "site-a" {
		t.Fatalf("the first write: %v", got)
	}
	settle(t, w, pass)
	reads, v := w.Reads, w.Version
	for i := 0; i < 50; i++ {
		pass(time.Hour)
		w.Get()
		w.Look()
		w.Err()
	}
	if w.Reads != reads || w.Version != v {
		t.Errorf("150 looks at a file that did not change read it %d times (version %d, was %d)", w.Reads-reads, w.Version, v)
	}
	// the same, shown by what cannot happen with a real clock: a write under a look that is held still
	release := holdTheLook(t, path)
	writeSite(t, path, "site-b")
	if a, b := w.Get()["site"], w.Look()["site"]; a != "site-a" || b != "site-a" || w.Version != v || w.Reads != reads {
		t.Errorf("with the same look, Get says %v and Look says %v (version %d, was %d): the file was read", a, b, w.Version, v)
	}
	release()
	if got := w.Get()["site"]; got != "site-b" || w.Version != v+1 || w.Reads != reads+1 {
		t.Errorf("the look as it is: %v, version %d (want %d), %d reads (want 1)", got, w.Version, v+1, w.Reads-reads)
	}
}

// Writes that nobody told the view of and that no look can see: two writes of another process inside one step
// of the file's clock, the second of the size of the file the view looked at and with the number of that file
// (the file system gave it out again). The look is held still at the first file to make exactly that. While
// the view has known the file for less than YoungFor it reads the file at every look, and sees them; after
// that time it has read the file once more and goes by the look alone. A file that then changes is young
// again.
func TestAWriteNobodyToldOfUnderTheSameStampIsSeenWhileTheFileIsYoung(t *testing.T) {
	pass := ownClock(t)
	path := filepath.Join(t.TempDir(), "state.json")
	w := NewWatchedEvery(path, 0)
	writeSite(t, path, "site-a")
	if got := w.Get()["site"]; got != "site-a" {
		t.Fatalf("the first write: %v", got)
	}
	v := w.Version
	release := holdTheLook(t, path)
	writeSite(t, path, "site-b") // another file
	writeSite(t, path, "site-c") // and "the first one's number again": the look says the first file
	if got := w.Get()["site"]; got != "site-c" || w.Version != v+1 {
		t.Errorf("after two writes under the same stamp the view says %v (version %d, want %d); the file says site-c", got, w.Version, v+1)
	}
	// nothing was written: the looks of a young file read it, and are no change
	for i := 0; i < 5; i++ {
		before := w.Get()
		if got := w.Look(); reflect.ValueOf(got).Pointer() != reflect.ValueOf(before).Pointer() || w.Version != v+1 {
			t.Fatalf("a look at a young file that did not change: version %d (want %d), another map: %v", w.Version, v+1, reflect.ValueOf(got).Pointer() != reflect.ValueOf(before).Pointer())
		}
	}
	// until the last moment of that time
	pass(YoungFor - time.Millisecond)
	writeSite(t, path, "site-d")
	if got := w.Look()["site"]; got != "site-d" || w.Version != v+2 {
		t.Errorf("a millisecond before the file is no longer young the view says %v (version %d, want %d); the file says site-d", got, w.Version, v+2)
	}
	if w.settled {
		t.Error("the view goes by the stamp alone before YoungFor has passed")
	}
	// the time has passed: one more read (a write up to now would be in it), and then none
	pass(time.Millisecond)
	writeSite(t, path, "site-e")
	if got := w.Get()["site"]; got != "site-e" || w.Version != v+3 || !w.settled {
		t.Errorf("at the end of that time the view says %v (version %d, want %d, settled %v); the file says site-e", got, w.Version, v+3, w.settled)
	}
	reads := w.Reads
	for i := 0; i < 50; i++ {
		pass(time.Hour)
		w.Get()
		w.Look()
	}
	if w.Reads != reads || w.Version != v+3 {
		t.Errorf("100 looks at a file the view has known for long read it %d times (version %d, want %d)", w.Reads-reads, w.Version, v+3)
	}
	// a file that changes is young again, however long the view has known the one before
	release()
	writeSite(t, path, "site-f")
	if got := w.Get()["site"]; got != "site-f" || w.settled {
		t.Fatalf("a new file: the view says %v, settled %v", got, w.settled)
	}
	release = holdTheLook(t, path)
	defer release()
	writeSite(t, path, "site-g")
	if got := w.Get()["site"]; got != "site-g" {
		t.Errorf("a write under the stamp of the new file: the view says %v; the file says site-g", got)
	}
}

// How long a file is young does not depend on what its time says: the time is counted from the view's first
// look, on the process's own clock. A file from the future (a clock that was set back, a restored backup, a
// file server with another clock) or from long ago is read at every look for YoungFor, once more after it,
// and then no more. A check that compared the file's time with the time of day would read the first at every
// look for a year, and the second never.
func TestAFilesOwnTimeDoesNotDecideHowLongItIsYoung(t *testing.T) {
	if YoungFor < 2*time.Second || YoungFor > 10*time.Second {
		t.Errorf("YoungFor is %v: it must be longer than the step of the coarsest file clock (one second) with room to spare, and short enough that reading at every look in it costs nothing that matters", YoungFor)
	}
	for name, at := range map[string]time.Time{
		"a year from now":  time.Now().Add(365 * 24 * time.Hour),
		"an hour from now": time.Now().Add(time.Hour),
		"now":              time.Now(),
		"1990":             time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC),
	} {
		pass := ownClock(t)
		path := filepath.Join(t.TempDir(), "state.json")
		writeSite(t, path, "site-a")
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		w := NewWatchedEvery(path, 0)
		if got := w.Get()["site"]; got != "site-a" || w.Reads != 1 {
			t.Fatalf("%s: the first look: %v, %d reads", name, got, w.Reads)
		}
		for i := 1; i <= 3; i++ { // three looks inside the time: each reads
			pass(YoungFor / 4)
			if w.Look(); w.Reads != 1+i || w.settled {
				t.Fatalf("%s: look %d inside YoungFor: %d reads (want %d), settled %v", name, i, w.Reads, 1+i, w.settled)
			}
		}
		pass(YoungFor / 4) // the time is over: one read, the last
		if w.Look(); w.Reads != 5 || !w.settled {
			t.Fatalf("%s: the look at the end of YoungFor: %d reads (want 5), settled %v", name, w.Reads, w.settled)
		}
		for i := 0; i < 200; i++ {
			pass(time.Hour)
			w.Look()
			w.Get()
		}
		if w.Reads != 5 || w.Version != 1 {
			t.Errorf("%s: 400 looks later the file was read %d times (want 5), version %d (want 1)", name, w.Reads, w.Version)
		}
		writeSite(t, path, "site-b") // and a write is seen as always
		if got := w.Look()["site"]; got != "site-b" {
			t.Errorf("%s: after a write the view says %v", name, got)
		}
	}
}

// A stamp is the file's time, its size and what the system says of the file beyond them; a file that is not
// there or cannot be looked at has no such thing in it.
func TestStamp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if s, err := look(path); err != nil || s != (stamp{what: missing}) {
		t.Errorf("no file: %+v, %v", s, err)
	}
	writeSite(t, path, "site-a")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := look(path)
	if err != nil || a.what != there || a.mtime != st.ModTime().UnixNano() || a.size != st.Size() || a.id != fileIDOf(st) {
		t.Errorf("a file: %+v, %v", a, err)
	}
	if b, _ := look(path); b != a {
		t.Errorf("two looks at one file: %+v and %+v", a, b)
	}
	if err := os.Symlink(filepath.Join(dir, "away", "state.json"), filepath.Join(dir, "link.json")); err == nil {
		if s, err := look(filepath.Join(dir, "link.json")); err == nil || s != (stamp{what: outOfReach}) {
			t.Errorf("a link to nothing: %+v, %v", s, err)
		}
	}
}
