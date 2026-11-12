package notifications

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// A notifications.json that cannot be read as the list is moved aside and the list starts empty. The copy has
// the time in its name and never replaces an older one (audit 5, r3-p4-09: it was <name>.corrupt, written over
// by the next damaged file).
func TestACorruptListIsKeptAsideEveryTime(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "notifications.json")
	for _, content := range []string{"{cut off", "not json at all"} {
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		s := Open(p)
		if page := s.List("u", func(Event) bool { return true }, Filter{}); len(page.Events) != 0 {
			t.Fatalf("a corrupt list starts with %d events", len(page.Events))
		}
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatal("the corrupt list is still in place")
		}
	}
	copies, _ := filepath.Glob(p + ".corrupt-*")
	var held []string
	for _, c := range copies {
		b, _ := os.ReadFile(c)
		held = append(held, string(b))
	}
	sort.Strings(held)
	if len(held) != 2 || held[0] != "not json at all" || held[1] != "{cut off" {
		t.Fatalf("the kept copies hold %q, want both damaged lists", held)
	}
	if _, err := os.Lstat(p + ".corrupt"); err == nil {
		t.Fatal("a copy under the old name")
	}
}
