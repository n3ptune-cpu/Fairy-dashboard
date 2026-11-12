//go:build !windows

package statefile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// r2-p2-10 (audit 5, round 2): a state file that is not a plain file. wake-update.json as a link to /dev/zero
// ended the panel with "out of memory" at every start (its size is 0, and reading it never ends); as a named
// pipe the panel never started (the open waits for a writer); a named pipe as the sign-in picture hung the
// public picture route for good. Every state file is now opened the same way: a regular file, the last part
// of the path not followed, without waiting, and read up to a limit.
func TestOnlyAPlainFileOfASaneSizeIsRead(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.json")
	if err := os.WriteFile(plain, []byte(`{"a": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	mk := func(name string, make func(p string) error) string {
		p := filepath.Join(dir, name)
		if err := make(p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := map[string]struct{ path, what string }{
		"a link to /dev/zero":    {mk("zero", func(p string) error { return os.Symlink("/dev/zero", p) }), "a symbolic link"},
		"a link to a plain file": {mk("link", func(p string) error { return os.Symlink(plain, p) }), "a symbolic link"},
		"a link to nothing":      {mk("dangling", func(p string) error { return os.Symlink(filepath.Join(dir, "none"), p) }), "a symbolic link"},
		"a named pipe":           {mk("pipe", func(p string) error { return syscall.Mkfifo(p, 0o600) }), "a named pipe"},
		"a folder":               {mk("folder", func(p string) error { return os.Mkdir(p, 0o700) }), "a folder"},
		"a file that is too large": {mk("large", func(p string) error {
			f, err := os.Create(p)
			if err != nil {
				return err
			}
			defer f.Close()
			return f.Truncate(MaxBytes + 1) // sparse: it takes no room on the disk
		}), "larger than 67108864 bytes"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			done := make(chan error, 3)
			go func() { _, err := Read(c.path); done <- err }()
			go func() { done <- Check(c.path) }()
			go func() { _, err := Update(c.path, func(d Data) error { d["x"] = 1; return nil }); done <- err }()
			for i := 0; i < 3; i++ {
				select {
				case err := <-done:
					var nf *NotAFileError
					if !errors.As(err, &nf) || nf.What != c.what || !strings.Contains(err.Error(), "Fairy does not read it") {
						t.Fatalf("the error: %v", err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("reading it does not come back")
				}
			}
			if st, err := os.Lstat(c.path); err != nil || st.Mode().IsRegular() && name != "a file that is too large" {
				t.Fatalf("it was replaced: %v %v", st, err) // a refused file is never overwritten
			}
		})
	}
	// a plain file and a missing one are read as before
	if d, err := Read(plain); err != nil || d["a"] != 1.0 {
		t.Fatalf("a plain file: %v %v", d, err)
	}
	if d, err := Read(filepath.Join(dir, "missing.json")); err != nil || len(d) != 0 {
		t.Fatalf("a missing file: %v %v", d, err)
	}
	if b, err := ReadFile(plain, 8); err != nil || string(b) != `{"a": 1}` {
		t.Fatalf("exactly at the limit: %q %v", b, err)
	}
	if _, err := ReadFile(plain, 7); !IsNotAFile(err) {
		t.Fatalf("one byte over the limit: %v", err)
	}
	if _, err := ReadFile(filepath.Join(dir, "missing.json"), 8); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing file: %v", err)
	}
	// the folder that holds the file may be reached through a link (a data folder on another disk)
	if err := os.Symlink(dir, dir+"-link"); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(dir + "-link")
	if d, err := Read(filepath.Join(dir+"-link", "plain.json")); err != nil || d["a"] != 1.0 {
		t.Fatalf("through a linked folder: %v %v", d, err)
	}
}
