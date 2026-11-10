package web

import (
	"encoding/json"
	"fmt"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

func (c *reqCtx) fanAgent(id string) (config.Agent, error) {
	if _, err := c.find(id); err != nil {
		return config.Agent{}, err
	}
	a, ok := c.s.Poller.AgentFor(id)
	if !ok || a.FanControl == nil {
		return config.Agent{}, apierr.New(404, "fan control is not configured for this system")
	}
	return a, nil
}

func (c *reqCtx) agentCall(a config.Agent, method, path string, body any) (map[string]any, error) {
	status, data, err := c.s.Poller.AgentRequest(c.r.Context(), a, method, path, body)
	if err != nil {
		return nil, apierr.New(502, "agent unreachable: "+devices.Describe(err))
	}
	if status == 401 { // never surface as our own 401 (that means "log in again")
		return nil, apierr.New(apierr.Upstream, "agent rejected the control token (check control_token_env)")
	}
	if status == 503 && (jsonx.S(data, "code") == api.ReasonHelperStopped || jsonx.S(data, "code") == api.ReasonHelperError) {
		return nil, apierr.New(409, fanHelperStopped) // the helper stopped since the last poll: not an agent failure
	}
	if status >= 400 { // the agent answered: its message must reach the user (no 502, see apierr.Upstream)
		st := apierr.Upstream
		switch status {
		case 400, 404, 409, 422, 429:
			st = status
		}
		msg := "agent error"
		if e, ok := data["error"]; ok {
			msg = fmt.Sprint(e)
		}
		return nil, apierr.New(st, msg)
	}
	return data, nil
}

func (s *Server) fanStatus(c *reqCtx) error {
	id := c.r.PathValue("id")
	a, err := c.fanAgent(id)
	if err != nil {
		return err
	}
	if x, _ := c.find(id); helperPaused(x, "fans") { // the agent cannot reach the BMC without its helper
		fs := model.FanStatus{SystemID: id, Error: model.P(fanHelperStopped), Available: model.P(false), Reason: api.ReasonHelperStopped}
		if x.Helper.State == api.HelperError {
			fs.Reason = api.ReasonHelperError
		}
		model.Normalize(&fs)
		c.json(200, fs)
		return nil
	}
	data, err := c.agentCall(a, "GET", "/fan/status", nil)
	if err != nil {
		return err
	}
	if len(data) == 0 { // the agent answered `null`, `{}` or nothing: no fan status (and a nil map cannot be written to)
		return apierr.New(apierr.Upstream, "agent returned an invalid fan status")
	}
	data["system_id"] = id
	b, _ := json.Marshal(data)
	var fs model.FanStatus
	if err := json.Unmarshal(b, &fs); err != nil {
		return apierr.New(apierr.Upstream, "agent returned an invalid fan status")
	}
	model.Normalize(&fs)
	c.json(200, fs)
	return nil
}

// fanHelperStopped is why fan control is unavailable while the host's elevated helper is not running.
const fanHelperStopped = "the elevated helper is stopped: the fans are on the BMC's own automatic control. Start the helper to set fan modes again."

type fanModeReq struct {
	Mode string `json:"mode"`
}

type fanDutyReq struct {
	Zone    *int `json:"zone"`
	Percent *int `json:"percent"`
}

func (s *Server) fanWrite(c *reqCtx, action string, body any, params map[string]any) error {
	u, err := c.controlGuard(s.control)
	if err != nil {
		return err
	}
	id := c.r.PathValue("id")
	a, err := c.fanAgent(id)
	if err != nil {
		return err
	}
	if x, _ := c.find(id); helperPaused(x, "fans") {
		err := apierr.New(409, fanHelperStopped)
		c.audit(u, action, id, params, errorOutcome(err))
		return err
	}
	path := "/fan/mode"
	if action == "fan.duty" {
		path = "/fan/duty"
	}
	data, err := c.agentCall(a, "POST", path, body)
	if err != nil {
		c.audit(u, action, id, params, errorOutcome(err))
		return err
	}
	c.audit(u, action, id, params, "ok")
	if data == nil { // the agent answered `null`: the result stays an object
		data = map[string]any{}
	}
	c.json(200, model.ControlResult{OK: true, Result: data})
	return nil
}

func (s *Server) fanMode(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	var b fanModeReq
	if err := c.decodeBody(&b); err != nil {
		return err
	}
	switch b.Mode {
	case "standard", "full", "optimal", "heavyio":
	default:
		return apierr.New(422, "mode must be standard, full, optimal or heavyio")
	}
	return s.fanWrite(c, "fan.mode", b, map[string]any{"mode": b.Mode})
}

// FanDutyFloor is the lowest fan duty (%) the panel sends, whatever the agent's own minimum: a wrong click
// or an old agent must never stop the fans of a server.
const FanDutyFloor = 30

func (s *Server) fanDuty(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	var b fanDutyReq
	if err := c.decodeBody(&b); err != nil {
		return err
	}
	if b.Zone == nil || (*b.Zone != 0 && *b.Zone != 1) || b.Percent == nil || *b.Percent < FanDutyFloor || *b.Percent > 100 {
		return apierr.New(422, fmt.Sprintf("zone must be 0 or 1 and percent %d-100 (the panel never sets fans below %d%%)",
			FanDutyFloor, FanDutyFloor))
	}
	return s.fanWrite(c, "fan.duty", map[string]any{"zone": *b.Zone, "percent": *b.Percent},
		map[string]any{"zone": *b.Zone, "percent": *b.Percent})
}
