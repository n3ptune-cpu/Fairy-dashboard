package web

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/normalise"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/powerctl"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
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

// maxAgentError is how much of an agent's own error text reaches the page and the audit log.
const maxAgentError = 300

// unanswered says whether a request of this package that changes something on a machine (a fan change, the
// "Update" button and "Update all", the helper's stop and start) got no answer of the agent: then it may have
// arrived all the same, and it must not read as the agent's "no". The rule is the power path's own,
// powerctl.NoAnswer, and so are the words: 502 and 504 whatever the body (a gateway between panel and agent
// that passed the request on; and the agent's own 504 for a request it handed to its helper, which did not
// answer), a redirect, an error status with a body that is not the agent's, a timeout, a broken connection.
// These used to be "the agent answered with an error" (424, "agent error"), and a redirect was "ok", "started".
// What the answer said, if it said something, is added in brackets: the agent's 504 says what may have happened
// ("... the fans may have been changed all the same: look at the fan status"), and its 502 to a helper's stop
// says why. (The agent's own 502 on a fan route, with the reason of its BMC, is its answer and stays one:
// powerctl, relaysOwn.)
//
// A good status is the agent's "yes" only with the agent's answer, as on the power path: the agent answers each
// of these requests with a JSON object that is not empty ({"mode":...}, {"zone":...,"percent":...},
// {"state":...}). A 200 with `{}` or `null`, a 204: something else at its address answered, and the change may
// or may not have been made. They were "ok", "started. Its row shows how it goes." and "helper stopped; the
// fans are on the BMC's own automatic control" (review of fix8, RF8-04); they are "no answer (http 200): it
// may have arrived" like everything else that is not the agent's.
func unanswered(path string, status int, data jsonx.M, err error) error {
	if err == nil && status >= 200 && status < 300 && len(data) > 0 {
		return nil
	}
	e := powerctl.NoAnswer(path, status, data, err)
	var lost *powerctl.Unanswered
	if said := textsafe.Clip(jsonx.S(data, "error"), maxAgentError); err == nil && said != "" && errors.As(e, &lost) {
		return &powerctl.Unanswered{Err: apierr.New(502, fmt.Sprintf("%v (the answer said: %s)", lost.Err.Detail, said)), Short: lost.Short}
	}
	return e
}

