package web

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
)

// r1-b2-02 (audit 5): "Update all" through the approval round trip always ended in "Update all agents: done",
// green, also when a machine had refused the approval or was sent nothing. The result of the kept action now
// carries the panel's own count and one line per machine, and it is "ok" only when every update that was to
// start did start.
func TestUpdateAllSaysWhatHappenedOnEachMachine(t *testing.T) {
	// 1. everything starts: said as such
	a := newApprovalEnv(t)
	a.selfUpdating(t)
	a.require(t, a.laptop, a.tower)
	run := func(a *approvalEnv) map[string]any {
		t.Helper()
		r := a.post("/api/admin/updates", map[string]any{})
		id, _, ok := approvalAsked(r)
		if !ok {
			t.Fatalf("Update all: %d %s", r.code, r.raw)
		}
		if cb := a.approve(t, id); cb.code != 303 {
			t.Fatal(cb.code)
		}
		return a.result(t)
	}
	res := run(a)
	if res["ok"] != true || res["message"] != "Update started on 2 agents." || fmt.Sprint(res["items"]) != "[laptop: started tower: started]" {
		t.Fatalf("both started: %v", res)
	}

	// 2. one machine refuses the approval (its agent restarted while the admin was at Pocket ID: its open
	// challenge is gone): not "done", and the line says who and why
	a = newApprovalEnv(t)
	a.selfUpdating(t)
	a.require(t, a.laptop, a.tower)
	a.b.Mu.Lock()
	a.b.Reply = func(id, path string) (int, jsonx.M, error) {
		if id == "tower" && path == "/update" {
			return 403, jsonx.M{"code": api.CodeApprovalRefused, "error": "approval refused (no_challenge): no entry holds an open challenge of this machine for exactly this request"}, nil
		}
		return 0, nil, nil
	}
	a.b.Mu.Unlock()
	a.gb.Lie("tower", func(path string) (int, jsonx.M) { // (the scripted refusal stands in for the machine's)
		if path == "/update" {
			return 403, jsonx.M{"code": api.CodeApprovalRefused, "error": "approval refused (no_challenge): no entry holds an open challenge of this machine for exactly this request"}
		}
		return 0, nil
	})
	res = run(a)
	items := fmt.Sprint(res["items"])
	if res["ok"] != false || res["message"] != "1 started, 1 not." || !strings.Contains(items, "laptop: started") ||
		!strings.Contains(items, "tower: refused (tower did not accept the approval") || strings.Contains(fmt.Sprint(res["message"]), "done") {
		t.Fatalf("one refused: %v", res)
	}

	// 3. a machine whose check cannot be used is sent nothing, and the audit entry gives ITS reason (it used to
	// say "This is not the request that was approved for ...")
	a = newApprovalEnv(t)
	a.selfUpdating(t)
	a.require(t, a.laptop)
	if err := os.WriteFile(a.tower.TrustPath, []byte("not a trust file"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.poll()
	res = run(a)
	items = fmt.Sprint(res["items"])
	if res["ok"] != false || res["message"] != "1 started, 1 not." || !strings.Contains(items, "tower: not sent (tower cannot check approvals right now") {
		t.Fatalf("one not sent: %v", res)
	}
	for _, e := range a.auditEntries("", "update.agent") {
		if e["target"] != "tower" {
			continue
		}
		if out := fmt.Sprint(e["outcome"]); !strings.Contains(out, "cannot check approvals right now") || strings.Contains(out, "not the request that was approved") {
			t.Errorf("the audit entry of the broken machine: %s", out)
		}
	}
	if len(a.tower.Passed()) != 0 {
		t.Fatal("the broken machine was sent something")
	}

	// 4. a machine that stops answering between the dialog and the approval is "offline", not "refused"
	a = newApprovalEnv(t)
	a.selfUpdating(t)
	a.require(t, a.laptop, a.tower)
	r := a.post("/api/admin/updates", map[string]any{})
	id, _, ok := approvalAsked(r)
	if !ok {
		t.Fatalf("Update all: %d %s", r.code, r.raw)
	}
	a.gb.Fail("tower", devices.Errf("timeout"))
	if cb := a.approve(t, id); cb.code != 303 {
		t.Fatal(cb.code)
	}
	res = a.result(t)
	if res["ok"] != false || res["message"] != "1 started, 1 not." || !strings.Contains(fmt.Sprint(res["items"]), "tower: offline (") {
		t.Fatalf("one offline: %v", res)
	}
}

// The same count and lines without a round trip (machines without the check, a sign-in inside the window), and
// a machine that is skipped is counted apart: it is not a failure.
func TestUpdateAllAnswerHasTheCountAndTheLines(t *testing.T) {
	a := newApprovalEnv(t)
	a.selfUpdating(t)
	fresh := a.cookie(controller(), 0)
	a.b.Mu.Lock()
	a.b.Metrics["tower"] = func() (jsonx.M, error) { return nil, devices.Errf("timeout") }
	a.b.Mu.Unlock()
	a.poll()
	r := a.do("POST", "/api/admin/updates", map[string]any{}, "Cookie", fresh, "X-Requested-With", "fairy", "Content-Type", "application/json")
	// (audit 6, r4-b1-13: a machine that was not answering when the button was pressed is not part of the press;
	// it read "1 started, 1 not." with "tower: offline (agent offline)", red)
	if r.code != 200 || r.body["all_ok"] != true || r.body["message"] != "Update started on 1 agent, 1 skipped." ||
		fmt.Sprint(r.body["lines"]) != "[laptop: started tower: skipped (agent offline)]" {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	states := map[string]any{}
	for _, x := range r.body["results"].([]any) {
		states[x.(map[string]any)["id"].(string)] = x.(map[string]any)["state"]
	}
	if states["laptop"] != "started" || states["tower"] != "skipped" {
		t.Fatalf("results: %v", states)
	}
	// the page: the panel's own sentence, the lines behind "Details", and they stay until closed
	js := appJS(t)
	for _, want := range []string{
		"if (r?.results) flash(`${label}: ${r.message || \"done\"}`, r.all_ok === false, r.lines);",
		"if (pr.items?.length) flash(`${pr.label}: ${pr.message || (pr.ok ? \"done\" : \"not everything worked\")}`, !pr.ok, pr.items);",
		`function flash(msg, bad = false, details = null) {`, `data-flash="more"`, `data-flash="x"`,
		"if (!lines.length) { f.textContent = msg; f.hidden = false; flash.t = setTimeout(() => (f.hidden = true), 5000); return; }",
		"${lines.map((x) => `<li>${esc(x)}</li>`).join(\"\")}",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
}

// A line of the list is short: the machine's own words go (they are in the audit entry), the reason of a
// refused approval stays, and nothing is cut in the middle of a path (r1-b2-04).
func TestShortWhy(t *testing.T) {
	for in, want := range map[string]string{
		"PC-B cannot check approvals right now (PC-B says: the approval trust file on this machine cannot be used, so nothing that needs an approval is run: /var/lib/fairy-agent/approval-trust.json: not a trust file (an administrator repairs it on the machine: fairy-agent helper-trust show)). Until that is repaired on the machine it refuses fan, power and update requests.": "PC-B cannot check approvals right now",
		"PC-D did not accept the approval (PC-D says: approval refused (user): this user may not approve actions on this machine). Nothing was done. You are not on that machine's list.":                                                                                                                                                                                               "PC-D did not accept the approval (user)",
		"PC-A shows another machine id than the one Fairy has on record for it. Until an admin accepts the new id, Fairy asks it for no approval. Nothing was sent.":                                                                                                                                                                                                                    "PC-A shows another machine id than the one Fairy has on record for it",
		"agent unreachable: timeout": "agent unreachable: timeout",
		"PC-E did not accept the approval (PC-E says: approval refused (clock): the token was issued " + strings.Repeat("y", 300): "PC-E did not accept the approval (clock)",
	} {
		if got := shortWhy(apierr.New(409, in)); got != want {
			t.Errorf("%q:\n got %q\nwant %q", in, got, want)
		}
	}
	if got := shortWhy(fmt.Errorf("something else")); got != "it did not work" {
		t.Errorf("an error without a text for people: %q", got)
	}
}

func TestUpdateSummary(t *testing.T) {
	r := func(states ...string) (out []updateResult) {
		for i, s := range states {
			out = append(out, updateResult{Name: fmt.Sprintf("PC-%d", i), State: s})
		}
		return out
	}
	for want, c := range map[string]struct {
		res []updateResult
		ok  bool
	}{
		"Update started on 1 agent.":            {r(updStarted), true},
		"Update started on 3 agents.":           {r(updStarted, updStarted, updStarted), true},
		"2 started, 1 not.":                     {r(updStarted, updRefused, updStarted), false},
		"0 started, 3 not.":                     {r(updRefused, updNotSent, updOffline), false},
		"Update started on 1 agent, 2 skipped.": {r(updStarted, updSkipped, updSkipped), true},
		"1 started, 1 not, 1 skipped.":          {r(updStarted, updNotSent, updSkipped), false},
		"No update was started: 2 skipped.":     {r(updSkipped, updSkipped), false},
	} {
		msg, lines, ok := updateSummary(c.res)
		if msg != want || ok != c.ok || len(lines) != len(c.res) || strings.Contains(msg, "done") {
			t.Errorf("%v: %q ok %v, want %q ok %v", c.res, msg, ok, want, c.ok)
		}
	}
}
