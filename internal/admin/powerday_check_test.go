package admin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `fairy admin config check` has one line about the power sources' last 24 hours: what is kept and where.
// There is no setting, so a config.yaml from before this build gets the line unchanged. Something that is not
// a plain file at the file's name is a note, and never a reason for exit code 1.
func TestConfigCheckSaysWhereTheLast24HoursAreKept(t *testing.T) {
	dir := t.TempDir()
	cfg, data := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "data")
	t.Setenv("FAIRY_SESSION_SECRET", "Xk3"+strings.Repeat("no-secret-", 5))
	y := "public_url: https://p.example\ndata_dir: " + data + "\noidc: {issuer: \"https://id.example\", client_id: c, allowed_groups: [users], control_groups: [admins]}\n"
	if err := os.WriteFile(cfg, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func() (int, string) {
		var out, errb bytes.Buffer
		code := Main([]string{"--config", cfg, "--env-file", dir + "/none.env", "config", "check"}, &out, &errb)
		return code, out.String() + errb.String()
	}
	line := "ok    power: the last 24 hours of every power source are kept, a minute a point, for at most 64 sources, in " + filepath.Join(data, "power-day.json") +
		" (no setting; an older build leaves the file alone)\n"
	code, got := run()
	if !strings.Contains(got, line) || strings.Contains(got, "is not a plain file") {
		t.Fatalf("exit %d:\n%s", code, got)
	}
	before := code
	// a good file changes nothing; a folder at its name is a note and the same exit code
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(data, "power-day.json")
	if err := os.WriteFile(file, []byte(`{"version":1,"step_s":60,"devices":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, got := run(); code != before || !strings.Contains(got, line) || strings.Contains(got, "is not a plain file") {
		t.Fatalf("with a good file: exit %d:\n%s", code, got)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(file, 0o700); err != nil {
		t.Fatal(err)
	}
	code, got = run()
	if code != before || !strings.Contains(got, line) || !strings.Contains(got, "note  "+file+" is not a plain file: the panel does not read or replace it") {
		t.Fatalf("with a folder at the file's name: exit %d (before: %d):\n%s", code, before, got)
	}
}
