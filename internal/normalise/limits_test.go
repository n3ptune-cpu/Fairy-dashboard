package normalise

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
)

// floodPayload is a /metrics answer with n entries in every list and texts of size bytes everywhere.
func floodPayload(n, size int) jsonx.M {
	long := strings.Repeat("é", size/2)
	list := func(row func(i int) any) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = row(i)
		}
		return out
	}
	return jsonx.M{"hostname": long, "platform": jsonx.M{"system": long, "pretty": long}, "uptime_s": 1.0,
		"cpu": jsonx.M{"percent": 1.0, "logical": 4.0}, "memory": jsonx.M{"total": 2.0, "used": 1.0},
		"disks": list(func(i int) any {
			return jsonx.M{"mount": fmt.Sprintf("/%d/%s", i, long), "fstype": long, "total": 2.0, "used": 1.0}
		}),
		"temps": list(func(i int) any {
			return jsonx.M{"sensor": long, "label": long, "current": 40.0}
		}),
		"fans":     list(func(i int) any { return jsonx.M{"name": long, "rpm": 900.0} }),
		"voltages": list(func(i int) any { return jsonx.M{"name": jsonx.M{"x": long}, "volts": 12.0} }),
		"gpus":     list(func(i int) any { return jsonx.M{"name": long, "vendor": long, "source": long} }),
		"load_avg": list(func(i int) any { return 1.0 }),
		"power": jsonx.M{"battery_state": long, "bmc_note": long, "sensors": list(func(i int) any { return jsonx.M{"name": long, "watts": 1.0} }),
			"source": list(func(i int) any { return long })},
		"bmc_relay":     jsonx.M{"targets": list(func(i int) any { return jsonx.M{"id": fmt.Sprintf("t%d", i)} })},
		"power_control": jsonx.M{"sleep": false, "sleep_note": long, "pending": jsonx.M{"action": "reboot", "at": 1.7e9, "requested_by": long}},
		"hardware": jsonx.M{"system_vendor": long, "system_model": long, "board_vendor": long, "board_model": jsonx.M{"x": long},
			"bios_version": long, "cpu_model": long},
		"helper": jsonx.M{"state": "error", "error": long, "features": list(func(i int) any { return long })},
	}
}

// One agent cannot make its entry arbitrarily large (M1): every list and every text taken from /metrics has an
// upper bound, whatever the agent sends.
func TestAgentListsAndTextsAreBounded(t *testing.T) {
	for _, tc := range []struct{ n, size int }{{10_000, 64}, {300, 1 << 16}, {1, 1 << 20}} {
		s := NormaliseAgent(config.Agent{ID: "evil"}, floodPayload(tc.n, tc.size), time.Now(), 0, NewRateTracker())
		if len(s.Disks) > MaxDisks || len(s.Temps) > MaxTemps || len(s.Fans) > MaxFans || len(s.Voltages) > MaxVoltages ||
			len(s.GPUs) > MaxGPUs || len(s.LoadAvg) > MaxLoadAvg || len(s.ComponentPower.Sensors) > MaxPowerSensors ||
			len(s.ComponentPower.Source) > maxSourceNames || len(s.BMCRelayTargets) > MaxRelayTargets {
			t.Fatalf("n=%d: lists not bounded: %d disks, %d temps, %d fans, %d voltages, %d gpus, %d load, %d sensors, %d sources, %d targets",
				tc.n, len(s.Disks), len(s.Temps), len(s.Fans), len(s.Voltages), len(s.GPUs), len(s.LoadAvg),
				len(s.ComponentPower.Sensors), len(s.ComponentPower.Source), len(s.BMCRelayTargets))
		}
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) > 256<<10 {
			t.Fatalf("n=%d size=%d: the entry is %d bytes", tc.n, tc.size, len(b))
		}
		if !utf8.Valid(b) || strings.Contains(string(b), "�") {
			t.Fatal("a text was cut inside a character")
		}
		var texts func(v any)
		texts = func(v any) {
			switch x := v.(type) {
			case string:
				if len(x) > maxMount+len("…") {
					t.Fatalf("a text of %d bytes got through", len(x))
				}
			case []any:
				for _, e := range x {
					texts(e)
				}
			case map[string]any:
				for _, e := range x {
					texts(e)
				}
			}
		}
		var generic any
		_ = json.Unmarshal(b, &generic)
		texts(generic)
	}
	// with the full flood the lists are filled up to their limit, not emptied
	s := NormaliseAgent(config.Agent{ID: "evil"}, floodPayload(10_000, 8), time.Now(), 0, NewRateTracker())
	if len(s.Disks) != MaxDisks || len(s.Temps) != MaxTemps || len(s.Fans) != MaxFans || len(s.Voltages) != MaxVoltages ||
		len(s.GPUs) != MaxGPUs || len(s.LoadAvg) != MaxLoadAvg || len(s.BMCRelayTargets) != MaxRelayTargets {
		t.Fatalf("%d %d %d %d %d %d %d", len(s.Disks), len(s.Temps), len(s.Fans), len(s.Voltages), len(s.GPUs), len(s.LoadAvg), len(s.BMCRelayTargets))
	}
}

// What a real machine sends is far below the limits and comes through unchanged.
func TestLimitsLeaveNormalAnswersAlone(t *testing.T) {
	if ClipText("héllo", 6) != "héllo" || ClipText("héllo", 2) != "h…" || ClipText("", 4) != "" {
		t.Fatal("ClipText")
	}
	mount := "/mnt/" + strings.Repeat("d", 200)
	m := jsonx.M{"hostname": "nas", "disks": []any{jsonx.M{"mount": mount, "fstype": "zfs", "total": 2.0, "used": 1.0}},
		"temps":    []any{jsonx.M{"sensor": "coretemp", "label": " Package id 0 ", "current": 40.0}},
		"fans":     []any{jsonx.M{"name": 3.0, "rpm": 900.0}},
		"hardware": jsonx.M{"system_vendor": " LENOVO ", "system_model": "T14"}}
	s := NormaliseAgent(config.Agent{ID: "nas"}, m, time.Now(), 0, NewRateTracker())
	if s.Name != "nas" || s.Disks[0].Mount != mount || *s.Disks[0].FSType != "zfs" || *s.Temps[0].Label != " Package id 0 " ||
		s.Fans[0].Name != "3" || *s.Hardware.SystemVendor != "LENOVO" || *s.Hardware.Display != "Lenovo T14" {
		t.Fatalf("%+v", s)
	}
}
