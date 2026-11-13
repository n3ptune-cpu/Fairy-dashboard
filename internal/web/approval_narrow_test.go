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
		".table.audit td:nth-child(3) > code { white-space: nowrap; }", // the name itself is never broken (the browser run: "wakeupdate.switch" still was)
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
		"<td><code>${esc(e.action)}</code>${auditDetails(e)}</td>", // the action's name is the cell's first child, a code element
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js: the columns changed, the minimum widths in app.css go by their number: %q", want)
		}
	}
}

// A host name of 100 characters on the Overview at 1440 px (audit 6, r4-b1-06): the name was 828 px in one line
// and pushed "RAM" half and "GPU" and "Wall power" wholly out of the panel, for every host. In the table it is
// cut with "…" now, in the room the other columns leave; the narrow layout (where it wraps) is untouched, and
// the tooltip of a long name starts with the whole name. Measured in the browser runs at 1440 px, with pictures.
func TestUIOverviewLongHostNameIsCut(t *testing.T) {
	css, js := appCSS(t), appJS(t)
	i := strings.Index(css, "@container hosts (min-width: 940.1px) {")
	j := strings.Index(css, "@container hosts (max-width: 940px) {")
	if i < 0 || j < i {
		t.Fatal("app.css: the rule for the table layout is not in front of the narrow layout's")
	}
	block := css[i:j]
	for _, want := range []string{
		".table.hosts .host-l1 > a { display: block; flex: 0 1 auto; min-width: 0; max-width: max(28ch, calc(100cqw - 860px)); overflow: hidden; text-overflow: ellipsis;",
		".table.hosts .host-l1 > a > .dot { margin-right: 7px;",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("app.css lacks %q in the table layout's block", want)
		}
	}
	// the name stays in one line there (the rule this one builds on), and wraps in the narrow layout as before
	for _, want := range []string{
		".table.hosts .host-l1 > a { flex: none; white-space: nowrap; gap: 7px; }",
		".table.hosts .host-l1 > a { flex: 0 1 auto; white-space: normal; min-width: 0; overflow-wrap: anywhere; }",
		".hosts-panel { container: hosts / inline-size; }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css lacks %q", want)
		}
	}
	if !strings.Contains(js, "const about = [String(s.name || \"\").length > 24 && s.name, subLine(s),") || !strings.Contains(js, `title="${esc(about)}"`) {
		t.Error("app.js: the tooltip of a long name does not start with the whole name")
	}
}
