package web

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// "Nothing was confirmed" is not said while a confirmation is open (second look at 4f94383, N1), through the
// real routes. The run's one confirmation gets no answer from its only machine, and the machine's agent is out
// of reach from then on. The notification said at once "no machine waits for the confirmation any more ...:
// nothing was confirmed", which nobody knew. Now nothing ends the question until the machine's agent is read
// or the question's time is over, and at that end the notification says that a confirmation is still open.
func TestTheEndOfTheQuestionNamesAConfirmationThatIsStillOpen(t *testing.T) {
	r := newRunRig(t, "pc-a")
	if _, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}}); res["ok"] != true {
		t.Fatal(res)
	}
	r.until(t, "asked", 300, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a != nil && a.Approval == "needed" })
	run, _ := r.s.WakeRuns.Snapshot()
	r.gb.Relay("pc-a", func(path string, body any) (int, jsonx.M) {
		if path != api.PathRunGo {
			return 0, nil
		}
		return 504, jsonx.M{} // no answer
	})
	_, res := r.approved(t, "POST", "/api/admin/wake-update/"+run.ID+"/confirm", map[string]any{})
	if lines, _ := res["items"].([]any); len(lines) != 1 || !strings.HasPrefix(lines[0].(string), "PC-A: no answer (") {
		t.Fatalf("what the admin is told: %v", res)
	}
	r.b.Mu.Lock()
	r.f.ready["pc-a"] = r.f.now.Add(time.Hour) // the link that lost the answer stays down
	r.b.Mu.Unlock()
	r.tick(20)
	cur, _ := r.s.WakeRuns.Snapshot()
	if cur == nil || cur.Approval != "needed" {
		t.Fatalf("the question while the confirmation is open: %+v", cur)
	}
	if got := r.overNotes(); len(got) != 0 {
		t.Fatalf("said before the machine's agent was read: %q", got)
	}
	if w := r.req("GET", "/api/admin/wake-update/waiting", nil); w.body["run"] != nil {
		t.Fatalf("the strip asks for a confirmation that is not sent twice: %s", w.raw)
	}
	r.until(t, "the question over", 1200, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a != nil && a.Approval == "over" })
	cur, _ = r.s.WakeRuns.Snapshot()
	if cur.ApprovalEnd != wakerun.EndTime || r.f.now.Before(*run.ConfirmBy) {
		t.Fatalf("the question ended as %q at %s; it asked until %s", cur.ApprovalEnd, r.f.now.Format("15:04:05"), run.ConfirmBy.Format("15:04:05"))
	}
	want := "the time to confirm is over, but a confirmation that got no answer the panel can go by is still open for PC-A: it is not known yet whether it arrived. What the machine's agent shows decides; see the run's report"
	if got := r.overNotes(); len(got) != 1 || got[0] != want {
		t.Fatalf("the notification: %q\nwant %q", got, want)
	}
	found := false
	for _, en := range r.auditEntries("", "wakeupdate.approval") {
		if en["outcome"] == "over" {
			found = strings.Contains(fmt.Sprint(en["params"]), "PC-A")
		}
	}
	if !found {
		t.Fatalf("the audit entry of the question's end does not name the open confirmation: %v", r.auditEntries("", "wakeupdate.approval"))
	}
	r.noTokenAnywhere(t)
}
