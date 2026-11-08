package admin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy/internal/audit"
	"github.com/n3ptune-cpu/Fairy/internal/statefile"
)

func TestBreakGlassCLI(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(cfg, []byte("public_url: https://p.example\ndata_dir: "+dir+"/data\noidc: {issuer: https://id.example, client_id: c}\n"), 0o600)
	run := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := Main(append([]string{"--config", cfg, "--env-file", dir + "/none.env"}, args...), &out, &errb)
		return code, out.String() + errb.String()
	}
	code, out := run("tokens", "create", "--name", "ha", "--owner", "system", "--scope", "control")
	if code != 0 || !strings.Contains(out, "fy_") {
		t.Fatal(code, out)
	}
	if code, out = run("tokens", "list"); code != 0 || !strings.Contains(out, "active") {
		t.Fatal(out)
	}
	if code, _ = run("tokens", "revoke", "ha"); code != 0 {
		t.Fatal("revoke")
	}
	if code, _ = run("tokens", "revoke", "ha"); code != 1 {
		t.Fatal("double revoke")
	}
	if code, _ = run("control", "disable", "--reason", "psu swap"); code != 0 {
		t.Fatal("disable")
	}
	st := adminstate.New(dir + "/data")
	if cs := st.ControlDisabled(); cs == nil || cs.Reason != "psu swap" {
		t.Fatal("kill switch not set")
	}
	if code, _ = run("host", "nsx-cyrene", "power-control", "disable"); code != 0 || len(st.PowerDisabledList()) != 1 {
		t.Fatal("host flag")
	}
	if code, _ = run("poll", "llm-box", "2s"); code != 0 {
		t.Fatal("poll")
	}
	po := statefile.Read(st.StatePath)["poll_overrides"].(map[string]any)
	if po["llm-box"] != 2.0 {
		t.Fatal(po)
	}
	if code, _ = run("poll", "llm-box", "999"); code == 0 {
		t.Fatal("out of range accepted")
	}
	if code, _ = run("sessions", "revoke-all"); code != 0 {
		t.Fatal("revoke-all")
	}
	if code, out = run("health"); code != 1 || !strings.Contains(out, "is the panel running") {
		t.Fatal(out)
	}
	if code, out = run("config", "check"); code != 1 || !strings.Contains(out, "WARN") || strings.Contains(out, "fy_") {
		t.Fatal(out)
	}
	entries, _ := audit.Read(filepath.Join(dir, "data", "audit.log"), "", "", "", nil, 50)
	if len(entries) < 6 || entries[0]["source"] != "cli" || entries[0]["ip"] != "local" {
		t.Fatalf("%d %v", len(entries), entries)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "data", "audit.log")); strings.Contains(string(b), "fy_") {
		t.Fatal("token in audit log")
	}
	if code, out = run("audit", "tail", "-n", "3"); code != 0 || strings.Count(out, "\n") != 3 {
		t.Fatal(out)
	}
	if code, _ = run("bogus"); code != 2 {
		t.Fatal("usage")
	}
}
