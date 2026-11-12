package notifications

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Audit 5, round 3 (r3-p4-04): a stop in the middle of a write of the list left its temporary file in the data
// folder (.notifications.json.<random>.tmp), and nothing ever removed it. Open removes them; nothing else.
func TestOpenRemovesTheTemporaryFilesOfACutOffWrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "notifications.json")
	s := Open(p)
	s.Add(Event{Kind: "wake_sent", Type: "power", Severity: Info, SystemID: "pc-a", Host: "PC-A", Text: "Wake-on-LAN sent by Ann"})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	good, _ := os.ReadFile(p)
	stale := []string{".notifications.json.1234567890.tmp", ".notifications.json.987.tmp"}
	keep := []string{".state.json.1234567890.tmp", "notifications.json.corrupt-20261112T140501Z", ".notifications.json.lock", "notifications.json.tmp", ".notifications.jsonx.1.tmp"}
	for _, n := range append(append([]string{}, stale...), keep...) {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("half a li"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, ".notifications.json.folder.tmp"), 0o700); err != nil { // not a plain file: left alone
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(p, filepath.Join(dir, ".notifications.json.link.tmp")); err != nil {
			t.Fatal(err)
		}
	}
	s = Open(p)
	for _, n := range stale {
		if _, err := os.Lstat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Errorf("%s is still there", n)
		}
	}
	for _, n := range append(keep, ".notifications.json.folder.tmp") {
		if _, err := os.Lstat(filepath.Join(dir, n)); err != nil {
			t.Errorf("%s was removed: %v", n, err)
		}
	}
	if runtime.GOOS != "windows" {
		if _, err := os.Lstat(filepath.Join(dir, ".notifications.json.link.tmp")); err != nil {
			t.Errorf("a link was removed: %v", err)
		}
	}
	if b, _ := os.ReadFile(p); string(b) != string(good) {
		t.Fatal("the list itself was changed")
	}
	if page := s.List("u", func(Event) bool { return true }, Filter{}); len(page.Events) != 1 {
		t.Fatalf("the list after the start: %d events", len(page.Events))
	}
}
