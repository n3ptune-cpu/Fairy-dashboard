package web

import (
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
)

// A machine that checks passkey approvals itself is in no "Update unpowered hosts" run yet: a run has no
// approval to give it for the update and the shutdown it would send many minutes later. The page says so
// (also while the machine is off: its last answer stays), and a start that names it is refused before anybody
// is asked to confirm; the other machines are as before.
func TestRunsLeaveOutMachinesThatCheckApprovals(t *testing.T) {
	a := signedIn(t)
	e := a.e
	f := newFarm(t, e)
	e.b.Mu.Lock()
	plain := e.b.Metrics["pc-a"]
	e.b.Metrics["pc-a"] = func() (jsonx.M, error) {
		m, err := plain()
		if err == nil {
			m["approval"] = jsonx.M{"state": "required", "enforced_by": "agent", "proto": 1.0, "issuer": "https://id.example.org",
				"client_id": "pc-actions", "users": 1.0, "fan_window_s": 30.0}
		}
		return m, err
	}
	f.on["pc-a"], f.on["pc-b"] = true, true
	e.b.Mu.Unlock()
	f.optIn("pc-a", "pc-b")
	f.tick()
	page := func() resp { return e.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess) }
	h := wuHostRow(t, page(), "pc-a")
	if h["choice"] != "" || h["approval"] != "required" || !strings.HasPrefix(h["why"].(string), "needs approval: not supported in runs yet") {
		t.Fatalf("pc-a while it is on: %v", h)
	}
	if h := wuHostRow(t, page(), "pc-b"); h["choice"] != "update_only" || h["approval"] != nil {
		t.Fatalf("pc-b asks for nothing and is as before: %v", h)
	}
	// off: what it last said about itself still counts
	e.b.Mu.Lock()
	f.on["pc-a"] = false
	e.b.Mu.Unlock()
	f.offline()
	if h := wuHostRow(t, page(), "pc-a"); h["power"] != "off" || h["choice"] != "" || !strings.HasPrefix(h["why"].(string), "needs approval: not supported in runs yet") {
		t.Fatalf("pc-a while it is off: %v", h)
	}
	before := len(f.requests())
	for _, body := range []any{
		map[string]any{"hosts": []string{"pc-a"}},
		map[string]any{"update_only": []string{"pc-a"}},
		map[string]any{"hosts": []string{"pc-a"}, "update_only": []string{"pc-b"}},
	} {
		r := e.wuStart(a.sess, body)
		if d, _ := r.body["detail"].(string); r.code != 400 || !strings.HasPrefix(d, "PC-A needs approval: not supported in runs yet") {
			t.Fatalf("a start with pc-a %v: %d %s", body, r.code, r.raw)
		}
	}
	if act, hist := e.s.WakeRuns.Snapshot(); act != nil || len(hist) != 0 || len(f.requests()) != before {
		t.Fatalf("no run was started and nothing was sent: %v %v %v", act, hist, f.requests()[before:])
	}
	// a run with the other machine starts as it always did: the confirmation first
	r := e.wuStart(a.sess, map[string]any{"update_only": []string{"pc-b"}})
	if d, _ := r.body["detail"].(map[string]any); r.code != 401 || d["code"] != "step_up_required" || d["approval"] != nil {
		t.Fatalf("pc-b alone: %d %s", r.code, r.raw)
	}
}
