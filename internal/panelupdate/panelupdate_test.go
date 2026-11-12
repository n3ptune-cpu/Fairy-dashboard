package panelupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/deploy"
)

func TestInstallAndStatus(t *testing.T) {
	dir := t.TempDir()
	unit, rules := filepath.Join(dir, "fairy-panel-update.service"), filepath.Join(dir, "rules.d", "49-fairy-panel.rules")
	var ran []string
	run := func(name string, args ...string) error {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return nil
	}
	if _, err := Install(unit, rules, run); err == nil {
		t.Fatal("no polkit folder must be reported")
	}
	if err := os.Mkdir(filepath.Dir(rules), 0o755); err != nil {
		t.Fatal(err)
	}
	changed, err := Install(unit, rules, run)
	if err != nil || len(changed) != 1 || ran[len(ran)-1] != "systemctl daemon-reload" { // the unit was written by the first try
		t.Fatal(changed, err, ran)
	}
	b, _ := os.ReadFile(unit)
	if !strings.Contains(string(b), "ExecStart=/usr/local/bin/fairy update --yes --from-panel") {
		t.Fatal(string(b))
	}
	r, _ := os.ReadFile(rules)
	if !strings.Contains(string(r), `action.lookup("unit") === "fairy-panel-update.service"`) || !strings.Contains(string(r), `subject.user === "fairy"`) {
		t.Fatal(string(r))
	}
	if changed, _ := Install(unit, rules, run); len(changed) != 0 {
		t.Fatal("second run changed", changed)
	}
	status := filepath.Join(dir, "status", "status.json")
	w := &Writer{Path: status, From: "8-a"}
	if _, ok := Read(status); ok {
		t.Fatal("no status yet")
	}
	w.Step("downloading", "9-b", "downloading")
	w.Step("done", "", "running")
	st, ok := Read(status)
	if !ok || st.State != "done" || !st.Finished || st.From != "8-a" || st.To != "9-b" || st.StartedAt.IsZero() {
		t.Fatalf("%+v", st)
	}
}

