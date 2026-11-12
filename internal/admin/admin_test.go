package admin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/audit"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/health"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
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
	if code, _ = run("host", "srv-tower", "power-control", "disable"); code != 0 || len(must(st.PowerDisabledList())) != 1 {
		t.Fatal("host flag")
	}
	if code, _ = run("poll", "llm-box", "2s"); code != 0 {
		t.Fatal("poll")
	}
	po := must(statefile.Read(st.StatePath))["poll_overrides"].(map[string]any)
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
	t.Setenv("FAIRY_OIDC_CLIENT_SECRET", "")
	if code, out = run("config", "check"); code != 1 || !strings.Contains(out, "WARN") || strings.Contains(out, "fy_") {
		t.Fatal(out)
	}
	if !strings.Contains(out, "ok    FAIRY_OIDC_CLIENT_SECRET: not set (public client: PKCE only)") {
		t.Fatal("config check must say the client is public", out)
	}
	t.Setenv("FAIRY_OIDC_CLIENT_SECRET", "not-printed-oidc-secret")
	if _, out = run("config", "check"); !strings.Contains(out, "ok    FAIRY_OIDC_CLIENT_SECRET: present") ||
		strings.Contains(out, "not-printed-oidc-secret") {
		t.Fatal("config check must report the client secret as present only", out)
	}
	if !strings.Contains(out, "ok    control_rate_per_min: 20 control requests a minute for one admin\n") {
		t.Fatal("config check must name the limit on control requests (the default, 20)", out)
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

// T-05: `config check` reports the example session secret (and one character repeated) and says what to do;
// it never prints the value. A real secret is "present" as before.
func TestConfigCheckReportsAWeakSessionSecret(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(cfg, []byte("public_url: https://p.example\ndata_dir: "+dir+"/data\noidc: {issuer: https://id.example, client_id: c}\n"), 0o600)
	check := func(secret string) (int, string) {
		t.Setenv("FAIRY_SESSION_SECRET", secret)
		var out, errb bytes.Buffer
		code := Main([]string{"--config", cfg, "--env-file", dir + "/none.env", "config", "check"}, &out, &errb)
		return code, out.String() + errb.String()
	}
	for _, weak := range []string{"REPLACE_WITH_AT_LEAST_32_RANDOM_CHARACTERS", strings.Repeat("k", 48)} {
		code, out := check(weak)
		if code != 1 || !strings.Contains(out, "WARN  FAIRY_SESSION_SECRET: ") || !strings.Contains(out, "openssl rand -base64 48") ||
			strings.Contains(out, "FAIRY_SESSION_SECRET: present") || strings.Contains(out, strings.Repeat("k", 8)) {
			t.Fatalf("config check with %q: exit %d\n%s", weak, code, out)
		}
	}
	if _, out := check("short"); !strings.Contains(out, "WARN  FAIRY_SESSION_SECRET: missing or shorter than 32 characters") {
		t.Fatalf("config check with a short secret:\n%s", out)
	}
	real := "b0pV3m2x9QmJt8sYw1Zr5uKc7NfHd4Ea6LgTi0Oq2WsXy3Rv8BnMk1Jh5GfDc7Vb"
	if _, out := check(real); !strings.Contains(out, "ok    FAIRY_SESSION_SECRET: present") || strings.Contains(out, real) ||
		strings.Contains(out, "WARN  FAIRY_SESSION_SECRET") {
		t.Fatalf("config check with a real secret:\n%s", out)
	}
}

func TestHealthReportsTheLastHealthFile(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(cfg, []byte("public_url: https://p.example\ndata_dir: "+dir+"/data\noidc: {issuer: https://id.example, client_id: c}\n"), 0o600)
	now := time.Now().UTC()
	err := health.Write(dir+"/data", health.Report{
		StartedAt: now.Add(-time.Hour).Format(time.RFC3339Nano), WrittenAt: now.Format(time.RFC3339Nano),
		Systems:      map[string]int{"online": 3, "stale": 0, "offline": 1, "stopped": 0, "paused": 0},
		SystemIDs:    []string{"srv-tower"},
		Sources:      []model.SourceStatus{{Name: "agent:srv-tower", OK: true, LatencyMS: model.P(1.5), LastSuccess: &now}},
		PowerSources: []health.Source{{ID: "plug", Status: "unreachable", Error: model.P("timeout")}},
		UPS:          []health.Source{{ID: "rack/ups", Status: "ok"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := Main([]string{"--config", cfg, "--env-file", dir + "/none.env", "health"}, &out, &errb)
	want := []string{"panel: RUNNING", "control: enabled", "systems: online 3, stale 0, offline 1, stopped 0, paused 0",
		"agent:srv-tower", "1.5 ms", "power:plug", "unreachable", "timeout", "ups:rack/ups"}
	for _, w := range want {
		if !strings.Contains(out.String(), w) {
			t.Fatalf("missing %q in\n%s", w, out.String())
		}
	}
	if code != 0 {
		t.Fatal("fresh health file must exit 0", code, errb.String())
	}
	// `poll` warns about ids the panel does not know (from the same file).
	out.Reset()
	errb.Reset()
	if code = Main([]string{"--config", cfg, "--env-file", dir + "/none.env", "poll", "nope", "5"}, &out, &errb); code != 0 ||
		!strings.Contains(errb.String(), `"nope" is not a known system id`) {
		t.Fatal(code, errb.String())
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// Run as another user than the data directory's owner (e.g. root on the service user's files), changes are
// refused before anything is written; looking (list) still works.
func TestCLIRefusesToWriteAsTheWrongUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to hand the data directory to another uid")
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(cfg, []byte("public_url: https://p.example\ndata_dir: "+dir+"/data\noidc: {issuer: https://id.example, client_id: c}\n"), 0o600)
	run := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := Main(append([]string{"--config", cfg, "--env-file", dir + "/none.env"}, args...), &out, &errb)
		return code, out.String() + errb.String()
	}
	if code, out := run("sites", "create", "Lab"); code != 0 {
		t.Fatal(out)
	}
	_ = filepath.Walk(dir+"/data", func(p string, _ os.FileInfo, _ error) error { return os.Chown(p, 4242, 4242) })
	code, out := run("users", "set-role", "x@example.com", "admin")
	if code == 0 || !strings.Contains(out, "sudo -u fairy") {
		t.Fatal("ran as the wrong user", code, out)
	}
	if code, out := run("tokens", "create", "--name", "x", "--owner", "system"); code == 0 {
		t.Fatal("token created as the wrong user", out)
	}
	if code, out := run("sites", "list"); code != 0 || !strings.Contains(out, "Lab") {
		t.Fatal("read-only commands still work", code, out)
	}
	var st os.FileInfo
	if st, _ = os.Stat(dir + "/data/users.json"); st == nil {
		t.Fatal("users.json")
	}
	if _, err := os.Stat(dir + "/data/tokens.json"); err == nil {
		t.Fatal("tokens.json created by the wrong user")
	}
}

// A token for an owner who is not a panel user would have no access: the CLI says so instead of creating it.
func TestTokenOwnerMustBeAPanelUser(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(cfg, []byte("public_url: https://p.example\ndata_dir: "+dir+"/data\noidc: {issuer: https://id.example, client_id: c}\n"), 0o600)
	var out bytes.Buffer
	code := Main([]string{"--config", cfg, "--env-file", dir + "/none.env", "tokens", "create", "--name", "x", "--owner", "nobody"}, &out, &out)
	if code == 0 || !strings.Contains(out.String(), "not a panel user") {
		t.Fatal(code, out.String())
	}
}

// config check: control_groups being empty is no longer a warning by itself; what matters is whether anyone is
// (or can become) admin.
func TestConfigCheckAdminModel(t *testing.T) {
	check := func(groups string, admin bool) []string {
		t.Helper()
		dir := t.TempDir()
		cfg, err := config.Parse([]byte(`public_url: https://panel.example.com
data_dir: ` + dir + `
audit_log: ` + dir + `/audit.log
oidc: {issuer: "https://id.example.com", client_id: cid, allowed_groups: [homelab]` + groups + `}
`))
		if err != nil {
			t.Fatal(err)
		}
		if admin {
			st := access.NewStore(dir)
			if _, _, err := st.Seen("sub-1", "n@example.com", "N", access.RoleAdmin, "test"); err != nil {
				t.Fatal(err)
			}
		}
		p, n := adminCheck(cfg, nil, nil)
		return append(p, n...)
	}
	out := check("", true)
	if len(out) != 1 || !strings.Contains(out[0], "admins in users.json: 1") {
		t.Fatal(out)
	}
	out = check(", control_groups: [admins]", false)
	if !strings.Contains(out[0], "first sign-in") {
		t.Fatal(out)
	}
	out = check("", false)
	if !strings.Contains(out[0], "nobody becomes admin") || strings.Contains(strings.Join(out, " "), "control actions from the UI") {
		t.Fatal(out)
	}
}
