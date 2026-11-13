package power

import (
	"strings"
	"testing"
	"time"
)

// The panel's own power manager takes no source of a type that only an agent reads (homeassistant), also for
// a configuration that did not come through the check of config.yaml: no Runner is made, and so the panel
// never sends a Home Assistant token, refused or not.
func TestThePanelsManagerTakesNoHomeAssistantSource(t *testing.T) {
	t.Setenv("HA_TOKEN", strings.Repeat("not-a-token-", 4)) // made here, for this test only
	ha := map[string]any{"id": "corner-plug", "type": "homeassistant", "url": "http://192.0.2.10:8123", "token_env": "HA_TOKEN", "entity": "sensor.corner_plug_power"}
	plug := map[string]any{"id": "p", "type": "shelly", "url": "http://10.0.0.1"}
	m, err := NewManager([]map[string]any{plug, ha}, 5, time.Second, 60, 3600, nil, func() float64 { return 0 })
	if err == nil || m != nil {
		t.Fatalf("a manager was made with a homeassistant source: %v", err)
	}
	if !strings.Contains(err.Error(), `power source "corner-plug": type homeassistant is read by an agent, not by the panel`) {
		t.Fatalf("the refusal: %v", err)
	}
	if m, err := NewManager([]map[string]any{plug}, 5, time.Second, 60, 3600, nil, func() float64 { return 0 }); err != nil || len(m.Runners) != 1 {
		t.Fatalf("a plug of the panel's own: %v", err)
	}
}
