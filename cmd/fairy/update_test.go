package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/panelupdate"
)

// The update flow itself (check, question, download, verify, swap, rollback) is release.CLI, tested in
// Fairy-agent's pkg/release; here only the panel's own parts.

func TestHealthAddr(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, "fairy.env")
	none := unitSettings{}
	if got, from := healthAddr("", env, none); got != "127.0.0.1:8080" || from != "the default" {
		t.Fatal(got, from)
	}
	if got, from := healthAddr("10.0.0.20:8080", env, none); got != "10.0.0.20:8080" || from != "listen: in config.yaml" {
		t.Fatal(got, from)
	}
	_ = os.WriteFile(env, []byte("FAIRY_LISTEN=0.0.0.0:9000\n"), 0o600)
	if got, _ := healthAddr("10.0.0.20:8080", env, none); got != "127.0.0.1:9000" {
		t.Fatal(got)
	}
	_ = os.WriteFile(env, []byte("FAIRY_LISTEN=\"10.0.0.20:8080\"\n"), 0o600)
	if got, _ := healthAddr("", env, none); got != "10.0.0.20:8080" {
		t.Fatal(got)
	}
	_ = os.WriteFile(env, []byte("export FAIRY_LISTEN=[::]:8443   # behind nginx\n"), 0o600)
	if got, _ := healthAddr("", env, none); got != "127.0.0.1:8443" {
		t.Fatal(got)
	}
}

// r2-p4-01 (audit 5): the health check after the restart asked 127.0.0.1:8080 whenever this command could not
// tell where the panel listens: with a config.yaml the installed version refuses (a key of the next release,
// written before the update), and with a --listen or FAIRY_LISTEN that only the unit knows. A good update was
// rolled back, and with such a config the old version then did not start: the panel was down.
func TestHealthAddrIsTheServicesOwn(t *testing.T) {
	dir := t.TempDir()
	// the auditor's case-05: the nginx guide's listen address, and the new release's block already in the file
	cfg := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(cfg, []byte("public_url: https://panel.example.com\nlisten: 127.0.0.1:8532\nupdates: {check_latest: false, panel_repo: someone/Fairy-dashboard}\n"+
		"oidc: {issuer: \"https://id.example.com\", client_id: x}\n\na_key_of_the_next_release:\n  settle_s: 30\n"), 0o600)
	if _, err := config.Load(cfg); err == nil || !strings.Contains(err.Error(), "a_key_of_the_next_release") {
		t.Fatal("setup: this version must refuse the file:", err)
	}
	listen, repo, err := config.Peek(cfg)
	if err != nil || listen != "127.0.0.1:8532" || repo != "someone/Fairy-dashboard" {
		t.Fatal(listen, repo, err)
	}
	if got, from := healthAddr(listen, filepath.Join(dir, "none.env"), unitSettings{}); got != "127.0.0.1:8532" || from != "listen: in config.yaml" {
		t.Fatal(got, from)
	}
	bad := filepath.Join(dir, "bad.yaml")
	_ = os.WriteFile(bad, []byte("listen: [unclosed\n"), 0o600)
	if _, _, err := config.Peek(bad); err == nil {
		t.Fatal("a file that is not YAML must be an error")
	}

	// what systemd says about the unit as shipped (--env-file "" is dropped by systemd's own output)
	shipped := parseUnit("ExecStart={ path=/usr/local/bin/fairy ; argv[]=/usr/local/bin/fairy serve --config /etc/fairy/config.yaml --env-file  ; ignore_errors=no ; " +
		"start_time=[Thu 2026-11-12 04:00:00 UTC] ; stop_time=[n/a] ; pid=812 ; code=(null) ; status=0/0 }\nEnvironment=\nEnvironmentFiles=/etc/fairy/fairy.env (ignore_errors=no)\n")
	if strings.Join(shipped.Args, "|") != "/usr/local/bin/fairy|serve|--config|/etc/fairy/config.yaml|--env-file" || len(shipped.Env) != 0 ||
		strings.Join(shipped.EnvFiles, "|") != "/etc/fairy/fairy.env" {
		t.Fatalf("%+v", shipped)
	}
	if v, ok := shipped.flag("env-file"); !ok || v != "" {
		t.Fatal(v, ok)
	}
	if v, ok := shipped.flag("config"); !ok || v != "/etc/fairy/config.yaml" {
		t.Fatal(v, ok)
	}
	if _, ok := shipped.flag("listen"); ok {
		t.Fatal("no --listen there")
	}

	// a unit that was changed: each place the service takes its address from, in the service's own order
	unitEnv := filepath.Join(dir, "unit.env")
	_ = os.WriteFile(unitEnv, []byte("FAIRY_SESSION_SECRET=x\nFAIRY_LISTEN=127.0.0.1:8601\n"), 0o600)
	ownEnv := filepath.Join(dir, "own.env")
	_ = os.WriteFile(ownEnv, []byte("FAIRY_LISTEN=127.0.0.1:8602\n"), 0o600)
	u := parseUnit("ExecStart={ path=/usr/local/bin/fairy ; argv[]=/usr/local/bin/fairy serve --config " + cfg + " --env-file " + ownEnv + " ; ignore_errors=no }\n")
	if got, from := healthAddr("", "", u); got != "127.0.0.1:8602" || from != "FAIRY_LISTEN in "+ownEnv {
		t.Fatal(got, from)
	}
	u.Env = []string{"A=b", "FAIRY_LISTEN=127.0.0.1:8603"}
	if got, from := healthAddr("", "", u); got != "127.0.0.1:8603" || from != "Environment=FAIRY_LISTEN of fairy.service" {
		t.Fatal(got, from)
	}
	u.EnvFiles = []string{unitEnv}
	if got, _ := healthAddr("", "", u); got != "127.0.0.1:8601" {
		t.Fatal(got)
	}
	u = parseUnit("ExecStart={ path=/usr/local/bin/fairy ; argv[]=/usr/local/bin/fairy serve --listen 0.0.0.0:8604 --config " + cfg + " ; ignore_errors=no }\nEnvironment=FAIRY_LISTEN=127.0.0.1:8603\n" +
		"EnvironmentFiles=-" + unitEnv + " (ignore_errors=yes)\n")
	if got, from := healthAddr("", "", u); got != "127.0.0.1:8604" || from != "--listen of fairy.service" {
		t.Fatal(got, from)
	}
	u = parseUnit("ExecStart={ path=/usr/local/bin/fairy ; argv[]=/usr/local/bin/fairy serve --listen=127.0.0.1:8605 ; ignore_errors=no }\n")
	if got, _ := healthAddr("127.0.0.1:1", "", u); got != "127.0.0.1:8605" {
		t.Fatal(got)
	}
	// another config.yaml than this command was given: the service's one counts
	u = parseUnit("ExecStart={ path=/usr/local/bin/fairy ; argv[]=/usr/local/bin/fairy serve --config " + cfg + " ; ignore_errors=no }\n")
	if got, from := healthAddr("127.0.0.1:1", "", u); got != "127.0.0.1:8532" || from != "listen: in "+cfg {
		t.Fatal(got, from)
	}
	if u := parseUnit(""); len(u.Args)+len(u.Env)+len(u.EnvFiles) != 0 { // no systemd: this command's own files decide
		t.Fatalf("%+v", u)
	}
}

