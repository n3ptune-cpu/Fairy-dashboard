package web

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
)

// One machine id is accepted for one host only, and an accept is for exactly the id the dialog showed
// (audit 5, r2-b2-02 and r2-x1-01). These tests press the button the way the page does: the request with what
// the page shows, "Confirm it's you" at the identity provider, and the result the panel reports afterwards.

// acceptAdmin is an admin who signed in through the fake identity provider, on the panel of an approvalEnv.
type acceptAdmin struct {
	*approvalEnv
	idp  *fakeIDP
	sess string
}

func newAcceptAdmin(t *testing.T) *acceptAdmin {
	t.Helper()
	x := &acceptAdmin{approvalEnv: newApprovalEnv(t), idp: newFakeIDP(t)}
	x.s.Cfg.OIDC.Issuer = x.idp.srv.URL
	x.idp.claims = map[string]any{"sub": approvalAdminSub, "groups": []string{"viewers", "controllers"}, "auth_time": time.Now().Unix()}
	q, rt := x.idp.login(x.env, "/auth/login?next=/", "")
	x.sess = sessionCookie(x.env, x.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt).hdr)
	if me := x.do("GET", "/api/me", nil, "Cookie", x.sess); me.code != 200 || me.body["can_control"] != true {
		t.Fatalf("the admin's sign-in: %d %s", me.code, me.raw)
	}
	return x
}

// press is the button's request: POST accept with what the page shows.
func (x *acceptAdmin) press(agent string, body any) resp {
	return x.do("POST", "/api/admin/approval/accept/"+agent, body, "Cookie", x.sess, "X-Requested-With", "fairy", "Content-Type", "application/json")
}

// confirm is "Continue" in the dialog r opened, the sign-in, and the result of the request the panel then ran.
func (x *acceptAdmin) confirm(t *testing.T, r resp) map[string]any {
	t.Helper()
	if r.code != 401 || pendingID(r) == "" {
		t.Fatalf("the accept must ask to confirm: %d %s", r.code, r.raw)
	}
	q, rt, st := x.idp.tryConfirmStart(x.env, pendingID(r), "/", x.sess)
	if st.code != 200 {
		t.Fatalf("Continue: %d %s", st.code, st.raw)
	}
	x.idp.claims["auth_time"] = time.Now().Unix()
	cb := x.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt)
	if cb.code != 303 {
		t.Fatalf("the sign-in's callback: %d %s", cb.code, cb.raw)
	}
	x.sess = sessionCookie(x.env, cb.hdr)
	pr, _ := x.do("GET", "/api/me", nil, "Cookie", x.sess).body["pending_result"].(map[string]any)
	if pr == nil {
		t.Fatal("no result of the confirmed accept")
	}
	return pr
}

func actionOf(r resp) string {
	d, _ := r.body["detail"].(map[string]any)
	return fmt.Sprint(d["action"])
}

