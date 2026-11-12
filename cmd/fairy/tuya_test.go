package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// An upgrade in place with the old example's tuya line in config.yaml: `fairy serve` builds the panel as
// before (same hosts, the other power sources), and says once, as a warning, that the entry is ignored.
// The config is the old release's own file (the v-before fixture) with the line added.
func TestServeStartsWithAnOldTuyaEntry(t *testing.T) {
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	v := testutil.NewVBefore(t)
	// made here, not written down: the fixture's own value is one the panel no longer starts with
	t.Setenv("FAIRY_SESSION_SECRET", fmt.Sprintf("%x", sha256.Sum256([]byte(t.Name()))))
	yaml, err := os.ReadFile(v.ConfigPath)
	if err != nil || bytes.Contains(yaml, []byte("power_sources:")) {
		t.Fatal("the fixture's config.yaml has no power_sources of its own", err)
	}
	start := func(sources string) (*panel, int, string) {
		t.Helper()
		if err := os.WriteFile(v.ConfigPath, append(bytes.Clone(yaml), sources...), 0o600); err != nil {
			t.Fatal(err)
		}
		var log bytes.Buffer
		slog.SetDefault(slog.New(slog.NewTextHandler(&log, nil)))
		p, code := setupPanel(serveFlags{cfgPath: v.ConfigPath})
		return p, code, log.String()
	}
	const shelly = "  - {id: plug-a, type: shelly, url: \"http://192.168.1.50\", system: laptop}\n"
	const tuya = "  - {id: plug-tower, type: tuya, device_id: bf0123456789abcdef, ip: 192.168.1.60, local_key_env: TUYA_KEY_PLUG, version: \"3.4\", system: srv-tower}\n"
	plain, code, log := start("\npower_sources:\n" + shelly)
	if plain == nil || code != 0 || strings.Contains(log, "tuya") {
		t.Fatalf("without the entry: exit %d\n%s", code, log)
	}
	p, code, log := start("\npower_sources:\n" + tuya + shelly)
	if p == nil || code != 0 {
		t.Fatalf("the panel must start with the old entry: exit %d\n%s", code, log)
	}
	const line = `level=WARN msg="config.yaml: power source \"plug-tower\": tuya is no longer supported and is ignored (remove the entry from config.yaml, and the line TUYA_KEY_PLUG=... from the env file: the plug's key is not used any more)"`
	if strings.Count(log, "tuya") != 1 || !strings.Contains(log, line) || strings.Contains(log, "level=ERROR") {
		t.Fatalf("one warning, no error:\n%s", log)
	}
	if len(p.cfg.PowerSources) != 1 || p.cfg.PowerSources[0]["id"] != "plug-a" || len(p.cfg.Agents) != len(plain.cfg.Agents) ||
		len(p.cfg.Proxmox) != len(plain.cfg.Proxmox) {
		t.Fatalf("the rest of the configuration is as without the entry: %v", p.cfg.PowerSources)
	}
	if got := p.poller.Power.Overview().Sources; len(got) != 1 || got[0].ID != "plug-a" {
		t.Fatalf("the power sources the panel runs: %v", got)
	}
}
