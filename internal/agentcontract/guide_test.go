package agentcontract_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/release"
)

// The Updates page shows an agent's own sentence when an update failed, and the guide tells the owner how to
// read its ending. The endings are made by Fairy-agent's installer (release.RolledBackError): the guide must
// name every one of them, in the installer's words.
func TestGuideNamesEveryEndingOfAFailedUpdate(t *testing.T) {
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	guide := strings.Join(strings.Fields(string(raw)), " ") // the guide's lines are wrapped
	cause, why := errors.New("CAUSE"), errors.New("WHY")
	seen := map[string]bool{}
	for _, e := range []*release.RolledBackError{
		{Cause: cause}, // the old version runs again and answered
		{Cause: cause, RollbackErr: why, Restored: true}, // put back and started, its check did not finish
		{Cause: cause, RollbackErr: why},                 // the old version did not come up either
	} {
		ending := strings.TrimPrefix(e.Error(), "CAUSE; ")
		if ending == e.Error() {
			t.Fatalf("the installer's sentence no longer starts with its cause: %q", e.Error())
		}
		ending = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(ending, "WHY"), ": "), " ")
		if seen[ending] || len(ending) < 20 {
			t.Fatalf("ending %q: not one of three different sentences", ending)
		}
		seen[ending] = true
		if !strings.Contains(guide, `"`+ending) {
			t.Errorf("README.md does not say how to read an update that ends with %q", ending)
		}
	}
}

// r1-b2-11 (audit 5): seven lines of the guides were wrong or stale. The ones a test can hold: the test
// config points at the right part, the guide names no file that is in neither tree, its log lines start as
// the program prints them, and the agent folder it tells to unpack is the one go.mod builds against.
func TestTestingLocalPartIFitsTheBuild(t *testing.T) {
	read := func(path string) string {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	guide, readme := read("../../TESTING-LOCAL.md"), read("../../README.md")
	if cfg := read("../../cmd/fairy/fairytest-config.yaml"); !strings.Contains(cfg, "(TESTING-LOCAL.md, part I)") {
		t.Error("fairytest-config.yaml does not point at part I")
	}
	if !strings.Contains(guide, "\n## I. Trying the check on the machine itself\n") {
		t.Error("part I is not the check on the machine itself any more")
	}
	for _, gone := range []string{"README-TEST-", "a new line that starts with\n   `TEST BUILD:", "may show its own\n   **Confirm (test build)** button"} {
		if strings.Contains(guide, gone) {
			t.Errorf("TESTING-LOCAL.md still has %q", gone)
		}
	}
	for _, want := range []string{
		"`TEST-ON-WINDOWS.md` in the agent folder", "level=WARN msg=\"!!! TEST BUILD: an approval was accepted without", "level=WARN msg=\"!!! TEST BUILD: approvals for the",
		"is on the **Updates** page", "It needs no passkey and no confirmation of any kind", "### I10. The machine id",
	} {
		if !strings.Contains(guide, want) {
			t.Errorf("TESTING-LOCAL.md lacks %q", want)
		}
	}
	for _, want := range []string{"before and after \"Switch on\"", "**Cancel** of a pending shutdown, restart or sleep needs no passkey and no \"Confirm it's you\"", "**Accept this machine's new id**"} {
		if !strings.Contains(readme, want) {
			t.Errorf("README.md lacks %q", want)
		}
	}
	// the folder the guide tells to unpack next to this one is the one this tree is built against
	for _, line := range strings.Split(read("../../go.mod"), "\n") {
		if _, dir, ok := strings.Cut(line, "=> ../"); ok && strings.HasPrefix(line, "replace github.com/n3ptune-cpu/Fairy-agent") {
			if dir = strings.TrimSpace(dir); !strings.Contains(guide, "`"+dir+"`") {
				t.Errorf("go.mod builds against ../%s, and TESTING-LOCAL.md does not name that folder", dir)
			}
		}
	}
}