func (c *reqCtx) agentCall(a config.Agent, method, path string, body any) (map[string]any, error) {
	status, data, err := c.s.Poller.AgentRequest(c.r.Context(), a, method, path, body)
	if method == "POST" {
		if e := unanswered(path, status, data, err); e != nil {
			return nil, e
		}
	}
	if err != nil {
		return nil, apierr.New(502, "agent unreachable: "+devices.Describe(err))
	}
	if status == 401 { // never surface as our own 401 (that means "log in again")
		return nil, apierr.New(apierr.Upstream, "agent rejected the control token (check control_token_env)")
	}
	if status == 503 && jsonx.S(data, "code") == api.ReasonHelperStopped {
		return nil, apierr.New(409, fanHelperStopped) // the helper stopped since the last poll: not an agent failure
	}
	if status == 503 && jsonx.S(data, "code") == api.ReasonHelperError { // it runs and did not answer: not "stopped", and the fans are not on automatic
		return nil, apierr.New(409, powerctl.HelperAnswerWhy(api.ReasonHelperError))
	}
	if status >= 400 { // the agent answered: its message must reach the user (no 502, see apierr.Upstream)
		st := apierr.Upstream
		switch status {
		case 400, 404, 409, 422, 429:
			st = status
		}
		msg := "agent error"
		if e, ok := data["error"]; ok {
			if e := textsafe.Clip(fmt.Sprint(e), maxAgentError); e != "" {
				msg = e
			}
		}
		// the machine's own check of approvals said no: what that means and what to do, around its own words
		if m, ok := approval.ErrorText(a.DisplayName(), jsonx.S(data, "code"), textsafe.Clip(jsonx.S(data, "error"), maxAgentError)); ok {
			msg = m
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
	release, err := c.agentRead(a.ID)
	if err != nil {
		return err
	}
	data, err := c.agentCall(a, "GET", "/fan/status", nil)
	release()
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
	boundFanStatus(&fs)
	model.Normalize(&fs)
	c.json(200, fs)
	return nil
}

// Limits for what one agent may put into its fan status, as for its system entry (normalise) and its process
// list (poller.DecodeProcessList): the answer goes to the page every 3 s while a Fans tab is open. Far above
// what a real board reports (Fairy-agent: two zones, the last 10 events, four modes).
const (
	maxFanWord    = 64  // vendor, mode, auto_mode, an unsupported mode, an event's action
	maxFanName    = 128 // a fan's name, as in the system entry
	maxFanDetail  = 256 // an event's detail
	maxFanEvents  = 50  // the newest are kept (the agent itself keeps 50)
	maxFanModes   = 16
	maxFanZones   = 16
	maxFanZoneKey = 8
)

// boundFanStatus cuts the texts and lists of an agent's fan status to the limits above.
func boundFanStatus(fs *model.FanStatus) {
	clip := func(p **string, n int) {
		if *p != nil {
			if v := normalise.ClipText(**p, n); v != **p {
				*p = &v
			}
		}
	}
	clip(&fs.Vendor, maxFanWord)
	clip(&fs.Mode, maxFanWord)
	clip(&fs.AutoMode, maxFanWord)
	clip(&fs.Error, textsafe.MaxError)
	fs.Reason = normalise.ClipText(fs.Reason, maxFanWord)
	if len(fs.Unsupported) > maxFanModes {
		fs.Unsupported = fs.Unsupported[:maxFanModes]
	}
	for i, m := range fs.Unsupported {
		fs.Unsupported[i] = normalise.ClipText(m, maxFanWord)
	}
	if len(fs.Fans) > normalise.MaxFans {
		fs.Fans = fs.Fans[:normalise.MaxFans]
	}
	for i := range fs.Fans {
		fs.Fans[i].Name = normalise.ClipText(fs.Fans[i].Name, maxFanName)
	}
	if n := len(fs.Events); n > maxFanEvents {
		fs.Events = fs.Events[n-maxFanEvents:]
	}
	for i := range fs.Events {
		fs.Events[i].Action = normalise.ClipText(fs.Events[i].Action, maxFanWord)
		fs.Events[i].Detail = normalise.ClipText(fs.Events[i].Detail, maxFanDetail)
	}
	if len(fs.Duty) > 0 { // zones are "0", "1", ...: a longer key is no zone, and the first few are kept
		keys := make([]string, 0, len(fs.Duty))
		for k := range fs.Duty {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, func(a, b string) int { return cmp.Or(cmp.Compare(len(a), len(b)), cmp.Compare(a, b)) })
		kept := 0
		for _, k := range keys {
			if len(k) > maxFanZoneKey || k != textsafe.Clean(k) || kept == maxFanZones {
				delete(fs.Duty, k)
				continue
			}
			kept++
		}
	}
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

// fanWrite sends a fan mode or duty. body is the request for the agent, ar the same request as a machine that
// checks approvals itself is asked to approve it.
func (s *Server) fanWrite(c *reqCtx, action string, body any, ar api.ApprovalRequest, params map[string]any) error {
	u, err := c.controlAllowed(true)
	if err != nil {
		return err
	}
	id := c.r.PathValue("id")
	// A machine that checks approvals itself is asked first (approval_control.go); any other is confirmed and
	// sent to as before. While its helper is stopped nothing is asked: the request is refused below.
	var target *approvalTarget
	if x, err := c.find(id); err == nil && !helperPaused(x, "fans") {
		if a, ok := s.Poller.AgentFor(id); ok && a.FanControl != nil && s.approvalWanted(a.ID) {
			target = &approvalTarget{agent: a, name: x.Name, req: ar}
		}
	}
	ap, err := c.confirmOne(u, "control actions", target)
	if err != nil {
		if target != nil && !isStepUp(err) {
			c.audit(u, action, id, params, errorOutcome(err))
		}
		return err
	}
	if !s.control.allow(u.Sub) {
		return c.limited(u, s.control, "too many control requests")
	}
	if ap != nil { // the same request, with the approval made for exactly it
		if ar.Verb == api.ApproveFanMode {
			body = map[string]any{"mode": *ar.Mode, "approval": ap}
		} else {
			body = map[string]any{"zone": *ar.Zone, "percent": *ar.Percent, "approval": ap}
		}
		params["approval"] = "sent" // the machine checks it; the token itself is never written anywhere
	}
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
	if ap != nil { // the machine accepted it with an approval: its fan window is open for this session's next steps
		s.codec.live.noteFan(c.session().SID, a.ID, c.client(), s.now())
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
	return s.fanWrite(c, "fan.mode", b, api.ApprovalRequest{Verb: api.ApproveFanMode, Mode: &b.Mode},
		map[string]any{"mode": b.Mode})
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
		api.ApprovalRequest{Verb: api.ApproveFanSet, Zone: b.Zone, Percent: b.Percent}, map[string]any{"zone": *b.Zone, "percent": *b.Percent})
}
