package web

import (
	"github.com/n3ptune-cpu/Fairy/internal/powerctl"
)

func (s *Server) powerCaps(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	x, err := c.find(c.r.PathValue("id"))
	if err != nil {
		return err
	}
	c.json(200, powerctl.Capabilities(s.Poller, x))
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
	who := deref(u.Name)
	if who == "" {
		who = deref(u.Email)
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
