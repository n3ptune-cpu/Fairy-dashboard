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
