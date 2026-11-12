//go:build !windows

package statefile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Audit 6, r4-p2-04: a state file that becomes a symbolic link whose target is away, under a running panel,
// read as "no such file": an empty state without an error, which for state.json is "no kill switch, no host
// switched off, no revoke-all". Thirteen other odd shapes failed closed; this one is what a link to a share
// that is not mounted looks like. Only a name that is not there at all is a fresh start.
func TestAWatchedFileThatIsALinkToNothingFailsClosed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := os.WriteFile(p, []byte(`{"control_disabled": {"by": "root"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w := NewWatchedEvery(p, 0)
	if d := w.Get(); d["control_disabled"] == nil || w.Err() != nil {
		t.Fatalf("the set-up: %v, %v", d, w.Err())
	}
	// the file is moved away and a link to a place that does not exist takes its name
	if err := os.Rename(p, filepath.Join(dir, "elsewhere")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "not-mounted", "state.json"), p); err != nil {
		t.Fatal(err)
	}
	if d, err := w.Get(), w.Err(); len(d) != 0 || err == nil || !strings.Contains(err.Error(), "a symbolic link whose target does not exist") {
		t.Errorf("a link to nothing: data %v, error %v; want no data and an error that names the link", d, err)
	}
	// looked at again and again, the same state is one version: nobody is told of a change that is none
	v := w.Version
	for i := 0; i < 20; i++ {
		w.Get()
	}
	if w.Version != v {
		t.Errorf("20 looks at the same unreadable file moved the version from %d to %d", v, w.Version)
	}
	// the target appears: still a link, which the panel does not read through (as before)
	if err := os.MkdirAll(filepath.Join(dir, "not-mounted"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "not-mounted", "state.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if d, err := w.Get(), w.Err(); len(d) != 0 || err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("a link to a file: data %v, error %v; want no data and an error", d, err)
	}
	// the name is gone altogether: a fresh start, no error (as before)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if d, err := w.Get(), w.Err(); len(d) != 0 || err != nil {
		t.Errorf("no file: data %v, error %v; want an empty state without an error", d, err)
	}
	// and the file itself back in its place is read again
	if err := os.Rename(filepath.Join(dir, "elsewhere"), p); err != nil {
		t.Fatal(err)
	}
	if d, err := w.Get(), w.Err(); d["control_disabled"] == nil || err != nil {
		t.Errorf("the file back: data %v, error %v", d, err)
	}
}
