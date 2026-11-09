package ups

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A hostile (or broken) agent cannot break the UPS page: odd types, unknown states, huge texts and lists, too
// many rows and a flapping UPS are cut down to what the page expects.
func TestHostileAgentUPSRows(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(nil, 5, time.Second, 60, 3600, filepath.Join(dir, "ups-events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("A", 1<<20)
	var rows []map[string]any
	rows = append(rows,
		map[string]any{"name": "evil", "state": "constructor", "model": long + "\x1b[2J", "mfr": 42.0, "flags": []any{"overload", 7.0, "__proto__", "overload"},
			"battery_charge": "full", "battery_runtime_s": 1e300, "status_raw": "OL\x1b]0;x\x07", "description": long},
		map[string]any{"name": "../../etc", "state": "online"},
		map[string]any{"name": long, "state": "online"},
		map[string]any{"name": 12.0, "state": "online"},
		map[string]any{"name": "flaky", "ok": false, "error": map[string]any{"x": long}},
	)
	for i := range 100 {
		rows = append(rows, map[string]any{"name": fmt.Sprintf("ups%d", i), "state": "online"})
	}
	m.UpdateFromAgent("bad", rows, false, nil, nil)
	ov := m.Overview(true)
	if len(ov.UPS) > MaxAgentUPS {
		t.Fatalf("%d UPS rows kept", len(ov.UPS))
	}
	for _, u := range ov.UPS {
		if !states[u.Status] || strings.ContainsAny(u.ID, "./") && !strings.HasPrefix(u.ID, "bad/") || len(u.Name) > maxLabel {
			t.Fatalf("%+v", u)
		}
	}
	st := m.State("bad/evil")
	if st == nil || st.Status != "unknown" || len(*st.Model) > maxText || strings.Contains(*st.Model, "\x1b") ||
		strings.Contains(*st.StatusRaw, "\x1b") || st.Mfr != nil || len(st.Flags) != 1 || st.Flags[0] != "overload" ||
		st.BatteryCharge != nil || st.BatteryRuntimeS != nil {
		t.Fatalf("%+v", st)
	}
	if m.State("bad/../../etc") != nil {
		t.Fatal("path-like name kept")
	}
	b, err := json.Marshal(ov)
	if err != nil || len(b) > 64<<10 {
		t.Fatal("overview", len(b), err)
	}
	// a UPS that flaps every poll: events (and their log lines and notifications) stop at the shared limiter's
	// burst (10 per 10 minutes)
	for i := range 200 {
		state := "online"
		if i%2 == 0 {
			state = "on_battery"
		}
		m.UpdateFromAgent("bad", []map[string]any{{"name": "flap", "state": state}}, false, nil, nil)
	}
	n := 0
	for _, e := range m.EventsSince(0) {
		if e.UpsID == "bad/flap" {
			n++
		}
	}
	if n != 10 {
		t.Fatalf("%d events", n)
	}
}

func TestEventsFileIsRotated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ups-events.jsonl")
	if err := os.WriteFile(path, make([]byte, maxEventsFile), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(nil, 5, time.Second, 60, 3600, path)
	if err != nil {
		t.Fatal(err)
	}
	m.UpdateFromAgent("a", []map[string]any{{"name": "u", "state": "on_battery"}}, false, nil, nil)
	if st, _ := os.Stat(path); st.Size() > 1000 || st.Size() == 0 {
		t.Fatal("not rotated", st.Size())
	}
	if st, err := os.Stat(path + ".1"); err != nil || st.Size() != maxEventsFile {
		t.Fatal("old file", err)
	}
}
