package web

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
)

// "This host keeps its machine id" ends the duplicate for the host it is pressed on, whatever the other host
// has on record (second review of passkey-g, G-01; the reviewer's sequence, pressed the way the page does).
//
// laptop was once made the keeper of an id it shared with tower. tower has an id of its own since. Now laptop
// shows tower's id (it answers in tower's name). tower is flagged, once; the admin says that tower keeps its
// id, and from then on tower is approved again. Before the fix the admin's word for tower did not count,
// because laptop "keeps" an id too, though ANOTHER one: the button answered "tower keeps the machine id" and
// tower stayed flagged, at every press and every poll.
//
// The other direction holds before and after: laptop shows an id that tower has on record and keeps, so laptop
// has no button, its accept is refused, and nothing is sent to it.
func TestAHostThatKeepsAnotherIDCannotFlagAHolderAgain(t *testing.T) {
	const shared = "shared-machine-id-0123456789abc"
	x := newAcceptAdmin(t)
	// two machines made from one disk, both set on the machine: both not on record, the first accept decides
	x.setTrustID(t, x.laptop, shared)
	x.setTrustID(t, x.tower, shared)
	x.poll()
	if what, _, err := x.accept("laptop", "nora"); err != nil || what != "id" {
		t.Fatal(what, err)
	}
	if b := x.s.Poller.BindingOf("laptop"); !b.OK() || !b.Record.Holder || b.Record.MachineID != shared {
		t.Fatalf("laptop keeps the shared id: %+v", b)
	}
	// tower gets an id of its own on the machine, and an admin accepts it
	x.setTrustID(t, x.tower, otherID)
	x.poll()
	if what, _, err := x.accept("tower", "nora"); err != nil || what != "id" {
		t.Fatal(what, err)
	}
	if b := x.s.Poller.BindingOf("tower"); !b.OK() || b.Record.MachineID != otherID {
		t.Fatalf("tower with its own id: %+v", b)
	}
	// laptop now shows tower's id
	x.setTrustID(t, x.laptop, otherID)
	x.poll()
	laptopRefused := func(when string) {
		t.Helper()
		if b := x.s.Poller.BindingOf("laptop"); b.Status != approval.BindDuplicate || fmt.Sprint(b.Held) != "[tower]" || b.Record.MachineID != shared {
			t.Fatalf("%s: laptop must be flagged, name tower as the one that keeps the id, and keep its own record: %+v", when, b)
		}
		_, ms := x.page(t)
		if m := ms["laptop"]; m["can_accept"] != nil || m["accept"] != nil || fmt.Sprint(m["held_by"]) != "[tower]" {
			t.Fatalf("%s: laptop has a button: %v", when, m)
		}
		if r := x.press("laptop", map[string]any{"machine_id": otherID}); r.code != 409 || pendingID(r) != "" ||
			!strings.Contains(fmt.Sprint(r.body["detail"]), "laptop: nothing was accepted. The machine id it shows is kept by tower") {
			t.Fatalf("%s: an accept on laptop: %d %s", when, r.code, r.raw)
		}
		var held *poller.HeldError
		if what, _, err := x.s.Poller.AcceptMachine("laptop", "nora", poller.AcceptWant{MachineID: otherID}); what != "" || !errors.As(err, &held) || fmt.Sprint(held.By) != "[tower]" {
			t.Fatalf("%s: AcceptMachine on laptop: %q %v", when, what, err)
		}
		x.stopped(t, when+": laptop's update", x.post("/api/admin/updates/laptop", map[string]any{}), "laptop has the same machine id as tower, which keeps it")
	}
	laptopRefused("before the admin's word")
	if b := x.s.Poller.BindingOf("tower"); b.Status != approval.BindDuplicate || fmt.Sprint(b.With) != "[laptop]" || len(b.Held) != 0 {
		t.Fatalf("tower is flagged until an admin says that it keeps its id: %+v", b)
	}

	// the admin's word, through the button and "Confirm it's you"
	_, ms := x.page(t)
	if m := ms["tower"]; m["can_accept"] != true || m["accept"] != "keep" || m["shown_id"] != otherID {
		t.Fatalf("tower on the approval page: %v", m)
	}
	res := x.confirm(t, x.press("tower", map[string]any{"machine_id": otherID}))
	if want := "tower keeps the machine id. laptop stays flagged until it shows an id of its own (on it: fairy-agent helper-trust set --new-machine-id)."; res["ok"] != true || res["message"] != want {
		t.Fatalf("the result: %v", res)
	}
	inOrder := func(when string) {
		t.Helper()
		if b := x.s.Poller.BindingOf("tower"); !b.OK() || !b.Record.Holder || b.Record.MachineID != otherID {
			t.Fatalf("%s: tower is still flagged: status=%q with=%v holder=%v", when, b.Status, b.With, b.Record.Holder)
		}
		if _, ms := x.page(t); ms["tower"]["binding"] != nil || ms["tower"]["can_accept"] != nil {
			t.Fatalf("%s: tower on the approval page: %v", when, ms["tower"])
		}
	}
	inOrder("after \"This host keeps its machine id\"")
	for i := 0; i < 3; i++ { // and it stays so while laptop goes on showing tower's id
		x.poll()
		inOrder(fmt.Sprintf("poll %d after the admin's word", i+1))
	}
	laptopRefused("after the admin's word")
	inOrder("after the refused accepts on laptop")
	if _, _, ok := approvalAsked(x.post("/api/systems/tower/fan/mode", map[string]any{"mode": "full"})); !ok {
		t.Fatal("tower is not asked for its approval after the admin's word")
	}
}
