package web

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// What "Update all" sends after its approval is what the dialog showed (audit 6, r4-x1-04; R5: "what runs after
// the sign-in is the request the dialog showed").
//
// The list of machines was made anew when the kept request ran. A machine without the check that did not
// answer when the button was pressed, and answered again before the passkey was given, was updated by a
// confirmation whose dialog had named the other machine only. The machines the dialog listed are kept with the
// plan now, and a machine without the check that is not among them is sent nothing: the next press is for it.
func TestUpdateAllSendsOnlyWhatTheDialogShowed(t *testing.T) {
	a := newApprovalEnv(t)
	a.selfUpdating(t)
	a.require(t, a.laptop) // laptop checks approvals; tower does not
	answers := func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["hostname"], m["agent_version"] = "tower", "41-aaaaaaa"
		m["features"].(jsonx.M)["self_update"] = true
		m["self_update"] = jsonx.M{"state": "idle", "since": 1.0}
		return m, nil
	}
	setTower := func(fn func() (jsonx.M, error)) {
		a.b.Mu.Lock()
		a.b.Metrics["tower"] = fn
		a.b.Mu.Unlock()
		a.poll()
	}
	setTower(func() (jsonx.M, error) { return nil, devices.Errf("timeout") }) // tower does not answer when the button is pressed

	r := a.post("/api/admin/updates", map[string]any{})
	id, machines, ok := approvalAsked(r)
	d, _ := r.body["detail"].(map[string]any)
	if !ok || fmt.Sprint(machines) != "[laptop]" || strings.Contains(fmt.Sprint(d["items"]), "tower") {
		t.Fatalf("the dialog names laptop only: %d %s", r.code, r.raw)
	}
	setTower(answers) // ... and answers again while the admin is at Pocket ID
	if cb := a.approve(t, id); cb.code != 303 {
		t.Fatalf("callback: %d %s", cb.code, cb.raw)
	}
	res := a.result(t)
	items := fmt.Sprint(res["items"])
	if res["ok"] != false || res["message"] != "1 started, 1 not." || !strings.Contains(items, "laptop: started") ||
		!strings.Contains(items, "tower: not sent (tower was not part of what was confirmed, so nothing was sent to it)") {
		t.Fatalf("the result: %v", res)
	}
	if p := a.laptop.Passed(); len(p) != 1 || p[0].Path != "/update" || !p[0].Approved {
		t.Fatalf("laptop: %+v", p)
	}
	if n := controlWrites(a.tower); n != 0 {
		t.Fatalf("tower was sent %q by a confirmation that did not show it", sentPaths(a.tower.Asked()))
	}
	var entry map[string]any
	for _, e := range a.auditEntries("", "update.agent") {
		if e["target"] == "tower" {
			entry = e
		}
	}
	if out := fmt.Sprint(entry["outcome"]); !strings.Contains(out, "was not part of what was confirmed") {
		t.Fatalf("the audit entry for tower: %v", entry)
	}

	// the next press shows tower, and then it is started by that confirmation, as before
	r = a.post("/api/admin/updates", map[string]any{})
	id, _, ok = approvalAsked(r)
	if items := fmt.Sprint(r.body["detail"].(map[string]any)["items"]); !ok ||
		items != "[laptop: update of its agent (checked on the machine) tower: update of its agent (no check on the machine)]" {
		t.Fatalf("the second dialog: %d %s", r.code, r.raw)
	}
	if cb := a.approve(t, id); cb.code != 303 {
		t.Fatalf("callback: %d %s", cb.code, cb.raw)
	}
	if res := a.result(t); res["ok"] != true || res["message"] != "Update started on 2 agents." {
		t.Fatalf("the second result: %v", res)
	}
	if p := a.tower.Passed(); len(p) != 1 || p[0].Path != "/update" || p[0].Approved {
		t.Fatalf("tower after the press that showed it: %+v", p)
	}
}

// A machine the dialog showed as "NOT sent" is not sent to after the approval, also when its check is gone by
// then (it read "nothing checks there" and was sent the update on the strength of a dialog that said it would
// not be).
func TestUpdateAllKeepsWhatTheDialogSaidIsNotSent(t *testing.T) {
	a := newApprovalEnv(t)
	a.selfUpdating(t)
	a.require(t, a.laptop)
	if err := os.WriteFile(a.tower.TrustPath, []byte("not a trust file"), 0o600); err != nil { // its check cannot be used; nothing on record for it
		t.Fatal(err)
	}
	a.tower.Gate.StatusNow()
	a.poll()
	r := a.post("/api/admin/updates", map[string]any{})
	id, _, ok := approvalAsked(r)
	if items := fmt.Sprint(r.body["detail"].(map[string]any)["items"]); !ok || !strings.Contains(items, "tower: NOT sent (tower cannot check approvals right now") {
		t.Fatalf("the dialog: %d %s", r.code, r.raw)
	}
	if err := os.Remove(a.tower.TrustPath); err != nil { // the broken file is removed on the machine meanwhile: no check at all
		t.Fatal(err)
	}
	a.tower.Gate.StatusNow()
	a.poll()
	if a.s.approvalWanted("tower") {
		t.Fatal("the test wants tower to read as a machine without the check now")
	}
	if cb := a.approve(t, id); cb.code != 303 {
		t.Fatalf("callback: %d %s", cb.code, cb.raw)
	}
	res := a.result(t)
	if items := fmt.Sprint(res["items"]); res["message"] != "1 started, 1 not." || !strings.Contains(items, "tower: not sent (tower cannot check approvals right now") {
		t.Fatalf("the result: %v", res)
	}
	if n := controlWrites(a.tower); n != 0 {
		t.Fatalf("tower was sent %q although the dialog said NOT sent", sentPaths(a.tower.Asked()))
	}
}
