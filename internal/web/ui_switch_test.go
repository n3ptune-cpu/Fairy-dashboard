package web

import (
	"regexp"
	"strings"
	"testing"
)

// The panel has one look for a switch: a pill with a knob, off on the left and on on the right. A box that is
// ticked (.check input) and a button that asks for a change (.switch) share its rules, so the two cannot drift
// apart. It keeps a visible focus ring, a dimmed look while it is disabled, and both can be hit a little outside
// of what is drawn. "On" has two colours (TestUISwitchColours); its size is TestUISwitchMeasuresAreWholePixels.
func TestUIThePanelHasOneSwitch(t *testing.T) {
	b, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	for _, want := range []string{
		"\n.check input, .switch { appearance: none;",
		"box-sizing: border-box; width: 32px; height: 18px;",
		"border: 1px solid var(--line2); border-radius: 9px;",
		"\n.check input::after, .switch::after { content: \"\"; position: absolute; top: 2px; left: 2px; width: 12px; height: 12px; border-radius: 50%; background: var(--faint);",
		"\n.check input:checked, .switch[aria-checked=\"true\"] { background: var(--accent); border-color: var(--accent); }",
		"\n.check input:checked::after, .switch[aria-checked=\"true\"]::after { left: 16px; background: var(--bg); }",
		"\n.check input:focus-visible, .switch:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }",
		"\n.check input:disabled, .switch:disabled { opacity: .45; cursor: default; }",
		"\n.switch::before { content: \"\"; position: absolute; inset: -7px -8px; }",
		"\n.check input::before { content: \"\"; position: absolute; inset: -5px 0; }",
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
		"border: 1px solid var(--line2); border-radius: 9px; background: var(--well);",
		"\n.check input:checked::after, .switch[aria-checked=\"true\"]::after { left: 16px; background: var(--bg); }",
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

// "15% smaller" (the owner, on pictures of three sizes): the switch is 32 by 18 px with a knob of 12, where it
// was 36 by 20 with a knob of 14. Every measure of it is a whole pixel. The size of the pictures, 31 by 17, has
// halves (a gap of 1.5 px around the knob), and a browser at 100% puts the knob on a whole pixel: it then sits
// one pixel from the top of the track and two from the bottom. So the test does the sums: the knob has the same
// gap on every side, off and on; the spinner of a busy switch is in the middle; the lock that takes a switch's
// place is as large as the switch, with its sign in the middle; and what is drawn under 24 px high can be
// pressed on 24 px or more.
func TestUISwitchMeasuresAreWholePixels(t *testing.T) {
	css := appCSS(t)
	rule := func(start string) string {
		t.Helper()
		if strings.Count(css, "\n"+start) != 1 {
			t.Fatalf("app.css lacks (or has twice) the rule %q", start)
		}
		r := css[strings.Index(css, "\n"+start)+1:]
		return r[:strings.Index(r, "}")+1]
	}
	px := func(r, name string) int {
		t.Helper()
		m := regexp.MustCompile(`[{;] *` + regexp.QuoteMeta(name) + `: (-?[0-9.]+)px[; ]`).FindStringSubmatch(r)
		if m == nil {
			t.Fatalf("no %s in px in %q", name, r)
		}
		n := 0
		for _, c := range strings.TrimPrefix(m[1], "-") {
			if c < '0' || c > '9' {
				t.Fatalf("%s is %spx, not a whole pixel, in %q", name, m[1], r)
			}
			n = n*10 + int(c-'0')
		}
		return n
	}
	track, knob, on := rule(".check input, .switch {"), rule(".check input::after, .switch::after {"), rule(`.check input:checked::after, .switch[aria-checked="true"]::after {`)
	spin, lock, sign := rule(".switch.busy .spinner {"), rule(".info-btn.sock-lock {"), rule(".info-btn.sock-lock svg {")
	// no half anywhere in these rules, also where the sums below do not look
	for _, r := range []string{track, knob, on, spin, lock, sign, rule(".switch::before {"), rule(".check input::before {"), rule(".info-btn.sock-lock::before {")} {
		if m := regexp.MustCompile(`[0-9]*\.[0-9]+px`).FindString(r); m != "" {
			t.Errorf("a measure that is no whole pixel (%s) in %q", m, r)
		}
	}
	const border = 1
	if !strings.Contains(track, "border: 1px solid") || !strings.Contains(lock, "border: 1px dashed") || !strings.Contains(track, "box-sizing: border-box;") {
		t.Fatal("the sums below take a border of 1 px inside the size")
	}
	w, h, k, gap := px(track, "width"), px(track, "height"), px(knob, "width"), px(knob, "top")
	if w != 32 || h != 18 || k != 12 {
		t.Errorf("the switch is %d by %d px with a knob of %d, want 32 by 18 with 12", w, h, k)
	}
	if px(knob, "height") != k || px(knob, "left") != gap || h-2*border-k != 2*gap {
		t.Errorf("the knob (%d px, %d from the top and %d from the left) is not in the middle of a track %d px high", k, gap, px(knob, "left"), h)
	}
	if got, want := px(on, "left"), w-2*border-k-gap; got != want {
		t.Errorf("the knob of a switch that is on is at %d px, want %d (the same gap on the right)", got, want)
	}
	if r := px(track, "border-radius"); 2*r != h {
		t.Errorf("the track's radius is %d px for a height of %d", r, h)
	}
	// the spinner of a busy switch
	if s := px(spin, "width"); px(spin, "height") != s || 2*px(spin, "top") != h-2*border-s || 2*px(spin, "left") != w-2*border-s {
		t.Errorf("the spinner is not in the middle of the switch: %q", spin)
	}
	// the lock
	if px(lock, "width") != w || px(lock, "height") != h || 2*px(lock, "border-radius") != h || !strings.Contains(lock, "place-items: center;") || !strings.Contains(lock, "position: relative;") {
		t.Errorf("the lock is not the size of the switch: %q", lock)
	}
	if s := px(sign, "width"); px(sign, "height") != s || (w-2*border-s)%2 != 0 || (h-2*border-s)%2 != 0 {
		t.Errorf("the lock's sign (%d px) cannot be in the middle of %d by %d on whole pixels", s, w, h)
	}
	// the place to press: the inside of the border, and what the rule adds above and below it
	for _, r := range []string{rule(".switch::before {"), rule(".check input::before {"), rule(".info-btn.sock-lock::before {")} {
		m := regexp.MustCompile(`inset: -([0-9]+)px (0|-[0-9]+px); }`).FindStringSubmatch(r)
		if m == nil || !strings.Contains(r, `content: ""; position: absolute;`) {
			t.Errorf("no place to press in %q", r)
			continue
		}
		up := 0
		for _, c := range m[1] {
			up = up*10 + int(c-'0')
		}
		if high := h - 2*border + 2*up; high < 24 {
			t.Errorf("the place to press is %d px high in %q, want 24 or more", high, r)
		}
	}
}
