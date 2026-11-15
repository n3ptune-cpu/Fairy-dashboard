package web

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// "What is confirmed is what is saved" holds for every line of a request, however many it has. "Confirm it's
// you" lists 32 lines and says how many more there are; the stamp of the kept request is made from all of
// them in full. A request of "Edit host" with 41 lines of changes (the name, then 40 plugs that four machines
// relay) and the notes about who sees the plugs after them:
//
//   - with nothing in between it is saved, and the audit entry has all 41 lines and the notes;
//   - when line 35 is no longer true (another admin linked that plug to another machine), nothing is saved;
//   - when line 41, the last, is no longer true, nothing is saved.
func TestEditHostComparesTheLinesTheDialogDoesNotList(t *testing.T) {
	readers := []string{"relay-a", "relay-b", "box-c", "box-d"}
	const each = 10
	plug := func(n int) string { return fmt.Sprintf("%s/p%02d", readers[n/each], n) } // the plug of line n+2
	more := 0                                                                         // how many lines the dialog says it does not list
	start := func(t *testing.T) (r *hedRig, id string) {
		t.Helper()
		b := hedBackend()
		for k, reader := range readers {
			b.Metrics[reader] = func() (jsonx.M, error) {
				m := testutil.AgentMetrics()
				m["hostname"] = reader
				var src []any
				for n := k * each; n < (k+1)*each; n++ {
					src = append(src, jsonx.M{"id": fmt.Sprintf("p%02d", n), "name": fmt.Sprintf("Plug %02d", n), "type": "homeassistant", "ok": true, "watts": 10.0})
				}
				m["power_sources"] = src
				return m, nil
			}
		}
		r = hedStart(t, hedConfig(t), b)
		var list []any
		for n := 0; n < len(readers)*each; n++ {
			list = append(list, map[string]any{"id": plug(n), "linked": true})
		}
		p := r.do("PUT", "/api/admin/hosts/pc-b/options", map[string]any{"plugs": list, "name": "Den"}, "Content-Type", "application/json", "Cookie", r.a.sess, "X-Requested-With", "fairy")
		d, _ := p.body["detail"].(map[string]any)
		items := strs(d["items"])
		if !stepUpRequired(p) || len(items) != maxActionItems+1 || items[0] != "Name: Den (was PC-B)" || !strings.Contains(items[1], "Plug 00") || !strings.Contains(items[maxActionItems-1], "Plug 30") {
			t.Fatalf("the confirmation of a request with 41 lines: %d %q", p.code, items)
		}
		if n, err := fmt.Sscanf(items[maxActionItems], "and %d more lines that are not listed here", &more); n != 1 || err != nil || more < 9 ||
			items[maxActionItems] != fmt.Sprintf("and %d more lines that are not listed here", more) {
			t.Fatalf("the line after the %d the dialog lists: %q", maxActionItems, items[maxActionItems])
		}
		return r, pendingID(p)
	}
	confirm := func(t *testing.T, r *hedRig, id string) map[string]any {
		t.Helper()
		if cb := r.a.confirm(id, false); cb.code != 303 {
			t.Fatalf("the confirmation: %d %s", cb.code, cb.raw)
		}
		return r.a.rtResult()
	}
	linked := func(r *hedRig) (n int) {
		for _, x := range r.options("pc-b").body["plugs"].([]any) {
			if e, _ := x.(map[string]any); e["linked"] == true {
				n++
			}
		}
		return n
	}

	// nothing in between: saved, every line on record
	r, id := start(t)
	res := confirm(t, r, id)
	_, params := r.lastChange("pc-b")
	lines := strs(params["lines"])
	if res["ok"] != true || linked(r) != 40 || r.row("pc-b")["name"] != "Den" || len(lines) != 41 || len(lines)+len(strs(params["visibility"])) != maxActionItems+more || lines[0] != "Name: Den (was PC-B)" || !strings.Contains(lines[34], "Plug 33") || !strings.Contains(lines[40], "Plug 39") {
		t.Fatalf("a request with 41 lines and nothing in between: %v, %d plugs linked, %d lines and %d notes on record (the dialog: %d and %d more)",
			res, linked(r), len(lines), len(strs(params["visibility"])), maxActionItems, more)
	}

	// line 35 or line 41 is no longer true: that plug is another machine's by then
	for _, line := range []int{35, 41} {
		r, id = start(t)
		r.ok("pc-a", map[string]any{"plugs": []any{map[string]any{"id": plug(line - 2), "linked": true}}})
		if res := confirm(t, r, id); !hoRefused(res) || linked(r) != 0 || r.row("pc-b")["name"] != "PC-B" {
			t.Errorf("line %d of the confirmation was no longer true: %v, %d plugs linked, called %v", line, res, linked(r), r.row("pc-b")["name"])
		}
		e, params := r.lastChange("pc-b")
		if confirmed := strs(params["confirmed"]); !strings.HasPrefix(fmt.Sprint(e["outcome"]), "error 409: the host was changed after the confirmation") ||
			len(confirmed) != maxActionItems+1 || confirmed[maxActionItems] != fmt.Sprintf("and %d more lines that are not listed here", more) {
			t.Errorf("line %d: the audit entry of the refusal: %v", line, e)
		}
	}
}

