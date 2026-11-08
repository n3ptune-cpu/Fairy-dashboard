package web

import (
	"github.com/n3ptune-cpu/Fairy/internal/apierr"
	"github.com/n3ptune-cpu/Fairy/internal/scheduler"
)

func (s *Server) getPoll(c *reqCtx) error {
	id := c.r.PathValue("id")
	if _, err := c.find(id); err != nil {
		return err
	}
	c.json(200, s.Poller.PollStatus(id))
	return nil
}

func (s *Server) putPoll(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	var body struct {
		IntervalS any `json:"interval_s"`
	}
	if err := c.decodeBody(&body); err != nil {
		return err
	}
	var ov *scheduler.Override
	var num *float64
	switch v := body.IntervalS.(type) {
	case float64:
		num = &v
		ov = &scheduler.Override{Seconds: v}
	case string:
		switch v {
		case "paused":
			ov = &scheduler.Override{Paused: true}
		case "default":
		default:
			return apierr.New(422, `interval_s must be seconds, "paused" or "default"`)
		}
	default:
		return apierr.New(422, `interval_s must be seconds, "paused" or "default"`)
	}
	u, err := c.controlGuard(s.pollCtl)
	if err != nil {
		return err
	}
	id := c.r.PathValue("id")
	if _, err := c.find(id); err != nil {
		return err
	}
	if num != nil && (*num < 1 || *num > 300) {
		c.audit(u, "poll.set", id, map[string]any{"interval_s": *num}, "error 422: out of range")
		return apierr.New(422, "interval_s must be between 1 and 300 seconds")
	}
	if err := s.Poller.SetPoll(id, ov); err != nil {
		return err
	}
	c.audit(u, "poll.set", id, map[string]any{"interval_s": body.IntervalS}, "ok")
	c.json(200, s.Poller.PollStatus(id))
	return nil
}
