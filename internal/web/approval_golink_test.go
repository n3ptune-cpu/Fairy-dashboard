package web

import (
	"strings"
	"testing"
)

// The sign for a machine whose check is broken or flagged (the Overview's host list, and next to the host's
// name on its page) says "Details are on its page". The details were the Approval box there; they are the
// dialog behind the header's button now, so the link opens the page with that dialog open. On the page itself
// it only opens the dialog. A link opened in another tab opens the page alone, and nothing is opened later.
func TestUIApprovalSignLeadsToTheDialog(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		"<p>Details are on <a href=\"#/system/${enc(s.id)}\" data-ap-go=\"${esc(s.id)}\">its page</a>.</p>`, { cls: \"hflag bad\", icon: I.apKey,",
		`const a = e.target.closest("a[data-ap-go]");`,
		`if (!a || e.button || e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) return;`, // another tab or window: the browser's own doing
		`if (r.page === "system" && r.id === id) { e.preventDefault(); closeInfo(false); openApproval(id); }`,
		`else apGo = { id, at: Date.now() };`,
		`if (go && r.page === "system" && r.id === go.id && Date.now() - go.at < 3000) openApproval(go.id);`, // only for the page the link led to, and only right away
		"  window.scrollTo(0, 0);\n  approvalGo(r);\n}",                                                      // the last thing route() does: after it closed a dialog of the page that was left
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	// the wish is taken once, whatever page comes
	if !strings.Contains(js, "function approvalGo(r) {\n  const go = apGo;\n  apGo = null;\n") {
		t.Error("approvalGo must forget the link it was called for")
	}
}
