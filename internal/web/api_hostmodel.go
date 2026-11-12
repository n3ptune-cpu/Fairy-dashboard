package web

import (
	"bytes"
	"io"
	"unicode/utf8"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// "Set model name" (the ⋯ menu of a host's page): an admin gives a host a model name of their own, which the
// page then shows in place of the model the machine reports. The name is the panel's (state.json,
// adminstate.SetHostModel); what the machine reports is not touched and comes back when the name is removed.
//
//	PUT /api/systems/{id}/custom-model   {"custom_model": "<name>"}   ("" removes it)
//
// Who may: an admin with a browser session, with the CSRF header and from the panel's own origin. It is a
// label and decides nothing, so it is guarded like the other thing an admin sets for everyone to see, the
// sign-in wallpaper (wallpaperAdmin): no "Confirm it's you", and the kill switch, which stops device control,
// does not stop it. API tokens are refused. Every change is in the audit log (host.model: old and new name).
//
// The name is checked here, whatever the page did before sending it (adminstate.ModelName).

// customModelAdmin: an admin's browser session (API tokens: 403), the CSRF header, the panel's own origin.
func (c *reqCtx) customModelAdmin() (*model.User, error) {
	u, err := c.sessionCSRF()
	if err != nil {
		return nil, err
	}
	if _, g, _ := c.grant(); !g.Admin() {
		return nil, c.deny(u, 403, "only admins can set a host's model name", "not an admin")
	}
	return u, nil
}

// customModelBody reads the request's {"custom_model": "..."}: at most 64 KiB like every JSON body, valid
// UTF-8 as sent (the JSON decoder would turn other bytes into U+FFFD without a word), one reading only (no
// field twice or under another spelling), no other field.
func (c *reqCtx) customModelBody() (string, error) {
	raw, err := io.ReadAll(io.LimitReader(c.r.Body, 64<<10))
	if err != nil {
		return "", apierr.New(400, "unreadable body")
	}
	if !utf8.Valid(raw) {
		return "", apierr.New(422, "invalid request body: not valid UTF-8")
	}
	if !strictBody(raw) {
		return "", apierr.New(422, "invalid request body")
	}
	var b struct {
		CustomModel *string `json:"custom_model"`
	}
	c.r.Body = io.NopCloser(bytes.NewReader(raw))
	if err := c.decodeBody(&b); err != nil {
		return "", err
	}
	if b.CustomModel == nil {
		return "", apierr.New(422, "custom_model must be a text")
	}
	return *b.CustomModel, nil
}

func (s *Server) customModelPut(c *reqCtx) error {
	u, err := c.customModelAdmin()
	if err != nil {
		return err
	}
	if !s.adminRL.allow(u.Sub) {
		return c.limited(u, s.adminRL, "too many changes")
	}
	raw, err := c.customModelBody()
	if err != nil {
		return err
	}
	id := c.r.PathValue("id")
	x, err := c.find(id)
	if err != nil {
		return err
	}
	if x.Kind != "node" && x.Kind != "agent" {
		return apierr.New(409, "only hosts have a model name")
	}
	name, err := adminstate.ModelName(raw)
	if err != nil {
		return apierr.New(422, err.Error())
	}
	prev, err := s.Poller.Admin.SetHostModel(id, name)
	if err != nil {
		c.audit(u, "host.model", id, map[string]any{"model": name}, "failed: not saved")
		return err
	}
	if prev != name { // setting the name it has, or removing none, is not a change
		c.audit(u, "host.model", id, map[string]any{"model": name, "before": prev}, "ok")
		s.Poller.Bump() // the system list carries the name: every open page gets it with the next snapshot
	}
	reported := ""
	if x.Hardware != nil {
		reported = model.Deref(x.Hardware.Display)
	}
	msg := x.Name + ": model name set"
	if name == "" {
		msg = x.Name + ": model name removed"
	}
	strOrNull := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	c.json(200, map[string]any{"ok": true, "message": msg, "custom_model": strOrNull(name), "reported_model": strOrNull(reported)})
	return nil
}
