package adminstate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// SetAgentSeenAside moves a plain last-seen.json to last-seen.json.bad, as it is, and the file can be started
// over. It moves nothing else: no link (whatever it points to), no folder; and it does not move when something
// that is no plain file has the name of the kept copy.
func TestSetAgentSeenAside(t *testing.T) {
	at := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	t.Run("a plain file, twice", func(t *testing.T) {
		dir := t.TempDir()
		a, path := New(dir), filepath.Join(dir, LastSeenFile)
		for _, content := range []string{"first, damaged", "second, damaged"} {
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			aside, err := a.SetAgentSeenAside()
			if err != nil || aside != filepath.Join(dir, LastSeenAside) {
				t.Fatalf("%q %v", aside, err)
			}
			if raw, _ := os.ReadFile(aside); string(raw) != content { // the older kept copy is replaced
				t.Fatalf("the kept copy holds %q, want %q", raw, content)
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the file is still there: %v", err)
			}
			// and the file can be written again
			if err := a.SetAgentSeen(map[string]time.Time{"pc-a": at}, nil); err != nil {
				t.Fatal(err)
			}
			if got, err := a.AgentSeen(); err != nil || !got["pc-a"].Equal(at) {
				t.Fatalf("after the fresh start: %v %v", got, err)
			}
		}
		names, _ := os.ReadDir(dir)
		for _, n := range names {
			if n.Name() != LastSeenFile && n.Name() != LastSeenAside && n.Name() != LastSeenFile+".lock" {
				t.Errorf("left in the folder: %s", n.Name())
			}
		}
	})
	t.Run("no file", func(t *testing.T) {
		if _, err := New(t.TempDir()).SetAgentSeenAside(); !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	})
	for name, make := range map[string]func(path, other string) error{
		"a link to a file":  func(path, other string) error { return os.Symlink(other, path) },
		"a link to nothing": func(path, other string) error { return os.Symlink(other+".none", path) },
		"a folder":          func(path, other string) error { return os.Mkdir(path, 0o700) },
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			a, path := New(dir), filepath.Join(dir, LastSeenFile)
			other := filepath.Join(t.TempDir(), "other.json")
			if err := os.WriteFile(other, []byte("somebody's file"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := make(path, other); err != nil {
				t.Skipf("cannot be made here: %v", err)
			}
			before, _ := os.Lstat(path)
			if aside, err := a.SetAgentSeenAside(); err == nil {
				t.Fatalf("moved to %s", aside)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("it was touched: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(dir, LastSeenAside)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a kept copy was made: %v", err)
			}
			if raw, _ := os.ReadFile(other); string(raw) != "somebody's file" {
				t.Fatalf("the file the link points to: %q", raw)
			}
		})
	}
	for name, make := range map[string]func(bad string) error{
		"a folder": func(bad string) error { return os.Mkdir(bad, 0o700) },
		"a link":   func(bad string) error { return os.Symlink(bad+".target", bad) },
	} {
		t.Run("the kept copy's name is "+name, func(t *testing.T) {
			dir := t.TempDir()
			a, path := New(dir), filepath.Join(dir, LastSeenFile)
			if err := os.WriteFile(path, []byte("damaged"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := make(filepath.Join(dir, LastSeenAside)); err != nil {
				t.Skipf("cannot be made here: %v", err)
			}
			if aside, err := a.SetAgentSeenAside(); err == nil {
				t.Fatalf("moved to %s", aside)
			}
			if raw, _ := os.ReadFile(path); string(raw) != "damaged" {
				t.Fatalf("the file was moved or changed: %q", raw)
			}
			if _, err := os.Lstat(filepath.Join(dir, LastSeenAside) + ".target"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("written through the link")
			}
		})
	}
}