// r2-p4-02 (audit 5): an "Update panel" run that is stopped half-way (a reboot, a crash, systemd's time limit
// on a stalled download) never writes its end. The page read "an update is running" from finished=false alone,
// for ever. A run now counts as running only while it can be alive.
func TestAStoppedRunIsNotARunningOne(t *testing.T) {
	now := time.Date(2026, 11, 12, 4, 30, 0, 0, time.UTC)
	run := func(age time.Duration) Status {
		return Status{State: "downloading", From: "50-a", To: "51-b", Message: "downloading fairy-linux-amd64",
			StartedAt: now.Add(-age), UpdatedAt: now.Add(-age + time.Second)}
	}
	for name, c := range map[string]struct {
		st   Status
		act  Activity
		dead bool
	}{
		"young, the unit runs":               {run(time.Minute), ActivityRunning, false},
		"young, systemd gives no answer":     {run(time.Minute), ActivityUnknown, false},
		"young, the unit is not running":     {run(time.Minute), ActivityNone, true},
		"at the unit's limit, unit running":  {run(RunLimit), ActivityRunning, false},
		"older than any run, no answer":      {run(StaleAfter + time.Second), ActivityUnknown, true},
		"older than any run, 'unit running'": {run(StaleAfter + time.Second), ActivityRunning, true}, // that is another, newer run
		"a week old":                         {run(7 * 24 * time.Hour), ActivityUnknown, true},
		"no start time":                      {Status{State: "checking"}, ActivityRunning, true},
		"finished":                           {Status{State: "failed", Finished: true, StartedAt: now.Add(-time.Hour)}, ActivityNone, false},
	} {
		if got := c.st.Dead(now, c.act); got != c.dead {
			t.Errorf("%s: dead = %v, want %v", name, got, c.dead)
		}
		s := c.st.Settled(now, c.act)
		if c.dead != (s.State == "stopped") || (c.dead && !s.Finished) || (!c.dead && s != c.st) {
			t.Errorf("%s: settled %+v", name, s)
		}
	}
	s := run(time.Hour).Settled(now, ActivityUnknown)
	if s.Message != "stopped before it finished (while downloading, 2026-11-12 03:30 UTC)" || s.To != "51-b" || !s.UpdatedAt.Equal(now.Add(-time.Hour+time.Second)) {
		t.Fatalf("%+v", s)
	}
	for out, want := range map[string]Activity{"activating\n": ActivityRunning, "active\n": ActivityRunning, "deactivating": ActivityRunning,
		"inactive\n": ActivityNone, "failed\n": ActivityNone, "": ActivityUnknown, "unknown\n": ActivityUnknown,
		"System has not been booted with systemd as init system": ActivityUnknown} {
		if got := parseActivity(out); got != want {
			t.Errorf("systemctl is-active said %q: %v, want %v", out, got, want)
		}
	}

	// the real Runner: the file of the auditor's proof (case-04), read 5 s after the run was stopped
	dir := t.TempDir()
	path := filepath.Join(dir, "status.json")
	stopped := run(5 * time.Second)
	stopped.StartedAt, stopped.UpdatedAt = time.Now().Add(-5*time.Second).UTC(), time.Now().Add(-4*time.Second).UTC()
	if err := writeFile(path, stopped); err != nil {
		t.Fatal(err)
	}
	asked := 0
	unit := ActivityRunning
	r := Systemd{StatusPath: path, Active: func() Activity { asked++; return unit }}
	if st, ok := r.Status(); !ok || st.Finished || st.State != "downloading" || asked != 1 {
		t.Fatalf("while the unit runs it is running: %+v (asked %d)", st, asked)
	}
	unit = ActivityNone
	if st, ok := r.Status(); !ok || !st.Finished || st.State != "stopped" || !strings.HasPrefix(st.Message, "stopped before it finished (while downloading, ") {
		t.Fatalf("the unit is gone: %+v", st)
	}
	if st, _ := Read(path); st.Finished { // reading as the panel does changes nothing on disk (the file is root's)
		t.Fatalf("the panel's read rewrote the file: %+v", st)
	}
	// a run by hand as root marks it; the unit is asked only for a file that could be a running one
	if _, was := SettleFile(path, time.Now(), ActivityRunning); was {
		t.Fatal("a run that can be alive was marked as stopped")
	}
	old, was := SettleFile(path, time.Now(), ActivityNone)
	st, _ := Read(path)
	if !was || old.State != "downloading" || st.State != "stopped" || !st.Finished || st.From != "50-a" {
		t.Fatalf("%v %+v %+v", was, old, st)
	}
	if _, was := SettleFile(path, time.Now(), ActivityNone); was {
		t.Fatal("marked twice")
	}
	asked = 0
	if st, _ := r.Status(); st.State != "stopped" || asked != 0 {
		t.Fatalf("a finished file needs no question to systemd: %+v (asked %d)", st, asked)
	}
	if _, was := SettleFile(filepath.Join(dir, "none.json"), time.Now(), ActivityNone); was {
		t.Fatal("no file is no stopped run")
	}
}

// What a stopped download leaves next to the program (up to 96 MiB each time) is removed by the next run,
// and nothing else is: not a download that can still be running, not another file.
func TestStaleDownloadsAreRemoved(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	mk := func(name string, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	mk(".download-111", 3*time.Hour)
	mk(".download-222", DownloadStaleAfter+time.Second)
	mk(".download-333", 9*time.Minute) // a download ends after 10 minutes at most: this one may be running
	mk("fairy", 30*24*time.Hour)
	mk("fairy.old", 30*24*time.Hour)
	mk(".downloads", 3*time.Hour)
	if err := os.Mkdir(filepath.Join(dir, ".download-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-3 * time.Hour)
	_ = os.Chtimes(filepath.Join(dir, ".download-dir"), old, old)
	got := RemoveStaleDownloads(dir, now)
	if strings.Join(got, " ") != ".download-111 .download-222" {
		t.Fatal(got)
	}
	left, _ := os.ReadDir(dir)
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	if strings.Join(names, " ") != ".download-333 .download-dir .downloads fairy fairy.old" {
		t.Fatal(names)
	}
	if got := RemoveStaleDownloads(filepath.Join(dir, "none"), now); got != nil {
		t.Fatal(got)
	}
}

// The unit's time limit and the panel's idea of it are one number.
func TestUnitTimeLimitMatches(t *testing.T) {
	want := fmt.Sprintf("\nTimeoutStartSec=%d\n", int(RunLimit.Seconds()))
	if !strings.Contains(string(deploy.PanelUpdateUnit), want) {
		t.Fatalf("deploy/fairy-panel-update.service must have %q (panelupdate.RunLimit)", strings.TrimSpace(want))
	}
	if StaleAfter <= RunLimit || RunLimit < 17*time.Minute { // `fairy update`: 15 minutes, and up to 135 s more for the way back
		t.Fatal(RunLimit, StaleAfter)
	}
}
