package web

import (
	"strings"
	"testing"
)

// r3-p2-06, the page's side: with the kill switch on the page disabled its own Cancel buttons (the countdown
// line's, and "Cancel pending" in the Power menu) before the server was ever asked. The server lets the cancel
// of a countdown through the kill switch now, so the page offers it to an admin: for the line always, in the
// menu when the server says that it is available. A viewer's stays disabled, as "Cancel run" does it.
func TestUICancelIsOfferedWithTheKillSwitchOn(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`if (state.me?.can_control) why = "";`,
		`${a.available && (!why || (a.action === "cancel" && state.me?.can_control)) ? "" : "disabled"}`,
		// "Cancel run" was already offered by the role alone
		`data-act="wu-cancel" data-id="${esc(r.id)}" ${state.me?.can_control ? "" : "disabled"}`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	// in the countdown line the kill switch is forgotten before the button is built, and only there
	i := strings.Index(js, "function pendingStrip(s, why) {")
	fn := js[i : i+strings.Index(js[i:], "\n}\n")]
	if clear, button := strings.Index(fn, `if (state.me?.can_control) why = "";`), strings.Index(fn, `data-action="cancel"`); clear < 0 || button < clear {
		t.Error("the countdown line's Cancel is built before the kill switch is set aside")
	}
	if strings.Count(js, `if (state.me?.can_control) why = "";`) != 1 {
		t.Error("the kill switch is set aside in more than one place")
	}
}
