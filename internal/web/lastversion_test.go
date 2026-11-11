package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/updates"
)

func agentRows(t *testing.T, e *env, cookie string) (map[string]map[string]any, resp) {
	t.Helper()
	r := e.do("GET", "/api/admin/updates", nil, "Cookie", cookie)
	if r.code != 200 {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	rows := map[string]map[string]any{}
	for _, x := range r.body["agents"].([]any) {
		rows[x.(map[string]any)["id"].(string)] = x.(map[string]any)
	}
	return rows, r
}

// The Updates page after a restart of the panel: a host that is off has the version its agent reported
// before, marked as last seen, and nothing that would send it an update; a host that was never on has none;
// a host that is on is as it was. The same mark is on a host that goes off while the panel runs.
func TestUpdatesPageKeepsTheLastSeenVersionOfAHostThatIsOff(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	latest := updates.Static{Tag: "build-42", Version: "42-bbbbbbb", Build: model.P(42), Verified: true}
	e.s.Updates = latest
	versioned := func(v string) func() (jsonx.M, error) {
		return func() (jsonx.M, error) {
			m := testutil.AgentMetrics()
			m["agent_version"] = v
			m["features"].(jsonx.M)["self_update"] = true
			m["self_update"] = jsonx.M{"state": "idle", "since": 1.0}
			return m, nil
		}
	}
	e.b.Mu.Lock()
	e.b.Metrics["laptop"] = versioned("41-aaaaaaa")
	e.b.Mu.Unlock()
	e.s.Poller.PollOnce(ctx)
	rows, _ := agentRows(t, e, e.cookie(controller(), 0))
	if l := rows["laptop"]; l["status"] != "online" || l["version"] != "41-aaaaaaa" || l["behind"] != true || l["can_update"] != true {
		t.Fatalf("while it is on: %v", l)
	}
	if _, has := rows["laptop"]["version_last_seen"]; has {
		t.Fatalf("a host that answers is not marked: %v", rows["laptop"])
	}
	e.srv.Close()

	// the panel starts again on the same data folder; the laptop is off
	b2 := testutil.NewBackend()
	b2.Metrics["laptop"] = b2.Metrics["tower"]
	secrets := config.Secrets{SessionSecret: strings.Repeat("s", 48)}
	p2, err := poller.New(e.s.Cfg, secrets, b2)
	if err != nil {
		t.Fatal(err)
	}
	p2.PollOnce(ctx)
	s2 := New(e.s.Cfg, secrets, p2, nil, "test")
	s2.Updates = latest
	srv2 := httptest.NewServer(s2.Handler())
	t.Cleanup(srv2.Close)
	e2 := &env{t: t, s: s2, srv: srv2, b: b2}
	admin := e2.cookie(controller(), 0)
	rows, page := agentRows(t, e2, admin)
	l := rows["laptop"]
	if l["status"] != "offline" || l["version"] != "41-aaaaaaa" || l["build"] != 41.0 || l["version_last_seen"] != true {
		t.Fatalf("after the restart, off: %v", l)
	}
	// nothing is offered that cannot run: no Update for it, alone or with the others
	if l["can_update"] != false || l["why"] != "agent offline" || l["self_update"] != nil || l["agent_enabled"] != false {
		t.Fatalf("after the restart, off: %v", l)
	}
	// that it was older than the latest release when last seen is said (the page words it so), and counted as before
	if l["behind"] != true || page.body["behind"] != 1.0 {
		t.Fatalf("after the restart, off: behind %v, page %v", l["behind"], page.body["behind"])
	}
	if tw := rows["tower"]; tw["version"] != nil || tw["build"] != nil || tw["behind"] != nil {
		t.Fatalf("a host that was never on: %v", tw)
	}
	if _, has := rows["tower"]["version_last_seen"]; has {
		t.Fatalf("a host without a version is not marked: %v", rows["tower"])
	}
	r := e2.do("POST", "/api/admin/updates", map[string]any{}, "Cookie", admin, "X-Requested-With", "fairy", "Content-Type", "application/json")
	if r.code != 200 || !strings.Contains(string(r.raw), `"error":"skipped: agent offline"`) {
		t.Fatalf("update all: %d %s", r.code, r.raw)
	}
	b2.Mu.Lock()
	for _, q := range b2.Requests {
		if strings.Contains(q, "/update") {
			t.Errorf("an update was sent to a host that is off: %s", q)
		}
	}
	b2.Mu.Unlock()

	// it comes back, updated by hand meanwhile: its own answer counts, unmarked
	b2.Mu.Lock()
	b2.Metrics["laptop"] = versioned("42-bbbbbbb")
	b2.Mu.Unlock()
	p2.PollOnce(ctx)
	rows, _ = agentRows(t, e2, admin)
	if l := rows["laptop"]; l["status"] != "online" || l["version"] != "42-bbbbbbb" || l["behind"] != false {
		t.Fatalf("back on: %v", l)
	}
	if _, has := rows["laptop"]["version_last_seen"]; has {
		t.Fatalf("back on, still marked: %v", rows["laptop"])
	}
	// and off again while the panel runs: the same mark as after a restart
	b2.Mu.Lock()
	b2.Metrics["laptop"] = b2.Metrics["tower"]
	b2.Mu.Unlock()
	p2.PollOnce(ctx)
	rows, _ = agentRows(t, e2, admin)
	if l := rows["laptop"]; l["status"] != "offline" || l["version"] != "42-bbbbbbb" || l["version_last_seen"] != true || l["can_update"] != false {
		t.Fatalf("off while the panel runs: %v", l)
	}
}

// The page: a version that is only "last seen" is marked on the Updates page, on the host list and host page
// (the chip), has no green or amber colour of its own, no "update available" arrow, and is never called what
// the host "runs".
func TestUIMarksAVersionThatIsOnlyLastSeen(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`const vcls = r.version_last_seen ? "" : r.behind === true ? "warn" : r.behind === false ? "ok" : "";`,
		"const vnote = r.version_last_seen ? `<div class=\"note\" title=\"${esc(LAST_SEEN_TIP)}\">last seen${r.behind === true ? \" · older than the latest release\" : \"\"}</div>`",
		`: r.behind === true ? '<div class="note">update available</div>' : "";`,
		`<td><span class="chip ${vcls}" title="${esc(r.version || "")}">${esc(cur)}</span>${vnote}</td>`,
		`const stale = !!r.version_last_seen;`,
		`const arrow = (r.behind === true && !stale) || updBusy(r);`,
		"${esc(r.version ? `${buildLabel(r.version)}${stale ? \" (last seen)\" : \"\"}` : \"agent ?\")}",
		`${r.version_last_seen ? "last reported" : "runs"} ${esc(buildLabel(r.version))}.`,
		// the Update button and "Update all" still go by can_update, which is false for a host that does not answer
		`${r.can_update && !why && !busy ? "" : "disabled"}`,
		`const ready = d.agents.filter((r) => r.can_update);`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
}
