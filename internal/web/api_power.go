package web

import (
	"fmt"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/powerctl"
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
	asking, err := c.requireUser()
	if err != nil {
		return err
	}
	var req powerctl.Request
	if err := c.decodeBody(&req); err != nil {
		return err
	}
	if err := req.Validate(); err != nil {
		return err
	}
	// Calling a countdown off (the page's Cancel, the Power menu's "Cancel pending") is not refused by the kill
	// switch or by the host's power switch (controlAllowed without the kill switch here, as in stopGuard;
	// powerctl.Capabilities for both switches): a cancel only ever stops a power action, and a countdown that
	// was running when an admin flipped the switch could otherwise not be called off from the panel at all
	// (audit 5, r3-p2-06). It is still an admin's action with the CSRF header, and the audit entry says that
	// control was off (control_off). Every other action is refused by the kill switch here, as before.
	//
	// Past the kill switch only for a browser session: the kill switch has always stopped tokens entirely. A
	// token's cancel is refused by it like its every other write (423; second review of fix7a, R7A-10: for one
	// batch a control token could cancel with the kill switch on). With the kill switch off a token's cancel
	// is what it was.
	u, err := c.controlAllowed(req.Action != "cancel" || asking.Auth == "token")
	if err != nil {
		return err
	}
	id := c.r.PathValue("id")
	if req.Action == "cancel" {
		return s.powerCancel(c, u, id, req)
	}
	// A machine that checks approvals itself is asked first (approval_control.go). Everything the action
	// would be refused for anyway is checked before that, so nobody is sent to Pocket ID for nothing. Any
	// other machine: "Confirm it's you" and then the checks, as before.
	var target *approvalTarget
	if x, err := c.find(id); err == nil {
		if a, ar, ok := powerctl.ApprovalRequest(s.Poller, x, req); ok && c.wantsTarget(a.ID) {
			if _, err := powerctl.Check(s.Poller, x, req); err != nil {
				c.audit(u, "power."+req.Action, id, map[string]any{"action": req.Action, "delay_s": *req.DelayS}, errorOutcome(err))
				return err
			}
			target = &approvalTarget{agent: a, name: x.Name, req: ar}
		}
	}
	ap, err := c.confirmOne(u, "control actions", target)
	if err != nil {
		if target != nil && !isStepUp(err) {
			c.audit(u, "power."+req.Action, id, map[string]any{"action": req.Action, "delay_s": *req.DelayS}, errorOutcome(err))
		}
		return err
	}
	if !s.control.allow(u.Sub) {
		return c.limited(u, s.control, "too many control requests")
	}
	x, err := c.find(id)
	if err != nil {
		return err
	}
	params := map[string]any{"action": req.Action, "delay_s": *req.DelayS}
	if ap != nil {
		params["approval"] = "sent" // the machine checks it; the token itself is never written anywhere
	}
	who := model.Deref(u.Name)
	if who == "" {
		who = model.Deref(u.Email)
	}
	if who == "" {
		who = u.Sub
	}
	res, err := powerctl.ExecuteApproved(c.r.Context(), s.Poller, x, req, who, ap)
	if err != nil {
		if ap == nil && refusalCode(err) == api.CodeApprovalRequired {
			// the machine wants an approval the panel did not know about (its state changed since the last poll)
			if a, ar, ok := powerctl.ApprovalRequest(s.Poller, x, req); ok {
				if ask := c.askAfterRefusal(u, approvalTarget{agent: a, name: x.Name, req: ar}); ask != nil {
					return ask
				}
			}
		}
		why := ""
		if a, ok := s.Poller.AgentFor(x.ID); ok {
			why = s.unavailableWhy(a.ID)
		}
		err = approvalRefusal(x.Name, err, why)
		c.audit(u, "power."+req.Action, id, params, errorOutcome(err))
		return err
	}
	params["via"] = res.Via
	c.audit(u, "power."+req.Action, id, params, "ok")
	s.powerEvent(x, req, res, who)
	c.json(200, res)
	return nil
}

// cancelLate is how long after the end of a countdown (by the panel's clock against the time the agent gave) a
// cancel is still reported as "sent at the end of the countdown" and not as "too late": the two clocks differ.
const cancelLate = 2 * time.Second

