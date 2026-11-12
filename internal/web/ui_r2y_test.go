package web

import (
	"strings"
	"testing"
)

// The second bug run of audit 5 (r2-b1): what the page's script and style sheet must hold for each fix. The
// behaviour itself was run in a browser (reports/audit5-panel-fixes-r2y); these tests keep the lines in place.

// r2-b1-04: the browser's Back with a dialog open changed the page behind it and left the dialog ("Shut down
// ALPHA?") open and usable over another page. A dialog now belongs to the address it was built on, and a
// change of address closes it the way Cancel does.
func TestUIADialogClosesWhenTheAddressChanges(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`new MutationObserver(() => { modalAt = location.hash; }).observe($("#modal"), { childList: true });`,
		`if (!$("#modal").hidden && modalAt !== location.hash) closeModal();`,
		// closeModal is Cancel for "Confirm it's you" too: the kept action is dropped, never run
		`if (stepUpOpen) { cancelStepUp(); return; }`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	// the check runs in route(), before the new page is drawn
	i, j := strings.Index(js, "function route() {"), strings.Index(js, `modalAt !== location.hash) closeModal();`)
	if i < 0 || j < i || j-i > 400 {
		t.Error("route() does not close a dialog of another address")
	}
}
