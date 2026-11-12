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

// r2-p4-10 (audit 5): a state file that exists with nothing in it was read as "no state yet": the kill switch
// off, the per-host power blocks gone, users.json open for a first admin again. It is damaged, never new.
func TestEmptyFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if d, err := Read(filepath.Join(dir, "users.json")); err != nil || d == nil || len(d) != 0 {
		t.Fatal("a missing file (a fresh install) is empty", d, err)
	}
	if err := Check(filepath.Join(dir, "users.json")); err != nil {
		t.Fatal("a missing file is fine", err)
	}
	for name, content := range map[string]string{"zero.json": "", "blank.json": " \n\t\n", "nul.json": "\x00\x00\x00\x00"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Read(p)
		if err == nil {
			t.Fatalf("%s: read as valid", name)
		}
		// the message names the file and the way out
		if !strings.Contains(err.Error(), p) || !strings.Contains(err.Error(), "restore it from a backup, or move it aside") {
			t.Fatalf("%s: %v", name, err)
		}
		if name != "nul.json" && !strings.Contains(err.Error(), "is empty") {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := Update(p, func(d Data) error { d["users"] = map[string]any{"a": map[string]any{"role": "admin"}}; return nil }); err == nil {
			t.Fatalf("%s: Update wrote over a damaged file", name)
		}
		if b, _ := os.ReadFile(p); string(b) != content {
			t.Fatalf("%s: the damaged file was changed: %q", name, b)
		}
		w := NewWatchedEvery(p, 0)
		if len(w.Get()) != 0 || w.Err() == nil || Check(p) == nil {
			t.Fatalf("%s: the watched view and Check must report it", name)
		}
	}
	// what Fairy itself writes for "nothing" is never empty, and reads back as that
	p := filepath.Join(dir, "state.json")
	if _, err := Update(p, func(Data) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); strings.TrimSpace(string(b)) != "{}" {
		t.Fatalf("%q", b)
	}
	if d, err := Read(p); err != nil || len(d) != 0 {
		t.Fatal(d, err)
	}
}

// The one way a file is replaced: the new content is synced to the disk BEFORE it takes the name, and the
// folder after, so a power cut leaves the old file or the whole new one, never a name with 0 bytes.
func TestWriteAtomicSyncsBeforeTheRename(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "users.json")
	if err := os.WriteFile(p, []byte(`{"old":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls []string
	real := fsync
	defer func() { fsync = real }()
	fsync = func(f *os.File) error {
		st, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		cur, _ := os.ReadFile(p)
		switch {
		case st.IsDir():
			calls = append(calls, "folder, name holds "+string(cur))
		default:
			tmp, _ := os.ReadFile(f.Name())
			calls = append(calls, fmt.Sprintf("temp file %q, name holds %s", tmp, cur))
			if f.Name() == p || filepath.Dir(f.Name()) != dir {
				t.Fatalf("the temporary file must be another file in the same folder: %s", f.Name())
			}
		}
		return real(f)
	}
	if err := WriteAtomic(p, []byte(`{"new":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	want := []string{`temp file "{\"new\":true}", name holds {"old":true}`, `folder, name holds {"new":true}`}
	if runtime.GOOS == "windows" {
		want = want[:1]
	}
	if strings.Join(calls, " | ") != strings.Join(want, " | ") {
		t.Fatalf("sync calls:\n got %q\nwant %q", calls, want)
	}
	if b, _ := os.ReadFile(p); string(b) != `{"new":true}` {
		t.Fatalf("%s", b)
	}
	if st, _ := os.Stat(p); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatal(st.Mode())
	}
	// Update goes the same way
	calls = nil
	if _, err := Update(p, func(d Data) error { d["x"] = 1; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(calls) != len(want) || !strings.HasPrefix(calls[0], "temp file ") || !strings.Contains(calls[0], `name holds {"new":true}`) {
		t.Fatalf("Update: %q", calls)
	}
	// a sync that fails: the old file stays, nothing is left behind
	fsync = func(*os.File) error { return errors.New("disk full") }
	before, _ := os.ReadFile(p)
	if err := WriteAtomic(p, []byte(`{"lost":true}`), 0o600); err == nil {
		t.Fatal("a failed sync must be an error")
	}
	if b, _ := os.ReadFile(p); string(b) != string(before) {
		t.Fatalf("the file changed although the write failed: %s", b)
	}
	fsync = real
	if err := WriteAtomic(filepath.Join(dir, "none", "x.json"), []byte("{}"), 0o600); err == nil {
		t.Fatal("a missing folder must be an error")
	}
	left, _ := os.ReadDir(dir)
	for _, e := range left {
		if e.Name() != "users.json" && e.Name() != "users.json.lock" {
			t.Fatalf("left behind: %s", e.Name())
		}
	}
	if err := WriteAtomic(filepath.Join(dir, "status.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(dir, "status.json")); runtime.GOOS != "windows" && st.Mode().Perm() != 0o644 {
		t.Fatal(st.Mode())
	}
}
