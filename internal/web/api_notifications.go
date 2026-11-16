package web

import (
	"strconv"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
)

// The Notifications page (everyone with a role): events newest first, filtered by severity, type and host,
// read / unread and dismissed per user. A viewer sees only events of the systems and UPSes of their sites;
// events without a system (Add host failures, removed hosts) are for admins.

func (s *Server) notificationRoutes() {
	s.mux.HandleFunc("GET /api/notifications", s.h(s.notificationsList))
	s.mux.HandleFunc("GET /api/notifications/count", s.h(s.notificationsCount))
	s.mux.HandleFunc("POST /api/notifications/read", s.h(s.notificationsRead))
	s.mux.HandleFunc("POST /api/notifications/dismiss", s.h(s.notificationsDismiss))
	s.mux.HandleFunc("GET /api/system-sites", s.h(s.systemSites))
}

// visibleEvents is what this request's user may see, with the view that decides it.
func (c *reqCtx) visibleEvents() (func(notifications.Event) bool, *view, string, error) {
	u, err := c.requireUser()
	if err != nil {
		return nil, nil, "", err
	}
	v, err := c.view()
	if err != nil {
		return nil, nil, "", err
	}
	return eventVisibility(v), v, u.Sub, nil
}

// plugNoteKind is the kind of the note of a switched socket (plugSwitch).
const plugNoteKind = "plug_switched"

// noteHost is the `host` of an event as the user of this view gets it. For every kind of event but one it is
// the name of the event's own machine, which that user sees (eventVisibility). The note of a switched socket
// is the machine's that hangs ON the socket, and has as host the machine that READS the plug: another machine,
// which can be in a site this user may not see. Somebody who does not see every site gets the name of the
// machine on the socket there, as that row is called now (third review, finding 4). An admin, and everybody
// else who sees every site, gets the event as it is.
func noteHost(v *view, e notifications.Event) string {
	if v.all || e.Kind != plugNoteKind {
		return e.Host
	}
	x, _ := v.Find(e.SystemID) // a row this user sees, or the event would not be theirs; "" if it is gone
	return x.Name
}

// eventVisibility decides which events a view's user may see.
func eventVisibility(v *view) func(notifications.Event) bool {
	if v.all {
		return func(notifications.Event) bool { return true }
	}
	return func(e notifications.Event) bool {
		switch {
		case e.SystemID != "":
			return v.vis[e.SystemID]
		case e.UPSID != "":
			return v.upsVis[e.UPSID]
		}
		return false
	}
}

func (s *Server) notificationsList(c *reqCtx) error {
	vis, v, sub, err := c.visibleEvents()
	if err != nil {
		return err
	}
	q := c.r.URL.Query()
	f := notifications.Filter{Severity: q.Get("severity"), Type: q.Get("type"), Host: q.Get("host"), Unread: q.Get("unread") == "1"}
	if !notifications.ValidSeverity(f.Severity) || !notifications.ValidType(f.Type) || len(f.Host) > 80 {
		return apierr.New(422, "invalid filter")
	}
	if f.Limit, err = queryInt(q, "limit", 50, 1, 200); err != nil {
		return err
	}
	if b := q.Get("before"); b != "" {
		if f.Before, err = strconv.ParseInt(b, 10, 64); err != nil || f.Before < 1 {
			return apierr.New(422, "before must be an event id")
		}
	}
	p := s.Notes.List(sub, vis, f)
	for i := range p.Events {
		p.Events[i].Host = noteHost(v, p.Events[i].Event)
	}
	c.json(200, p)
	return nil
}

func (s *Server) notificationsCount(c *reqCtx) error {
	vis, _, sub, err := c.visibleEvents()
	if err != nil {
		return err
	}
	unread, active := s.Notes.Counts(sub, vis)
	c.json(200, map[string]int{"unread": unread, "active_alerts": active})
	return nil
}

type notesBody struct {
	IDs []int64 `json:"ids"`
	All bool    `json:"all"`
}

func (c *reqCtx) notesWrite() (string, notesBody, error) {
	var b notesBody
	u, err := c.requireUser()
	if err != nil {
		return "", b, err
	}
	if err := c.checkCSRF(u); err != nil {
		return "", b, err
	}
	if _, err := c.view(); err != nil { // pending users have nothing to mark
		return "", b, err
	}
	if err := c.decodeBody(&b); err != nil {
		return "", b, err
	}
	if len(b.IDs) > 500 {
		return "", b, apierr.New(422, "at most 500 ids")
	}
	return u.Sub, b, nil
}

func (s *Server) notificationsRead(c *reqCtx) error {
	sub, b, err := c.notesWrite()
	if err != nil {
		return err
	}
	s.Notes.MarkRead(sub, b.IDs, b.All)
	c.json(200, map[string]any{"ok": true})
	return nil
}

func (s *Server) notificationsDismiss(c *reqCtx) error {
	sub, b, err := c.notesWrite()
	if err != nil {
		return err
	}
	s.Notes.Dismiss(sub, b.IDs)
	c.json(200, map[string]any{"ok": true})
	return nil
}

// systemSites is the site of every visible host and guest (the UI groups the sidebar and Overview by site).
func (s *Server) systemSites(c *reqCtx) error {
	v, err := c.view()
	if err != nil {
		return err
	}
	data := s.Access.Load()
	sites := s.sites(data, v.snap)
	out := map[string]string{}
	for _, x := range v.Systems() {
		if r := sites[x.ID].Site; r != "" {
			out[x.ID] = r
		}
	}
	c.json(200, map[string]any{"sites": out})
	return nil
}
