package web

import (
	"strings"
	"testing"
)

// At 390 px two tables crushed the column that is read (audit 6, r4-b1-05): in the audit log "Action" and
// "Target" were 54 and 53 px wide while "Result" took 234 px, so "approval.turn_on" broke into "appr / oval /
// .tur / n_on"; on the Notifications page "What happened" was 65 to 81 px, one or two words per line. The
// columns have a minimum width now; both tables scroll sideways inside their box as they did, and in a wide
// window nothing changes (the columns are wider than the minimum there). Measured in the browser runs at
// 390 px, with pictures.
//
// Left: the two tables of the Updates page, whose button column is fixed at the right edge on a phone (audit 5,
// r1-b1-08) and so lies over the column next to it. CSS alone cannot show both in 320 px.
func TestUINarrowTablesKeepAWordsWidth(t *testing.T) {
	css := appCSS(t)
	for _, want := range []string{
		".table.audit td:nth-child(3), .table.audit th:nth-child(3) { min-width: 17ch; }", // "approval.turn_on" is 16 letters
		".table.audit td:nth-child(4), .table.audit th:nth-child(4) { min-width: 12ch; }",
		".table.notes td:nth-child(4), .table.notes th:nth-child(4) { min-width: 30ch; }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css lacks %q", want)
		}
	}
	// the columns these rules count on
	js := appJS(t)
	for _, want := range []string{
		`<table class="table audit"><thead><tr><th>When</th><th>Who</th><th>Action</th><th>Target</th><th>Result</th>`,
		`<table class="table notes"><thead><tr><th>When</th><th>Severity</th><th>Host</th><th>What happened</th><th>Type</th>`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js: the columns changed, the minimum widths in app.css go by their number: %q", want)
		}
	}
}
