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
		`setTimeout(() => $("#ap-x")?.focus(), 0);`,
		`if (appr) setTimeout(() => $("#su-cancel")?.focus(), 0);`,
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
