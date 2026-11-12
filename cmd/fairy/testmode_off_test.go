//go:build !fairytest

package main

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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
	for _, backwards := range []string{"DLIUB TSET", "ffo si ni-ngis", "nimda-tset", "snimda-tsetyriaf", "ETATS-TSET-YRIAF", "edoMtseTelbanE", "dilavni.tsetyriaf",
		// the start key of a test build (audit 5, r1-p1-01): its cookie, its refusal, the printed line, its check
		"yek_tsetyriaf", "wodniw elosnoc sti ni detnirp sserdda eht nepo", "yek trats eht sdloh ti", "yeKtseTdilaV"} {
		needle := []byte(backwards)
		slices.Reverse(needle)
		if bytes.Contains(prog, needle) {
			t.Errorf("this build contains %q: test-build code is compiled in without the tag", needle)
		}
	}
}

// The release workflow builds without build tags, and stops if a built program is a test build anyway.
//
// This test is the only thing in the tree that notices a change of the workflow file, so it reads the file as
// the YAML it is (audit 5, r2-p4-04: looking for two pieces of text let a guard pass that ends with "exit 0",
// has "if: false", or sits behind a "-tags fairytest" on the Build step's second line):
//
//   - the workflow starts on a push to main and by hand, nothing else, and sets no environment or shell of its own;
//   - the job has the steps Build, "Refuse a test build", Sign and "Publish the release" in this order, the
//     guard right after Build, and nothing signs or uploads before the guard;
//   - the guard step is a name and a script, nothing else (no "if:", no "continue-on-error:", no shell), and
//     the script is the one below, character for character;
//   - nothing outside the guard's script mentions the tag, build tags or a way to pass them.
//
// A change of the guard or of the Build step on purpose has to change this test with it.
func TestReleaseWorkflowNeverBuildsATestPanel(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range releaseWorkflowProblems(b) {
		t.Error("release.yml: " + p)
	}
}

// releaseGuardScript is the "Refuse a test build" step's script, as it must be.
const releaseGuardScript = `# A panel built with the Go build tag "fairytest" has no sign-in (TESTING-LOCAL.md). No step of this
# workflow passes a build tag. If a build ever got one anyway, nothing is signed or published.
set -eu
for f in dist/fairy-linux-*; do
  if go version -m "$f" | grep -q fairytest; then
    echo "::error::$f was built with the fairytest tag: it is a test build without sign-in. Not released."
    exit 1
  fi
done
f=dist/fairy-linux-amd64
if "$f" version | grep -qi 'test build'; then
  echo "::error::$f says it is a test build. Not released."
  exit 1
fi
`