// powerCancel is "Cancel" of a pending shutdown, restart or sleep (audit 5, r1-b2-01). Stopping is the safe
// direction and it has to be quick: a countdown can be ten seconds long. So it needs an admin, the CSRF header
// and, for an API token, the kill switch (powerAction: a browser session's cancel passes the kill switch and
// the host's power switch), and nothing more: no "Confirm it's you" and no approval of the machine, on machines
// with and without the check (the agent takes a cancel without an approval, and has a request budget of its
// own for it). It is the only power action that is let through like this.
//
// The agent answers a cancel with 200 whether or not something was still pending (it keeps a countdown that
// ran out for another minute). So the panel does not repeat "cancelled" after it: it says what it knows. If
// the countdown it showed had run out when the answer came, the machine is already shutting down, and the
// answer is an error that says so.
func (s *Server) powerCancel(c *reqCtx, u *model.User, id string, req powerctl.Request) error {
	if !s.stopRL.allow(u.Sub) { // its own budget: calling a countdown off is never refused because of other control requests
		return c.limited(u, s.stopRL, "too many control requests")
	}
	x, err := c.find(id)
	if err != nil {
		return err
	}
	params := map[string]any{"action": req.Action}
	if off := powerctl.ControlOff(s.Poller, x.ID); off != "" {
		params["control_off"] = off // the cancel went through although control is off: that is said
	}
	who := displayName(u)
	was := x.PowerPending // what the page showed when Cancel was pressed
	res, err := powerctl.Execute(c.r.Context(), s.Poller, x, req, who)
	if err != nil {
		why := ""
		if a, ok := s.Poller.AgentFor(x.ID); ok {
			why = s.unavailableWhy(a.ID)
		}
		err = approvalRefusal(x.Name, err, why)
		c.audit(u, "power.cancel", id, params, errorOutcome(err))
		return err
	}
	params["via"] = res.Via
	what := "action"
	if was != nil {
		what = was.Action
		params["pending"] = was.Action
		if late := s.now().Sub(was.At); late > cancelLate {
			// too late: the time was up before the cancel reached the machine
			msg := fmt.Sprintf("Too late: the %s of %s had started %d s before the cancel reached it. Nothing was cancelled.", what, x.Name, int(late.Seconds()))
			c.audit(u, "power.cancel", id, params, "error 409: too late, the countdown had run out")
			return apierr.New(409, msg)
		} else if late > -cancelLate {
			res.Message = fmt.Sprintf("Cancel sent as the countdown of the %s of %s ran out: look whether it is still on.", what, x.Name)
			c.audit(u, "power.cancel", id, params, "ok (sent as the countdown ran out)")
			s.powerEvent(x, req, res, who)
			c.json(200, res)
			return nil
		}
	}
	res.Message = fmt.Sprintf("The %s of %s is cancelled.", what, x.Name)
	c.audit(u, "power.cancel", id, params, "ok")
	s.powerEvent(x, req, res, who)
	c.json(200, res)
	return nil
}

// powerEvent is the notification for a power action, and tells the watcher that a shutdown / reboot from the
// panel is expected (the host going offline is then "off", not an alarm).
func (s *Server) powerEvent(x model.System, req powerctl.Request, res model.PowerResult, who string) {
	s.Watch.NotePanelPower(x.ID, req.Action, time.Now())
	switch {
	case req.Action == "sleep" && res.Pending != nil:
		s.Poller.NoteSleep(x.ID, &res.Pending.At)
	case req.Action == "sleep":
		s.Poller.NoteSleep(x.ID, model.P(time.Now().Add(time.Duration(*req.DelayS)*time.Second)))
	case req.Action == "cancel":
		s.Poller.NoteSleep(x.ID, nil)
	}
	e := notifications.Event{Type: "power", Severity: notifications.Info, SystemID: x.ID, Host: x.Name}
	switch req.Action {
	case "shutdown", "reboot", "sleep":
		e.Kind, e.Text = "power_requested", fmt.Sprintf("%s requested by %s (%s)", req.Action, who, res.Message)
	case "cancel":
		e.Kind, e.Text = "power_cancelled", "pending shutdown / reboot / sleep cancelled by "+who
	case "wake":
		e.Kind, e.Text = "wake_sent", "Wake-on-LAN sent by "+who
	case "bmc_on":
		e.Kind, e.Text = "bmc_power_on", "power on through the BMC requested by "+who+" ("+res.Message+")"
	default:
		e.Kind, e.Text = "power_"+req.Action, req.Action+" by "+who
	}
	s.Notes.Add(e)
}
