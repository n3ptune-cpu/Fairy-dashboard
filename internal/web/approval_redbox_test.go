package web

import (
	"strings"
	"testing"
)

// The inner box of the Approval dialog is red where the state is red (audit 6, r4-b1-09): the states whose
// header button and line under the header are red ("Machine id changed", "Check gone", …, and a broken trust
// file) had the amber box of an ordinary warning inside the dialog. It has the colours of the red line now,
// which are the colours of .alert-strip.bad: no new colour. The amber box stays for what is amber.
func TestUIApprovalRedBox(t *testing.T) {
	js, css := appJS(t), appCSS(t)
	const red = "{ border-color: rgba(255, 93, 108, .6); background: rgba(255, 93, 108, .13); color: #ffc9ce; }"
	if !strings.Contains(css, ".approval .warnbox.bad "+red) || !strings.Contains(css, ".pending-strip.bad "+red) {
		t.Error("app.css: the red box of the dialog has not the colours of the red line")
	}
	for _, want := range []string{
		`<div class="warnbox bad ap-bind" id="ap-bind">`,                                    // every state of "which machine it is"
		`<div class="${a.state === "broken" ? "warnbox bad" : "note warn"}" id="ap-error">`, // broken; "unknown" stays amber
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	if strings.Contains(js, `class="warnbox ap-bind"`) {
		t.Error("app.js: the box of a red state is the amber one")
	}
}
