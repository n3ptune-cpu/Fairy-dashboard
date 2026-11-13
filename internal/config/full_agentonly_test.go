package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/power"
)

// A power source of type homeassistant (Fairy-agent v0.8.0) is read by an agent, never by the panel itself: an
// agent waits after Home Assistant refused its token (5 minutes, 15, then an hour), and the panel's own
// poller asks a failing source again after a minute at most, for good; Home Assistant counts each such
// request as a failed login. The panel's own power_sources refuse the type when the file is read, with what
// to do instead, and with the file's line like every other mistake in the list.
func TestThePanelsOwnPowerSourcesRefuseHomeAssistant(t *testing.T) {
	t.Setenv("HA_TOKEN", strings.Repeat("not-a-token-", 4))                                                              // made here, for this test only
	const head = "public_url: https://panel.example.com\noidc: {issuer: \"https://id.example.com\", client_id: fairy}\n" // lines 1-2
	const entry = "{id: corner-plug, type: homeassistant, url: 'http://192.0.2.10:8123', token_env: HA_TOKEN, entity: sensor.corner_plug_power}"
	// the entry itself is in order: it is what an agent's config.yaml takes
	raw := map[string]any{"id": "corner-plug", "type": "homeassistant", "url": "http://192.0.2.10:8123", "token_env": "HA_TOKEN", "entity": "sensor.corner_plug_power"}
	if err := power.ValidateConfigs([]map[string]any{raw}); err != nil {
		t.Fatalf("the fixture: an agent would refuse this entry too: %v", err)
	}
	if err := PanelPowerSource(raw); err == nil {
		t.Fatal("PanelPowerSource accepts it")
	}
	for name, c := range map[string]struct{ yaml, line string }{
		"the only entry":   {"power_sources:\n  - " + entry + "\n", "line 4 (power_sources, entry 1)"},
		"the second entry": {"power_sources:\n  - {id: p, type: shelly, url: 'http://10.0.0.1'}\n  - " + entry + "\n", "line 5 (power_sources, entry 2)"},
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(head+c.yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadFull(path)
		if err == nil {
			t.Fatalf("%s: the file was accepted", name)
		}
		for _, want := range []string{c.line, `power source "corner-plug": type homeassistant is read by an agent, not by the panel`,
			"put it into the power_sources of an agent on that network", "power_links"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the refusal lacks %q: %v", name, want, err)
			}
		}
		if strings.Contains(err.Error(), "192.0.2.10") || strings.Contains(err.Error(), "sensor.") || strings.Contains(err.Error(), "not-a-token") {
			t.Errorf("%s: the refusal repeats a value of the entry: %v", name, err)
		}
		if cfg, perr := Parse([]byte(head + c.yaml)); perr != nil || cfg.CheckDevices() == nil {
			t.Errorf("%s: CheckDevices (no file) must refuse it too: %v", name, perr)
		}
	}
	// every other type the agent's package knows is the panel's too, as before
	for _, typ := range power.Types {
		want := typ == "homeassistant"
		if got := PanelPowerSource(map[string]any{"id": "x", "type": typ}) != nil; got != want {
			t.Errorf("type %s: refused %v, want %v", typ, got, want)
		}
	}
	if PanelPowerSource(map[string]any{"id": "x"}) != nil || PanelPowerSource(map[string]any{"id": "x", "type": 7}) != nil {
		t.Error("an entry without a type is the device check's to refuse, in its own words")
	}
}
