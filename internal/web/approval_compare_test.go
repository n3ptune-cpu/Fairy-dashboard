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
// accepted; the audit log keeps the first six characters of an id. The accept of "its check is off" has no id
// and no such line.
//
// Changed with audit 6, r4-p3-02: the dialog's lines now give the id that would be accepted IN FULL (and the
// one on record, when it is another one), above the line that says what to compare. This test held the
// opposite ("never the id in full"), which let a look-alike id pass: TestTheAcceptDialogShowsTheIDInFull.
func TestTheAcceptDialogSaysWhatToCompare(t *testing.T) {
	const compare = `Compare it first with the machine's own: on the machine, fairy-agent helper-status, line "machine id:".`
	x := newAcceptAdmin(t)
	idL := machineIDOf("laptop")
	dialog := func(when string, r resp, want string, lines ...string) {
		t.Helper()
		got := actionOf(r)
		if r.code != 401 || pendingID(r) == "" || !strings.Contains(got, want) {
			t.Fatalf("%s: the dialog: %d %q, want %q", when, r.code, got, want)
		}
		// (audit 6, r4-p3-02: the ids in full, then what to compare; it was the one line, and no id in full)
		if items := itemsOf(r); fmt.Sprint(items) != fmt.Sprint(append(lines, compare)) {
			t.Fatalf("%s: the dialog's own lines: %v", when, items)
		}
	}
	// 1. not on record
	x.setTrust(t, x.laptop)
	x.poll()
	r := x.press("laptop", map[string]any{"machine_id": idL})
	dialog("not on record", r, "it shows the machine id "+approval.CompareID(idL)+", which is not on record", "Machine id it shows: "+idL)
	if res := x.confirm(t, r); res["ok"] != true {
		t.Fatalf("the accept: %v", res)
	}
	// 2. tower shows laptop's id: laptop keeps it
	x.setTrustID(t, x.tower, idL)
	x.poll()
	r = x.press("laptop", map[string]any{"machine_id": idL})
	dialog("keeps", r, "it keeps its machine id "+approval.CompareID(idL)+"; tower must get a new one", "Machine id it keeps: "+idL)
	if res := x.confirm(t, r); res["ok"] != true {
		t.Fatalf("the accept: %v", res)
	}
	// 3. a new id on laptop
	x.setTrustID(t, x.laptop, otherID)
	x.poll()
	r = x.press("laptop", map[string]any{"machine_id": otherID})
	dialog("a new id", r, "it shows the machine id "+approval.CompareID(otherID)+"; on record is "+approval.CompareID(idL), "Machine id it shows: "+otherID, "Machine id on record: "+idL)
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
		"const items = kept && Array.isArray(detail.items) && detail.items.length ? suIds(`<ul class=\"su-items\" id=\"su-items\">${detail.items.map((x) => `<li>${esc(x)}</li>`).join(\"\")}</ul>`) : \"\";", // (r4-p3-02: suIds around it)
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

// A machine on record shows a NEW id that is not accepted yet (on the machine: --new-machine-id), and in that
// window a host without a record shows the same new id (second review of passkey-g, G-03; the reviewer's
// TestReviewG_ANewcomerShowsTheNewIDOfAMachineThatIsWaitingForItsAccept, pressed the way the page does). The
// dialog of the machine's own accept names the other host, as the other host's dialog names the machine. Who
// may accept is as it was: each has its button, the first accept decides, the other is refused afterwards.
func TestTheDialogOfANewIDNamesAHostThatShowsTheSameID(t *testing.T) {
	x := newAcceptAdmin(t)
	x.require(t, x.laptop)
	idL := machineIDOf("laptop")
	x.setTrustID(t, x.laptop, otherID) // on the machine: --new-machine-id
	x.poll()
	// alone: the sentence as it was
	r := x.press("laptop", map[string]any{"machine_id": otherID})
	alone := "(it shows the machine id " + approval.CompareID(otherID) + "; on record is " + approval.CompareID(idL) + ")"
	if got := actionOf(r); r.code != 401 || !strings.HasSuffix(got, alone) {
		t.Fatalf("the dialog while no other host shows the id: %d %q", r.code, got)
	}
	x.setTrustID(t, x.tower, otherID) // the newcomer shows it too
	x.poll()
	_, ms := x.page(t)
	// (laptop's box says "One machine id on two hosts" and names tower; its button is "Accept this machine's new id")
	if m := ms["laptop"]; m["binding"] != approval.BindDuplicate || m["can_accept"] != true || m["accept"] != "id" || m["held_by"] != nil || fmt.Sprint(m["with"]) != "[tower]" {
		t.Fatalf("laptop on the approval page: %v", m)
	}
	if m := ms["tower"]; m["binding"] != approval.BindNoRecord || m["can_accept"] != true || m["accept"] != "first" || m["held_by"] != nil {
		t.Fatalf("tower on the approval page: %v", m)
	}
	// tower's dialog named laptop before; laptop's now names tower
	r = x.press("tower", map[string]any{"machine_id": otherID})
	if got := actionOf(r); r.code != 401 || !strings.Contains(got, "which is not on record; laptop shows the same id and must get a new one") {
		t.Fatalf("the dialog for tower: %d %q", r.code, got)
	}
	r = x.press("laptop", map[string]any{"machine_id": otherID})
	want := "(it shows the machine id " + approval.CompareID(otherID) + "; on record is " + approval.CompareID(idL) + "; tower shows the same id)"
	if got := actionOf(r); r.code != 401 || !strings.HasSuffix(got, want) || len(itemsOf(r)) != 3 { // (r4-p3-02: the two ids in full and the line; it was the line alone)
		t.Fatalf("the dialog for laptop: %d %q %v", r.code, got, itemsOf(r))
	}
	// the admin presses the right one: the machine that has a record
	res := x.confirm(t, r)
	if res["ok"] != true || res["message"] != "laptop has the machine id on record now, and keeps it. tower stays flagged until it shows an id of its own (on it: fairy-agent helper-trust set --new-machine-id)." {
		t.Fatalf("the result: %v", res)
	}
	if b := x.s.Poller.BindingOf("laptop"); !b.OK() || !b.Record.Holder || b.Record.MachineID != otherID {
		t.Fatalf("laptop: %+v", b)
	}
	if r := x.press("tower", map[string]any{"machine_id": otherID}); r.code != 409 || pendingID(r) != "" || !strings.Contains(fmt.Sprint(r.body["detail"]), "kept by laptop") {
		t.Fatalf("tower after laptop was accepted: %d %s", r.code, r.raw)
	}
	if _, ms := x.page(t); ms["tower"]["can_accept"] != nil || fmt.Sprint(ms["tower"]["held_by"]) != "[laptop]" {
		t.Fatalf("tower on the approval page afterwards: %v", ms["tower"])
	}
}
