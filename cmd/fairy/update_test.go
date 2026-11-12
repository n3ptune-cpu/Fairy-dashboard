package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/panelupdate"
)

// The update flow itself (check, question, download, verify, swap, rollback) is release.CLI, tested in
// Fairy-agent's pkg/release; here only the panel's own parts.

func TestHealthAddr(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, "fairy.env")
	if got := healthAddr(nil, env); got != "127.0.0.1:8080" {
		t.Fatal(got)
	}
	_ = os.WriteFile(env, []byte("FAIRY_LISTEN=0.0.0.0:9000\n"), 0o600)
	if got := healthAddr(nil, env); got != "127.0.0.1:9000" {
		t.Fatal(got)
	}
	_ = os.WriteFile(env, []byte("FAIRY_LISTEN=\"10.0.0.20:8080\"\n"), 0o600)
	if got := healthAddr(nil, env); got != "10.0.0.20:8080" {
		t.Fatal(got)
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
