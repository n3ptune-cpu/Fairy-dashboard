//go:build !windows

package settings

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// noMoreFiles makes every open of a file fail with "too many open files" until the returned function is
// called: the limit of open files of this process is set to none. Files that are open stay open.
func noMoreFiles(t *testing.T) (restore func()) {
	t.Helper()
	var was syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &was); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &syscall.Rlimit{Cur: 0, Max: was.Max}); err != nil {
		t.Fatal(err)
	}
	done := false
	restore = func() {
		if !done {
			done = true
			if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &was); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(restore)
	return restore
}

// Second review of fix7a (R7A-07): settings.json was moved aside on every read error except "does not
// exist", not only when its content cannot be used. An error that passes (too many open files, an I/O error,
// a wrong owner after a restore) moved a good file away: everybody's settings started over, and five such
// moments removed the oldest kept copies. A file that could not be READ is left where it is, with one ERROR;
// nothing is saved until it can be read again (defaults are never written over a file that was not read), and
// then everything is as it was.
func TestASettingsFileThatCannotBeReadIsLeftInPlace(t *testing.T) {
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	dir := t.TempDir()
	s := NewStore(dir)
	for _, sub := range []string{"ann", "bob"} {
		st := Defaults()
		st.Dim = 33
		if _, err := s.Put(sub, st); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(dir, "settings.json")
	good, err := os.ReadFile(p)
	if err != nil || logs.Len() != 0 {
		t.Fatal(err, logs.String())
	}
	copies := func() []string {
		m, _ := filepath.Glob(p + ".corrupt-*")
		return m
	}

	restore := noMoreFiles(t)
	// the panel's start, and requests of two users, while the file cannot be opened
	s.Check()
	got := s.Get("bob")
	st := Defaults()
	st.Dim = 77
	_, putErr := s.Put("ann", st)
	_, delErr := s.DeleteBackground("bob")
	_, upErr := s.SaveBackground("bob", "image/png", []byte("not looked at here"))
	restore()

	if c := copies(); len(c) != 0 {
		t.Errorf("a settings.json that could not be opened (too many open files) was moved aside: %v", c)
	}
	if b, err := os.ReadFile(p); err != nil || !bytes.Equal(b, good) {
		t.Fatalf("settings.json after the time it could not be read: %q %v", b, err)
	}
	if got.Dim != Defaults().Dim {
		t.Errorf("while the file cannot be read a user sees the defaults, got %+v", got)
	}
	for what, err := range map[string]error{"a save": putErr, "removing a background": delErr, "a new background": upErr} {
		if err == nil || !strings.Contains(err.Error(), "could not read settings.json") || !strings.Contains(err.Error(), "Try again") {
			t.Errorf("%s while the file cannot be read must be refused, with a sentence for the person: %v", what, err)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "backgrounds", "*")); len(m) != 0 {
		t.Errorf("a background picture was written although its settings could not be saved: %v", m)
	}
	log := logs.String()
	if strings.Count(log, "level=ERROR") != 1 || !strings.Contains(log, "settings.json could not be read") || !strings.Contains(log, "left in place") ||
		!strings.Contains(log, "too many open files") || !strings.Contains(log, p) {
		t.Errorf("want one ERROR that names the file and why, for the whole time: %s", log)
	}
	// it can be read again: everybody's settings are what they were, a save works, nothing more is said
	logs.Reset()
	if s.Get("ann").Dim != 33 || s.Get("bob").Dim != 33 {
		t.Errorf("after the file can be read again: ann %d, bob %d, want 33 and 33", s.Get("ann").Dim, s.Get("bob").Dim)
	}
	if _, err := s.Put("ann", st); err != nil || s.Get("ann").Dim != 77 || s.Get("bob").Dim != 33 {
		t.Errorf("a save after the file can be read again: %v; ann %d, bob %d", err, s.Get("ann").Dim, s.Get("bob").Dim)
	}
	if strings.Contains(logs.String(), "level=ERROR") || len(copies()) != 0 {
		t.Errorf("after the file can be read again: %s; copies %v", logs.String(), copies())
	}
	// and the next time it cannot be read is said again, once
	restore = noMoreFiles(t)
	s.Get("ann")
	s.Get("bob")
	restore()
	if n := strings.Count(logs.String(), "level=ERROR"); n != 1 {
		t.Errorf("the second time the file cannot be read: %d ERROR lines, want 1: %s", n, logs.String())
	}
}
