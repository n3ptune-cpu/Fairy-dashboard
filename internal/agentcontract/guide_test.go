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
