package web

import (
	"fmt"
	"time"

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
	// Calling a countdown off (the page's Cancel, the Power menu's "Cancel pending") is never refused because
	// of other control requests: it has the request budget of the cancels (stopRL). And it is not refused by
	// the kill switch or by the host's power switch either (stopGuard here, powerctl.Capabilities for both
	// switches): a cancel only ever stops a power action, and a countdown that was running when an admin
	// flipped the switch could otherwise not be called off from the panel at all (audit 5, r3-p2-06). It is
	// still an admin's action with the CSRF header and "Confirm it's you", and the audit entry says that
	// control was off (control_off). Every other action is refused by the kill switch here, as before.
	var u *model.User
	var err error
	if req.Action == "cancel" {
		u, err = c.stopGuard(s.stopRL)
	} else {
		u, err = c.controlGuard(s.control)
	}
	if err != nil {
		return err
	}
	id := c.r.PathValue("id")
	x, err := c.find(id)
	if err != nil {
		return err
	}
	params := map[string]any{"action": req.Action, "delay_s": *req.DelayS}
	if off := powerctl.ControlOff(s.Poller, x.ID); off != "" && req.Action == "cancel" {
		params["control_off"] = off // the cancel went through although control is off: that is said
	}
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
