package web

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
)

// The id an admin accepts is shown in full (audit 6, r4-p3-02).
//
// Before an id comes on record the admin compares it with the machine's own (`fairy-agent helper-status`, line
// "machine id:"). The dialog showed the first eight and the last six characters of each id. An id that differs
// in the middle only looked the same on both lines ("it shows the machine id id-of-la…abcdef; on record is
// id-of-la…abcdef"), passed "Confirm it's you" and came on record. Now the dialog's lines give the id that
// would be accepted, and the one on record, in full, and what comes on record is exactly the id of that line.
func TestTheAcceptDialogShowsTheIDInFull(t *testing.T) {
	x := newAcceptAdmin(t)
	x.require(t, x.laptop)
	idL := machineIDOf("laptop")
	alike := idL[:8] + strings.Repeat("X", len(idL)-14) + idL[len(idL)-6:] // the same first eight and last six
	if alike == idL || approval.CompareID(alike) != approval.CompareID(idL) {
		t.Fatalf("the test's look-alike: %q %q", alike, idL)
	}
	x.setTrustID(t, x.laptop, alike)
	x.poll()
	r := x.press("laptop", map[string]any{"machine_id": alike})
	if r.code != 401 || pendingID(r) == "" {
		t.Fatalf("the dialog: %d %s", r.code, r.raw)
	}
	items := itemsOf(r)
	if len(items) != 3 || items[0] != "Machine id it shows: "+alike || items[1] != "Machine id on record: "+idL || items[2] != acceptCompare {
		t.Fatalf("the dialog's lines: %q", items)
	}
	if items[0] == strings.Replace(fmt.Sprint(items[1]), "on record", "it shows", 1) {
		t.Fatal("the two lines read the same")
	}
	if res := x.confirm(t, r); res["ok"] != true {
		t.Fatalf("the accept: %v", res)
	}
	if b := x.s.Poller.BindingOf("laptop"); b.Record.MachineID != alike {
		t.Fatalf("on record is not the id the dialog showed in full: %+v", b)
	}
	// an API token never gets these lines: the accept is for a browser session
	tok, _, err := x.s.Tokens.Create("script", approvalAdminSub, "control", "test", 0)
	if err != nil {
		t.Fatal(err)
	}
	x.setTrustID(t, x.laptop, idL)
	x.poll()
	if r := x.do("POST", "/api/admin/approval/accept/laptop", map[string]any{"machine_id": idL}, "Authorization", "Bearer "+tok, "Content-Type", "application/json"); r.code != 403 || strings.Contains(string(r.raw), idL) || strings.Contains(string(r.raw), alike) {
		t.Fatalf("a token: %d %s", r.code, r.raw)
	}
}

// The page: a line of the dialog that gives a machine id shows the id in a line of its own, monospace, that
// wraps and is selected with one click; every other line is as it was, and nothing in a line is taken as markup.
func TestUIAcceptDialogShowsAnIDLineInFull(t *testing.T) {
	js := appJS(t)
	css, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	rule := regexp.MustCompile(`(?m)^\.su-items li\.su-id code \{[^}]*\}`).FindString(string(css))
	for _, want := range []string{"display: block", "overflow-wrap: anywhere", "user-select: all"} {
		if !strings.Contains(rule, want) {
			t.Errorf("app.css: the id's line lacks %q: %q", want, rule)
		}
	}
	if !strings.Contains(string(css), `code { font-family: ui-monospace`) {
		t.Error("app.css: code is not monospace any more")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the page's own functions are not run here")
	}
	var parts []string
	for _, re := range []string{`(?m)^const SU_ID = .*$`, `(?m)^const suIds = .*$`} {
		m := regexp.MustCompile(re).FindString(js)
		if m == "" {
			t.Fatalf("app.js has no %s", re)
		}
		parts = append(parts, m)
	}
	script := `"use strict";
const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
` + strings.Join(parts, "\n") + `
const fail = (m) => { console.log("FAIL " + m); process.exitCode = 1; };
const list = (items) => suIds(` + "`<ul>${items.map((x) => `<li>${esc(x)}</li>`).join(\"\")}</ul>`" + `);
const id = "id-of-laptop-0123456789abcdef", long = "A".repeat(64);
const cases = [
  ["Machine id it shows: " + id, '<li class="su-id">Machine id it shows<code>' + id + "</code></li>"],
  ["Machine id on record: " + id, '<li class="su-id">Machine id on record<code>' + id + "</code></li>"],
  ["Machine id it keeps: " + long, '<li class="su-id">Machine id it keeps<code>' + long + "</code></li>"],
  // not an id line: as it was, escaped
  ["Compare it first with the machine's own: on the machine, fairy-agent helper-status, line \"machine id:\".", "<li>Compare it first with the machine&#39;s own: on the machine, fairy-agent helper-status, line &quot;machine id:&quot;.</li>"],
  ["PC-A: update of its agent (checked on the machine)", "<li>PC-A: update of its agent (checked on the machine)</li>"],
  ["Machine id it shows: <img src=x>", "<li>Machine id it shows: &lt;img src=x&gt;</li>"],
  ["Machine id <b>: " + id, "<li>Machine id &lt;b&gt;: " + id + "</li>"],
  ["x</li><li>Machine id it shows: " + id, "<li>x&lt;/li&gt;&lt;li&gt;Machine id it shows: " + id + "</li>"],
];
for (const [line, want] of cases) { const got = list([line]); if (got !== "<ul>" + want + "</ul>") fail(JSON.stringify(line) + " -> " + got); }
// three lines, as the accept of a new id has them
const three = list(["Machine id it shows: " + id, "Machine id on record: " + long, "Compare it first."]);
if ((three.match(/class="su-id"/g) || []).length !== 2 || !three.endsWith("<li>Compare it first.</li></ul>")) fail("three lines: " + three);
`
	file := filepath.Join(t.TempDir(), "suitem.js")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, file).CombinedOutput(); err != nil || strings.Contains(string(out), "FAIL") {
		t.Fatalf("%v\n%s", err, out)
	}
}
