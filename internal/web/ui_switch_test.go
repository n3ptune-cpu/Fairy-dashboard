package web

import (
	"regexp"
	"strings"
	"testing"
)

// The panel has one look for a switch: a pill with a knob, off on the left and on on the right. A box that is
// ticked (.check input) and a button that asks for a change (.switch) share its rules, so the two cannot drift
// apart. It keeps a visible focus ring, a dimmed look while it is disabled, and a .switch button can be hit a
// little outside of what is drawn. "On" has two colours (TestUISwitchColours).
func TestUIThePanelHasOneSwitch(t *testing.T) {
	b, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	for _, want := range []string{
		"\n.check input, .switch { appearance: none;",
		"box-sizing: border-box; width: 36px; height: 20px;",
		"border: 1px solid var(--line2); border-radius: 10px;",
		"\n.check input::after, .switch::after { content: \"\"; position: absolute; top: 2px; left: 2px; width: 14px; height: 14px; border-radius: 50%; background: var(--faint);",
		"\n.check input:checked, .switch[aria-checked=\"true\"] { background: var(--accent); border-color: var(--accent); }",
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

// "Green for power, blue for settings" (the owner, on the first pictures of the switch). A switch that is on is
// green, the green of "ok", where it switches power: the socket of a plug, and anything else that gives or
// takes a machine's power, which says so with the class "power". Every other switch that is on is blue, the
// blue the style sheet already has for links and for the focus ring (--accent): a setting, an option of a
// dialog, a row of a pick list. Off is the same for both: the knob left, on the dark track.
func TestUISwitchColours(t *testing.T) {
	b, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	for _, want := range []string{
		// on: blue, and green for power, which comes after it and has one class more
		"\n.check input:checked, .switch[aria-checked=\"true\"] { background: var(--accent); border-color: var(--accent); }\n" +
			".check.power input:checked, .switch.power[aria-checked=\"true\"] { background: var(--ok); border-color: var(--ok); }\n",
		// off, and the knob, are one rule for every switch
		"border: 1px solid var(--line2); border-radius: 10px; background: var(--well);",
		"\n.check input:checked::after, .switch[aria-checked=\"true\"]::after { left: 18px; background: var(--bg); }",
		// the blue is the one links have
		"\na { color: var(--accent); text-decoration: none; }",
	} {
		if strings.Count(css, want) != 1 {
			t.Errorf("app.css lacks (or has twice) %q", want)
		}
	}
	if n := len(regexp.MustCompile(`--accent:`).FindAllString(css, -1)); n != 1 {
		t.Errorf("the style sheet defines --accent %d times (a second blue?)", n)
	}
	// no rule gives a switch a colour of its own beside these (the look while the panel is not reached is grey)
	for _, m := range regexp.MustCompile(`(?m)^[^{}\n]*(\.switch|\.check)[^{}\n]*\{[^}]*background: (var\(--[a-z0-9]+\)|#[0-9a-fA-F]+)`).FindAllStringSubmatch(css, -1) {
		switch m[2] {
		case "var(--accent)", "var(--ok)", "var(--well)", "var(--faint)", "var(--bg)":
		default:
			t.Errorf("a switch with another colour: %s", m[0])
		}
	}
	js := appJS(t)
	// the socket of a plug is the page's one power switch, and it says so
	if n := strings.Count(js, `<button class="switch power${busy ? " busy" : ""}" type="button" role="switch"`); n != 1 {
		t.Errorf("the socket's switch does not have the class power (%d)", n)
	}
	if n := strings.Count(js, `role="switch"`); n != 1 {
		t.Errorf("places that draw a switch button: %d (a new one has to say whether it is power)", n)
	}
	// none of the page's tick boxes switches power: the sites a viewer sees, "Unread only", "Show pop-ups", the
	// options of Add host and of Edit host, and the pick lists of "Update unpowered hosts" and "Turn on the check"
	if regexp.MustCompile(`class="check[^"]* power`).MatchString(js) {
		t.Error("a tick box of the page has the class power")
	}
	for _, box := range []string{`data-act="u-site"`, `id="nf-unread"`, `id="pop-on"`, `data-opt="${k}"`, `data-eh-f="wol-on"`, `data-eh="${field}"`, `data-wu="${kind}"`, `id="wu-ifon"`, `data-ap `} {
		if !regexp.MustCompile(`<(label|span) class="check( opt| opt wu-ifon)?"><input type="checkbox" ` + regexp.QuoteMeta(box)).MatchString(js) {
			t.Errorf("the tick box %s is not a plain .check any more", box)
		}
	}
}
