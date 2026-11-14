package main

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/release"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/release/releasetest"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/panelupdate"
)

// asNewProgram: with this variable set, the test binary stands in for a downloaded `fairy` of that version: it
// answers `version` and `check-config FILE` with the program's own code (checkConfig). The tests below publish
// a two-line script that starts it so, as a signed release on a stand-in release host.
const asNewProgram = "FAIRY_TEST_AS_NEW_PROGRAM"

// ranAsDownload: a file that gets a line whenever this test program is run with a first argument that is no
// test flag, as an update runs a downloaded program (`version`, `check-config`): update_testbuild_test.go.
const ranAsDownload = "FAIRY_TEST_RAN_AS_DOWNLOAD"

func TestMain(m *testing.M) {
	if mark := os.Getenv(ranAsDownload); mark != "" && len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		_ = os.WriteFile(mark, []byte(strings.Join(os.Args[1:], " ")+"\n"), 0o600)
		os.Exit(2)
	}
	if v := os.Getenv(asNewProgram); v != "" {
		switch {
		case len(os.Args) == 2 && os.Args[1] == "version":
			fmt.Println("fairy", v)
			os.Exit(0)
		case len(os.Args) > 1 && os.Args[1] == checkConfigCmd:
			os.Exit(checkConfig(os.Args[2:], os.Stdout))
		}
		os.Exit(2)
	}
	os.Exit(m.Run())
}

const cfgHead = "public_url: https://panel.example.com\noidc: {issuer: \"https://id.example.com\", client_id: fairy}\n"

