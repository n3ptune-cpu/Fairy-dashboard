package web

import (
	"fmt"
	"time"

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
	// Calling a countdown off (the page's Cancel, the Power menu's "Cancel pending") is never refused because
	// of other control requests: it has the request budget of the cancels (stopRL). And it is not refused by
	// the kill switch or by the host's power switch either (controlAllowed without the kill switch here, as in
	// stopGuard; powerctl.Capabilities for both switches): a cancel only ever stops a power action, and a
	// countdown that was running when an admin flipped the switch could otherwise not be called off from the
	// panel at all (audit 5, r3-p2-06). It is still an admin's action with the CSRF header and "Confirm it's
	// you", and the audit entry says that control was off (control_off). Every other action is refused by the
	// kill switch here, as before.
	//
	// Past the kill switch only for a browser session: an API token cannot "confirm it's you", and the kill
	// switch has always stopped tokens entirely. A token's cancel is refused by it like its every other write
	// (423; second review of fix7a, R7A-10: for one batch a control token could cancel with the kill switch on).
	// With the kill switch off a token's cancel is what it was.
	bucket, killSwitch := s.control, true
	if req.Action == "cancel" {
		bucket, killSwitch = s.stopRL, asking.Auth == "token"
	}
	u, err := c.controlAllowed(killSwitch)
	if err != nil {
		return err
	}
	id := c.r.PathValue("id")
	// A machine that checks approvals itself is asked first (approval_control.go). Everything the action
	// would be refused for anyway is checked before that, so nobody is sent to Pocket ID for nothing. Any
	// other machine: "Confirm it's you" and then the checks, as before.
	var target *approvalTarget
	if x, err := c.find(id); err == nil {
		if a, ar, ok := powerctl.ApprovalRequest(s.Poller, x, req); ok && s.approvalWanted(a.ID) {
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
	if !bucket.allow(u.Sub) {
		return apierr.New(429, "too many control requests; slow down")
	}
	x, err := c.find(id)
	if err != nil {
		return err
	}
	params := map[string]any{"action": req.Action, "delay_s": *req.DelayS}
	if off := powerctl.ControlOff(s.Poller, x.ID); off != "" && req.Action == "cancel" {
		params["control_off"] = off // the cancel went through although control is off: that is said
	}
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
		err = approvalRefusal(x.Name, err)
		c.audit(u, "power."+req.Action, id, params, errorOutcome(err))
		return err
	}
	params["via"] = res.Via
	c.audit(u, "power."+req.Action, id, params, "ok")
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