// The stamp of a list of lines: all of them, in full. Lists that differ in a line the dialog does not list,
// or after the part of a line it shows, have different stamps; the same list has the same stamp; and where a
// line ends is part of it.
func TestTheStampOfAConfirmationIsOfEveryLineInFull(t *testing.T) {
	var a []string
	for i := 0; i < 39; i++ {
		a = append(a, fmt.Sprintf("Plug p%d (read by NAS): in the total again", i))
	}
	b := slices.Clone(a)
	a = append(a, "Counts as off after: back to Run timings: 900 s = 15 minutes (was 1800 s = 30 minutes)")
	b = append(b, "Counts as off after: back to Run timings: 300 s = 5 minutes (was 1800 s = 30 minutes)")
	if rtShown(shownItems(a)) == rtShown(shownItems(b)) || rtShown(a) != rtShown(slices.Clone(a)) {
		t.Error("two lists that differ in line 40 have one stamp, or one list has two")
	}
	if !slices.Equal(dialogItems(a), dialogItems(b)) || len(dialogItems(a)) != maxActionItems+1 || dialogItems(a)[maxActionItems] != "and 8 more lines that are not listed here" {
		t.Errorf("the dialog's list of 40 lines: %q", dialogItems(a))
	}
	if got := dialogItems(a[:maxActionItems+1]); got[maxActionItems] != "and 1 more line that is not listed here" {
		t.Errorf("one line more than the dialog lists: %q", got[maxActionItems])
	}
	if got := dialogItems(a[:maxActionItems]); len(got) != maxActionItems || !slices.Equal(got, a[:maxActionItems]) {
		t.Errorf("as many lines as the dialog lists: %q", got)
	}
	long := "Name: " + strings.Repeat("x", 250)
	if rtShown([]string{long + " (was A)"}) == rtShown([]string{long + " (was B)"}) || !slices.Equal(dialogItems([]string{long + " (was A)"}), dialogItems([]string{long + " (was B)"})) {
		t.Error("two lines that differ after the part the dialog shows have one stamp")
	}
	for _, pair := range [][2][]string{{{"a", "b"}, {"a\nb"}}, {{"a", ""}, {"a"}}, {{}, {""}}, {{"1:a"}, {"a"}}} {
		if rtShown(pair[0]) == rtShown(pair[1]) {
			t.Errorf("%q and %q have one stamp", pair[0], pair[1])
		}
	}
	// the lines of a refusal's audit entry are the ones the dialog listed
	if got := confirmedLines(rtShown(a)); !slices.Equal(got, dialogItems(a)) {
		t.Errorf("confirmedLines: %q", got)
	}
	if got := confirmedLines(rtShown(nil)); got == nil || len(got) != 0 {
		t.Errorf("confirmedLines of no lines: %#v", got)
	}
}

// The README says it, with the line as the dialog has it.
func TestREADMESaysALongConfirmationIsComparedWhole(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for i := 0; i < maxActionItems+9; i++ {
		lines = append(lines, "x")
	}
	want := fmt.Sprintf("the confirmation lists %d lines and says how many more there are (%q), and every line counts.", maxActionItems, dialogItems(lines)[maxActionItems])
	if !strings.Contains(strings.Join(strings.Fields(string(b)), " "), want) {
		t.Errorf("the README lacks %q", want)
	}
}
