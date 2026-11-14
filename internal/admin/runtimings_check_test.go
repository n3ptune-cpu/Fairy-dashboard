package admin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// Build 16: the timings of "Update unpowered hosts" can be changed in the panel ("Run timings"; they are kept
// in state.json and laid over config.yaml). `fairy admin config check` reads config.yaml, so it says which
// timings are changed in the panel: the value in force, config.yaml's, who and when; the notes about a short
// off_after_s and a countdown that is also the Power menu's are made from the values in force; and what the
// running panel ignores of the stored values is a WARN.
func TestConfigCheckSaysWhichRunTimingsAreChangedInThePanel(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config.yaml")
	t.Setenv("TOK_R", "r")
	t.Setenv("FAIRY_SESSION_SECRET", strings.Repeat("no-secret-", 5)) // made here, for this test only
	check := func(more string) (string, int) {
		t.Helper()
		y := "public_url: https://p.example\ndata_dir: " + data + "\noidc: {issuer: \"https://id.example\", client_id: c, allowed_groups: [users], control_groups: [admins]}\n" +
			"agents:\n  - {id: pc-a, url: \"http://pc-a:9101\", token_env: TOK_R}\n" + more
		if err := os.WriteFile(cfg, []byte(y), 0o600); err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		code := Main([]string{"--config", cfg, "--env-file", dir + "/none.env", "config", "check"}, &out, &errb)
		if !strings.Contains(out.String(), "config "+cfg+": valid") {
			t.Fatalf("the fixture is not a valid config:\n%s%s", out.String(), errb.String())
		}
		return out.String(), code
	}
	// nothing changed in the panel: no line about it, with or without a state.json
	if out, code := check(""); strings.Contains(out, "Run timings") || strings.Contains(out, "changed in the panel") || code != 0 {
		t.Fatalf("nothing changed (exit %d):\n%s", code, out)
	}
	st := adminstate.New(data)
	if err := st.SetWakeUpdate("pc-a", true); err != nil {
		t.Fatal(err)
	}
	if out, code := check(""); strings.Contains(out, "Run timings") || strings.Contains(out, "changed in the panel") || code != 0 {
		t.Fatalf("a state.json without the key (exit %d):\n%s", code, out)
	}
	// the owner's case: config.yaml says a countdown of 30 s and 15 minutes for the second passkey, and
	// off_after_s 360 is set in the panel
	if _, _, err := st.EditRunTimings("nora@example.com", func(o *adminstate.RunTimings) error {
		o.Set(config.KeyOffAfter, 360)
		o.Set(config.KeyRequestsPerMin, 6)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	const own = "wake_update: {shutdown_delay_s: 30}\napproval: {client_id: \"3f6c0c0e-0000-4000-8000-000000000000\", run_approval_wait_s: 900}\n"
	out, code := check(own)
	for _, want := range []string{
		"ok    wake_update.off_after_s: 360 s, changed in the panel (\"Run timings\") by nora@example.com, 20",
		" UTC, used INSTEAD of config.yaml's 900 until Reset\n",
		"ok    wake_update.agent_requests_per_min: 6 a minute, changed in the panel (\"Run timings\") by nora@example.com, 20",
		" UTC, used INSTEAD of config.yaml's 8 until Reset\n",
		"note  wake_update.off_after_s (changed in the panel; config.yaml: 900) is 360 (the default is 900): a machine that restarts for longer than 6 min can be taken for off by a run, which then wakes it, updates it and switches it off as a machine it woke\n",
		"ok    approval.run_approval_wait_s: 900 (how long a run of \"Update unpowered hosts\" asks for its second passkey",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q", want)
		}
	}
	if n := strings.Count(out, "changed in the panel (\"Run timings\")"); n != 2 || strings.Contains(out, "WARN  Run timings") || strings.Contains(out, "cannot be told from the run's own") {
		t.Errorf("%d lines for two changed timings, or a WARN, or a note about a countdown of 30 s (exit %d):\n%s", n, code, out)
	}
	// config.yaml has its own off_after_s line: the panel's decision is named with it, and the line about
	// config.yaml's own value is not said as if it counted
	out, _ = check("wake_update: {off_after_s: 600}\n")
	if !strings.Contains(out, "used INSTEAD of config.yaml's 600 until Reset") || strings.Contains(out, "wake_update.off_after_s: 600 (a machine counts as off after") ||
		!strings.Contains(out, "note  wake_update.off_after_s (changed in the panel; config.yaml: 600) is 360 (the default is 900)") {
		t.Errorf("config.yaml's own off_after_s next to the panel's:\n%s", out)
	}
	// the panel RAISED what config.yaml lowered: nothing risky is in force, and no note says so
	if _, _, err := st.EditRunTimings("nora@example.com", func(o *adminstate.RunTimings) error {
		o.Set(config.KeyOffAfter, 1800)
		o.Set(config.KeyShutdownDelay, 300)
		o.Set(config.KeyRunApprovalWait, 1200)
		o.Drop(config.KeyRequestsPerMin)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out, code = check("wake_update: {off_after_s: 300, shutdown_delay_s: 45}\napproval: {client_id: \"3f6c0c0e-0000-4000-8000-000000000000\"}\n")
	for _, want := range []string{
		"ok    wake_update.off_after_s: 1800 s, changed in the panel",
		"used INSTEAD of config.yaml's 300 until Reset",
		"ok    wake_update.shutdown_delay_s: 300 s, changed in the panel",
		"used INSTEAD of config.yaml's 45 until Reset",
		"ok    approval.run_approval_wait_s: 1200 s, changed in the panel",
		"used INSTEAD of config.yaml's 600 until Reset",
		"ok    approval.run_approval_wait_s: 1200 (changed in the panel; config.yaml: 600) (how long a run",
		// the countdown in force, five minutes, is also one of the Power menu
		"note  wake_update.shutdown_delay_s (changed in the panel; config.yaml: 45) is 300, which is also a countdown a person can choose in the Power menu",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q", want)
		}
	}
	if strings.Contains(out, "can be taken for off by a run") || strings.Contains(out, "agent_requests_per_min") || code != 0 {
		t.Errorf("a note about a short off_after_s while 1800 is in force, or a line for a timing that was reset (exit %d):\n%s", code, out)
	}
	// what the running panel ignores of the stored values: a WARN each (and the exit code a WARN has), and
	// config.yaml's value is what the notes are made from
	if err := os.WriteFile(filepath.Join(data, "state.json"), []byte(`{"run_timings": {"rev": 3, "values": {"off_after_s": 60, "settle_s": 90, "wake_gap_s": 5}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code = check("")
	if code != 1 || strings.Count(out, "WARN  Run timings: a timing in state.json is ignored: ") != 2 ||
		!strings.Contains(out, "off_after_s is not a whole number in 300-3600 (found 60): config.yaml's value counts") ||
		!strings.Contains(out, "\"wake_gap_s\" is no timing this version knows") ||
		!strings.Contains(out, "ok    wake_update.settle_s: 90 s, changed in the panel (\"Run timings\"), used INSTEAD of config.yaml's 60 until Reset") ||
		strings.Contains(out, "can be taken for off by a run") {
		t.Errorf("a stored value below the floor (exit %d):\n%s", code, out)
	}
	// a state.json that cannot be read: said, and nothing else about the timings (the panel does not start then)
	if err := os.WriteFile(filepath.Join(data, "state.json"), []byte(`{"run_timings": `), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, _ = check(""); !strings.Contains(out, "note  Run timings: state.json cannot be read, so the timings changed in the panel were not looked at") {
		t.Errorf("a state.json that is not JSON:\n%s", out)
	}
}
