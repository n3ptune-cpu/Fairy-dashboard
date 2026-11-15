package statefile

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// holdTheLook makes every look at a file say what the file is right now, until the returned function is
// called: the case a stamp cannot tell (see stamp), made on purpose.
func holdTheLook(t *testing.T, path string) (release func()) {
	t.Helper()
	held, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	real := statFile
	statFile = func(string) (fs.FileInfo, error) { return held, nil }
	return func() { statFile = real }
}

// Refresh reads the file. It does not ask a look at the file whether that is needed: after a write of this
// process the answer can be "no" and be wrong. And it changes nothing, the version included, when the file
// holds what the view holds.
func TestRefreshReadsTheFileWhateverALookAtItSays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	w := NewWatchedEvery(path, 0)
	writeSite(t, path, "site-a")
	before := w.Refresh()
	if before["site"] != "site-a" {
		t.Fatalf("the first write: %v", before)
	}
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

// Get and Look read the file only when a look at it finds another file: the switches are asked at every
// request, and a request does not read state.json for it.
func TestALookThatFindsTheSameFileDoesNotReadIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	w := NewWatchedEvery(path, 0)
	writeSite(t, path, "site-a")
	if got := w.Get()["site"]; got != "site-a" {
		t.Fatalf("the first write: %v", got)
	}
	release := holdTheLook(t, path)
	writeSite(t, path, "site-b")
	v := w.Version
	if a, b := w.Get()["site"], w.Look()["site"]; a != "site-a" || b != "site-a" || w.Version != v {
		t.Errorf("with the same look, Get says %v and Look says %v (version %d, was %d): the file was read", a, b, w.Version, v)
	}
	release()
	if got := w.Get()["site"]; got != "site-b" || w.Version != v+1 {
		t.Errorf("the look as it is: %v, version %d (want %d)", got, w.Version, v+1)
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
