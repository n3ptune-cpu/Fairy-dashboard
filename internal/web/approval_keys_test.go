package web

import (
	"strings"
	"testing"
)

// "Turn on" opened from the Approval dialog: Escape does what Cancel does (audit 6, r4-b1-10). Cancel led back
// to the Approval dialog and Escape closed everything. And the focus of "Turn on" and of its "Confirm it's
// you" starts on Cancel: it started on the first control, which is the info button of "Client ID" in both.
// What the keys really do is checked in the browser runs (the approval run: Escape, and the focus of both).
func TestUITurnOnEscapeIsCancel(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`$("#ap-x").onclick = back ? () => openApproval(back) : closeModal;`, // Cancel, as it was
		`$("#ap-form").dataset.back = back || "";`,
		// Escape: the same button, and only where Cancel is not a plain close; an info box's Escape comes first
		`if (e.key !== "Escape" || e.cancelBubble || $("#modal").hidden || !$("#ap-form")?.dataset.back) return;`,
		`e.stopPropagation(); $("#ap-x").click();`,
		`focusAtTop("#ap-x");`, `if (appr) focusAtTop("#su-cancel");`, // (r4-p3-04: without scrolling the dialog; they were setTimeout(() => $(…)?.focus(), 0))
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	// the handler stands before the one that closes every dialog, and takes the key in the capture phase
	mine := strings.Index(js, `!$("#ap-form")?.dataset.back) return;`)
	all := strings.Index(js, `document.addEventListener("keydown", (e) => { if (e.key === "Escape") { state.menu = null; if (!$("#modal").hidden) closeModal(); render(); } });`)
	info := strings.Index(js, `document.addEventListener("keydown", (e) => { if (e.key === "Escape" && info.key) { e.stopPropagation(); closeInfo(true); } }, true);`)
	if mine < 0 || all < 0 || info < 0 || !(info < mine && mine < all) {
		t.Fatalf("the order of the Escape handlers: info %d, Turn on %d, every dialog %d", info, mine, all)
	}
	if end := strings.Index(js[mine:], "\n}, true);"); end < 0 || end > 200 {
		t.Error("the Escape of Turn on is not a capture listener any more")
	}
}

// The kill switch's tooltip names the command as the strip on the same page does (audit 6, r4-b1-12): it said
// "admin control enable", which is not a command; the program is "fairy".
func TestUIKillSwitchTooltipNamesTheWholeCommand(t *testing.T) {
	js := appJS(t)
	if !strings.Contains(js, "Re-enable on the server: fairy admin control enable`") || strings.Contains(js, "server: admin control enable") {
		t.Error("app.js: the tooltip of a disabled control does not say `fairy admin control enable`")
	}
	if n := strings.Count(js, "admin control enable"); n != strings.Count(js, "fairy admin control enable") {
		t.Errorf("app.js names the command without its program somewhere (%d places in all)", n)
	}
}

// The Approval dialog opens at its top (audit 6, r4-p3-04). The focus is put on Close when it opens; in a window
// lower than the dialog (360x640, 390x664) the browser scrolled Close into view, so the title, the state and
// the red sentence were out of sight. The focus is set without scrolling now and the dialog's own scroll is
// put to 0; the same for "Turn on" and for "Confirm it's you" of a machine's approval. Measured in the browser
// runs at 360x640 and 390x664 (scrollTop 0, the title in sight), with pictures.
func TestUIApprovalDialogOpensAtItsTop(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`function focusAtTop(sel) {`, `b.focus({ preventScroll: true });`, `if (box) box.scrollTop = 0;`,
		`focusAtTop("#apd-x");`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	// no focus that scrolls is left on the buttons of these three dialogs
	for _, old := range []string{`$("#apd-x")?.focus()`, `$("#ap-x")?.focus()`, `$("#su-cancel")?.focus()`} {
		if strings.Contains(js, old) {
			t.Errorf("app.js still has %s, which scrolls the dialog to its end", old)
		}
	}
	// the dialog's box is what scrolls: it is the one with a height limit
	if css := appCSS(t); !strings.Contains(css, ".modal-box.wide { max-width: 640px; max-height: calc(100vh - 32px); overflow-y: auto; }") ||
		!strings.Contains(js, `<div class="modal-box wide approval" role="dialog"`) {
		t.Error("the Approval dialog is not the box that scrolls any more: focusAtTop sets the scroll of .modal-box")
	}
}
