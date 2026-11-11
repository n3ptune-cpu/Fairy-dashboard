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

// The last known agent versions in state.json: a round trip, a file from before the key existed, entries that
// are not of the form, and a panel that does not know the key (it rewrites the file as the object it read).
func TestAgentVersionsInStateFile(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)
	if got := a.AgentVersions(); len(got) != 0 {
		t.Fatal("no file yet:", got)
	}
	at := time.Date(2026, 11, 11, 19, 20, 36, 0, time.UTC)
	for id, v := range map[string]string{"pc-a": "16-a791c55", "nas": "15-0123abc"} {
		if err := a.SetAgentVersion(id, v, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.SetAgentVersion("pc-a", "17-bbbbbbb", at.Add(time.Hour)); err != nil { // after an update
		t.Fatal(err)
	}
	want := "map[nas:15-0123abc pc-a:17-bbbbbbb]"
	if got := fmt.Sprint(a.AgentVersions()); got != want {
		t.Fatal("after writing:", got)
	}
	if got := fmt.Sprint(New(dir).AgentVersions()); got != want { // what the next start of the panel reads
		t.Fatal("read again:", got)
	}
	raw, _ := os.ReadFile(a.StatePath)
	if !strings.Contains(string(raw), `"recorded": "2026-11-11T20:20:36Z"`) {
		t.Fatalf("the entry says when it was written: %s", raw)
	}
	if st, err := os.Stat(a.StatePath); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatal("state.json mode:", st.Mode().Perm(), err)
	}

	// the other keys of the file and this one do not touch each other
	if err := a.SetWakeUpdate("pc-a", true); err != nil {
		t.Fatal(err)
	}
	if err := a.SetPoll("nas", 5.0); err != nil {
		t.Fatal(err)
	}
	if err := a.SetAgentVersion("nas", "", time.Time{}); err != nil { // the host was removed
		t.Fatal(err)
	}
	if got := fmt.Sprint(a.AgentVersions()); got != "map[pc-a:17-bbbbbbb]" || !a.WakeUpdateEnabled("pc-a") {
		t.Fatal("after removing one:", got)
	}
	if d, _ := a.StateData(); fmt.Sprint(d["poll_overrides"]) != "map[nas:5]" {
		t.Fatal("poll_overrides:", d["poll_overrides"])
	}
	before, _ := os.ReadFile(a.StatePath)
	if err := a.SetAgentVersion("nobody", "", time.Time{}); err != nil { // nothing to forget: nothing is written
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(a.StatePath); string(after) != string(before) {
		t.Fatal("forgetting an agent that has no entry rewrote the file")
	}
	if err := a.SetAgentVersion("pc-a", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if d, _ := statefile.Read(a.StatePath); d["agent_versions"] != nil || d["wake_update_hosts"] == nil {
		t.Fatal("the last entry gone: the key goes too, the rest stays:", d)
	}
}

// A state.json written before the key existed loads as it did, and gets the key when an agent first answers.
func TestStateFileFromBeforeAgentVersions(t *testing.T) {
	dir := t.TempDir()
	old, err := os.ReadFile(filepath.Join("..", "testutil", "testdata", "v-before", "var-lib-fairy", "state.json.old"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), old, 0o600); err != nil {
		t.Fatal(err)
	}
	a := New(dir)
	if got := a.AgentVersions(); len(got) != 0 {
		t.Fatal("an old file has no versions:", got)
	}
	if d, _ := a.StateData(); fmt.Sprint(d["poll_overrides"]) != "map[laptop:5]" || a.ControlDisabled() != nil {
		t.Fatal("the old file's own content:", d)
	}
	if err := a.SetAgentVersion("laptop", "16-a791c55", time.Now()); err != nil {
		t.Fatal(err)
	}
	if d, _ := a.StateData(); fmt.Sprint(d["poll_overrides"]) != "map[laptop:5]" || fmt.Sprint(a.AgentVersions()) != "map[laptop:16-a791c55]" {
		t.Fatal("after the first entry:", d)
	}
}

// Entries that are not {version: text} are skipped, whatever else is in the file; nothing fails.
func TestAgentVersionsOfAnotherForm(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ file, want string }{
		{`{"agent_versions": "16-a791c55"}`, "map[]"},
		{`{"agent_versions": ["16-a791c55"]}`, "map[]"},
		{`{"agent_versions": null, "poll_overrides": {"laptop": 5}}`, "map[]"},
		{`{"agent_versions": {"a": "16-a791c55", "b": 16, "c": {"version": 16}, "d": {"version": ""}, "e": {}, "": {"version": "1-abcdefg"},
		   "f": {"version": "` + strings.Repeat("9", 65) + `"}, "ok": {"version": "16-a791c55", "recorded": 7, "later_field": [1, 2]}}}`, "map[ok:16-a791c55]"},
	} {
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(tc.file), 0o600); err != nil {
			t.Fatal(err)
		}
		a := New(dir)
		if got := fmt.Sprint(a.AgentVersions()); got != tc.want || a.state.Err() != nil {
			t.Errorf("%s: %s (%v)", tc.file, got, a.state.Err())
		}
	}
}

// A panel from before this key (a downgrade) reads state.json as one JSON object, uses the keys it knows and
// writes the whole object back: the versions are neither read wrongly nor lost. This is that read-change-write,
// done the way every older setter does it.
func TestOlderPanelLeavesAgentVersionsAlone(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)
	if err := a.SetAgentVersion("pc-a", "16-a791c55", time.Date(2026, 11, 11, 19, 20, 36, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	// what the older panel does with the file: its kill switch, its polling override, its sessions cut-off
	for _, change := range []func(statefile.Data) error{
		func(d statefile.Data) error {
			d["control_disabled"] = map[string]any{"by": "admin", "reason": "test"}
			return nil
		},
		func(d statefile.Data) error { d["poll_overrides"] = map[string]any{"pc-a": "paused"}; return nil },
		func(d statefile.Data) error {
			delete(d, "control_disabled")
			d["sessions_revoked_before"] = 1.0
			return nil
		},
	} {
		if _, err := statefile.Update(a.StatePath, change); err != nil {
			t.Fatal(err)
		}
	}
	d, err := statefile.Read(a.StatePath)
	if err != nil || fmt.Sprint(d["agent_versions"]) != "map[pc-a:map[recorded:2026-11-11T19:20:36Z version:16-a791c55]]" {
		t.Fatal("after the older panel's writes:", d, err)
	}
	if got := fmt.Sprint(New(dir).AgentVersions()); got != "map[pc-a:16-a791c55]" { // and the newer panel, started again
		t.Fatal(got)
	}
}