// The auditor's case: tower shows a copy of laptop's machine id. Laptop has that id on record; tower has its
// own. Tower gets no button and cannot be accepted, before and after laptop is; laptop keeps the id; what the
// panel says is what it did.
func TestAnAcceptOnTheCopyIsRefused(t *testing.T) {
	x := newAcceptAdmin(t)
	x.require(t, x.laptop, x.tower)
	idL, idT := machineIDOf("laptop"), machineIDOf("tower")
	x.setTrustID(t, x.tower, idL) // the copy: tower's trust file is laptop's
	x.poll()
	if b := x.s.Poller.BindingOf("laptop"); b.Status != approval.BindDuplicate || len(b.Held) != 0 {
		t.Fatalf("laptop: %+v", b)
	}
	if b := x.s.Poller.BindingOf("tower"); b.Status != approval.BindDuplicate || fmt.Sprint(b.Held) != "[laptop]" || b.Record.MachineID != idT {
		t.Fatalf("tower: %+v", b)
	}
	untouched := func(when string) {
		t.Helper()
		recs, err := adminstate.New(x.s.Cfg.DataDir).Machines()
		if err != nil || recs["laptop"].MachineID != idL || recs["tower"].MachineID != idT {
			t.Fatalf("%s: the record: %v %+v", when, err, recs)
		}
		if b := x.s.Poller.BindingOf("tower"); b.Status != approval.BindDuplicate || b.Record.Holder {
			t.Fatalf("%s: tower: %+v", when, b)
		}
	}
	towerRefused := func(when string) {
		t.Helper()
		// the page: no button for tower, the host that keeps the id by name, and what to do instead
		_, ms := x.page(t)
		if m := ms["tower"]; m["can_accept"] != nil || m["accept"] != nil || fmt.Sprint(m["held_by"]) != "[laptop]" ||
			!strings.Contains(fmt.Sprint(m["binding_text"]), "tower has the same machine id as laptop, which keeps it: one id is accepted for one host only") {
			t.Fatalf("%s: tower on the approval page: %v", when, m)
		}
		// everyone who sees the host: the other host by name, never an id
		blk := x.do("GET", "/api/systems/tower", nil, "Cookie", x.sess)
		if ap, _ := blk.body["approval"].(map[string]any); fmt.Sprint(ap["binding_held"]) != "[laptop]" || fmt.Sprint(ap["binding_with"]) != "[laptop]" ||
			strings.Contains(string(blk.raw), idL) || strings.Contains(string(blk.raw), idT) {
			t.Fatalf("%s: tower's block: %s", when, blk.raw)
		}
		// a request made up without the page: refused at once, with the reason and what to do; nobody is asked to confirm
		for _, body := range []any{map[string]any{"machine_id": idL}} {
			r := x.press("tower", body)
			if d := fmt.Sprint(r.body["detail"]); r.code != 409 || pendingID(r) != "" || !strings.Contains(d, "tower: nothing was accepted. The machine id it shows is kept by laptop, and one id is accepted for one host only.") ||
				!strings.Contains(d, "Give tower a new id on the machine itself (fairy-agent helper-trust set --new-machine-id), then accept the new id here.") {
				t.Fatalf("%s: a made-up accept on tower: %d %s", when, r.code, r.raw)
			}
		}
		var held *poller.HeldError
		if what, _, err := x.s.Poller.AcceptMachine("tower", "nora", poller.AcceptWant{MachineID: idL}); what != "" || !errors.As(err, &held) || fmt.Sprint(held.By) != "[laptop]" {
			t.Fatalf("%s: AcceptMachine on tower: %q %v", when, what, err)
		}
		x.stopped(t, when+": tower's fan change", x.post("/api/systems/tower/fan/mode", map[string]any{"mode": "full"}), "tower has the same machine id as laptop, which keeps it")
		untouched(when)
	}
	towerRefused("before any accept")
	if al := x.auditEntries("", "approval.accept"); len(al) != 1 || al[0]["target"] != "tower" || !strings.HasPrefix(fmt.Sprint(al[0]["outcome"]), "error 409") ||
		fmt.Sprint(al[0]["params"].(map[string]any)["held_by"]) != "[laptop]" {
		t.Fatalf("the refused accept in the audit log: %v", al)
	}

	// laptop, the original: the button is "This host keeps its machine id", the dialog says what would be accepted
	_, ms := x.page(t)
	if m := ms["laptop"]; m["can_accept"] != true || m["accept"] != "keep" || m["shown_id"] != idL || m["held_by"] != nil {
		t.Fatalf("laptop on the approval page: %v", m)
	}
	r := x.press("laptop", map[string]any{"machine_id": idL})
	if got := actionOf(r); !strings.Contains(got, "it keeps its machine id "+approval.CompareID(idL)+"; tower must get a new one") {
		t.Fatalf("the dialog for laptop: %q", got)
	}
	res := x.confirm(t, r)
	if want := "laptop keeps the machine id. tower stays flagged until it shows an id of its own (on it: fairy-agent helper-trust set --new-machine-id)."; res["ok"] != true || res["message"] != want {
		t.Fatalf("the result: %v", res)
	}
	if b := x.s.Poller.BindingOf("laptop"); !b.OK() || !b.Record.Holder || b.Record.MachineID != idL || b.Record.How != approval.PinAccepted {
		t.Fatalf("laptop after its accept: %+v", b)
	}
	// the press on the copy afterwards (what moved the id away from the original): still refused, nothing moved
	towerRefused("after the accept on laptop")
	if b := x.s.Poller.BindingOf("laptop"); !b.OK() || !b.Record.Holder {
		t.Fatalf("laptop was flagged again by a press on tower: %+v", b)
	}
	if _, _, ok := approvalAsked(x.post("/api/systems/laptop/power", x.shutdownBody())); !ok {
		t.Fatal("laptop is asked for its approval")
	}

	// the way out: tower gets an id of its own on the machine, and that one is accepted on its page
	x.setTrustID(t, x.tower, otherID)
	x.poll()
	_, ms = x.page(t)
	if m := ms["tower"]; m["binding"] != "changed" || m["can_accept"] != true || m["accept"] != "id" || m["shown_id"] != otherID || m["held_by"] != nil {
		t.Fatalf("tower with its own id: %v", m)
	}
	r = x.press("tower", map[string]any{"machine_id": otherID})
	if got := actionOf(r); !strings.Contains(got, "it shows the machine id "+approval.CompareID(otherID)+"; on record is "+approval.CompareID(idT)) {
		t.Fatalf("the dialog for tower: %q", got)
	}
	if res := x.confirm(t, r); res["ok"] != true || res["message"] != "tower: the machine id it shows is on record now." {
		t.Fatalf("the result: %v", res)
	}
	if !x.s.Poller.BindingOf("tower").OK() || !x.s.Poller.BindingOf("laptop").OK() {
		t.Fatal("both must be in order at the end")
	}
}