// releaseWorkflowProblems says what is wrong with a release.yml (nothing for the tree's own).
func releaseWorkflowProblems(raw []byte) []string {
	var problems []string
	bad := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return []string{fmt.Sprintf("not a YAML mapping: %v", err)}
	}
	// keys of a mapping, and one value; a key that is there twice is a problem of its own (the last one would win)
	get := func(m *yaml.Node, key string) *yaml.Node {
		var found *yaml.Node
		for i := 0; m != nil && m.Kind == yaml.MappingNode && i+1 < len(m.Content); i += 2 {
			if m.Content[i].Value == key {
				if found != nil {
					bad("the key %q is there twice (line %d)", key, m.Content[i].Line)
				}
				found = m.Content[i+1]
			}
		}
		return found
	}
	keys := func(m *yaml.Node) string {
		var ks []string
		for i := 0; m != nil && m.Kind == yaml.MappingNode && i+1 < len(m.Content); i += 2 {
			ks = append(ks, m.Content[i].Value)
		}
		slices.Sort(ks)
		return strings.Join(ks, " ")
	}
	text := func(n *yaml.Node) string {
		if n == nil {
			return ""
		}
		out, _ := yaml.Marshal(n)
		return string(out)
	}
	top := doc.Content[0]

	// when it starts, and with what
	if got := keys(top); got != "concurrency jobs name on permissions" {
		bad("top-level keys are %q: an env:, defaults: or another key there can change every step", got)
	}
	on := get(top, "on")
	if keys(on) != "push workflow_dispatch" {
		bad("it must start on push and workflow_dispatch only, not on: %s", keys(on))
	}
	if push := get(on, "push"); keys(push) != "branches paths-ignore" || strings.TrimSpace(text(get(push, "branches"))) != "[main]" {
		bad("a push must start it for the branch main only: %s", strings.TrimSpace(text(push)))
	}
	jobs := get(top, "jobs")
	if keys(jobs) != "release" {
		bad("the jobs must be the one job \"release\": %s", keys(jobs))
	}
	job := get(jobs, "release")
	if got := keys(job); got != "environment runs-on steps timeout-minutes" {
		bad("the job's keys are %q: an env:, defaults:, if: or continue-on-error: there can change every step", got)
	}

	// the steps and their order
	steps := get(job, "steps")
	if steps == nil || steps.Kind != yaml.SequenceNode {
		return append(problems, "the job has no steps")
	}
	at := map[string]int{}
	for i, st := range steps.Content {
		name := ""
		if n := get(st, "name"); n != nil {
			name = n.Value
		}
		if _, twice := at[name]; twice && name != "" {
			bad("two steps are named %q", name)
		}
		at[name] = i
	}
	build, okB := at["Build"]
	guard, okG := at["Refuse a test build"]
	sign, okS := at["Sign"]
	publish, okP := at["Publish the release"]
	if !okB || !okG || !okS || !okP {
		return append(problems, "the steps Build, \"Refuse a test build\", Sign and \"Publish the release\" must all be there, by these names")
	}
	if guard != build+1 || sign != guard+1 || publish < sign {
		bad("the order must be Build, \"Refuse a test build\" right after it, then Sign, then Publish (they are steps %d, %d, %d, %d)", build+1, guard+1, sign+1, publish+1)
	}
	g := steps.Content[guard]
	if got := keys(g); got != "name run" {
		bad("the guard step must be a name and a script and nothing else (no if:, continue-on-error:, shell:, env:); it has: %s", got)
	}
	run := get(g, "run")
	if run == nil || run.Value != releaseGuardScript {
		got := ""
		if run != nil {
			got = run.Value
		}
		bad("the guard's script is not the one this test knows (releaseGuardScript). It is:\n%s", got)
	}
	for _, name := range []string{"Build", "Sign", "Publish the release"} {
		st := steps.Content[at[name]]
		for _, k := range []string{"if", "continue-on-error", "shell", "uses"} {
			if get(st, k) != nil {
				bad("the step %q has %s:", name, k)
			}
		}
	}
	if get(steps.Content[build], "env") != nil {
		bad("the Build step has an env: block (GOFLAGS and GOENV can pass a build tag)")
	}

	// nothing signs or uploads before the guard has run, and nothing builds after it
	for i, st := range steps.Content {
		s := text(st)
		if i < guard && (strings.Contains(s, "fairy-release-key") || strings.Contains(s, "gh release") || strings.Contains(s, "upload")) {
			bad("step %d signs or uploads before the guard", i+1)
		}
		if i > guard && regexp.MustCompile(`\bgo\s+(build|install)\b`).MatchString(s) {
			bad("step %d builds after the guard has looked", i+1)
		}
	}

	// build tags, and the ways to pass them: nowhere but in the guard's own script (which names the tag)
	rest := strings.Replace(string(raw), indent(releaseGuardScript, "          "), "", 1)
	if rest == string(raw) {
		bad("the guard's script is not in the file as this test knows it")
	}
	flat := strings.Join(strings.Fields(strings.ReplaceAll(rest, "\\\n", " ")), " ") // continuation lines joined
	for _, word := range []string{"fairytest", "-tags", "GOFLAGS", "GOENV", "go env", "-toolexec", "-overlay", "-modfile", "$GITHUB_ENV", "GITHUB_ENV"} {
		if strings.Contains(strings.ToLower(flat), strings.ToLower(word)) {
			bad("%q outside the guard's script: no step may pass a build tag, or set one for a later step", word)
		}
	}
	// every `go build` of the job is the Build step's one command, with exactly these arguments
	wantBuild := `CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" go build -trimpath -ldflags "-s -w -X main.version=${version} -X main.releaseKey=${pub}" -o "dist/fairy-linux-${goarch}" ./cmd/fairy`
	n := 0
	for _, st := range steps.Content {
		r := get(st, "run")
		if r == nil {
			continue
		}
		script := strings.ReplaceAll(r.Value, "\\\n", " ")
		for _, line := range strings.Split(script, "\n") {
			if !regexp.MustCompile(`\bgo\s+build\b`).MatchString(line) {
				continue
			}
			n++
			if got := strings.Join(strings.Fields(line), " "); got != wantBuild {
				bad("a go build that is not the release's: %s", got)
			}
		}
	}
	if n != 1 {
		bad("there must be exactly one go build command; found %d", n)
	}
	return problems
}

func indent(s, by string) string {
	var out strings.Builder
	for _, line := range strings.SplitAfter(s, "\n") {
		if line != "" {
			out.WriteString(by + line)
		}
	}
	return out.String()
}

