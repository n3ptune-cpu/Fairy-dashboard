package main

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

// r2-p4-07 (audit 5): `fairy update` by hand wrote the update unit and the polkit rule and ran `apt-get
// install polkitd` before it knew whether there was an update and before its question; after "N" it printed
// "Nothing changed." Now nothing is written before a yes.
func TestTheButtonIsSetUpOnlyAfterAYes(t *testing.T) {
	todo := []string{"write /etc/systemd/system/fairy-panel-update.service"}
	cli := func(typed string) *answers { // release.CLI asked its question and read one line
		a := &answers{r: bufio.NewReader(strings.NewReader(typed))}
		line, _ := bufio.NewReader(a).ReadString('\n')
		if line != strings.SplitAfter(typed, "\n")[0] {
			t.Fatalf("the CLI read %q of %q", line, typed)
		}
		return a
	}
	for typed, want := range map[string]bool{"y\n": true, "yes\n": true, " YES \n": true, "n\n": false, "\n": false, "no\n": false, "": false, "yess\n": false} {
		var out bytes.Buffer
		if got := buttonWanted(cli(typed), false, todo, &out); got != want || out.Len() != 0 {
			t.Errorf("the CLI's question answered %q: set up = %v, said %q", typed, got, out.String())
		}
	}
	// --yes and nothing asked (an install with --yes, or already up to date with --yes)
	var out bytes.Buffer
	if !buttonWanted(&answers{r: bufio.NewReader(strings.NewReader(""))}, true, todo, &out) || out.Len() != 0 {
		t.Fatal("--yes", out.String())
	}
	// already up to date, no --yes: the CLI asked nothing, so this command asks its own question
	for typed, want := range map[string]bool{"y\n": true, "n\n": false, "": false} {
		out.Reset()
		got := buttonWanted(&answers{r: bufio.NewReader(strings.NewReader(typed))}, false, todo, &out)
		if got != want || !strings.HasPrefix(out.String(), "Set up the Update panel button now (write /etc/systemd/system/fairy-panel-update.service)? [y/N] ") {
			t.Errorf("own question answered %q: %v, %q", typed, got, out.String())
		}
		if !want && !strings.HasSuffix(out.String(), "Nothing changed.\n") {
			t.Errorf("after a no: %q", out.String())
		}
	}
	// the CLI's question and then a second line: the second one is still there for this command
	a := &answers{r: bufio.NewReader(strings.NewReader("y\nn\n"))}
	if line, _ := bufio.NewReader(a).ReadString('\n'); line != "y\n" || !a.yes() {
		t.Fatal(line)
	}
	if rest, _ := a.r.ReadString('\n'); rest != "n\n" {
		t.Fatalf("the reader took more than one line: %q left", rest)
	}

	// what would be written is known without writing it
	dir := t.TempDir()
	unit, rules := filepath.Join(dir, "fairy-panel-update.service"), filepath.Join(dir, "rules.d", "49-fairy-panel.rules")
	p := panelupdate.Pending(unit, rules)
	if len(p) != 3 || p[0] != "write "+unit || !strings.Contains(p[1], "apt-get install -y -q polkitd") || p[2] != "write "+rules {
		t.Fatal(p)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatal("Pending wrote something")
	}
	_ = os.Mkdir(filepath.Dir(rules), 0o755)
	if _, err := panelupdate.Install(unit, rules, nil); err != nil {
		t.Fatal(err)
	}
	if p := panelupdate.Pending(unit, rules); len(p) != 0 {
		t.Fatal("after the setup nothing is pending:", p)
	}
	_ = os.WriteFile(unit, []byte("[Unit]\nDescription=older\n"), 0o644)
	if p := panelupdate.Pending(unit, rules); len(p) != 1 || p[0] != "write "+unit {
		t.Fatal(p)
	}
}

// r2-p4-06 (audit 5): two `fairy update` runs that overlap took each other's fairy.old away; with a release
// that does not come up no working program was left. One lock, held for the whole run, keeps them apart.
func TestOneUpdateAtATime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fairy update installs on Linux only")
	}
	dir := t.TempDir()
	lock := updateLockPath(filepath.Join(dir, "fairy"))
	if lock != filepath.Join(dir, ".fairy-update.lock") {
		t.Fatal(lock)
	}
	unlock, err := lockUpdate(lock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockUpdate(lock); !errors.Is(err, errUpdateRunning) {
		t.Fatal("a second run got the lock:", err)
	}
	// the second run, started by the button: it says so on the page, unless the page shows the first run
	statusPath := filepath.Join(dir, "state", "status.json")
	second := &panelupdate.Writer{Path: statusPath, From: "50-a"}
	if code := updateLocked(errUpdateRunning, lock, second, statusPath); code != 1 {
		t.Fatal(code)
	}
	st, ok := panelupdate.Read(statusPath)
	if !ok || st.State != "failed" || !st.Finished || !strings.Contains(st.Message, "an update is already running on this host") {
		t.Fatalf("%+v", st)
	}
	first := &panelupdate.Writer{Path: statusPath, From: "50-a"}
	first.Step("downloading", "51-b", "downloading fairy-linux-amd64") // the run that holds the lock is the button's
	if code := updateLocked(errUpdateRunning, lock, second, statusPath); code != 1 {
		t.Fatal(code)
	}
	if st, _ := panelupdate.Read(statusPath); st.State != "downloading" || st.Finished {
		t.Fatalf("the running run's status was written over: %+v", st)
	}
	// by hand: no status file is touched
	if code := updateLocked(errUpdateRunning, lock, nil, statusPath); code != 1 {
		t.Fatal(code)
	}
	// the first run ends (also by a crash: the system lets go of the lock): the next one gets it
	unlock()
	unlock2, err := lockUpdate(lock)
	if err != nil {
		t.Fatal("after the first run ended:", err)
	}
	unlock2()
	if _, err := os.Stat(lock); err != nil {
		t.Fatal("the lock file stays:", err)
	}
	// a lock that cannot be made is an error of its own, not "already running"
	if _, err := lockUpdate(filepath.Join(dir, "none", ".fairy-update.lock")); err == nil || errors.Is(err, errUpdateRunning) {
		t.Fatal(err)
	}
}
