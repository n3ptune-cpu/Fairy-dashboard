package web

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/panelupdate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/updates"
)

type fakeRunner struct {
	mu     sync.Mutex
	starts int
	st     *panelupdate.Status
	ready  bool
}

func (f *fakeRunner) Ready() (bool, string) {
	if f.ready {
		return true, ""
	}
	return false, "one-time setup missing"
}
func (f *fakeRunner) Start(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	f.st = &panelupdate.Status{State: "downloading", StartedAt: time.Now()}
	return nil
}
func (f *fakeRunner) Status() (panelupdate.Status, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.st == nil {
		return panelupdate.Status{}, false
	}
	return *f.st, true
}

// The Update panel button: admins in a browser session, CSRF, a sign-in from the last 2 minutes (stricter than
// the 5-minute window of other control actions), audit; only to a newer release; never twice at once.
func TestUpdatePanelButton(t *testing.T) {
	e := setup(t)
	f := &fakeRunner{}
	e.s.PanelRunner = f
	e.s.Version = "8-ddddddd"
	e.s.PanelUpdates = updates.Static{Tag: "build-9", Version: "9-ccccccc", Build: model.P(9), Verified: true}
	csrf := []string{"X-Requested-With", "fairy"}
	post := func(cookie string, h ...string) resp {
		return e.do("POST", "/api/admin/panel-update", map[string]any{}, append([]string{"Cookie", cookie}, h...)...)
	}
	if r := post(e.cookie(viewer, 0), csrf...); r.code != 403 {
		t.Fatal("viewer", r.code)
	}
	fresh := e.cookie(controller(), 0)
	if r := post(fresh); r.code != 403 {
		t.Fatal("no CSRF header", r.code)
	}
	if r := post(fresh, csrf...); r.code != 409 || !strings.Contains(string(r.raw), "setup") {
		t.Fatal("not set up", r.code, string(r.raw))
	}
	f.ready = true
	// 2.5 minutes is fresh enough for power actions (3 min window) but not for updating the panel
	r := post(e.cookie(controller(), 150*time.Second), csrf...)
	if d, _ := r.body["detail"].(map[string]any); r.code != 401 || d["code"] != "step_up_required" || f.starts != 0 {
		t.Fatal("must ask for a fresh sign-in", r.code, string(r.raw))
	}
	if r := post(fresh, csrf...); r.code != 202 || f.starts != 1 {
		t.Fatal("start", r.code, string(r.raw))
	}
	if r := post(fresh, csrf...); r.code != 409 || f.starts != 1 {
		t.Fatal("twice at once", r.code)
	}
	g := e.do("GET", "/api/admin/panel-update", nil, "Cookie", fresh)
	if g.code != 200 || g.body["ready"] != true || g.body["behind"] != true || g.body["status"].(map[string]any)["state"] != "downloading" {
		t.Fatal(string(g.raw))
	}
	f.st.Finished, f.st.State = true, "done"
	e.s.Version = "9-ccccccc"
	if r := post(fresh, csrf...); r.code != 409 || f.starts != 1 {
		t.Fatal("same version must not be installed again", r.code)
	}
	if !strings.Contains(auditOutcomes(e.s.Cfg.AuditLog), "panel.update started") {
		t.Fatal("not audited")
	}
}
