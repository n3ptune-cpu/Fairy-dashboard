package adminstate

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
)

// machines.json: what is written is what is read, also by a State made later (a restart of the panel); the
// file has mode 0600 and no temporary file is left next to it; a record that is dropped is gone.
func TestMachinesFile(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)
	if recs, err := a.Machines(); err != nil || len(recs) != 0 {
		t.Fatal("no file yet: no records, no error:", recs, err)
	}
	at := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	want := map[string]approval.Record{
		"pc-a": {MachineID: "machine-id-A-0123456789ab", How: approval.PinTurnOn, At: at, By: "nora@example.test", State: "required", StateAt: at},
		"pc-b": {MachineID: "machine-id-B-0123456789ab", How: approval.PinFirstSight, At: at, By: "Fairy", State: "broken", StateAt: at.Add(time.Hour), Holder: true},
		"pc-c": {State: "required", StateAt: at}, // an old test build: the check without an id the panel could take
	}
	if err := a.SetMachines(want, nil); err != nil {
		t.Fatal(err)
	}
	got, err := New(dir).Machines()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("read back: %v\n got %+v\nwant %+v", err, got, want)
	}
	p := MachinesPath(dir)
	if st, err := os.Stat(p); err != nil || (runtime.GOOS != "windows" && st.Mode().Perm() != 0o600) {
		t.Fatal("the file:", st, err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*"))
	for _, f := range left {
		if b := filepath.Base(f); b != MachinesFile && b != MachinesFile+".lock" {
			t.Errorf("left in the data folder: %s", b)
		}
	}
	raw, _ := os.ReadFile(p)
	for _, w := range []string{`"machines"`, `"machine_id": "machine-id-A-0123456789ab"`, `"pinned": "turn-on"`, `"pinned_by": "nora@example.test"`,
		`"pinned_at": "2026-11-12T10:00:00Z"`, `"state": "broken"`, `"holder": true`} {
		if !strings.Contains(string(raw), w) {
			t.Errorf("the file lacks %s:\n%s", w, raw)
		}
	}
	// one record changes, one goes: the third is as it was
	b := want["pc-b"]
	b.MachineID, b.How, b.Holder = "machine-id-B2-0123456789a", approval.PinAccepted, false
	if err := a.SetMachines(map[string]approval.Record{"pc-b": b}, []string{"pc-c"}); err != nil {
		t.Fatal(err)
	}
	got, err = a.Machines()
	if err != nil || len(got) != 2 || got["pc-b"] != b || got["pc-a"] != want["pc-a"] {
		t.Fatalf("after the change: %v %+v", err, got)
	}
}

// It fails closed: a file that is not what it should be is an error, never "no records", and it is not
// written over.
func TestMachinesFileFailsClosed(t *testing.T) {
	for name, content := range map[string]string{
		"not JSON":                `{"machines": {`,
		"an empty file":           ``, // the shared helper's rule: Fairy never writes one, so it is damaged
		"machines is a list":      `{"machines": []}`,
		"a record that is a text": `{"machines": {"pc-a": "machine-id-A-0123456789ab"}}`,
		"an id that is none":      `{"machines": {"pc-a": {"machine_id": "short"}}}`,
		"an id with a space":      `{"machines": {"pc-a": {"machine_id": "machine id A 0123456789abcdef"}}}`,
	} {
		dir := t.TempDir()
		p := MachinesPath(dir)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		a := New(dir)
		if recs, err := a.Machines(); err == nil {
			t.Errorf("%s: read as %v", name, recs)
		}
		err := a.SetMachines(map[string]approval.Record{"pc-b": {MachineID: "machine-id-B-0123456789ab", State: "required"}}, nil)
		after, _ := os.ReadFile(p)
		if err == nil || string(after) != content {
			t.Errorf("%s: written over (%v)", name, err)
		}
	}
}
