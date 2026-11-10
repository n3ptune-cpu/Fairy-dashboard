package ups

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// N9: the panel's own range check of a UPS's values (L11), on rows handed to the panel as they are, so the
// test does not depend on what the agent module's shared reader does with such values before. Every value has
// its range written out here, both ends included; a value outside it is not shown, the UPS is marked stale
// and the error names the value. A UPS that lies about its charge must not look healthy with that charge.
func TestPanelRangeCheckOfUPSValues(t *testing.T) {
	type tc struct {
		field string
		value any
		kept  bool
	}
	cases := []tc{
		{"battery_charge", 0.0, true}, {"battery_charge", 100.0, true}, {"battery_charge", 100.5, false}, {"battery_charge", 250.0, false},
		{"battery_charge", -0.5, false}, {"battery_charge", "NaN", false}, {"battery_charge", "full", false},
		{"load_percent", 0.0, true}, {"load_percent", 10000.0, true}, {"load_percent", 10001.0, false}, {"load_percent", -5.0, false},
		{"battery_runtime_s", 0.0, true}, {"battery_runtime_s", 1e9, true}, {"battery_runtime_s", 1e9 + 1, false},
		{"battery_runtime_s", -1.0, false}, {"battery_runtime_s", 1e300, false},
		{"watts", 0.0, true}, {"watts", 1e9, true}, {"watts", 1e9 + 1, false}, {"watts", -1.0, false}, {"watts", 1e308, false},
		{"nominal_w", 900.0, true}, {"nominal_w", -1.0, false}, {"nominal_w", 1e308, false},
		{"battery_voltage", 27.1, true}, {"battery_voltage", -1e9, true}, {"battery_voltage", 1e300, false}, {"battery_voltage", -1e300, false},
		{"input_voltage", 231.5, true}, {"input_voltage", 1e300, false},
		{"output_voltage", 230.0, true}, {"output_voltage", -1e300, false},
		{"input_frequency", 0.0, true}, {"input_frequency", 50.0, true}, {"input_frequency", -1.0, false}, {"input_frequency", 1e300, false},
	}
	for i, c := range cases {
		m, err := NewManager(nil, 5, time.Second, 60, 3600, "")
		if err != nil {
			t.Fatal(err)
		}
		agent := fmt.Sprintf("a%d", i)
		m.UpdateFromAgent(agent, []map[string]any{{"name": "apc", "state": "online", c.field: c.value}}, false, nil, nil)
		st := m.State(agent + "/apc")
		if st == nil {
			t.Fatalf("%s=%v: no state", c.field, c.value)
		}
		b, err := json.Marshal(st)
		if err != nil {
			t.Fatalf("%s=%v: %v", c.field, c.value, err)
		}
		var shown map[string]any
		_ = json.Unmarshal(b, &shown)
		if c.kept {
			if shown[c.field] == nil || st.Stale || st.Error != nil {
				t.Errorf("%s=%v is a possible value: shown %v, stale %v, error %v", c.field, c.value, shown[c.field], st.Stale, st.Error)
			}
			continue
		}
		want := "the UPS reported values that cannot be right; they are not shown: " + c.field
		if shown[c.field] != nil || !st.Stale || st.Error == nil || *st.Error != want {
			t.Errorf("%s=%v cannot be right: shown %v, stale %v, error %v", c.field, c.value, shown[c.field], st.Stale, st.Error)
		}
	}
}
