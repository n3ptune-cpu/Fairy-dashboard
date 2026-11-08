package web

import (
	"github.com/n3ptune-cpu/Fairy/internal/model"
	"github.com/n3ptune-cpu/Fairy/internal/powerctl"
)

func (s *Server) powerCaps(c *reqCtx) error {
	x, err := c.find(c.r.PathValue("id"))
	if err != nil {
		return err
	}
	caps := powerctl.Capabilities(s.Poller, x)
	if _, g, _ := c.grant(); !g.Admin() { // read-only: no actions, and no relay / panel-host details
		why := "read-only: only admins can use power actions"
		for i := range caps.Actions {
			a := &caps.Actions[i]
			a.Available, a.Reason, a.Via, a.ViaLabel = false, model.P(why), nil, nil
		}
		caps.KillsPanel, caps.Warning = false, nil
	}
	c.json(200, caps)
	return nil
}

func (s *Server) powerAction(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	var req powerctl.Request
	if err := c.decodeBody(&req); err != nil {
		return err
	}
	if err := req.Validate(); err != nil {
		return err
	}
	u, err := c.controlGuard(s.control)
	if err != nil {
		return err
	}
	id := c.r.PathValue("id")
	x, err := c.find(id)
	if err != nil {
		return err
	}
	params := map[string]any{"action": req.Action, "delay_s": *req.DelayS}
	who := model.Deref(u.Name)
	if who == "" {
		who = model.Deref(u.Email)
	}
	if who == "" {
		who = u.Sub
	}
	res, err := powerctl.Execute(c.r.Context(), s.Poller, x, req, who)
	if err != nil {
		c.audit(u, "power."+req.Action, id, params, errorOutcome(err))
		return err
	}
	params["via"] = res.Via
	c.audit(u, "power."+req.Action, id, params, "ok")
	c.json(200, res)
	return nil
}
