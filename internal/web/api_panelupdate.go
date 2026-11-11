package web

import (
	"fmt"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/release"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/panelupdate"
)

// The Updates page's "Update panel" button. Admins in a browser session only (no API tokens), CSRF, the kill
// switch, a Pocket ID sign-in from the last 2 minutes (always, also inside the usual step-up window), rate
// limit, audit (panel.update). The panel itself only asks systemd to start fairy-panel-update.service, which
// takes no input from the request; that unit checks the signed release and refuses older ones.

// panelUpdateFresh is how recent the sign-in must be for the panel's own update.
const panelUpdateFresh = 120

type panelUpdatePage struct {
	Ready   bool                `json:"ready"`
	Why     string              `json:"why,omitempty"`
	Version string              `json:"version"` // running now
	Behind  *bool               `json:"behind"`
	Status  *panelupdate.Status `json:"status"` // the last run (nil: never)
}

func (s *Server) panelUpdatePage() panelUpdatePage {
	p := panelUpdatePage{Version: s.Version}
	if s.PanelRunner == nil {
		p.Why = "the panel cannot update itself here (not a Linux panel host)"
	} else {
		p.Ready, p.Why = s.PanelRunner.Ready()
		if st, ok := s.PanelRunner.Status(); ok {
			p.Status = &st
		}
	}
	if n, ok := release.BuildNumber(s.Version); ok {
		if l := s.panelLatest(); l.Build != nil {
			b := n < *l.Build
			p.Behind = &b
		}
	}
	return p
}

func (s *Server) panelUpdateGet(c *reqCtx) error {
	if _, err := c.requireAdmin("the panel's update"); err != nil {
		return err
	}
	c.json(200, s.panelUpdatePage())
	return nil
}

func (s *Server) panelUpdatePost(c *reqCtx) error {
	u, err := c.sessionCSRF()
	if err != nil {
		return err
	}
	if _, g, _ := c.grant(); !g.Admin() {
		return c.deny(u, 403, "only admins can update the panel", "panel update denied")
	}
	if off := s.Poller.Admin.ControlDisabled(); off != nil {
		return c.deny(u, 423, "control is disabled by an administrator ("+off.By+")", "control disabled (kill switch)")
	}
	if err := c.stepUpWithin(u, "updating the panel", panelUpdateFresh); err != nil {
		return err
	}
	if !s.control.allow(u.Sub) {
		return apierr.New(429, "too many control requests; slow down")
	}
	p := s.panelUpdatePage()
	params := map[string]any{"from": s.Version, "latest": s.panelLatest().Version}
	switch {
	case !p.Ready:
		return apierr.New(409, p.Why)
	case p.Status != nil && !p.Status.Finished:
		return apierr.New(409, "an update of the panel is already running")
	case p.Behind == nil || !*p.Behind:
		return apierr.New(409, "no newer signed panel release is known (the panel never installs an older or the same one)")
	}
	if a, _ := s.WakeRuns.Snapshot(); a != nil { // the update restarts the panel, and a run is never continued after a restart
		return apierr.Errf(409, "\"Update machines that are off\" is running (started by %s): the panel's update would interrupt it. "+
			"Wait for it, or cancel it on the Updates page.", a.By)
	}
	if err := s.PanelRunner.Start(c.r.Context()); err != nil {
		c.audit(u, "panel.update", "panel", params, "error: "+err.Error())
		return apierr.New(502, err.Error())
	}
	c.audit(u, "panel.update", "panel", params, "started")
	s.Notes.Add(notifications.Event{Kind: "panel_update_started", Type: "update", Severity: notifications.Info,
		Host: "Fairy panel", Text: fmt.Sprintf("panel update started by %s (%v to %v)", actorOf(u), params["from"], params["latest"])})
	c.json(202, map[string]any{"ok": true, "message": "panel update started: it restarts in a minute or two"})
	return nil
}

// notePanelUpdateResult makes one notification for the last finished run of the panel's update (called at
// start: the panel restarted into the new version, or the old one after a rollback).
func (s *Server) notePanelUpdateResult() {
	if s.PanelRunner == nil {
		return
	}
	st, ok := s.PanelRunner.Status()
	if !ok || !st.Finished || st.State == "up_to_date" || time.Since(st.UpdatedAt) > 24*time.Hour {
		return
	}
	key := "panelupdate:" + st.StartedAt.UTC().Format(time.RFC3339Nano)
	if s.Notes.HasKey(key) {
		return
	}
	e := notifications.Event{Kind: "panel_updated", Type: "update", Severity: notifications.Info, Host: "Fairy panel", Key: key,
		Text: "panel updated to " + st.To}
	switch st.State {
	case "rolled_back":
		e.Kind, e.Severity, e.Text = "panel_update_rolled_back", notifications.Warning, "panel update rolled back: "+st.Message
	case "failed":
		e.Kind, e.Severity, e.Text = "panel_update_failed", notifications.Warning, "panel update failed: "+st.Message
	}
	s.Notes.Add(e)
}
