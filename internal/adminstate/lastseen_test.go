package adminstate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
)

// seen is AgentSeen without its error.
func seen(a *State) map[string]time.Time {
	m, _ := a.AgentSeen()
	return m
}

// When each agent was last seen, in last-seen.json: a round trip, no file yet, entries that are not of the
// form, and that state.json is not touched by it.
func TestAgentSeenInItsFile(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)
	path := filepath.Join(dir, LastSeenFile)
	if got, err := a.AgentSeen(); len(got) != 0 || err != nil {
		t.Fatal("no file yet:", got, err)
	}
	if err := a.SetWakeUpdate("pc-a", true); err != nil { // state.json is there, with something in it
		t.Fatal(err)
	}
	state, _ := os.Stat(a.StatePath)
	at := time.Date(2026, 11, 13, 16, 25, 7, 987654321, time.FixedZone("x", 3600))
	if err := a.SetAgentSeen(map[string]time.Time{"pc-a": at, "nas": at.Add(-time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	want := "map[nas:2026-11-13 14:25:07 +0000 UTC pc-a:2026-11-13 15:25:07 +0000 UTC]"
	if got := fmt.Sprint(seen(a)); got != want {
		t.Fatal("after writing:", got)
	}
	if got := fmt.Sprint(seen(New(dir))); got != want { // what the next start of the panel reads
		t.Fatal("read again:", got)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"pc-a": "2026-11-13T15:25:07Z"`) {
		t.Fatalf("an entry is the id and a time to the second: %s", raw)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatal("the file's mode:", st.Mode().Perm(), err)
	}
	// the same values again: the file is not written
	before, _ := os.Stat(path)
	if err := a.SetAgentSeen(map[string]time.Time{"pc-a": at}, []string{"nobody"}); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(path); !os.SameFile(before, after) {
		t.Error("a write that changes nothing replaced the file")
	}
	if err := a.SetAgentSeen(map[string]time.Time{"pc-a": at.Add(time.Minute)}, []string{"nas"}); err != nil { // nas was removed
		t.Fatal(err)
	}
	if got := fmt.Sprint(seen(a)); got != "map[pc-a:2026-11-13 15:26:07 +0000 UTC]" {
		t.Fatal("after removing one:", got)
	}
	// state.json is another file: none of these writes touched it, and it holds nothing of this
	if st, _ := os.Stat(a.StatePath); !os.SameFile(state, st) || !a.WakeUpdateEnabled("pc-a") {
		t.Error("state.json was written")
	}
	if raw, _ := os.ReadFile(a.StatePath); strings.Contains(string(raw), "2026-11-13") || strings.Contains(string(raw), "seen") {
		t.Fatalf("state.json: %s", raw)
	}

	// what is not of the form is not used, and goes with the next write
	_, err := statefile.Update(path, func(d statefile.Data) error {
		d["agents"] = map[string]any{"good": "2026-11-13T15:25:07Z", "no-time": "yesterday", "a-number": 17.0, "an-object": map[string]any{"at": "2026-11-13T15:25:07Z"},
			"": "2026-11-13T15:25:07Z", "year-1": "0001-01-01T00:00:00Z", strings.Repeat("i", 129): "2026-11-13T15:25:07Z", "long": "2026-11-13T15:25:07Z" + strings.Repeat(" ", 40)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	b := New(dir)
	if got := fmt.Sprint(seen(b)); got != "map[good:2026-11-13 15:25:07 +0000 UTC]" {
		t.Fatal("entries that are not of the form:", got)
	}
	if err := b.SetAgentSeen(map[string]time.Time{"pc-b": at}, nil); err != nil {
		t.Fatal(err)
	}
	d, _ := statefile.Read(path)
	if m, _ := d["agents"].(map[string]any); len(m) != 2 || m["good"] == nil || m["pc-b"] == nil {
		t.Fatalf("after the next write: %v", d["agents"])
	}
	// a value of another type under the key, and a key the panel does not write: read as nothing, and
	// replaced by the next write
	if _, err := statefile.Update(path, func(d statefile.Data) error { d["agents"], d["other"] = []any{"x"}, "y"; return nil }); err != nil {
		t.Fatal(err)
	}
	c := New(dir)
	if got := seen(c); len(got) != 0 {
		t.Fatal("a list under the key:", got)
	}
	if err := c.SetAgentSeen(map[string]time.Time{"pc-c": at}, nil); err != nil || len(seen(c)) != 1 {
		t.Fatal("a write over a list:", err, seen(c))
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "other") {
		t.Fatalf("the file holds more than its one key: %s", raw)
	}
}

// A file that cannot be used is not used, says why, and is not overwritten: empty, cut off, not an object.
func TestAgentSeenFileThatCannotBeUsed(t *testing.T) {
	for name, content := range map[string]string{"empty": "", "cut off": `{"agents": {"pc-a": "2026-11-13T15:2`, "not an object": `["pc-a"]`} {
		dir := t.TempDir()
		path := filepath.Join(dir, LastSeenFile)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		a := New(dir)
		if got, err := a.AgentSeen(); len(got) != 0 || err == nil || !strings.Contains(err.Error(), LastSeenFile) {
			t.Errorf("%s: %v, error %v", name, got, err)
		}
		if err := a.SetAgentSeen(map[string]time.Time{"pc-a": time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)}, nil); err == nil {
			t.Errorf("%s: the file was written", name)
		}
		if raw, _ := os.ReadFile(path); string(raw) != content {
			t.Errorf("%s: the file was changed: %q", name, raw)
		}
	}
}

// The file stays small: at most MaxAgentSeen entries are kept (the newest), and a file that holds more, which
// this panel never wrote, is not used.
func TestAgentSeenIsBounded(t *testing.T) {
	a := New(t.TempDir())
	base := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	set := map[string]time.Time{}
	for i := 0; i < MaxAgentSeen+50; i++ {
		set[fmt.Sprintf("agent-%04d", i)] = base.Add(time.Duration(i) * time.Second)
	}
	if err := a.SetAgentSeen(set, nil); err != nil {
		t.Fatal(err)
	}
	got := seen(a)
	if _, oldest := got["agent-0049"]; len(got) != MaxAgentSeen || oldest || got["agent-0050"].IsZero() || got[fmt.Sprintf("agent-%04d", MaxAgentSeen+49)].IsZero() {
		t.Fatalf("%d entries, the oldest kept: %v", len(got), oldest)
	}
	path := filepath.Join(filepath.Dir(a.StatePath), LastSeenFile)
	if st, _ := os.Stat(path); st.Size() > 64<<10 {
		t.Errorf("the file is %d bytes when it is full", st.Size())
	}
	_, err := statefile.Update(path, func(d statefile.Data) error {
		m := d["agents"].(map[string]any)
		for i := 0; i < 10; i++ {
			m[fmt.Sprintf("more-%d", i)] = "2026-11-13T15:25:07Z"
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	b := New(filepath.Dir(a.StatePath))
	if got, err := b.AgentSeen(); len(got) != 0 || err == nil {
		t.Fatalf("a list longer than the limit is used: %d entries, error %v", len(got), err)
	}
	// the next write keeps the newest MaxAgentSeen of what is there
	if err := b.SetAgentSeen(map[string]time.Time{"new": base.Add(48 * time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	if got := seen(b); len(got) != MaxAgentSeen || got["new"].IsZero() {
		t.Fatalf("after the next write: %d entries", len(got))
	}
}
