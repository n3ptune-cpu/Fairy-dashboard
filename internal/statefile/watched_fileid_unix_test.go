//go:build !windows

package statefile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every write puts another file under the name, and the stamp says so: the device and the number of the file
// are in it, and they are not those of the file the write replaced.
func TestEveryWriteIsAnotherFileToAStamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	writeSite(t, path, "site-a")
	last, err := look(path)
	if err != nil || last.id.ino == 0 {
		t.Fatalf("the first file: %+v, %v", last, err)
	}
	for _, site := range []string{"site-b", "site-c", "site-d", "site-e"} {
		writeSite(t, path, site)
		now, err := look(path)
		if err != nil || now.id.ino == 0 || now.id.ino == last.id.ino || now.id.dev != last.id.dev {
			t.Fatalf("%s: the file is %+v, the one before it was %+v (%v)", site, now.id, last.id, err)
		}
		last = now
	}
}

// A file that is written in place (an editor, a tool of the administrator) keeps its number.
func TestAFileWrittenInPlaceKeepsItsNumber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	writeSite(t, path, "site-a")
	a, _ := look(path)
	if err := os.WriteFile(path, []byte(`{"site": "site-b"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := look(path); err != nil || b.id.ino != a.id.ino || b.id.dev != a.id.dev {
		t.Errorf("written in place: %+v, was %+v (%v)", b.id, a.id, err)
	}
}

// A name that cannot be looked at is judged again at every look, and is one version for as long as the reason
// is the same one. Another reason is another thing: the version moves and the view says the new reason.
func TestAnotherReasonForAFileOutOfReachIsAChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.Symlink(filepath.Join(dir, "not-mounted", "state.json"), path); err != nil {
		t.Fatal(err)
	}
	w := NewWatchedEvery(path, 0)
	if d, err := w.Get(), w.Err(); len(d) != 0 || err == nil || !strings.Contains(err.Error(), "a symbolic link whose target does not exist") {
		t.Fatalf("a link to nothing: data %v, error %v", d, err)
	}
	v := w.Version
	for i := 0; i < 5; i++ {
		w.Get()
		w.Look()
		w.Refresh()
	}
	if w.Version != v {
		t.Errorf("looks at the same link to nothing moved the version from %d to %d", v, w.Version)
	}
	// the name is now a link to itself: still out of reach, for another reason
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, path); err != nil {
		t.Fatal(err)
	}
	d, err := w.Get(), w.Err()
	if len(d) != 0 || err == nil || strings.Contains(err.Error(), "target does not exist") || w.Version != v+1 {
		t.Errorf("a link to itself: data %v, error %v, version %d (want %d)", d, err, w.Version, v+1)
	}
	for i := 0; i < 5; i++ {
		w.Get()
		w.Look()
		w.Refresh()
	}
	if w.Version != v+1 {
		t.Errorf("looks at the same link to itself moved the version from %d to %d", v+1, w.Version)
	}
}
