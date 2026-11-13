package web

import (
	"errors"
	"fmt"
	"sync"
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
	if lost, ok := s.unseen.get(x.ID, s.now()); ok && x.PowerPending == nil {
		// A shutdown, reboot or sleep request of this panel got no answer a moment ago and may have arrived:
		// "Cancel pending" is on offer although no countdown shows (unseenPower). The same look at the routes
		// as with a countdown that shows: to the system's own agent with power control, or not at all.
		y := x
		y.PowerPending = &model.PowerPending{Action: lost.action}
		for _, a := range powerctl.Capabilities(s.Poller, y).Actions {
			for i := range caps.Actions {
				if a.Action == "cancel" && caps.Actions[i].Action == "cancel" {
					caps.Actions[i] = a
				}
			}
		}
	}
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
	who := actorOf(u) // the one form the panel names a person by (audit 5, r1-b2-10): the countdown said the display name
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
		var lost *powerctl.Unanswered
		if errors.As(err, &lost) && res.Via == "agent" && (req.Action == "shutdown" || req.Action == "reboot" || req.Action == "sleep") {
			// no answer of the machine's agent: the request may have arrived, and until the countdown shows
			// (or never does) a Cancel must be able to reach the agent (unseenPower)
			s.unseen.note(x.ID, req.Action, s.now().Add(time.Duration(*req.DelayS)*time.Second+cancelUnseenFor))
		}
		err = approvalRefusal(x.Name, err, why)
		c.audit(u, "power."+req.Action, id, params, errorOutcome(err))
		return err
	}
	if res.Via == "agent" {
		s.unseen.drop(x.ID) // the agent holds one action, and it is this one now: an earlier request that got no answer cannot start
	}
	params["via"] = res.Via
	c.audit(u, "power."+req.Action, id, params, "ok")
	s.powerEvent(x, req, res, who)
	c.json(200, res)
	return nil
}

// cancelUnseenFor is how long after the time a shutdown, reboot or sleep request without an answer would have
// been due the panel still lets a Cancel through to the machine's agent while no countdown shows. It is the
// time a wake / update / shut down run goes on asking for its own lost request to be called off (wakerun:
// "until 5 minutes after it was due").
const cancelUnseenFor = 5 * time.Minute

// unseenPower remembers, per system and in memory, the panel's own shutdown, reboot and sleep requests that
// got no answer of the agent (powerctl.Unanswered: a timeout, a connection that broke, 502 and 504, the
// agent's own 504 for a helper that was handed the request and did not answer).
//
// Such a request may have arrived. The page then shows no countdown: the panel's view shows one only after a
// poll that carries it, and an agent whose helper did not answer cannot tell what is pending. powerctl offers
// a cancel only while a countdown shows, and answered the Cancel with "nothing pending" itself, so nothing
// reached the agent: the agent's own answer in that minute ("... may still be scheduled: send the cancel
// again", Fairy-agent f439fc0) could not be followed from the panel, and a machine whose countdown never showed
// went off with no way to stop it from here. While such a request can still arrive (until cancelUnseenFor
// after its due time) a Cancel for that system is sent to its agent whatever the view shows
// (powerctl.Request.PendingUnseen, the rule a run already uses for its own lost request), and "Cancel pending"
// is on offer in the Power menu. Nothing else changes about a cancel: an admin, the CSRF header, the cancels'
// own budget, the kill switch as for every cancel (a browser session passes it, a token gets 423), and only
// to the system's own agent with power control.
//
// The entry goes when the time is over, when a countdown that showed was cancelled, and when a later power
// request to that agent was accepted. A restart of the panel forgets it: then a Cancel is possible again only
// once the countdown shows.
type unseenPower struct {
	mu sync.Mutex
	m  map[string]unseenReq
}

type unseenReq struct {
	action string
	until  time.Time
}

func (l *unseenPower) note(id, action string, until time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]unseenReq{}
	}
	if old, ok := l.m[id]; ok && old.until.After(until) {
		until = old.until // an earlier lost request with a later time can still arrive too
	}
	l.m[id] = unseenReq{action: action, until: until}
}

func (l *unseenPower) get(id string, now time.Time) (unseenReq, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.m[id]
	if ok && now.After(r.until) {
		delete(l.m, id)
		return unseenReq{}, false
	}
	return r, ok
}

func (l *unseenPower) drop(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, id)
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
	who := actorOf(u)     // the same form as everywhere else the panel names who did something (audit 5, r1-b2-10)
	was := x.PowerPending // what the page showed when Cancel was pressed
	// No countdown shows, and a power request of this panel to this machine got no answer a moment ago: the
	// cancel goes to the agent all the same (unseenPower).
	lost, unseen := s.unseen.get(x.ID, s.now())
	if unseen = unseen && was == nil; unseen {
		req.PendingUnseen = true
		params["pending"] = "not shown (a " + lost.action + " request got no answer)"
	}
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
	if unseen {
		// The agent answered the cancel with "ok, nothing pending", which it also answers when it had nothing to
		// stop: the panel saw no countdown and cannot say that one was cancelled. And the request that got no
		// answer may still be on its way (a gateway that passed it on late): the entry stays for its time, so
		// the cancel can be sent again. No "cancelled", in the answer, the audit log or the notifications.
		res.Message = fmt.Sprintf("Cancel sent to %s, and its agent answered that nothing is pending there now. If the %s request that got no answer "+
			"arrives after this, its countdown shows on this page: cancel it then.", x.Name, lost.action)
		c.audit(u, "power.cancel", id, params, "ok (no countdown was shown; the agent answered that nothing is pending)")
		c.json(200, res)
		return nil
	}
	s.unseen.drop(x.ID) // a countdown showed and the agent took its cancel: nothing of an earlier request is left to wait for
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
