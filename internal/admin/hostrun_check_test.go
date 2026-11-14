package admin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `fairy admin config check` and a machine's own run timings changed in "Edit host": the host's line names
// each with config.yaml's value, the line of its systems: entry says which key is not in force, and the
// notes about a short off_after_s and a countdown a person can also choose are made from the value in force.
func TestConfigCheckAndAMachinesOwnRunTimingsOfEditHost(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	t.Setenv("TOK_R", "r")
	t.Setenv("CTL_R", "c")
	t.Setenv("FAIRY_SESSION_SECRET", strings.Repeat("no-secret-", 5)) // made here, for this test only
	t.Setenv("FAIRY_OIDC_CLIENT_SECRET", "")
	y := "public_url: https://p.example\ndata_dir: " + dir + "/data\noidc: {issuer: \"https://id.example\", client_id: c, allowed_groups: [users], control_groups: [admins]}\n" +
		"agents:\n" +
		"  - {id: relay-a, url: \"http://relay-a:9101\", token_env: TOK_R, control_token_env: CTL_R, wol_relay: true}\n" +
		"  - {id: pc-a, name: PC-A, url: \"http://pc-a:9101\", token_env: TOK_R}\n" +
		"  - {id: pc-b, url: \"http://pc-b:9101\", token_env: TOK_R}\n" +
		"systems:\n" +
		"  - {id: pc-a, wol: {mac: \"02:00:00:00:00:a1\", relay: relay-a}, wake_update: {off_after_s: 600, shutdown_delay_s: 90}}\n"
	if err := os.WriteFile(cfg, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir+"/data", 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(hosts string) (string, int) {
		t.Helper()
		if err := os.WriteFile(dir+"/data/state.json", []byte(`{"host_options": {"rev": 3, "hosts": `+hosts+`}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		code := Main([]string{"--config", cfg, "--env-file", dir + "/none.env", "config", "check"}, &out, &errb)
		if !strings.Contains(out.String(), "config "+cfg+": valid") {
			t.Fatalf("the fixture is not a valid config:\n%s%s", out.String(), errb.String())
		}
		return out.String(), code
	}
	has := func(out string, want ...string) {
		t.Helper()
		for _, w := range want {
			if strings.Count(out, w) != 1 {
				t.Errorf("the check lacks %q (or has it twice):\n%s", w, out)
			}
		}
	}
	const (
		systemLine = "ok    system pc-a: wake_update of its own: off_after_s 600 (it counts as off after 10 min without an answer), shutdown_delay_s 90 (the countdown of its shutdown in a run)\n"
		systemNote = "note  system pc-a: wake_update.off_after_s is 600 (the default is 900): a machine that restarts for longer than 10 min can be taken for off by a run"
	)
	// nothing changed in the panel: config.yaml's block, and the note about its short time
	out, base := run(`{}`)
	has(out, systemLine, systemNote)
	if strings.Contains(out, "host pc-a") || strings.Contains(out, "NOT in force") {
		t.Errorf("nothing is changed in the panel, and the check says so:\n%s", out)
	}
	// both machines have a timing of their own from "Edit host"
	out, code := run(`{"pc-a": {"wake_update": {"off_after_s": 360}, "by": "admin", "at": "2026-11-14T10:00:00Z"}, "pc-b": {"name": "Den", "wake_update": {"off_after_s": 1800, "shutdown_delay_s": 60}}}`)
	has(out,
		"ok    host pc-a: changed in the panel (\"Edit host\") by admin, 2026-11-14 10:00 UTC, used INSTEAD of what config.yaml says about the same thing until Reset: wake_update.off_after_s 360 (config.yaml: 600)\n",
		"ok    host pc-b: changed in the panel (\"Edit host\"), used INSTEAD of what config.yaml says about the same thing until Reset: name \"Den\" (config.yaml: the name the machine or its node reports); "+
			"wake_update.off_after_s 1800 (config.yaml: none of its own, the panel's counts); wake_update.shutdown_delay_s 60 (config.yaml: none of its own, the panel's counts)\n",
		"ok    system pc-a: wake_update of its own: off_after_s 600 (it counts as off after 10 min without an answer; NOT in force: changed in the panel (\"Edit host\") to 360), shutdown_delay_s 90 (the countdown of its shutdown in a run)\n",
		"note  host pc-a: wake_update.off_after_s (changed in the panel) is 360 (the default is 900): a machine that restarts for longer than 6 min can be taken for off by a run, which then wakes it, updates it and switches it off as a machine it woke\n",
		"note  host pc-b: wake_update.shutdown_delay_s (changed in the panel) is 60, which is also a countdown a person can choose in the Power menu of a machine's page",
	)
	if strings.Contains(out, systemNote) || strings.Contains(out, "host pc-b: wake_update.off_after_s (changed") {
		t.Errorf("a note about a value that is not in force, or about a long time:\n%s", out)
	}
	if code != base || strings.Contains(out, "WARN  Edit host") {
		t.Errorf("decisions that are used change the exit code (%d, without them %d) or have a WARN:\n%s", code, base, out)
	}
	// the countdown of the block changed to one a person can choose: the note is the host's
	out, _ = run(`{"pc-a": {"wake_update": {"shutdown_delay_s": 300}}}`)
	has(out, systemNote, "note  host pc-a: wake_update.shutdown_delay_s (changed in the panel) is 300, which is also a countdown",
		"shutdown_delay_s 90 (the countdown of its shutdown in a run; NOT in force: changed in the panel (\"Edit host\") to 300)\n")
	// a number its key cannot have is no decision: a WARN says that it is ignored, and config.yaml's lines stand
	out, code = run(`{"pc-a": {"wake_update": {"off_after_s": 60, "shutdown_delay_s": 5}}, "pc-b": {"wake_update": {"off_after_s": 299}}, "old-pc": {"wake_update": {"off_after_s": 600}}}`)
	has(out, systemLine, systemNote,
		"WARN  Edit host: an override in state.json is ignored: host pc-a: wake_update.off_after_s is not a whole number from 300 to 3600: the configuration's value counts\n",
		"WARN  Edit host: an override in state.json is ignored: host pc-a: wake_update.shutdown_delay_s is not a whole number from 10 to 3600: the configuration's value counts\n",
		"WARN  Edit host: an override in state.json is ignored: host pc-b: wake_update.off_after_s is not a whole number from 300 to 3600: the configuration's value counts\n",
		"WARN  Edit host: state.json has changes about old-pc itself (name, Wake-on-LAN, run timings), which is no agent's id and not the id of a node of a proxmox: entry: they are ignored (a host that was removed?)\n",
	)
	if code == 0 || strings.Contains(out, "ok    host pc-a") || strings.Contains(out, "NOT in force") {
		t.Errorf("numbers below the floors: exit %d\n%s", code, out)
	}
}
