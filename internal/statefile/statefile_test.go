package statefile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	if d, err := Read(p); err != nil || len(d) != 50 {
		t.Fatal(len(d), err)
	}
	w := NewWatchedEvery(p, 0)
	v := w.Version
	w.Get()
	_, _ = Update(p, func(d Data) error { d["x"] = 1; return nil })
	if w.Get()["x"] != 1.0 || w.Version <= v {
		t.Fatal("watched reload")
	}
}

// A corrupt state file is an error, never "empty": Update refuses and leaves it as it was, Watched reports it.
func TestCorruptFileFailsClosed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "users.json")
	if d, err := Read(p); err != nil || len(d) != 0 {
		t.Fatal("a missing file is empty", d, err)
	}
	garbage := []byte(`{"users": {"a": {"role": "admin"}`) // truncated
	if err := os.WriteFile(p, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(p); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatal("corrupt file read as valid", err)
	}
	if _, err := Update(p, func(d Data) error { d["users"] = map[string]any{}; return nil }); err == nil {
		t.Fatal("Update rewrote a corrupt file")
	}
	if b, _ := os.ReadFile(p); string(b) != string(garbage) {
		t.Fatal("corrupt file was overwritten")
	}
	w := NewWatchedEvery(p, 0)
	if len(w.Get()) != 0 || w.Err() == nil || Check(p) == nil {
		t.Fatal("watched view must report the error")
	}
	if err := os.WriteFile(p, []byte(`{"ok": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if w.Get()["ok"] != true || w.Err() != nil {
		t.Fatal("recovers once fixed")
	}
}

// An unreadable file (e.g. rewritten 0600 by root) is an error, and a file owned by another user is never
// written (needs root to set up).
func TestUnreadableAndForeignFilesAreRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "tokens.json")
	if err := os.WriteFile(p, []byte(`{"tokens": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		_ = os.Chmod(p, 0)
		if _, err := Read(p); err == nil {
			t.Fatal("unreadable file read as empty")
		}
		return
	}
	if err := os.Chown(p, 4242, 4242); err != nil {
		t.Fatal(err)
	}
	_, err := Update(p, func(d Data) error { d["x"] = 1; return nil })
	var oe *OwnerError
	if err == nil || !strings.Contains(err.Error(), "sudo -u fairy") || !errors.As(err, &oe) || oe.Owner != 4242 {
		t.Fatal("wrote a file owned by another user", err)
	}
	if st, _ := os.Stat(p); st.Sys() == nil {
		t.Fatal("stat")
	}
	if Check(p) == nil {
		t.Fatal("Check must refuse a foreign file")
	}
}