// The auditor's changes to release.yml (r2-p4-04, 01-guard/mutate.sh), each on a copy of the real file: every
// one of them must be noticed. Before, only two of the nine were.
func TestReleaseWorkflowTestNoticesTampering(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	orig := string(b)
	if p := releaseWorkflowProblems(b); len(p) != 0 {
		t.Fatalf("control: the unchanged file has problems: %q", p)
	}
	guardExit0 := func(y string) string {
		i, j := strings.Index(y, "- name: Refuse a test build"), strings.Index(y, "- name: Sign")
		return y[:i] + strings.ReplaceAll(y[i:j], "exit 1", "exit 0") + y[j:]
	}
	tagOnSecondLine := func(y string) string {
		return strings.Replace(y, "go build -trimpath \\\n", "go build -trimpath \\\n              -tags fairytest \\\n", 1)
	}
	guardKey := func(line string) func(string) string {
		return func(y string) string {
			return strings.Replace(y, "      - name: Refuse a test build\n", "      - name: Refuse a test build\n        "+line+"\n", 1)
		}
	}
	rep := func(old, new string) func(string) string {
		return func(y string) string { return strings.Replace(y, old, new, 1) }
	}
	cases := []struct {
		name   string
		change func(string) string
	}{
		{"1 both 'exit 1' of the guard become 'exit 0'", guardExit0},
		{"2 the guard step gets 'if: false'", guardKey("if: false")},
		{"3 the guard step gets 'continue-on-error: true'", guardKey("continue-on-error: true")},
		{"4 '-tags fairytest' on a continuation line of the Build step's go build", tagOnSecondLine},
		{"5 GOFLAGS spelled GOFLAG${x}S in the Build step", rep("CGO_ENABLED=0 GOOS=linux", "CGO_ENABLED=0 GOFLAG${x}S=-tags=fairytest GOOS=linux")},
		{"6 'go  build -tags fairytest' with two spaces after go", rep("go build -trimpath \\", "go  build -tags fairytest -trimpath \\")},
		{"7 plain 'go build -tags fairytest'", rep("go build -trimpath \\", "go build -tags fairytest -trimpath \\")},
		{"8 the release also runs on pushes to test/** branches", rep("    branches: [main]", "    branches: [main, \"test/**\"]")},
		{"9 cases 1 and 4 together", func(y string) string { return tagOnSecondLine(guardExit0(y)) }},
		// more of the same kind
		{"the guard's first check loses its exit", rep("Not released.\"\n              exit 1\n            fi\n          done", "Not released.\"\n            fi\n          done")},
		{"the guard looks at another folder", rep("for f in dist/fairy-linux-*; do\n            if go version", "for f in out/fairy-linux-*; do\n            if go version")},
		{"the guard after Sign", func(y string) string {
			i, j, k := strings.Index(y, "      - name: Refuse a test build"), strings.Index(y, "      - name: Sign"), strings.Index(y, "      - name: Release notes")
			return y[:i] + y[j:k] + y[i:j] + y[k:]
		}},
		{"the guard removed", func(y string) string {
			i, j := strings.Index(y, "      - name: Refuse a test build"), strings.Index(y, "      - name: Sign")
			return y[:i] + y[j:]
		}},
		{"a second, later step named like the guard's neighbour builds again", rep("      - name: Sign\n", "      - name: Rebuild\n        run: |\n          go build -o dist/fairy-linux-amd64 ./cmd/fairy\n\n      - name: Sign\n")},
		{"the tag through the job's env", rep("    environment: release", "    env:\n      GOFLAGS: -tags=fairytest\n    environment: release")},
		{"the tag through the workflow's env", rep("permissions:\n", "env:\n  GOFLAGS: -tags=fairytest\n\npermissions:\n")},
		{"the tag through an earlier step and $GITHUB_ENV", rep("          go vet ./...\n", "          echo \"GOFLAGS=-tags=fairy\"test >> \"$GITHUB_ENV\"\n          go vet ./...\n")},
		{"the tag through go env -w", rep("          go vet ./...\n", "          go env -w GOFLAGS=-tags=fairy\"\"test\n          go vet ./...\n")},
		{"the guard step with its own shell", guardKey("shell: bash {0} || true")},
		{"the whole job may fail", rep("    timeout-minutes: 30", "    timeout-minutes: 30\n    continue-on-error: true")},
		{"the release on every pull request", rep("  workflow_dispatch:", "  pull_request:\n  workflow_dispatch:")},
	}
	for _, c := range cases {
		changed := c.change(orig)
		if changed == orig {
			t.Errorf("%s: the change did not apply (release.yml moved on: adapt this case)", c.name)
			continue
		}
		if p := releaseWorkflowProblems([]byte(changed)); len(p) == 0 {
			t.Errorf("%s: NOT noticed", c.name)
		} else {
			t.Logf("%s: noticed (%s)", c.name, strings.SplitN(p[0], "\n", 2)[0])
		}
	}
}
