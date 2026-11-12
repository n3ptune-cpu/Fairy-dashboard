package web

import (
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// The log at start says that the approval issuer must be https (audit 6, r4-p3-09), in the words of the
// dialog's "Not ready": fairy serve prints DeploymentWarnings, the test build prints ApprovalWarnings.
func TestApprovalWarningsAtStart(t *testing.T) {
	a := newApprovalEnv(t)
	if w := ApprovalWarnings(a.s.Cfg); len(w) != 0 {
		t.Fatalf("an https issuer: %q", w)
	}
	u := httpIssuer(t, a)
	w := ApprovalWarnings(a.s.Cfg)
	if len(w) != 1 || !strings.HasPrefix(w[0], "approval: the issuer must be https; it is "+u+". ") || !strings.Contains(w[0], "no machine is ready") {
		t.Fatalf("the warning at start: %q", w)
	}
	found := false
	for _, x := range DeploymentWarnings(a.s.Cfg, config.Secrets{}) {
		found = found || x == w[0]
	}
	if !found {
		t.Error("fairy serve does not print the warning at start")
	}
	a.s.Cfg.Approval = config.Approval{} // no approval: block at all: nothing to say about its issuer
	if w := ApprovalWarnings(a.s.Cfg); len(w) != 0 {
		t.Fatalf("no approval block: %q", w)
	}
}
