//go:build !fairytest

package main

import (
	"bytes"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The test build (sign-in off, TESTING-LOCAL.md) is not in this build. Releases are built like this one, without
// a build tag, so this is the proof for them: the hooks find nothing, and the program does not hold one of the
// test build's texts or functions.
func TestNoTestModeInThisBuild(t *testing.T) {
	if testBuild || testNote != "" || testUpdateOff != "" || testAdminOff != "" {
		t.Fatal("a build without the fairytest tag says it is a test build")
	}
	if p, code := setupTest(serveFlags{}); p != nil || code == 0 {
		t.Fatal("setupTest must not be compiled in")
	}
	f, _ := parseServeFlags(nil)
	before := f
	testDefaults(&f)
	if f != before {
		t.Fatal("testDefaults changed the flags of a normal build")
	}
	// This test program is cmd/fairy and everything it links, built the way the release builds it. The texts
	// are written backwards here, so the test itself does not bring them in.
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	prog, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	for _, backwards := range []string{"DLIUB TSET", "ffo si ni-ngis", "nimda-tset", "snimda-tsetyriaf", "ETATS-TSET-YRIAF", "edoMtseTelbanE", "dilavni.tsetyriaf"} {
		needle := []byte(backwards)
		slices.Reverse(needle)
		if bytes.Contains(prog, needle) {
			t.Errorf("this build contains %q: test-build code is compiled in without the tag", needle)
		}
	}
}

// The release workflow builds without build tags, and stops if a built program is a test build anyway.
func TestReleaseWorkflowNeverBuildsATestPanel(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	y := string(b)
	goCmd := regexp.MustCompile(`\bgo (build|run|test|vet|install)\b`)
	for _, line := range strings.Split(y, "\n") {
		if strings.Contains(line, "GOFLAGS") || (goCmd.MatchString(line) && strings.Contains(line, "-tags")) {
			t.Errorf("release.yml passes build tags or GOFLAGS: %s", strings.TrimSpace(line))
		}
	}
	build, guard, sign := strings.Index(y, "- name: Build"), strings.Index(y, "- name: Refuse a test build"), strings.Index(y, "- name: Sign")
	if build < 0 || guard < build || sign < guard {
		t.Fatal("release.yml: the step \"Refuse a test build\" must come after Build and before Sign")
	}
	for _, want := range []string{`go version -m "$f" | grep -q fairytest`, `"$f" version | grep -qi 'test build'`} {
		if !strings.Contains(y[guard:sign], want) {
			t.Errorf("release.yml: the guard step no longer has: %s", want)
		}
	}
}
