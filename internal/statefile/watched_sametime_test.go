package statefile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Two writes of a state file can carry the same time and the same size: a file's time comes from a clock that
// moves with the timer tick on Linux before 6.13 (every 4 ms at 250 Hz), Fairy's writes are often less than
// that apart, and "site-b" is as long as "site-a". A view that told the files apart by time and size kept the
// first write (seven tests of internal/adminstate and internal/web failed for it on a Debian 13 machine with
// /tmp in memory, and passed wherever a write took longer or the clock was finer).
//
// These tests make the two files alike on any machine: the second file is given the first one's time.

// writeSite puts one value into the file, as every writer of a state file does (a new file takes the name).
func writeSite(t *testing.T, path, site string) {
	t.Helper()
	if _, err := Update(path, func(d Data) error { d["site"] = site; return nil }); err != nil {
		t.Fatal(err)
	}
}

// sameTimeAs gives the file the time of an earlier one, and says so only when time and size are then the same.
func sameTimeAs(t *testing.T, path string, first os.FileInfo) {
	t.Helper()
	if err := os.Chtimes(path, first.ModTime(), first.ModTime()); err != nil {
		t.Fatal(err)
	}
	now, err := os.Stat(path)
	if err != nil || !now.ModTime().Equal(first.ModTime()) || now.Size() != first.Size() {
		t.Fatalf("the rig: the second file has time %v and %d bytes, the first had %v and %d (%v)", now.ModTime(), now.Size(), first.ModTime(), first.Size(), err)
	}
}

// What this process wrote is in its view after Refresh, whatever the file's time and size say.
func TestRefreshSeesAWriteOfTheSameTimeAndSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	w := NewWatched(path)
	writeSite(t, path, "site-a")
	if got := w.Refresh()["site"]; got != "site-a" {
		t.Fatalf("the first write: %v", got)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	v := w.Version
	writeSite(t, path, "site-b")
	sameTimeAs(t, path, first)
	if got := w.Refresh()["site"]; got != "site-b" {
		t.Errorf("after the second write the view says %v; the file says site-b", got)
	}
	if w.Version != v+1 {
		t.Errorf("the version moved from %d to %d with one change of the file", v, w.Version)
	}
	// and it stays: neither a later look nor time brings the first write back
	if got := w.Get()["site"]; got != "site-b" {
		t.Errorf("a Get after it: %v", got)
	}
}

// The write of another process (`fairy admin ...`), which this one is not told of: the next look sees that
// another file has the name, also when its time and size are those of the file before.
func TestAWriteOfAnotherProcessWithTheSameTimeAndSizeIsSeen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a look at a file says no more than its time and size on Windows (fileid_windows.go)")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	w := NewWatchedEvery(path, 0) // a look at every Get
	writeSite(t, path, "site-a")
	if got := w.Get()["site"]; got != "site-a" {
		t.Fatalf("the first write: %v", got)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	v := w.Version
	writeSite(t, path, "site-b")
	sameTimeAs(t, path, first)
	if got := w.Get()["site"]; got != "site-b" {
		t.Errorf("after the other process wrote, the view says %v; the file says site-b", got)
	}
	if w.Version != v+1 {
		t.Errorf("the version moved from %d to %d with one change of the file", v, w.Version)
	}
	// the views with a look at most once a second: the same, with Look (the switches) and after the second
	slow := NewWatchedEvery(path, time.Hour)
	if got := slow.Get()["site"]; got != "site-b" {
		t.Fatalf("a new view: %v", got)
	}
	first, _ = os.Stat(path)
	writeSite(t, path, "site-c")
	sameTimeAs(t, path, first)
	if got := slow.Get()["site"]; got != "site-b" {
		t.Errorf("a Get inside the view's hour looked at the file: %v", got)
	}
	if got := slow.Look()["site"]; got != "site-c" {
		t.Errorf("Look after the other process wrote: the view says %v; the file says site-c", got)
	}
}
