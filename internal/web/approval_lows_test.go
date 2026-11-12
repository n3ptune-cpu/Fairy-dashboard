package web

import (
	"strings"
	"testing"
)

// r1-b2-06 (audit 5): in the Turn on dialog the list of people who may approve broke into lines that started
// with ";": the reason of each person is a .note, and .opts .opt .note is a block.
func TestTurnOnDialogKeepsEachApproverInOneLine(t *testing.T) {
	if css := appCSS(t); !strings.Contains(css, ".opts .opt .ap-users .note { display: inline; }") {
		t.Error("the reason of an approver is a block of its own again")
	}
}
