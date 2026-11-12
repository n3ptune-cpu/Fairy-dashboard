package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
)

// What the agent's changes of 2026-11-12 (the protocol's CHANGED lines 1 to 7 and 14) ask of the panel.

// 429: the machine has had its questions, or holds too many open ones. The page says when to try again, in the
// machine's own seconds; the panel asks ONCE and does not try again by itself; nobody is sent to Pocket ID.
func TestTooManyRequestsSaysWhenToTryAgain(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop)
	for name, c := range map[string]struct {
		answer jsonx.M
		want   []string
	}{
		"too_many_challenges": {jsonx.M{"code": api.CodeTooManyChallenges, "error": "16 approval challenges are open. Try again in 37 s", "retry_after_s": 37.0},
			[]string{"laptop holds as many open approval questions as it takes", "somebody else is asking", "Try again in 37 s."}},
		"the challenge budget": {jsonx.M{"error": "too many approval challenge requests; try again in a minute (...)", "retry_after_s": 60.0},
			[]string{"laptop has had its approval questions for this minute", "Try again in 60 s."}},
		"no number at all": {jsonx.M{"error": "too many approval challenge requests; try again in a minute"},
			[]string{"Try again in 60 s."}},
	} {
		asked := 0
		a.gb.Lie("laptop", func(path string) (int, jsonx.M) {
			if path == api.PathApprovalChallenge {
				asked++
				return 429, c.answer
			}
			return 0, nil
		})
		r := a.post("/api/systems/laptop/power", a.shutdownBody())
		msg := fmt.Sprint(r.body["detail"])
		for _, w := range c.want {
			if r.code != 429 || !strings.Contains(msg, w) {
				t.Errorf("%s: %d %q lacks %q", name, r.code, msg, w)
			}
		}
		if asked != 1 || len(a.iss.Asked()) != 0 || keptID(a.env, a.admin) != "" || len(a.laptop.Passed()) != 0 {
			t.Errorf("%s: the machine was asked %d times; kept %q", name, asked, keptID(a.env, a.admin))
		}
	}
	// the action itself, after the approval: the machine's control budget
	a.gb.Lie("laptop", nil)
	a.b.Mu.Lock()
	a.b.Reply = func(id, path string) (int, jsonx.M, error) {
		if id == "laptop" && path == "/power" {
			return 429, jsonx.M{"error": "too many control requests; try again in a minute", "retry_after_s": 60.0}, nil
		}
		return 0, nil, nil
	}
	a.b.Mu.Unlock()
	r := a.post("/api/systems/laptop/power", a.shutdownBody())
	id, _, ok := approvalAsked(r)
	if !ok {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	if cb := a.approve(t, id); cb.code != 303 {
		t.Fatal(cb.code)
	}
	if res := a.result(t); res["ok"] == true || !strings.Contains(fmt.Sprint(res["message"]), "laptop has had its control requests for this minute") ||
		!strings.Contains(fmt.Sprint(res["message"]), "Try again in 60 s.") {
		t.Fatalf("the action's 429: %v", res)
	}
}