func TestCheckConfigCommand(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(cfgHead+body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	run := func(args ...string) (int, string) {
		var out bytes.Buffer
		code := checkConfig(args, &out)
		return code, out.String()
	}
	if code, out := run(write("good.yaml", "ups:\n  - {id: rack, type: nut, host: 10.0.0.2}\n")); code != 0 || out != "fairy check-config 1 ok\n" {
		t.Fatal(code, out)
	}
	code, out := run(write("ups.yaml", "ups:\n  - {id: rack-ups, type: nutt, host: 127.0.0.1, ups: apc}\n"))
	if code != 1 || !strings.HasPrefix(out, "fairy check-config 1 refused config: line 4 (ups, entry 1): ups rack-ups: ") ||
		strings.Count(out, "\n") != 1 {
		t.Fatal(code, out)
	}
	if code, out := run(write("key.yaml", "wake_updte:\n  enabled: true\n")); code != 1 || !strings.Contains(out, "refused config: yaml: unmarshal errors: line 3: field wake_updte not found") ||
		strings.Count(out, "\n") != 1 {
		t.Fatal(code, out)
	}
	if code, out := run(filepath.Join(dir, "none.yaml")); code != 1 || out != "fairy check-config 1 refused the file does not exist\n" {
		t.Fatal(code, out)
	}
	if code, out := run(); code != 2 || !strings.HasPrefix(out, "fairy check-config 1 refused usage") {
		t.Fatal(code, out)
	}
	// the answers the update reads
	for in, want := range map[string]int{"fairy check-config 1 ok\n": newAccepts, "fairy check-config 1 refused config: line 4: x\n": newRefuses,
		"": newCannotSay, "Fairy homelab panel\n\nusage:\n": newCannotSay, "fairy check-config 1 refused \n": newCannotSay,
		"fairy check-config 2 ok\n": newCannotSay, "ok\n": newCannotSay, "x\nfairy check-config 1 refused y\n": newCannotSay} {
		if got, _ := readCheckAnswer(in); got != want {
			t.Errorf("readCheckAnswer(%q) = %d, want %d", in, got, want)
		}
	}
	if _, why := readCheckAnswer("fairy check-config 1 refused config: line 4 (ups, entry 1): ups \"r\": unknown type\n"); why != `config: line 4 (ups, entry 1): ups "r": unknown type` {
		t.Fatal(why)
	}
}

// r3-p4-01, the run itself: a release is published on a stand-in release host, signed; the "installed program"
// is a file in a folder of the test. With a config.yaml that has a mistake inside a `ups:` entry (the
// auditor's), the new version is asked after its download was verified and before anything is replaced: the
// run ends there, the installed program is untouched, nothing is restarted, and the status the page shows
// names the line. Without the new version's check (RunNew that only runs `version`, as before) the same run
// swaps the program and restarts.
func TestTheUpdateStopsBeforeTheSwapOnAConfigTheNewVersionRefuses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in for the downloaded program is a shell script")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	newProgram := []byte("#!/bin/sh\n" + asNewProgram + "=102-bbbbbbb exec '" + self + "' \"$@\"\n")
	// a release from before this command: it knows `version` only
	oldRelease := []byte("#!/bin/sh\nif [ \"$1\" = version ]; then echo 'fairy 102-bbbbbbb'; exit 0; fi\necho 'Fairy homelab panel' >&2\nexit 2\n")
	asset := release.AssetName(release.PanelProgram, runtime.GOOS, runtime.GOARCH)

	type result struct {
		err             error
		target, out     string
		starts          int
		leftovers       []string
		status          panelupdate.Status
		consoleLastLine string
	}
	run := func(t *testing.T, program []byte, config string, check bool) result {
		dir := t.TempDir()
		cfg := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(cfg, []byte(cfgHead+config), 0o600); err != nil {
			t.Fatal(err)
		}
		bin := filepath.Join(dir, "bin")
		target := filepath.Join(bin, "fairy")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("the installed program"), 0o755); err != nil {
			t.Fatal(err)
		}
		gh := releasetest.New(t, release.PanelRepo)
		gh.Publish("build-102", release.PanelProgram, "102-bbbbbbb", map[string][]byte{asset: program}, nil)
		status := &panelupdate.Writer{Path: filepath.Join(dir, "status.json"), From: "101-aaaaaaa"}
		var out bytes.Buffer
		var res result
		asked := &newVersion{file: cfg}
		cli := &release.CLI{Client: gh.Client(), Key: gh.Pub, Program: release.PanelProgram, Current: "101-aaaaaaa", Target: target,
			Asset: asset, Out: &out, In: strings.NewReader(""), Yes: true,
			Installer: func(target, _ string) *release.Installer {
				return &release.Installer{Target: target, HealthPasses: 1, Interval: 1,
					Start:   func(context.Context) error { res.starts++; return nil },
					Healthy: func(context.Context) error { return nil }}
			},
			RunNew:   asked.runNew,
			Progress: func(state, to, msg string) { status.Step(state, to, asked.step(state, to, msg)) }}
		if !check { // the run as it was: the new program is only asked for its version
			cli.RunNew = (&newVersion{file: filepath.Join(dir, "another-file-that-is-fine.yaml")}).runNew
			if err := os.WriteFile(filepath.Join(dir, "another-file-that-is-fine.yaml"), []byte(cfgHead), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		res.err = cli.Run(context.Background())
		b, _ := os.ReadFile(target)
		res.target, res.out = string(b), out.String()
		_ = filepath.WalkDir(bin, func(p string, d fs.DirEntry, _ error) error {
			if p != bin && p != target {
				res.leftovers = append(res.leftovers, filepath.Base(p))
			}
			return nil
		})
		res.status, _ = panelupdate.Read(status.Path)
		if asked.problem != "" {
			res.consoleLastLine = asked.stops()
		}
		return res
	}
	const upsTypo = "ups:\n  - {id: rack-ups, type: nutt, host: 127.0.0.1, ups: apc}\n"

	t.Run("a mistake inside a ups: entry stops the run before the swap", func(t *testing.T) {
		r := run(t, newProgram, upsTypo, true)
		if r.err == nil || r.target != "the installed program" || r.starts != 0 || len(r.leftovers) != 0 {
			t.Fatalf("err=%v target=%q restarts=%d left=%v\n%s", r.err, r.target, r.starts, r.leftovers, r.out)
		}
		st := r.status
		if st.State != "failed" || !st.Finished || st.To != "102-bbbbbbb" || st.Message != r.consoleLastLine ||
			!strings.HasPrefix(st.Message, "the new version (102-bbbbbbb) refuses ") || !strings.Contains(st.Message, "config.yaml: config: line 4 (ups, entry 1): ups rack-ups: ") ||
			!strings.HasSuffix(st.Message, ". Nothing was changed. Correct the file on the panel's host, then update again.") {
			t.Fatalf("the status the page shows: %+v", st)
		}
	})
	t.Run("the same for power_sources: and for a key no version knows", func(t *testing.T) {
		for _, y := range []string{"power_sources:\n  - {id: plug-1, type: shely, url: 'http://127.0.0.1'}\n", "wake_updte:\n  enabled: true\n"} {
			if r := run(t, newProgram, y, true); r.err == nil || r.target != "the installed program" || r.starts != 0 || len(r.leftovers) != 0 ||
				r.status.State != "failed" || !strings.Contains(r.status.Message, "line ") {
				t.Fatalf("%q: err=%v target=%q restarts=%d left=%v status=%+v", y, r.err, r.target, r.starts, r.leftovers, r.status)
			}
		}
	})
	t.Run("a file the new version accepts: installed", func(t *testing.T) {
		r := run(t, newProgram, "ups:\n  - {id: rack-ups, type: nut, host: 127.0.0.1}\n", true)
		if r.err != nil || r.target != string(newProgram) || r.starts != 1 || r.status.State != "done" {
			t.Fatalf("err=%v restarts=%d status=%+v\n%s", r.err, r.starts, r.status, r.out)
		}
	})
	t.Run("without the new version's check the same file is swapped in (what the check is for)", func(t *testing.T) {
		r := run(t, newProgram, upsTypo, false)
		if r.err != nil || r.target != string(newProgram) || r.starts != 1 {
			t.Fatalf("err=%v restarts=%d\n%s", r.err, r.starts, r.out)
		}
	})
	t.Run("a release that does not know the command cannot say: the installed version's check is what there is", func(t *testing.T) {
		r := run(t, oldRelease, upsTypo, true)
		if r.err != nil || r.target != string(oldRelease) || r.starts != 1 {
			t.Fatalf("err=%v restarts=%d\n%s", r.err, r.starts, r.out)
		}
	})
	// a long refusal leaves room for the way out in the 300 bytes the status file keeps
	long := (&newVersion{file: "/etc/fairy/config.yaml", to: "102-bbbbbbb", problem: strings.Repeat("line 3: field ü not found in type config.Panel; ", 20)}).stops()
	path := filepath.Join(t.TempDir(), "status.json")
	(&panelupdate.Writer{Path: path, From: "101-aaaaaaa"}).Step("failed", "102-bbbbbbb", long)
	if st, _ := panelupdate.Read(path); st.Message != long || len(long) > 300 || !strings.HasSuffix(long, "then update again.") {
		t.Fatalf("a long refusal is cut in the status (%d bytes): %q", len(long), st.Message)
	}
}

// With a config file in a folder with a long name the reason was cut to what the path left of the 300 bytes:
// "... refuses /srv/.../config.yaml: it is a test build (-tags fairytest), which has…. Nothing was changed."
// (TestTheUpdateStopsOnATestBuildsAnswerToCheckConfig failed for that with a TMPDIR of 25 characters or more.)
// The reason is what the admin does not know yet: the path gives way first, from its beginning, and its end
// with the file's name stays.
func TestTheReasonOfARefusalIsNotCutForALongPath(t *testing.T) {
	const tail = ". Nothing was changed. Correct the file on the panel's host, then update again."
	folder := "/srv/" + strings.Repeat("a-folder-with-a-long-näme/", 8)
	for name, c := range map[string]struct {
		file, problem string
		whole         bool // the reason fits whole
	}{
		"a short path":                          {"/etc/fairy/config.yaml", testNeverInstalled, true},
		"a long path":                           {folder + "config.yaml", testNeverInstalled, true},
		"a long path and a refusal of the file": {folder + "config.yaml", "line 12: field wake_updates not found in type config.Panel", true},
		"a long path and a very long reason":    {folder + "config.yaml", strings.Repeat("line 3: field ü not found in type config.Panel; ", 20), false},
	} {
		says := (&newVersion{file: c.file, to: "102-ccccccc", problem: c.problem}).stops()
		if len(says) > 300 || !utf8.ValidString(says) || !strings.HasPrefix(says, "the new version (102-ccccccc) refuses ") || !strings.HasSuffix(says, tail) {
			t.Errorf("%s: %d bytes: %q", name, len(says), says)
		}
		if c.whole && !strings.Contains(says, ": "+c.problem+tail) {
			t.Errorf("%s: the reason is cut: %q", name, says)
		}
		if !c.whole && !strings.Contains(says, ": "+c.problem[:60]) {
			t.Errorf("%s: the beginning of the reason is not there: %q", name, says)
		}
		// the file is named: whole when there is room, else by its end
		if end := c.file[len(c.file)-min(len(c.file), 30):]; !strings.Contains(says, end+": ") {
			t.Errorf("%s: the message does not name the file (%s): %q", name, end, says)
		}
		if len(c.file)+len(c.problem) < 150 && !strings.Contains(says, " refuses "+c.file+": ") {
			t.Errorf("%s: a path that fits was cut: %q", name, says)
		}
		// a path that is cut gives way only as far as the reason needs: the message uses its 300 bytes (a
		// character of several bytes at the cut can leave a few unused), and is not cut down to pathEndKept
		if strings.Contains(says, " refuses …") && len(says) < 300-len("…")-utf8.UTFMax {
			t.Errorf("%s: the path was cut further than the reason needs (%d of 300 bytes used): %q", name, len(says), says)
		}
		if c.whole && len(c.file) > 200 {
			if kept := strings.TrimSuffix(strings.TrimPrefix(says, "the new version (102-ccccccc) refuses "), ": "+c.problem+tail); len(kept) <= pathEndKept+len("…") {
				t.Errorf("%s: of a path of %d bytes only %d are left, and there was room for more: %q", name, len(c.file), len(kept), says)
			}
		}
	}
}

// One check of a config file (config.LoadFull): `fairy serve`, `fairy admin config check` and `fairy update`
// must not each check the device lists in their own way again (r3-p4-01: the update did not check them at all).
func TestOneCheckOfAConfigFile(t *testing.T) {
	root := filepath.Join("..", "..")
	var elsewhere []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel = filepath.ToSlash(rel); rel != "internal/config/full.go" && bytes.Contains(b, []byte(".ValidateConfigs")) {
			elsewhere = append(elsewhere, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(elsewhere) > 0 {
		t.Fatalf("the device lists of config.yaml are checked outside config.LoadFull: %v", elsewhere)
	}
	for file, want := range map[string]string{"serve.go": "config.LoadFull(f.cfgPath)", "update.go": "config.LoadFull(",
		filepath.Join(root, "internal", "admin", "configcheck.go"): "config.LoadFull(c.cfgPath)"} {
		if b, err := os.ReadFile(file); err != nil || !bytes.Contains(b, []byte(want)) {
			t.Errorf("%s no longer checks the file with %s (%v)", file, want, err)
		}
	}
}
