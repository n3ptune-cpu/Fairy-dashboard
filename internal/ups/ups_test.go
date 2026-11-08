package ups

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	upssrc "github.com/n3ptune-cpu/Fairy-agent/pkg/ups"
)

func rows(status, charge string) []map[string]any {
	return []map[string]any{upssrc.RowMap(devices.UPSFromVars("cyberpower", map[string]string{"ups.status": status,
		"battery.charge": charge, "battery.runtime": "1500", "ups.load": "20", "ups.realpower.nominal": "900"}, ""))}
}

func TestTransitionsAndEvents(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(nil, 5, time.Second, 0.05, 3600, filepath.Join(dir, "ups-events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	powers := map[string][]string{"cyberpower": {"node-a", "pc"}}
	names := map[string]string{"cyberpower": "Straw's CyberPower"}
	m.UpdateFromAgent("asus", rows("OL", "100"), false, powers, names)
	st := m.State("asus/cyberpower")
	if st.Name != "Straw's CyberPower" || st.Status != "online" || *st.Via != "asus" || len(m.SystemsMap()["pc"]) != 1 {
		t.Fatalf("%+v", st)
	}
	m.UpdateFromAgent("asus", rows("OB DISCHRG", "87"), false, powers, names)
	m.UpdateFromAgent("asus", rows("OB DISCHRG LB", "12"), false, powers, names)
	m.UpdateFromAgent("asus", rows("OL CHRG RB", "14"), false, powers, names)
	kinds := []string{}
	for _, e := range m.EventsSince(0) {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "on_battery,low_battery,power_restored,replace_battery" {
		t.Fatal(kinds)
	}
	if ev := m.EventsSince(0)[0]; !strings.Contains(ev.Message, "87.0% charge, 25:00 runtime") || ev.Severity != "warning" {
		t.Fatal(ev.Message)
	}
	// agent unreachable: first stale, then (after offline_after) unreachable + communication_lost
	m.UpdateFromAgent("asus", nil, true, powers, names)
	time.Sleep(80 * time.Millisecond)
	m.UpdateFromAgent("asus", nil, true, powers, names)
	st = m.State("asus/cyberpower")
	if st.Status != "unreachable" || !st.Stale || *st.Error != "agent unreachable" {
		t.Fatalf("%+v", st)
	}
	m.UpdateFromAgent("asus", rows("OL", "100"), false, powers, names)
	evs := m.EventsSince(4)
	if len(evs) != 2 || evs[0].Kind != "communication_lost" || evs[1].Kind != "communication_restored" {
		t.Fatalf("%+v", evs)
	}
	m.UpdateFromAgent("asus", []map[string]any{}, false, powers, names) // relay turned off -> removed
	if m.State("asus/cyberpower") != nil {
		t.Fatal("not removed")
	}
	m.SampleHistory()
}

func TestNUTSourceFailureAndConfig(t *testing.T) {
	m, err := NewManager([]map[string]any{{"id": "rack", "host": "127.0.0.1", "port": 1, "powers": map[string]any{"ups": []any{"node-a"}}}},
		5, 200*time.Millisecond, 60, 60, "")
	if err != nil {
		t.Fatal(err)
	}
	m.PollAll(context.Background()) // nothing listens on port 1: no states, no panic
	if len(m.Overview(true).UPS) != 0 {
		t.Fatal("unexpected state")
	}
	if _, err := upssrc.Build(map[string]any{"id": "x", "type": "apcupsd", "host": "h"}); err == nil {
		t.Fatal("unknown type")
	}
	if _, err := upssrc.Build(map[string]any{"id": "x", "host": "h", "powers": []any{"Not Valid"}}); err == nil {
		t.Fatal("bad system id")
	}
	if FmtRuntime(nil) != "?" || FmtRuntime(func() *int { v := 125; return &v }()) != "2:05" {
		t.Fatal("runtime fmt")
	}
}
