package web

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The Sessions tables never scroll sideways. Their columns need about 920px, and next to the open sidebar a
// window has that only from about 1240px on, so the layout with one block per session (made for phones) is
// used below that: it used to start at 760px, and between the two the table was cut off and its last column
// squeezed. In the table, the values that can be long wrap, and the note in the last column has a fixed width.
// (The widths themselves are looked at in a browser: reports/part1-polish-logs.)
func TestSessionsTablesFitTheirWindow(t *testing.T) {
	raw, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(raw)
	// the one block of rules that stacks the table, and the width it starts at
	blocks := regexp.MustCompile(`@media \(max-width: (\d+)px\) \{\n  \.table\.sessions, \.table\.sessions tbody \{ display: block; \}`).FindAllStringSubmatch(css, -1)
	if len(blocks) != 1 {
		t.Fatalf("the stacked layout of the sessions table: %d blocks", len(blocks))
	}
	// the sidebar and the page's padding take 322px; the "everyone" table's columns that cannot shrink take
	// 661px plus the browser line's 20% of the window: it fits from 1229px on
	if at, _ := strconv.Atoi(blocks[0][1]); at < 1229 {
		t.Errorf("the table layout starts at %dpx: too narrow for its columns next to the open sidebar", at+1)
	}
	from := strings.Index(css, blocks[0][0])
	stacked := css[from : from+strings.Index(css[from:], "\n}\n")]
	for _, want := range []string{
		".table.sessions thead { display: none; }",
		".table.sessions tr.row { display: block;",
		".table.sessions td { display: block;",
		".table.sessions td[data-l]:not(.namecell)::before { content: attr(data-l);",
		".table.sessions td.act .note { width: auto; text-align: left; }",
		".table.sessions td.act .btn { width: 100%; max-width: 420px; justify-content: center; }", // not a button as wide as a desktop window
	} {
		if !strings.Contains(stacked, want) {
			t.Errorf("the stacked layout lacks %q", want)
		}
	}
	for _, want := range []string{
		// the note "Sign out is in the menu": two lines, whatever room the table has
		".table.sessions td.act .note { display: inline-block; width: 13ch; white-space: normal; text-align: right; }",
		`.table.sessions td[data-l="IP address"] { min-width: 15ch; overflow-wrap: anywhere; }`,
		".table.sessions .namecell .sub:not(.ua) { overflow-wrap: anywhere; }",
		".table.sessions .namecell .sub.ua { max-width: min(64ch, 30vw); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css lacks %q", want)
		}
	}
	// the cell those rules are for
	if js := appJS(t); !strings.Contains(js, `<td data-l="IP address">`) {
		t.Error(`app.js: the address cell is no longer <td data-l="IP address">`)
	}
}
