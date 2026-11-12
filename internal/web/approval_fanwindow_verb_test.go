package web

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
)

// The fan window is for fan changes (audit 6, r4-x1-06). Inside it this session's next fan change needs neither
// an approval nor the panel's own confirmation. The panel's shortcut did not look at the request: a host that
// answered "fan_window" to the question about an update (a real agent says that to fan requests only) got the
// update sent with no "Confirm it's you" at all, within a minute of a fan change this session had approved.
//
// A host that is on record with the check is refused such an answer outright (TestAHostOnRecordThatSays...).
// This is the host that is not: its check was accepted as off a moment ago, and now it shows one that cannot
// be used, so it is asked and nothing is on record for it.
func TestTheFanWindowLetsOnlyFanChangesThrough(t *testing.T) {
	a := newApprovalEnv(t)
	a.selfUpdating(t)
	a.require(t, a.tower)
	id, _, ok := approvalAsked(a.post("/api/systems/tower/fan/mode", map[string]any{"mode": "full"}))
	if !ok {
		t.Fatal("the first fan change asks for the machine's approval")
	}
	if cb := a.approve(t, id); cb.code != 303 || a.result(t)["ok"] != true {
		t.Fatalf("the approved fan change: %d", cb.code)
	}
	// its check goes, an admin accepts that, and then it shows a trust file that cannot be used
	a.clearTrust(t, a.tower)
	a.poll()
	if what, _, err := a.accept("tower", "test admin"); what != "off" || err != nil {
		t.Fatalf("the accept: %q %v", what, err)
	}
	if err := os.WriteFile(a.tower.TrustPath, []byte("not a trust file"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.tower.Gate.StatusNow()
	a.poll()
	if b := a.s.Poller.BindingOf("tower"); !a.s.approvalWanted("tower") || !b.OK() || b.Record.MachineID != "" || b.Record.Unaccepted {
		t.Fatalf("tower must be asked, with nothing on record: %+v", b)
	}
	// whatever is asked, it answers "inside my fan window"
	a.gb.Relay("tower", func(path string, body any) (int, jsonx.M) {
		if path != api.PathApprovalChallenge {
			return 0, nil
		}
		var req api.ApprovalRequest
		raw, _ := json.Marshal(body)
		_ = json.Unmarshal(raw, &req)
		canon, _ := req.Canonical()
		return 200, jsonx.M{"needed": false, "reason": "fan_window", "request": canon}
	})
	sent := func(path string) (n int) {
		for _, s := range a.tower.Asked() {
			if s.Path == path {
				n++
			}
		}
		return n
	}
	// an update: the ordinary "Confirm it's you" (the admin's sign-in is an hour old), and nothing is sent
	r := a.post("/api/admin/updates/tower", map[string]any{})
	if d, _ := r.body["detail"].(map[string]any); r.code != 401 || d["code"] != "step_up_required" || d["approval"] != nil {
		t.Fatalf("an update on the strength of the fan window: %d %s", r.code, r.raw)
	}
	if sent("/update") != 0 {
		t.Fatal("the update was sent without a confirmation")
	}
	// a fan change is what the window is for: sent without one, as before
	before := sent("/fan/duty")
	if r := a.post("/api/systems/tower/fan/duty", map[string]any{"zone": 0, "percent": 60}); r.code == 401 || sent("/fan/duty") != before+1 {
		t.Fatalf("a fan change inside the window: %d %s", r.code, r.raw)
	}
}
