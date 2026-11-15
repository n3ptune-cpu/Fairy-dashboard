//go:build linux

package statefile

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// sysOf is what the system says of a file (the test's own reading of it, not fileIDOf's).
func sysOf(t *testing.T, path string) (os.FileInfo, syscall.Stat_t) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no stat of %s", path)
	}
	return st, *s
}

// A file that is written in place keeps its number, and can keep its time and size (a tool that sets the time
// back, a copy that keeps it). The time its entry changed moves with every such write and nothing sets it
// back; it is in the stamp, and the next look reads the file.
func TestAWriteInPlaceWithTheSameTimeAndSizeIsSeen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"site": "site-a"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pass := ownClock(t)
	w := NewWatchedEvery(path, 0)
	if got := w.Get()["site"]; got != "site-a" {
		t.Fatalf("the first write: %v", got)
	}
	settle(t, w, pass) // the view goes by the stamp alone: a young file would be read whatever is in it
	first, a := sysOf(t, path)
	// The clock of that time moves with the timer tick on an older kernel: written again until it has moved
	// (some milliseconds at most).
	b := a
	for end := time.Now().Add(5 * time.Second); b.Ctim == a.Ctim; {
		if time.Now().After(end) {
			t.Skip("the time of the file's entry does not move on this file system")
		}
		time.Sleep(time.Millisecond)
		if err := os.WriteFile(path, []byte(`{"site": "site-b"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		sameTimeAs(t, path, first)
		_, b = sysOf(t, path)
	}
	if b.Ino != a.Ino || b.Dev != a.Dev || b.Mtim != a.Mtim || b.Size != a.Size {
		t.Fatalf("the rig: the same file with the same time and size was wanted: %+v, then %+v", a, b)
	}
	if got := w.Get()["site"]; got != "site-b" {
		t.Errorf("after the write in place the view says %v; the file says site-b", got)
	}
}

// onAnotherDevice is a look at a file that says everything as it is, and another device.
type onAnotherDevice struct {
	fs.FileInfo
	sys syscall.Stat_t
}

func (f onAnotherDevice) Sys() any { return &f.sys }

// The number of a file is one of its device: a data folder that is another file system at the next look (a
// share that came back, a disk that was mounted over it) can hold a file with the same number, time and size.
func TestTheSameNumberOnAnotherDeviceIsAnotherFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	writeSite(t, path, "site-a")
	pass := ownClock(t)
	w := NewWatchedEvery(path, 0)
	if got := w.Get()["site"]; got != "site-a" {
		t.Fatalf("the first write: %v", got)
	}
	settle(t, w, pass) // the view goes by the stamp alone
	st, sys := sysOf(t, path)
	sys.Dev++
	real := statFile
	defer func() { statFile = real }()
	statFile = func(string) (fs.FileInfo, error) { return onAnotherDevice{st, sys}, nil }
	v := w.Version
	if w.Get(); w.Version != v+1 {
		t.Errorf("the same file on another device: the version is %d, was %d: the file was not read again", w.Version, v)
	}
	if w.Get(); w.Version != v+1 {
		t.Errorf("and looked at again: the version is %d, want %d", w.Version, v+1)
	}
}