// Two hosts without a record that show the same id (the copy was made before the panel saw either): both are
// "not on record", neither keeps anything, so the first accept decides, and from then on the other one is
// refused.
//
// CHANGED (audit 5, round 3, r3-x1-01). This test used to start with laptop on record and tower first seen
// with laptop's id, and held that the two were then equals, each with the button "This host keeps its machine
// id", so that a press on tower took the id away from laptop. That is no longer so
// (TestAHostFirstSeenWithAnIDThatIsOnRecordElsewhereGetsNoButton). What is left of "two equals" is two hosts
// that are BOTH not on record: each has the button "Accept this machine's id", and the dialog names the other.
func TestTheFirstAcceptDecidesBetweenTwoEquals(t *testing.T) {
	x := newAcceptAdmin(t)
	idL := machineIDOf("laptop")
	x.setTrust(t, x.laptop)
	x.setTrustID(t, x.tower, idL) // both are first seen with one id
	x.poll()
	_, ms := x.page(t)
	for id, other := range map[string]string{"laptop": "tower", "tower": "laptop"} {
		if m := ms[id]; m["binding"] != approval.BindNoRecord || m["can_accept"] != true || m["accept"] != "first" || m["held_by"] != nil || m["machine_id"] != nil || fmt.Sprint(m["with"]) != "["+other+"]" {
			t.Fatalf("%s: %v", id, m)
		}
	}
	x.stopped(t, "laptop's update", x.post("/api/admin/updates/laptop", map[string]any{}), "laptop checks approvals itself, and its machine id is not on record")
	x.stopped(t, "tower's fan change", x.post("/api/systems/tower/fan/mode", map[string]any{"mode": "full"}), "tower checks approvals itself, and its machine id is not on record")
	r := x.press("tower", map[string]any{"machine_id": idL})
	if got := actionOf(r); !strings.Contains(got, "it shows the machine id "+approval.CompareID(idL)+", which is not on record; laptop shows the same id and must get a new one") {
		t.Fatalf("the dialog for tower: %q", got)
	}
	res := x.confirm(t, r)
	if res["ok"] != true || res["message"] != "tower has the machine id on record now, and keeps it. laptop stays flagged until it shows an id of its own (on it: fairy-agent helper-trust set --new-machine-id)." {
		t.Fatalf("the result: %v", res)
	}
	if r := x.press("laptop", map[string]any{"machine_id": idL}); r.code != 409 || !strings.Contains(fmt.Sprint(r.body["detail"]), "kept by tower") {
		t.Fatalf("laptop after tower was accepted: %d %s", r.code, r.raw)
	}
	if b := x.s.Poller.BindingOf("tower"); !b.OK() || !b.Record.Holder || b.Record.How != approval.PinAccepted {
		t.Fatalf("tower: %+v", b)
	}
	if b := x.s.Poller.BindingOf("laptop"); b.Status != approval.BindDuplicate || fmt.Sprint(b.Held) != "[tower]" || b.Record.Holder || b.Record.MachineID != "" {
		t.Fatalf("laptop: %+v", b)
	}
	if _, ms := x.page(t); ms["laptop"]["can_accept"] != nil || fmt.Sprint(ms["laptop"]["held_by"]) != "[tower]" {
		t.Fatalf("laptop on the approval page afterwards: %v", ms["laptop"])
	}
}

