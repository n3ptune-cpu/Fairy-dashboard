package admin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `fairy admin config check` shows why a power source of type homeassistant in the panel's own power_sources
// is refused, with the file's line and what to do instead; the exit code is 1 and nothing else is checked.
func TestConfigCheckRefusesAHomeAssistantSourceOfThePanelsOwn(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	t.Setenv("HA_TOKEN", strings.Repeat("not-a-token-", 4)) // made here, for this test only
	t.Setenv("FAIRY_SESSION_SECRET", strings.Repeat("no-secret-", 5))
	y := "public_url: https://p.example\ndata_dir: " + dir + "/data\noidc: {issuer: \"https://id.example\", client_id: c, allowed_groups: [users], control_groups: [admins]}\n" +
		"power_sources:\n  - {id: corner-plug, type: homeassistant, url: \"http://192.0.2.10:8123\", token_env: HA_TOKEN, entity: sensor.corner_plug_power}\n"
	if err := os.WriteFile(cfg, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := Main([]string{"--config", cfg, "--env-file", dir + "/none.env", "config", "check"}, &out, &errb)
	got := out.String()
	if code != 1 || !strings.Contains(got, "FAIL  config "+cfg+" is invalid:") {
		t.Fatalf("exit %d:\n%s%s", code, got, errb.String())
	}
	for _, want := range []string{"line 5 (power_sources, entry 1)", `power source "corner-plug": type homeassistant is read by an agent, not by the panel`,
		"put it into the power_sources of an agent on that network"} {
		if !strings.Contains(got, want) {
			t.Errorf("the output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "not-a-token") {
		t.Errorf("the output has the secret:\n%s", got)
	}
}
