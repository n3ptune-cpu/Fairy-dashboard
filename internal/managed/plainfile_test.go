package managed

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Audit 5, round 3 (r3-p4-03): managed-hosts.yaml, and the tokens of the hosts in it, are read as plain files
// only, like every state file. A list that is a symbolic link is an error of Merge, which the panel's start
// refuses on; it used to start without the hosts. A data folder that is itself a link is fine.
func TestTheListAndTheTokensMustBePlainFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need a privilege on Windows")
	}
	fill := func(dir string) *Store {
		t.Helper()
		s := NewStore(dir)
		for _, id := range []string{"pc-a", "pc-b"} {
			if err := s.Put(Host{ID: id, Name: id, URL: "http://" + id + ":9101", OS: "linux"}, Secret{Token: tokA + id, ControlToken: tokB + id}); err != nil {
				t.Fatal(err)
			}
		}
		return s
	}
	merge := func(s *Store) (int, []string, error) {
		cfg := testutil.Config(t, []string{"https://pve.example:8006"}, "http://127.0.0.1:1", "http://127.0.0.1:2")
		added, skipped, err := s.Merge(cfg)
		return len(added), skipped, err
	}
	// as written by the panel: both hosts
	s := fill(t.TempDir())
	if n, skipped, err := merge(s); err != nil || n != 2 || len(skipped) != 0 {
		t.Fatal(n, skipped, err)
	}
	// the list is a link to the list (somebody moved it to another disk)
	s = fill(t.TempDir())
	elsewhere := filepath.Join(t.TempDir(), "managed-hosts.yaml")
	if err := os.Rename(s.Path(), elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, s.Path()); err != nil {
		t.Fatal(err)
	}
	n, skipped, err := merge(s)
	if err == nil || n != 0 || skipped != nil || !statefile.IsNotAFile(err) || !strings.Contains(err.Error(), s.Path()+" is a symbolic link") {
		t.Fatalf("managed-hosts.yaml as a link: %d hosts, skipped %v, error %v", n, skipped, err)
	}
	if hosts, err := s.Load(); err == nil || hosts != nil {
		t.Fatalf("Load: %v %v", hosts, err)
	}
	// one host's tokens file is a link
	s = fill(t.TempDir())
	elsewhere = filepath.Join(t.TempDir(), "agent-pc-b.json")
	if err := os.Rename(s.secretPath("pc-b"), elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, s.secretPath("pc-b")); err != nil {
		t.Fatal(err)
	}
	n, skipped, err = merge(s)
	if err == nil || n != 0 || !statefile.IsNotAFile(err) || !strings.Contains(err.Error(), "the tokens of the host pc-b: "+s.secretPath("pc-b")+" is a symbolic link") {
		t.Fatalf("a tokens file as a link: %d hosts, skipped %v, error %v", n, skipped, err)
	}
	if _, err := s.Secret("pc-b"); !statefile.IsNotAFile(err) {
		t.Fatalf("Secret through a link: %v", err)
	}
	// a tokens file that is missing, or holds no tokens, is one host that is skipped, as before
	s = fill(t.TempDir())
	if err := os.Remove(s.secretPath("pc-a")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.secretPath("pc-b"), []byte("{cut"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, skipped, err = merge(s)
	if err != nil || n != 0 || len(skipped) != 2 || !strings.Contains(skipped[0], "managed host pc-a: tokens unreadable") || !strings.Contains(skipped[1], "Re-pair it") {
		t.Fatalf("tokens missing and cut off: %d hosts, skipped %v, error %v", n, skipped, err)
	}
	if _, err := s.Secret("pc-a"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	// a list that is not YAML is an error too
	s = fill(t.TempDir())
	if err := os.WriteFile(s.Path(), []byte("hosts: [{id: pc-a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, _, err := merge(s); err == nil || n != 0 || !strings.Contains(err.Error(), s.Path()) {
		t.Fatalf("a list that is cut off: %d hosts, error %v", n, err)
	}
	// the data folder itself is a link, and so is its secrets folder: fine (only the file's own name is not followed)
	real := t.TempDir()
	fill(real)
	moved := filepath.Join(t.TempDir(), "secrets-on-another-disk")
	if err := os.Rename(filepath.Join(real, "secrets"), moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, filepath.Join(real, "secrets")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "data")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if n, skipped, err := merge(NewStore(link)); err != nil || n != 2 || len(skipped) != 0 {
		t.Fatalf("a linked data folder with a linked secrets folder: %d hosts, skipped %v, error %v", n, skipped, err)
	}
}
