package web

import (
	"context"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/updates"
)

// Audit 5, r2-b2-04 (the fix of the approval branch, commit e90585d there, brought here): a single "Update" on
// the Updates page said "Update LAPTOP: done" after "Confirm it's you", next to a row that could say FAILED a
// moment later. The agent had only taken the request; "done" was the page's word for an answer that says
// nothing itself. The answer now carries its own words, and the page shows them in both cases.
func TestASingleUpdateSaysStartedNotDone(t *testing.T) {
	a := signedIn(t) // an admin whose sign-in is an hour old
	e := a.e
	e.s.Updates = updates.Static{Tag: "build-42", Version: "42-bbbbbbb", Build: model.P(42), Verified: true}
	e.b.Mu.Lock()
	e.b.Metrics["laptop"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["agent_version"] = "41-aaaaaaa"
		m["features"].(jsonx.M)["self_update"] = true
		m["self_update"] = jsonx.M{"state": "idle", "since": 1.0}
		return m, nil
	}
	e.b.Mu.Unlock()
	e.s.Poller.PollOnce(context.Background())
	post := func() resp {
		return e.do("POST", "/api/admin/updates/laptop", map[string]any{}, "Cookie", a.sess, "X-Requested-With", "fairy", "X-Fairy-Action", "Update LAPTOP")
	}
	// after "Confirm it's you": what the page shows is "<label>: <message>"
	r := post()
	if !stepUpRequired(r) || pendingID(r) == "" {
		t.Fatalf("the update must be asked to confirm: %d %s", r.code, r.raw)
	}
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatalf("the confirmation: %d %s", cb.code, cb.raw)
	}
	pr, _ := e.do("GET", "/api/me", nil, "Cookie", a.sess).body["pending_result"].(map[string]any)
	if pr == nil || pr["ok"] != true || pr["label"] != "Update LAPTOP" || pr["message"] != "started. Its row shows how it goes." {
		t.Fatalf("after the confirmation the page would show %q: %q (it was \"done\")", pr["label"], pr["message"])
	}
	// inside the window: the answer itself
	r = post()
	if r.code != 200 || r.body["ok"] != true || r.body["message"] != "started. Its row shows how it goes." || r.body["result"] == nil {
		t.Fatalf("the update that runs at once: %d %s", r.code, r.raw)
	}
	js := appJS(t)
	if !strings.Contains(js, "else if (r) flash(`${label}: ${r.message || \"started\"}`);") {
		t.Error("app.js: a single Update that ran at once must show the panel's own words too")
	}
	if !strings.Contains(js, "flash(`${pr.label}: ${pr.ok ? pr.message || \"done\"") {
		t.Error("app.js: the result after a confirmation is no longer the label and the panel's message")
	}
}
