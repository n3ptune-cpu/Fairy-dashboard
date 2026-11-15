package web

import (
	"regexp"
	"strings"
	"testing"
)

// The panel has one look for a switch: a pill with a knob, off on the left and on (green) on the right. A box
// that is ticked (.check input) and a button that asks for a change (.switch) share its rules, so the two
// cannot drift apart. It keeps a visible focus ring, a dimmed look while it is disabled, and a .switch button
// can be hit a little outside of what is drawn.
func TestUIThePanelHasOneSwitch(t *testing.T) {
	b, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	for _, want := range []string{
		"\n.check input, .switch { appearance: none;",
		" width: 36px; height: 20px;",
		"border-radius: 10px;",
		"\n.check input::after, .switch::after { content: \"\"; position: absolute; top: 2px; left: 2px; width: 14px; height: 14px; border-radius: 50%; background: var(--faint);",
		"\n.check input:checked, .switch[aria-checked=\"true\"] { background: var(--ok); border-color: var(--ok); }",
		"\n.check input:checked::after, .switch[aria-checked=\"true\"]::after { left: 18px; background: var(--bg); }",
		"\n.check input:focus-visible, .switch:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }",
		"\n.check input:disabled, .switch:disabled { opacity: .45; cursor: default; }",
		"\n.switch::before { content: \"\"; position: absolute; inset: -9px -6px; }",
	} {
		if strings.Count(css, want) != 1 {
			t.Errorf("app.css lacks (or has twice) %q", want)
		}
	}
	// no second look for a tick box is left over, and none is made beside the shared one
	if regexp.MustCompile(`\.check input[^{,]*\{[^}]*(width: 14px; height: 14px; margin|rotate\(-45deg\))`).MatchString(css) {
		t.Error("app.css still has the small square box")
	}
	// every tick box of the page is in a .check, so it has the look (the one exception is named here)
	js := appJS(t)
	boxes := regexp.MustCompile(`<input type="checkbox"`).FindAllStringIndex(js, -1)
	in := len(regexp.MustCompile(`<(label|span) class="check[^"]*"><input type="checkbox"`).FindAllString(js, -1))
	if len(boxes) != 10 || in != 9 || strings.Count(js, `<label class="note"><input type="checkbox" id="td-renamed">`) != 1 {
		t.Errorf("tick boxes: %d, of them in a .check: %d (want 10 and 9: the box of Re-pair's dialog is the browser's own)", len(boxes), in)
	}
}
