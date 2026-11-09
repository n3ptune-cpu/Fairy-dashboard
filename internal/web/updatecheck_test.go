package web

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/demo"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/updates"
)

// fakeSource is a release source that counts Check for updates calls.
type fakeSource struct {
	l     updates.Latest
	err   string
	calls atomic.Int32
}

func (f *fakeSource) Latest() updates.Latest { return f.l }
func (f *fakeSource) CheckNow(context.Context) updates.Latest {
	f.calls.Add(1)
	l := f.l
	if f.err != "" {
		l.Error = f.err
		return l
	}
	now := time.Now().UTC()
	l.CheckedAt = &now
	f.l = l
	return l
}

// Check for updates: admins in a browser session, CSRF header, no step-up (read-only), once per 30 s for the
// whole panel, audited as update_check; answers with the refreshed Updates page and a short result.
func TestUpdateCheck(t *testing.T) {
	e := setup(t)
	agent := &fakeSource{l: updates.Latest{Tag: "build-42", Version: "42-bbbbbbb", Build: model.P(42), Verified: true}}
	panel := &fakeSource{l: updates.Latest{Tag: "build-9", Version: "9-ccccccc", Build: model.P(9), Verified: true}}
	e.s.Updates, e.s.PanelUpdates = agent, panel
	e.s.Version = "9-ccccccc" // the panel is on the latest release
	csrf := []string{"X-Requested-With", "fairy"}
	post := func(h ...string) resp { return e.do("POST", "/api/admin/update-check", map[string]any{}, h...) }
	calls := func() int32 { return agent.calls.Load() + panel.calls.Load() }

	if r := post(); r.code != 401 {
		t.Fatal("anonymous", r.code)
	}
	if r := post(append([]string{"Cookie", e.cookie(viewer, 0)}, csrf...)...); r.code != 403 {
		t.Fatal("viewer", r.code, string(r.raw))
	}
	// a sign-in from hours ago is fine: checking is read-only (no step-up)
	admin := e.cookie(controller(), 3*time.Hour)
	if r := post("Cookie", admin); r.code != 403 || !strings.Contains(string(r.raw), "X-Requested-With") {
		t.Fatal("no CSRF header", r.code, string(r.raw))
	}
	if r := post(append([]string{"Cookie", admin, "Origin", "https://evil.example"}, csrf...)...); r.code != 403 {
		t.Fatal("cross-origin", r.code)
	}
	if r := post(append([]string{"Authorization", "Bearer fy_nope"}, csrf...)...); r.code != 403 {
		t.Fatal("API tokens are refused", r.code)
	}
	if r := e.do("GET", "/api/admin/update-check", nil, "Cookie", admin); r.code == 200 {
		t.Fatal("GET", r.code)
	}
	if calls() != 0 {
		t.Fatal("a refused request reached GitHub")
	}

	r := post(append([]string{"Cookie", admin}, csrf...)...)
	if r.code != 200 || r.body["ok"] != true || calls() != 2 {
		t.Fatal("check", r.code, string(r.raw))
	}
	page, _ := r.body["page"].(map[string]any)
	if page == nil || page["latest"].(map[string]any)["checked_at"] == nil || page["last_check"] == nil || page["check_retry_s"].(float64) < 29 || page["check_running"] != false {
		t.Fatal("page", string(r.raw))
	}
	wantMsg := "Up to date"
	if page["behind"].(float64) > 0 { // the test agents may run an older build
		wantMsg = "available"
	}
	if !strings.Contains(r.body["message"].(string), wantMsg) {
		t.Fatal(r.body["message"], string(r.raw))
	}

	// once per 30 s for the whole panel, also for another admin
	other := controller()
	other.Sub = "u3"
	otherCookie := e.cookie(other, 0)
	if _, _, err := e.s.Access.SetRole(other.Sub, access.RoleAdmin, nil, "test", true); err != nil {
		t.Fatal(err)
	}
	r = post(append([]string{"Cookie", otherCookie}, csrf...)...)
	d, _ := r.body["detail"].(map[string]any)
	if r.code != 429 || d["code"] != "update_check_too_soon" || d["retry_after_s"].(float64) < 1 || r.hdr.Get("Retry-After") == "" || calls() != 2 {
		t.Fatal("rate limit", r.code, string(r.raw))
	}
	g := e.do("GET", "/api/admin/updates", nil, "Cookie", admin)
	if g.code != 200 || g.body["last_check"] == nil || g.body["check_retry_s"].(float64) < 1 || g.body["check_running"] != false {
		t.Fatal("page shows the last check", string(g.raw))
	}

	// 30 s later: allowed again; the panel is behind now; GitHub fails for the agent's repo
	e.s.checks.mu.Lock()
	e.s.checks.last = time.Now().Add(-31 * time.Second)
	e.s.checks.mu.Unlock()
	e.s.Version = "8-ddddddd"
	agent.err = "cannot check GitHub: GitHub refused the request (HTTP 403, rate limit?)"
	r = post(append([]string{"Cookie", admin}, csrf...)...)
	if r.code != 200 || r.body["ok"] != false || !strings.Contains(r.body["message"].(string), "Fairy-agent: cannot check GitHub") || calls() != 4 {
		t.Fatal("error", r.code, string(r.raw))
	}
	if r.body["page"].(map[string]any)["panel_behind"] != true {
		t.Fatal("panel behind", string(r.raw))
	}

	// a fixed answer still works; and the check off answers 409 without reaching anything
	e.s.checks.mu.Lock()
	e.s.checks.last = time.Time{}
	e.s.checks.mu.Unlock()
	agent.err = ""
	e.s.Updates = updates.Static{Tag: "build-42", Version: "42-bbbbbbb", Build: model.P(42)}
	e.s.PanelUpdates = nil
	if r := post(append([]string{"Cookie", admin}, csrf...)...); r.code != 200 || r.body["ok"] != true {
		t.Fatal("static", r.code, string(r.raw))
	}
	e.s.Updates = nil
	e.s.checks.mu.Lock()
	e.s.checks.last = time.Time{}
	e.s.checks.mu.Unlock()
	if r := post(append([]string{"Cookie", admin}, csrf...)...); r.code != 409 || !strings.Contains(string(r.raw), "check_latest") {
		t.Fatal("check off", r.code, string(r.raw))
	}

	log := auditOutcomes(e.s.Cfg.AuditLog)
	if strings.Count(log, "update_check ok") != 2 || strings.Count(log, "update_check error: Fairy-agent: cannot check GitHub") != 1 {
		t.Fatal("audit", log)
	}
	if !strings.Contains(log, "auth.denied 403: update check denied") {
		t.Fatal("the viewer's attempt is not audited", log)
	}
}

// Demo mode: Check for updates works without GitHub, takes a moment, and moves "last checked".
func TestUpdateCheckDemo(t *testing.T) {
	e := setup(t)
	u := controller()
	u.Auth = "demo"
	e.s.DemoUser = &u
	agent, panel := demo.Latest(), demo.PanelLatest()
	agent.Delay, panel.Delay = 50*time.Millisecond, 50*time.Millisecond
	e.s.Updates, e.s.PanelUpdates = agent, panel
	before := *agent.Latest().CheckedAt
	start := time.Now()
	r := e.do("POST", "/api/admin/update-check", map[string]any{}, "X-Requested-With", "fairy")
	if r.code != 200 || r.body["ok"] != true || time.Since(start) < 50*time.Millisecond {
		t.Fatal(r.code, string(r.raw))
	}
	if !agent.Latest().CheckedAt.After(before) || !strings.Contains(auditOutcomes(e.s.Cfg.AuditLog), "update_check ok") {
		t.Fatal("not recorded", agent.Latest())
	}
	if r := e.do("POST", "/api/admin/update-check", map[string]any{}, "X-Requested-With", "fairy"); r.code != 429 {
		t.Fatal("demo is rate limited too", r.code)
	}
}