// Before anything is replaced the running panel must answer where the check after the restart will ask.
func TestHealthPreflight(t *testing.T) {
	asked := 0
	ok := func(string) error { asked++; return nil }
	refused := func(string) error { asked++; return errors.New("connect: connection refused") }
	on, off := func() bool { return true }, func() bool { return false }
	if err := healthPreflight("http://127.0.0.1:8080/healthz", "the default", "", on, ok); err != nil || asked != 1 {
		t.Fatal(err, asked)
	}
	// the panel is down already: an update may repair it, so it is not asked
	if err := healthPreflight("http://127.0.0.1:8080/healthz", "the default", "", off, refused); err != nil || asked != 1 {
		t.Fatal(err, asked)
	}
	err := healthPreflight("http://127.0.0.1:8080/healthz", "the default", "", on, refused)
	if err == nil || !strings.Contains(err.Error(), "does not answer at http://127.0.0.1:8080/healthz") ||
		!strings.Contains(err.Error(), "That address is from the default") || !strings.Contains(err.Error(), "nothing was changed") ||
		strings.Contains(err.Error(), "Update first") {
		t.Fatal(err)
	}
	err = healthPreflight("http://127.0.0.1:8080/healthz", "the default", "field wake_update not found in type config.Panel", on, refused)
	if err == nil || !strings.Contains(err.Error(), "field wake_update not found") || !strings.Contains(err.Error(), "Update first, then add new config keys, then restart.") {
		t.Fatal(err)
	}
}

func TestUpdateUsage(t *testing.T) {
	// a local build has no release key: refuse before touching anything
	if code := update([]string{"--check", "--config", filepath.Join(t.TempDir(), "none.yaml")}); code != 1 {
		t.Fatal(code)
	}
}

// r2-p4-02 (audit 5): what a run by hand does about an "Update panel" run that was stopped half-way.
func TestClearStoppedRun(t *testing.T) {
	dir := t.TempDir()
	bin, status := filepath.Join(dir, "bin"), filepath.Join(dir, "state", "status.json")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	stale, live := filepath.Join(bin, ".download-1"), filepath.Join(bin, ".download-2")
	for p, age := range map[string]time.Duration{stale: time.Hour, live: time.Minute, filepath.Join(bin, "fairy"): time.Hour} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	w := &panelupdate.Writer{Path: status, From: "50-a"}
	w.Step("downloading", "51-b", "downloading fairy-linux-amd64") // and then the run was stopped
	asked := 0
	unit := func(a panelupdate.Activity) func() panelupdate.Activity {
		return func() panelupdate.Activity { asked++; return a }
	}
	// the button's own run: it removes the old download and writes its own status, so it leaves the file alone
	clearStoppedRun(bin, status, true, now, unit(panelupdate.ActivityNone))
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("the stale download is still there")
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatal("a download that can be running was removed")
	}
	if st, _ := panelupdate.Read(status); st.Finished || asked != 0 {
		t.Fatalf("%+v asked %d", st, asked)
	}
	// by hand while the button's run is alive: nothing
	clearStoppedRun(bin, status, false, now, unit(panelupdate.ActivityRunning))
	if st, _ := panelupdate.Read(status); st.Finished || asked != 1 {
		t.Fatalf("%+v asked %d", st, asked)
	}
	// by hand after it was stopped: marked, so the page and the button are free again
	clearStoppedRun(bin, status, false, now, unit(panelupdate.ActivityNone))
	st, _ := panelupdate.Read(status)
	if !st.Finished || st.State != "stopped" || !strings.HasPrefix(st.Message, "stopped before it finished (while downloading") {
		t.Fatalf("%+v", st)
	}
	// a finished status: systemd is not even asked
	asked = 0
	clearStoppedRun(bin, status, false, now, unit(panelupdate.ActivityNone))
	if asked != 0 {
		t.Fatal("asked", asked)
	}
}
