//go:build !windows

package wakerun

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
)

// r2-p2-10 (audit 5, round 2): wake-update.json as a link to /dev/zero (size 0, no end: the panel died of "out
// of memory" at every start) or as a named pipe (the open waited for a writer: the panel never started). The
// README says: "If that file cannot be read, no run can be started ... The panel starts all the same."
func TestAJournalThatIsNotAPlainFileIsNotRead(t *testing.T) {
	for name, c := range map[string]struct {
		make func(p string) error
		what string
	}{
		"a link to /dev/zero": {func(p string) error { return os.Symlink("/dev/zero", p) }, "a symbolic link"},
		"a named pipe":        {func(p string) error { return syscall.Mkfifo(p, 0o600) }, "a named pipe"},
		"a folder":            {func(p string) error { return os.Mkdir(p, 0o700) }, "a folder"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.off("pc-a")
			if err := c.make(w.m.path); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- w.m.Load() }()
			select {
			case err := <-done:
				if !statefile.IsNotAFile(err) || !strings.Contains(err.Error(), c.what) || w.m.JournalError() == nil {
					t.Fatalf("Load: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the panel would not start: reading the journal does not come back")
			}
			// no run starts, nothing is sent, and what is there is left alone
			if _, err := w.m.Start(StartRequest{Hosts: []string{"pc-a"}, By: "Ann"}); err == nil || !strings.Contains(err.Error(), "the run journal cannot be used") {
				t.Fatalf("a run started: %v", err)
			}
			w.advance(5 * time.Second)
			if st, err := os.Lstat(w.m.path); err != nil || st.Mode().IsRegular() || len(w.calls) != 0 {
				t.Fatalf("it was replaced, or something was sent: %v %v %v", st, err, w.calls)
			}
			// moved away, the panel works again
			if err := os.Remove(w.m.path); err != nil {
				t.Fatal(err)
			}
			if err := w.m.Load(); err != nil || w.m.JournalError() != nil {
				t.Fatalf("after it is gone: %v", err)
			}
			w.start(StartRequest{Hosts: []string{"pc-a"}})
			w.finish()
		})
	}
}
