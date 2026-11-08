package health

import (
	"os"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy/internal/model"
)

func TestWriteReadRoundTripAndKeyOrder(t *testing.T) {
	dir := t.TempDir() + "/data"
	in := Report{
		Jobs:         map[string]Job{"agent:a": {EffectiveS: model.P(1.0), Runs: 3}},
		PowerSources: []Source{{ID: "plug", Status: "ok"}},
		Sources:      []model.SourceStatus{{Name: "agent:a", OK: true}},
		StartedAt:    "2026-11-08T10:00:00Z",
		SystemIDs:    []string{"a"},
		Systems:      map[string]int{"online": 1},
		UPS:          []Source{},
		Version:      7,
		WrittenAt:    "2026-11-08T10:00:05Z",
	}
	if err := Write(dir, in); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(Path(dir))
	if err != nil || st.Mode().Perm() != 0o600 && os.PathSeparator == '/' {
		t.Fatal("health.json must be written with mode 0600", err)
	}
	b, _ := os.ReadFile(Path(dir))
	// The file was historically written from a map, so its keys are in alphabetical order.
	keys := []string{"control_disabled", "jobs", "power_sources", "sources", "started_at", "system_ids", "systems",
		"ups", "version", "written_at"}
	last := -1
	for _, k := range keys {
		i := strings.Index(string(b), "\n \""+k+"\":")
		if i <= last {
			t.Fatalf("key %q missing or out of order in\n%s", k, b)
		}
		last = i
	}
	out, err := Read(dir)
	if err != nil || out.Version != 7 || out.Jobs["agent:a"].Runs != 3 || out.Systems["online"] != 1 ||
		out.WrittenAt != in.WrittenAt || len(out.Sources) != 1 || !out.Sources[0].OK {
		t.Fatalf("round trip: %+v %v", out, err)
	}
	if _, err := Read(t.TempDir()); err == nil {
		t.Fatal("a missing file must be an error")
	}
}
