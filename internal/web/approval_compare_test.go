package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
)

func itemsOf(r resp) []any {
	d, _ := r.body["detail"].(map[string]any)
	l, _ := d["items"].([]any)
	return l
}

// The last thing an admin reads before a machine id comes on record says what to compare, and shows the id in
// the form the Approval box shows it (second review of passkey-g, G-02). The comparison was only behind the
// info button of the box: an admin who read the dialogs and did not open it accepted blind.
//
// For each of the three accepts of an id ("Accept this machine's id", "This host keeps its machine id",
// "Accept this machine's new id") "Confirm it's you" has the panel's own line, in full, under what would be
// accepted; the id is the first eight and the last six characters there, never the id in full; the audit log
// keeps the first six. The accept of "its check is off" has no id and no such line.
func TestTheAcceptDialogSaysWhatToCompare(t *testing.T) {
	const compare = `Compare it first with the machine's own: on the machine, fairy-agent helper-status, line "machine id:".`
	x := newAcceptAdmin(t)
	idL := machineIDOf("laptop")
	dialog := func(when string, r resp, want string, ids ...string) {
		t.Helper()
		got := actionOf(r)
		if r.code != 401 || pendingID(r) == "" || !strings.Contains(got, want) {
			t.Fatalf("%s: the dialog: %d %q, want %q", when, r.code, got, want)
		}
		if items := itemsOf(r); len(items) != 1 || items[0] != compare {
			t.Fatalf("%s: the dialog's own line: %v", when, items)
		}
		for _, id := range ids {
			if strings.Contains(string(r.raw), id) {
				t.Fatalf("%s: the dialog's answer has the id in full: %s", when, r.raw)
			}
		}
	}
	// 1. not on record
	x.setTrust(t, x.laptop)
	x.poll()
	r := x.press("laptop", map[string]any{"machine_id": idL})
	dialog("not on record", r, "it shows the machine id "+approval.CompareID(idL)+", which is not on record", idL)
	if res := x.confirm(t, r); res["ok"] != true {
		t.Fatalf("the accept: %v", res)
	}
	// 2. tower shows laptop's id: laptop keeps it
	x.setTrustID(t, x.tower, idL)
	x.poll()
	r = x.press("laptop", map[string]any{"machine_id": idL})
	dialog("keeps", r, "it keeps its machine id "+approval.CompareID(idL)+"; tower must get a new one", idL)
	if res := x.confirm(t, r); res["ok"] != true {
		t.Fatalf("the accept: %v", res)
	}
	// 3. a new id on laptop
	x.setTrustID(t, x.laptop, otherID)
	x.poll()
	r = x.press("laptop", map[string]any{"machine_id": otherID})
	dialog("a new id", r, "it shows the machine id "+approval.CompareID(otherID)+"; on record is "+approval.CompareID(idL), idL, otherID)
	if res := x.confirm(t, r); res["ok"] != true {
		t.Fatalf("the accept: %v", res)
	}
	// the form is the box's: eight, the ellipsis, six
	if got := approval.CompareID(otherID); got != otherID[:8]+"…"+otherID[len(otherID)-6:] {
		t.Fatalf("the form: %q", got)
	}
	// the audit log keeps the first six characters
	n := 0
	for _, e := range x.auditEntries("", "approval.accept") {
		if p, _ := e["params"].(map[string]any); e["outcome"] == "ok" {
			n++
			if id := fmt.Sprint(p["machine_id"]); id != approval.ShortID(idL) && id != approval.ShortID(otherID) {
				t.Fatalf("the audit log's id: %v", e)
			}
		}
	}
	if n != 3 {
		t.Fatalf("%d accepts in the audit log, want 3", n)
	}
	// 4. the check is gone: nothing to compare
	x.clearTrust(t, x.laptop)
	x.poll()
	r = x.press("laptop", map[string]any{"off": true})
	if got := actionOf(r); r.code != 401 || !strings.Contains(got, "it shows no check any more") || itemsOf(r) != nil {
		t.Fatalf("the dialog for off: %d %q %v", r.code, got, itemsOf(r))
	}
	// the page shows the lines of the dialog, and makes the same form of an id in the box
	js := appJS(t)
	for _, want := range []string{
		"const items = kept && Array.isArray(detail.items) && detail.items.length ? `<ul class=\"su-items\" id=\"su-items\">${detail.items.map((x) => `<li>${esc(x)}</li>`).join(\"\")}</ul>` : \"\";",
		"const shortKey = (t) => (typeof t === \"string\" && t.length > 16 ? `${t.slice(0, 8)}…${t.slice(-6)}` : String(t ?? \"\"));",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	// the line can never be cut: it is shorter than what a line of the dialog may be, and names no host
	if len(acceptCompare) >= maxTarget || acceptCompare != compare {
		t.Fatalf("the line: %d bytes, %q", len(acceptCompare), acceptCompare)
	}
}