// Line 1: a Windows machine whose RUNNING helper is from before approvals while it has a trust file says
// "unknown" with that reason, and refuses. The panel says what helps: restart the helper (Stop, then Start);
// "Start" alone does nothing.
func TestAnOldRunningHelperIsToBeRestarted(t *testing.T) {
	a := newApprovalEnv(t)
	const why = "the running fairy-agent-helper is older than this agent (an update did not restart it): restart it"
	a.gb.Stale("laptop", jsonx.M{"state": "unknown", "enforced_by": "helper", "proto": 2.0, "error": why})
	a.gb.Lie("laptop", func(path string) (int, jsonx.M) {
		switch path {
		case api.PathApprovalChallenge:
			return 409, jsonx.M{"code": api.CodeHelperTooOld, "error": "the helper is older than this agent"}
		case "/power", "/update":
			return 503, jsonx.M{"code": api.CodeApprovalUnavailable, "error": "not run: " + why}
		}
		return 0, nil
	})
	a.poll()
	for _, c := range []struct {
		path string
		body any
	}{{"/api/systems/laptop/power", a.shutdownBody()}, {"/api/admin/updates/laptop", map[string]any{}}} {
		r := a.post(c.path, c.body)
		msg := fmt.Sprint(r.body["detail"])
		if r.code != 409 || !strings.Contains(msg, "The helper that runs on laptop is older than its agent") || !strings.Contains(msg, "Restart it: Stop helper, then Start helper") ||
			strings.Contains(msg, "helper-trust") {
			t.Fatalf("%s: %d %s", c.path, r.code, r.raw)
		}
	}
	if len(a.iss.Asked()) != 0 || len(a.laptop.Passed()) != 0 {
		t.Fatal("somebody was sent to Pocket ID, or something was sent to the machine")
	}
	_, ms := a.page(t)
	if m := ms["laptop"]; m["state"] != "unknown" || m["ready"] == true || !strings.Contains(fmt.Sprint(m["why"]), "restart the helper (Stop, then Start") {
		t.Fatalf("the approval page: %v", m)
	}
	// a helper that only does not answer: start it
	a.gb.Stale("laptop", jsonx.M{"state": "unknown", "enforced_by": "helper", "proto": 2.0, "error": "the helper is not answering"})
	a.gb.Lie("laptop", func(path string) (int, jsonx.M) {
		if path == api.PathApprovalChallenge {
			return 503, jsonx.M{"code": api.CodeApprovalUnavailable, "error": "the helper is not answering"}
		}
		return 0, nil
	})
	a.poll()
	if r := a.post("/api/systems/laptop/power", a.shutdownBody()); r.code != 409 || !strings.Contains(fmt.Sprint(r.body["detail"]), "Start it (next to the host name)") {
		t.Fatalf("a helper that does not answer: %d %s", r.code, r.raw)
	}
}

// Line 2: the machine's fan window opens only after an approved fan change that was APPLIED. After an approved
// change the machine (its BMC) refused, the panel assumes no window: the next fan change asks again.
func TestNoFanWindowAfterAnApprovedChangeThatWasNotApplied(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.tower)
	a.b.Mu.Lock()
	a.b.Reply = func(id, path string) (int, jsonx.M, error) {
		if id == "tower" && path == "/fan/mode" {
			return 502, jsonx.M{"error": "BMC refused the fan mode: timeout"}, nil
		}
		return 0, nil, nil
	}
	a.b.Mu.Unlock()
	r := a.post("/api/systems/tower/fan/mode", map[string]any{"mode": "full"})
	id, _, ok := approvalAsked(r)
	if !ok {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	if cb := a.approve(t, id); cb.code != 303 || a.result(t)["ok"] == true {
		t.Fatal("the approved change the BMC refused must be shown as failed")
	}
	a.b.Mu.Lock()
	a.b.Reply = nil
	a.b.Mu.Unlock()
	r = a.post("/api/systems/tower/fan/duty", map[string]any{"zone": 0, "percent": 60})
	if _, _, ok := approvalAsked(r); !ok {
		t.Fatalf("after a change that was not applied the next one must ask again: %d %s", r.code, r.raw)
	}
}

// Line 6: how a machine's trust came to be, and whether it is what this panel would set, are shown.
func TestTrustSetByAndOtherTrustAreShown(t *testing.T) {
	a := newApprovalEnv(t)
	r := a.turnOn("laptop")
	id, _, ok := approvalAsked(r)
	if !ok {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	if cb := a.approve(t, id); cb.code != 303 || a.result(t)["ok"] != true {
		t.Fatal("Turn on")
	}
	a.poll()
	x := a.do("GET", "/api/systems/laptop", nil, "Cookie", a.admin).body["approval"].(map[string]any)
	if x["set_by"] != "panel" || x["set_at"] == nil || x["trust_hash"] != nil {
		t.Fatalf("the block: %v", x)
	}
	_, ms := a.page(t)
	if m := ms["laptop"]; m["set_by"] != "panel" || m["other_trust"] == true {
		t.Fatalf("set from this panel, with this panel's values: %v", m)
	}
	a.s.Cfg.Approval.Users = []string{"another-user-0001"} // the panel would set another list now
	_, ms = a.page(t)
	if m := ms["laptop"]; m["other_trust"] != true {
		t.Fatalf("the machine trusts another list than the panel would set: %v", m)
	}
}
