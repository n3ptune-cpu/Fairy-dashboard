package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/release"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/release/releasetest"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/panelupdate"
)

// Audit 6, r4-p4-03 (R6). A test build (-tags fairytest: no sign-in), published and signed as the newest
// release, was asked `check-config`, answered "ok", was swapped in, refused to start, and the panel was away
// until the old program was back (about 50 s). The update now refuses such a download before it replaces
// anything: by what the file says of itself (the build tag in the lines `go version -m` prints, the word the
// release workflow's guard looks for; the file is not run). And a test build answers `check-config` with
// "refused", which also stops an update of an older version that does not look at the file yet.

// updateTo runs an update (release.CLI with this program's own RunNew and Progress, as update() sets them up)
// from an installed "fairy 101" to a release 102 whose program is the given file. It tells what the run
// said, whether the installed program was replaced, how often the service was started, what the status file
// of the Updates page says, and whether the downloaded program was run.
func updateTo(t *testing.T, program []byte) (err error, says string, replaced bool, starts int, st panelupdate.Status) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(cfgHead), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "bin", "fairy")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("the installed program"), 0o755); err != nil {
		t.Fatal(err)
	}
	asset := release.AssetName(release.PanelProgram, runtime.GOOS, runtime.GOARCH)
	gh := releasetest.New(t, release.PanelRepo)
	gh.Publish("build-102", release.PanelProgram, "102-ccccccc", map[string][]byte{asset: program}, nil)
	status := &panelupdate.Writer{Path: filepath.Join(dir, "status.json"), From: "101-aaaaaaa"}
	var out bytes.Buffer
	asked := &newVersion{file: cfg}
	cli := &release.CLI{Client: gh.Client(), Key: gh.Pub, Program: release.PanelProgram, Current: "101-aaaaaaa", Target: target,
		Asset: asset, Out: &out, In: strings.NewReader(""), Yes: true,
		Installer: func(target, _ string) *release.Installer {
			return &release.Installer{Target: target, HealthPasses: 1, Interval: 1,
				Start:   func(context.Context) error { starts++; return nil },
				Healthy: func(context.Context) error { return nil }}
		},
		RunNew:   asked.runNew,
		Progress: func(state, to, msg string) { status.Step(state, to, asked.step(state, to, msg)) }}
	err = cli.Run(context.Background())
	if asked.stopped() {
		says = asked.stops()
	}
	now, _ := os.ReadFile(target)
	if left, _ := os.ReadDir(filepath.Dir(target)); len(left) != 1 && string(now) == "the installed program" {
		t.Errorf("the download was left next to the installed program: %v", left)
	}
	st, _ = panelupdate.Read(status.Path)
	return err, says, string(now) != "the installed program", starts, st
}

const testBuildStop = "the new version (102-ccccccc) was not installed: it is a test build (-tags fairytest), which has no sign-in and is never installed. Nothing was changed."

// What an update of an older version sees, which does not look at the file itself: the test build's answer to
// `check-config`. The stand-in is a script that answers as a test build does (checkConfigCommand, below); it
// has no build information. The run stops before the swap on that answer, in the words it has for a refusal.
func TestTheUpdateStopsOnATestBuildsAnswerToCheckConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in for the downloaded program is a shell script")
	}
	script := "#!/bin/sh\nif [ \"$1\" = version ]; then echo 'fairy 102-ccccccc'; exit 0; fi\n" +
		"echo '" + checkConfigLead + "refused " + testNeverInstalled + "'\nexit 1\n"
	err, says, replaced, starts, st := updateTo(t, []byte(script))
	if err == nil || replaced || starts != 0 || !strings.Contains(says, "refuses ") || !strings.Contains(says, ": "+testNeverInstalled+". Nothing was changed.") {
		t.Fatalf("err=%v replaced=%v restarts=%d\n%s", err, replaced, starts, says)
	}
	if st.State != "failed" || !st.Finished || st.Message != says {
		t.Errorf("the status the page shows: %+v", st)
	}
	// the same script with "ok" is installed: it is that answer that stops the run
	ok := "#!/bin/sh\nif [ \"$1\" = version ]; then echo 'fairy 102-ccccccc'; exit 0; fi\necho '" + checkConfigLead + "ok'\n"
	err, _, replaced, starts, st = updateTo(t, []byte(ok))
	if err != nil || !replaced || starts != 1 || st.State != "done" {
		t.Fatalf("a release: err=%v replaced=%v restarts=%d status=%+v", err, replaced, starts, st)
	}
}

// What the file says of itself. This test's own program is a Go program built without the tag, or with it
// (go test -tags fairytest): the update takes the first as far as `version` (which this test program does not
// answer as a release would, so the run ends there, as "does not run here"), and never runs the second.
func TestTheUpdateReadsTheBuildTagOfTheDownloadedProgram(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if got := testBuildFile(self); got != testBuild {
		t.Fatalf("testBuildFile(this test program) = %v; it was built with fairytest: %v", got, testBuild)
	}
	if testBuildFile(filepath.Join(t.TempDir(), "none")) || testBuildFile("update_config.go") {
		t.Error("a file that is no Go program is called a test build")
	}
	program, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	mark := filepath.Join(t.TempDir(), "ran")
	t.Setenv(ranAsDownload, mark) // TestMain leaves the mark when the downloaded copy of this program is run
	uerr, says, replaced, starts, st := updateTo(t, program)
	ran, _ := os.ReadFile(mark)
	if uerr == nil || replaced || starts != 0 {
		t.Fatalf("err=%v replaced=%v restarts=%d", uerr, replaced, starts)
	}
	if !testBuild {
		if says != "" || len(ran) == 0 {
			t.Errorf("a program without the tag: %q, run: %q", says, ran)
		}
		return
	}
	if says != testBuildStop || st.State != "failed" || st.Message != testBuildStop {
		t.Errorf("a test build as the newest release:\n%s\nstatus %+v", says, st)
	}
	if len(ran) != 0 {
		t.Errorf("the test build was run (%s): what the file says of itself must stop the run before that", ran)
	}
}

// `fairy check-config` of a test build: "refused", whatever the file. An update of an older version, which
// does not look at the file itself, stops on that answer before the swap.
func TestATestBuildAnswersCheckConfigWithRefused(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte(cfgHead), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code := checkConfigCommand([]string{cfg}, &out)
	verdict, why := readCheckAnswer(out.String())
	if !testBuild {
		if code != 0 || verdict != newAccepts {
			t.Fatalf("a release: %d %q", code, out.String())
		}
		return
	}
	if code != 1 || verdict != newRefuses || why != testNeverInstalled {
		t.Fatalf("a test build: %d %q", code, out.String())
	}
}
