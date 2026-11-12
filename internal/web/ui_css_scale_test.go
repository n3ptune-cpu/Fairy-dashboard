package web

import (
	"regexp"
	"strings"
	"testing"
)

// Found during the integration (part 1, "Noticed", 4): CSS that came in with the later branches did not go
// through the pass that settled the stylesheet's scale. It had again rules with font-weight 700 (the page's
// bold is 600), with 11.5px (not a size of the scale) and two raw near-black colours that have a variable.
func TestUIStylesheetKeepsToItsScale(t *testing.T) {
	b, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	root, rest, ok := strings.Cut(css, "\n}\n") // :root, where the colours are named
	if !ok || !strings.Contains(root, "--rule: #121212;") || !strings.Contains(root, "--press: #161616;") {
		t.Fatal("app.css does not start with the :root block that names --rule and --press")
	}
	for _, re := range []string{`font-weight: ?(700|800|900|bold)`, `font-size: ?11\.5px`, `#121212`, `#1c1c1c`} {
		if m := regexp.MustCompile(re).FindAllString(rest, -1); len(m) != 0 {
			t.Errorf("app.css has %d × %s outside :root", len(m), m[0])
		}
	}
	for _, want := range []string{
		`.info-btn:hover, .info-btn[aria-expanded="true"] { color: var(--text); background: var(--press); }`,
		`.info-pop .info-t { color: var(--text); font-weight: 600; font-size: 13px;`,
		`font-size: 12px; line-height: 1.15; }`, // the model line under a host's name: 13.8px high, as before
		`border-bottom: 1px solid var(--rule); }`,
		`content: attr(data-l); color: var(--faint); font-size: 12px; font-weight: 600; }`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css lacks %q", want)
		}
	}
}
