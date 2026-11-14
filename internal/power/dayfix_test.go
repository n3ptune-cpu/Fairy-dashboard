package power

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// What the review of the day history found in its file and its limits, each with the case that showed it.

// "A write that fails is said once in the log" was not so for the two failures that happen: a full disk and a
// folder that cannot be written. The write's error names its temporary file, and that name is new with every
// write; the log line was held back only while the error's whole text stayed the same, so it came every five
// minutes. It is the failure that is compared now, not the file's name.
func TestDayFileWriteThatFailsWithANewTemporaryNameIsStillSaidOnce(t *testing.T) {
	log := captureLog(t)
	path := dayPath(t)
	d := loaded(path, at(0, 0, 0))
	n, cause := 0, error(syscall.ENOSPC)
	d.writeFile = func(p string, _ []byte, _ os.FileMode) error {
		n++
		return &fs.PathError{Op: "write", Path: filepath.Join(filepath.Dir(p), fmt.Sprintf(".power-day.json.%d.tmp", 1000+n*7919)), Err: cause}
	}
	fail := func(periods int) {
		t.Helper()
		for i := 0; i < periods; i++ {
			rec(d, at(0, n*5, 0), 1)
			if d.Flush(at(0, n*5, 1), 5*time.Second) {
				t.Fatal("a write that fails counts as written")
			}
		}
	}
	said := func() int { return strings.Count(log.String(), "could not keep the last 24 hours") }
	fail(6)
	if said() != 1 {
		t.Fatalf("a full disk for 6 periods is in the log %d times, want once:\n%s", said(), log)
	}
	// the line names the folder's problem and one of the temporary names; another failure is said again
	if !strings.Contains(log.String(), "no space left on device") {
		t.Errorf("the line does not say why:\n%s", log)
	}
	cause = syscall.EROFS
	fail(3)
	if said() != 2 || !strings.Contains(log.String(), "read-only file system") {
		t.Errorf("another failure after the first: in the log %d times in all, want twice:\n%s", said(), log)
	}
	// the real thing: a folder nobody can write a file into, root neither (the kernel's own)
	if st, err := os.Stat("/sys/kernel"); runtime.GOOS != "linux" || err != nil || !st.IsDir() {
		return
	}
	log.Reset()
	e := loaded("/sys/kernel/"+DayFile, at(0, 0, 0))
	for i := 1; i <= 4; i++ {
		rec(e, at(0, i*5, 0), 1)
		if e.Flush(at(0, i*5, 1), 5*time.Second) {
			t.Skip("a file could be written into /sys/kernel here")
		}
	}
	if said() != 1 {
		t.Errorf("four failed writes into a folder that cannot be written are in the log %d times, want once:\n%s", said(), log)
	}
}

// A write that the panel was killed in the middle of left its temporary file in the data folder, and nothing
// ever took it away: 6 MB each time, for ever. The start removes them now, and nothing else: only plain files
// with the name a write of this file gives its temporary file.
func TestDayFileLeftoversOfAKilledWriteAreRemovedAtTheStart(t *testing.T) {
	log := captureLog(t)
	path := dayPath(t)
	dir := filepath.Dir(path)
	d := loaded(path, at(0, 0, 0))
	rec(d, at(0, 0, 0), 40)
	saved(t, d, at(0, 1, 0))
	put := func(name string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(`{"version":1`), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	gone := []string{put(".power-day.json.1234567.tmp"), put(".power-day.json.tmp-of-an-older-build.tmp")}
	stay := []string{path, put(".notifications.json.1234567.tmp"), put(".power-day.json.1234567.tmp.keep"),
		put("power-day.json.1234567.tmp"), put(".power-day.jsonx.tmp"), put("power-day.json.bak")}
	folder := filepath.Join(dir, ".power-day.json.folder.tmp")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".power-day.json.link.tmp")
	if err := os.Symlink(filepath.Join(dir, "power-day.json.bak"), link); err != nil {
		t.Fatal(err)
	}
	stay = append(stay, folder, link)

	e := loaded(path, at(0, 2, 0))
	for _, p := range gone {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s is still there after the start (%v)", filepath.Base(p), err)
		}
	}
	for _, p := range stay {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s is not a leftover of a write of this file and was taken away: %v", filepath.Base(p), err)
		}
	}
	if n := strings.Count(log.String(), "removed the temporary file"); n != len(gone) {
		t.Errorf("the log names %d removed files, want %d:\n%s", n, len(gone), log)
	}
	if !e.Has("plug-a", at(0, 2, 0)) {
		t.Error("the day itself was not read")
	}
	// a start without a data folder, or with one that cannot be listed, is still a start
	loaded(filepath.Join(dir, "none", "deeper", DayFile), at(0, 2, 0))
}
