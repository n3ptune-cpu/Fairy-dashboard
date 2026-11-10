package web

import (
	"fmt"
	"slices"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
)

// Stop / Start of a Windows agent's elevated helper (Fairy-agent v0.5.0+): POST /api/systems/{id}/helper/stop and
// .../start with {"confirm": "<the host's name>"}. The same checks as a power action: admins only, CSRF, the
// control kill switch, a recent Pocket ID sign-in, the control rate limit, the typed name, and an audit entry
// ("helper_stop" / "helper_start", with the host and the user). The panel calls the agent's POST /helper/stop
// or /helper/start (api.PathHelperStop / PathHelperStart) with that agent's control token and body {}; the agent
// answers 200 with its helper block (api.HelperStatus, "state" at the top level), 409 {"error": "not_installed"},
// or 502 when the service manager refused.

// helperStates are the states the agent may answer with (anything else reads as "unknown").
var helperStates = map[string]bool{api.HelperRunning: true, api.HelperStopped: true, api.HelperStarting: true,
	api.HelperNotInstalled: true, api.HelperError: true}

// HelperResult is the answer to a helper Stop / Start.
type HelperResult struct {
	SystemID string `json:"system_id"`
	Action   string `json:"action"` // stop | start
	State    string `json:"state"`  // what the agent says the helper is doing now
	Message  string `json:"message"`
}

func (s *Server) helperStop(c *reqCtx) error  { return s.helperAction(c, "stop") }
func (s *Server) helperStart(c *reqCtx) error { return s.helperAction(c, "start") }

func (s *Server) helperAction(c *reqCtx, action string) error {
	u, err := c.controlGuard(s.control) // admins only (plus CSRF, kill switch, step-up, rate limit)
	if err != nil {
		return err
	}
	id := c.r.PathValue("id")
	x, err := c.find(id) // a host the user cannot see is "not found", as for power actions
	if err != nil {
		return err
	}
	auditAction := "helper_" + action
	params := map[string]any{"host": x.Name}
	fail := func(err error) error {
		c.audit(u, auditAction, id, params, errorOutcome(err))
		return err
	}
	a, ok := s.Poller.AgentByID(id)
	if !ok || x.Kind != "agent" {
		return fail(apierr.New(404, "this system has no Fairy agent"))
	}
	if x.Helper == nil {
		return fail(apierr.New(409, "this host's agent has no elevated helper (Windows, Fairy-agent v0.5.0 or newer)"))
	}
	if x.Helper.State == "not_installed" {
		return fail(apierr.New(409, "the elevated helper is not installed on this host"))
	}
	if !a.HasControl() {
		return fail(apierr.New(409, "no control token is configured for this agent (control_token_env)"))
	}
	if err := c.typedName(x.Name, map[string]string{"stop": "stopping its helper", "start": "starting its helper"}[action]); err != nil {
		return fail(err)
	}
	path := map[string]string{"stop": api.PathHelperStop, "start": api.PathHelperStart}[action]
	status, data, err := s.Poller.AgentRequest(c.r.Context(), a, "POST", path, map[string]any{}) // the agent logs "panel <ip>"
	switch {
	case err != nil:
		return fail(apierr.New(502, "agent unreachable: "+devices.Describe(err)))
	case status == 401: // never our own 401 (that means "sign in again")
		return fail(apierr.New(apierr.Upstream, "agent rejected the control token (check control_token_env)"))
	case status == 409 && jsonx.S(data, "error") == api.ErrHelperNotInstalled.Error():
		return fail(apierr.New(409, "the elevated helper is not installed on this host"))
	case status == 404:
		return fail(apierr.New(409, "this agent has no helper control (Fairy-agent v0.5.0 or newer on Windows)"))
	case status >= 400:
		st := apierr.Upstream
		if status == 400 || status == 409 || status == 422 || status == 429 {
			st = status
		}
		msg := textsafe.Clip(jsonx.S(data, "error"), maxAgentError)
		if msg == "" {
			msg = fmt.Sprintf("agent error %d", status)
		}
		return fail(apierr.New(st, msg))
	}
	state := jsonx.S(data, "state")
	if !helperStates[state] {
		state = "unknown"
	}
	params["state"] = state
	c.audit(u, auditAction, id, params, "ok")
	who := actorOf(u)
	res := HelperResult{SystemID: id, Action: action, State: state}
	e := notifications.Event{Type: "host", Severity: notifications.Info, SystemID: id, Host: x.Name}
	if action == "stop" {
		res.Message = "helper stopped; the fans are on the BMC's own automatic control"
		e.Kind, e.Text = "helper_stopped", "elevated helper stopped by "+who+" (fans on BMC automatic control, Core Temp / BMC readings paused)"
	} else {
		res.Message = "helper " + map[bool]string{true: "running", false: "starting"}[state == "running"]
		e.Kind, e.Text = "helper_started", "elevated helper started by "+who
	}
	s.Notes.Add(e)
	s.Poller.Scheduler.Kick("agent:" + a.ID) // show the new state now, not at the next poll
	c.json(200, res)
	return nil
}

// helperPaused reports whether a group of readings (coretemp, ipmi, fans) is paused on a host because its helper is not running.
func helperPaused(x model.System, group string) bool {
	return x.Helper != nil && slices.Contains(x.Helper.Paused, group)
}
