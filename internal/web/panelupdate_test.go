package web

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
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

// stoppedRunner is the real runner's reading of the status file (panelupdate.Systemd.Status) with a button
// that can be pressed in a test.
type stoppedRunner struct {
	panelupdate.Systemd
	starts int
}

func (r *stoppedRunner) Ready() (bool, string)       { return true, "" }
func (r *stoppedRunner) Start(context.Context) error { r.starts++; return nil }

// r2-p4-02 (audit 5), the auditor's case-04: the button's run was stopped while it downloaded, and its status
// file says finished=false for ever. The page showed "Updating…" and the button answered 409 "already running"
// from then on. Now the page gets the run as "stopped" (finished), and the button starts a new one.
func TestAStoppedPanelUpdateDoesNotBlockTheButton(t *testing.T) {
	e := setup(t)
	path := filepath.Join(t.TempDir(), "status.json")
	started := time.Now().Add(-3 * time.Hour).UTC()
	file := fmt.Sprintf(`{"state":"downloading","from":"8-ddddddd","to":"9-ccccccc","message":"downloading fairy-linux-amd64",`+
		`"started_at":%q,"updated_at":%q,"finished":false}`, started.Format(time.RFC3339Nano), started.Add(time.Second).Format(time.RFC3339Nano))
	if err := os.WriteFile(path, []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	unit := panelupdate.ActivityNone
	f := &stoppedRunner{Systemd: panelupdate.Systemd{StatusPath: path, Active: func() panelupdate.Activity { return unit }}}
	e.s.PanelRunner = f
	e.s.Version = "8-ddddddd"
	e.s.PanelUpdates = updates.Static{Tag: "build-9", Version: "9-ccccccc", Build: model.P(9), Verified: true}
	fresh := e.cookie(controller(), 0)
	g := e.do("GET", "/api/admin/panel-update", nil, "Cookie", fresh)
	st, _ := g.body["status"].(map[string]any)
	msg, _ := st["message"].(string)
	if g.code != 200 || st["state"] != "stopped" || st["finished"] != true || !strings.HasPrefix(msg, "stopped before it finished (while downloading, ") {
		t.Fatal(string(g.raw))
	}
	if r := e.do("POST", "/api/admin/panel-update", map[string]any{}, "Cookie", fresh, "X-Requested-With", "fairy"); r.code != 202 || f.starts != 1 {
		t.Fatal("the button must work again", r.code, string(r.raw))
	}
	// a run that is seconds old: running while the unit runs, stopped as soon as it does not
	young := time.Now().Add(-5 * time.Second).UTC()
	file = fmt.Sprintf(`{"state":"restarting","from":"8-ddddddd","to":"9-ccccccc","message":"installing and restarting",`+
		`"started_at":%q,"updated_at":%q,"finished":false}`, young.Format(time.RFC3339Nano), young.Add(time.Second).Format(time.RFC3339Nano))
	if err := os.WriteFile(path, []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	unit = panelupdate.ActivityRunning
	if r := e.do("POST", "/api/admin/panel-update", map[string]any{}, "Cookie", fresh, "X-Requested-With", "fairy"); r.code != 409 ||
		!strings.Contains(string(r.raw), "already running") || f.starts != 1 {
		t.Fatal("a run that is alive", r.code, string(r.raw))
	}
	unit = panelupdate.ActivityNone
	g = e.do("GET", "/api/admin/panel-update", nil, "Cookie", fresh)
	if st, _ := g.body["status"].(map[string]any); st["state"] != "stopped" || st["finished"] != true {
		t.Fatal(string(g.raw))
	}
	// the panel's notification at its next start says what happened, once
	e.s.notePanelUpdateResult()
	e.s.notePanelUpdateResult()
	n := 0
	for _, ev := range e.s.Notes.List("x", func(notifications.Event) bool { return true }, notifications.Filter{}).Events {
		if ev.Kind == "panel_update_stopped" {
			n++
			if ev.Severity != notifications.Warning || !strings.HasPrefix(ev.Text, "panel update stopped before it finished (while restarting, ") {
				t.Fatalf("%+v", ev)
			}
		}
	}
	if n != 1 {
		t.Fatal("notifications for the stopped run:", n)
	}
}