// What is accepted is the id the dialog showed (audit 5, r2-x1-01). A host that shows one id while the admin
// is asked to confirm and another one when the confirmation is back gets nothing on record.
func TestAnAcceptIsForTheIDTheDialogShowed(t *testing.T) {
	const thirdID = "a-third-machine-id-0123456789"
	x := newAcceptAdmin(t)
	x.require(t, x.laptop)
	idL := machineIDOf("laptop")
	x.setTrustID(t, x.laptop, otherID)
	x.poll()
	onRecord := func(when, id string, st string) {
		t.Helper()
		if b := x.s.Poller.BindingOf("laptop"); b.Record.MachineID != id || b.Status != st {
			t.Fatalf("%s: %+v", when, b)
		}
		if recs, err := adminstate.New(x.s.Cfg.DataDir).Machines(); err != nil || recs["laptop"].MachineID != id {
			t.Fatalf("%s: machines.json: %v %+v", when, err, recs)
		}
	}
	// the request must say what is accepted, once
	for _, body := range []any{map[string]any{}, map[string]any{"machine_id": otherID, "off": true}, map[string]any{"machine_id": strings.Repeat("x", 200)}} {
		if r := x.press("laptop", body); r.code != 422 {
			t.Fatalf("accept with %v: %d %s", body, r.code, r.raw)
		}
	}
	const changed = "laptop: nothing was accepted. What it shows is not what the dialog showed: it changed while you were confirming."
	// another id than the one it shows, or "off" while it shows an id: refused before anybody is asked to confirm
	for _, body := range []any{map[string]any{"machine_id": thirdID}, map[string]any{"off": true}} {
		if r := x.press("laptop", body); r.code != 409 || pendingID(r) != "" || !strings.Contains(fmt.Sprint(r.body["detail"]), changed) {
			t.Fatalf("accept of %v while it shows another id: %d %s", body, r.code, r.raw)
		}
	}
	// the flip: the dialog shows otherID; the host shows thirdID when the confirmation is back
	r := x.press("laptop", map[string]any{"machine_id": otherID})
	if got := actionOf(r); !strings.Contains(got, "it shows the machine id "+approval.CompareID(otherID)+"; on record is "+approval.CompareID(idL)) {
		t.Fatalf("the dialog: %q", got)
	}
	x.setTrustID(t, x.laptop, thirdID)
	x.poll()
	res := x.confirm(t, r)
	if res["ok"] != false || !strings.Contains(fmt.Sprint(res["message"]), changed) {
		t.Fatalf("the result after the flip: %v", res)
	}
	onRecord("after the flip", idL, approval.BindChanged)
	// the same when the check is gone by then
	r = x.press("laptop", map[string]any{"machine_id": thirdID})
	x.clearTrust(t, x.laptop)
	x.poll()
	if res := x.confirm(t, r); res["ok"] != false || !strings.Contains(fmt.Sprint(res["message"]), changed) {
		t.Fatalf("the result after the check went: %v", res)
	}
	onRecord("after the check went", idL, approval.BindVanished)
	// and the other way round: "off" confirmed, an id shown by then
	r = x.press("laptop", map[string]any{"off": true})
	if got := actionOf(r); !strings.Contains(got, "it shows no check any more") {
		t.Fatalf("the dialog for off: %q", got)
	}
	x.setTrustID(t, x.laptop, thirdID)
	x.poll()
	if res := x.confirm(t, r); res["ok"] != false || !strings.Contains(fmt.Sprint(res["message"]), changed) {
		t.Fatalf("the result of an accepted off for a host that shows an id: %v", res)
	}
	onRecord("after the refused off", idL, approval.BindChanged)
	for _, e := range x.auditEntries("", "approval.accept") {
		if !strings.HasPrefix(fmt.Sprint(e["outcome"]), "error 409") {
			t.Fatalf("an accept went through: %v", e)
		}
	}
	// what the dialog shows and the host still shows: accepted, and it is that id
	res = x.confirm(t, x.press("laptop", map[string]any{"machine_id": thirdID}))
	if res["ok"] != true || res["message"] != "laptop: the machine id it shows is on record now." {
		t.Fatalf("the accept: %v", res)
	}
	onRecord("after the accept", thirdID, approval.BindOK)
	if what, _, err := x.s.Poller.AcceptMachine("laptop", "nora", poller.AcceptWant{MachineID: thirdID}); what != "" || !errors.Is(err, poller.ErrNothingToAccept) {
		t.Fatalf("a second accept: %q %v", what, err)
	}
}
