package web

import (
	"errors"
	"fmt"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/powerctl"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
)

// Uninstalling a machine's agent (Fairy-agent 0.11.0, with uninstall.enabled in that machine's config.yaml):
// POST /api/systems/{id}/uninstall {"confirm": "<the host's name>"}.
//
// It is the one request that removes the panel's way to the machine, so it goes the way of a plug switch, not
// of a shutdown: never without a passkey approval that the machine checks itself. The order:
//
//  1. an admin with a browser session (sessionOnlyControl), the CSRF header, the kill switch; the control rate
//     limit, before anything is read;
//  2. a host with an agent and its control token, the host's name typed, and an agent that offers it
//     (features.uninstall): the panel's own "no", audited, before the machine is asked anything;
//  3. the machine's challenge for exactly "uninstall", then the passkey (confirmOne). Always asked, whatever
//     the panel's view of the machine: a machine without a trust file answers approval_off, and a machine that
//     says "needs none" is not believed (notNeeded). No approval: nothing is sent;
//  4. POST /uninstall with the approval. 202 "uninstalling": the agent removes itself and goes offline.
//
// The host's entry stays in the panel; removing it is separate (DELETE /api/admin/hosts/{id}).
func (s *Server) agentUninstall(c *reqCtx) error {
	u, err := c.controlAllowed(true)
	if err != nil {
		return err
	}
	if !s.control.allow(u.Sub) {
		return c.limited(u, s.control, "too many control requests")
	}
	id := c.r.PathValue("id")
	x, err := c.find(id) // a host the user cannot see is "not found", as for power actions
	if err != nil {
		return err
	}
	params := map[string]any{"host": x.Name}
	fail := func(err error) error {
		c.audit(u, uninstallAudit, id, params, errorOutcome(err))
		return err
	}
	a, ok := s.Poller.AgentByID(id)
	if !ok || x.Kind != "agent" {
		return fail(apierr.New(404, "this system has no Fairy agent"))
	}
	if !a.HasControl() {
		return fail(apierr.New(409, "no control token is configured for this agent (control_token_env)"))
	}
	if err := c.typedName(x.Name, "uninstalling its agent"); err != nil {
		return fail(err)
	}
	if !x.Uninstall && c.approved() == nil {
		return fail(apierr.New(409, x.Name+"'s agent does not offer uninstall: its config.yaml needs uninstall.enabled: true (Fairy-agent 0.11.0 or newer). Nothing was sent."))
	}
	// The machine's passkey check, by the panel's record (socketGate: the same rule as a plug switch): a machine
	// that is not on record with a usable check is refused here, before anybody is sent to sign in for nothing.
	// The machine decides again itself (approval_off), whatever the panel's record says.
	if c.approved() == nil {
		switch s.socketGate(a.ID) {
		case api.WhyApprovalOff:
			return fail(uninstallRefusal(x.Name, 409, jsonx.M{"code": api.CodeApprovalOff}, ""))
		case api.WhyApprovalUnavailable:
			if err := s.bindingRefusal(a.ID, x.Name); err != nil {
				return fail(err)
			}
			return fail(uninstallRefusal(x.Name, 503, jsonx.M{"code": api.CodeApprovalUnavailable}, s.unavailableWhy(a.ID)))
		}
	}
	ap, err := c.confirmOne(u, "control actions", &approvalTarget{agent: a, name: x.Name, req: api.ApprovalRequest{Verb: api.ApproveUninstall}})
	if err != nil {
		if isStepUp(err) {
			return err
		}
		return fail(err)
	}
	if ap == nil {
		// confirmOne lets a request through without an approval where a machine checks nothing; an agent is
		// never uninstalled that way
		return fail(apierr.New(409, "No passkey approval was given, and an agent is uninstalled only with one. Nothing was sent."))
	}
	params["approval"] = "sent" // the machine checks it; the token itself is never written anywhere
	who := s.whoShown(u)
	status, data, err := s.Poller.AgentRequest(c.r.Context(), a, "POST", api.PathUninstall, api.UninstallRequest{Approval: ap, RequestedBy: who})
	s.Poller.Scheduler.Kick("agent:" + a.ID)
	if e := unanswered(api.PathUninstall, status, data, err); e != nil {
		var lost *powerctl.Unanswered
		if errors.As(e, &lost) { // no answer is never the agent's "no": the request may have arrived
			return fail(apierr.New(502, fmt.Sprintf("No answer from %s. The uninstall may have started: if it did, %s goes offline within a minute.", x.Name, x.Name)))
		}
		return fail(apierr.New(502, x.Name+" could not be reached. Nothing was sent."))
	}
	if status != 202 {
		if status >= 200 && status < 300 {
			return fail(apierr.New(502, fmt.Sprintf("%s answered with HTTP %d, which is no answer to an uninstall. It may have started: if it did, %s goes offline within a minute.", x.Name, status, x.Name)))
		}
		return fail(uninstallRefusal(x.Name, status, data, s.unavailableWhy(a.ID)))
	}
	if jsonx.S(data, "state") != api.UninstallStarted {
		return fail(apierr.New(502, x.Name+" answered for another request than the one sent. The uninstall may have started: if it did, "+x.Name+" goes offline within a minute."))
	}
	params["state"] = api.UninstallStarted
	c.audit(u, uninstallAudit, id, params, "ok")
	s.Notes.Add(notifications.Event{Type: "host", Severity: notifications.Warning, SystemID: id, Host: x.Name,
		Kind: uninstallAudit, Text: "agent uninstalled by " + who + " (approved with a passkey)"})
	c.json(200, AgentUninstallResult{SystemID: id, State: api.UninstallStarted,
		Message: x.Name + "'s agent is removing itself: the host goes offline within a minute. Its config and tokens stay on the machine; the host stays here until you remove it."})
	return nil
}

// uninstallAudit is the action of the audit entry and the kind of the notification.
const uninstallAudit = "agent_uninstall"

// AgentUninstallResult is the answer to an uninstall the agent started.
type AgentUninstallResult struct {
	SystemID string `json:"system_id"`
	State    string `json:"state"` // uninstalling
	Message  string `json:"message"`
}

// uninstallRefusal says a machine's refusal of POST /uninstall in words, with what to do.
func uninstallRefusal(name string, status int, data jsonx.M, why string) error {
	code, text := jsonx.S(data, "code"), textsafe.Clip(jsonx.S(data, "error"), maxAgentError)
	switch code {
	case api.CodeApprovalOff:
		return apierr.New(409, name+" has no passkey check set up (no approval trust), and an agent is uninstalled only with a passkey approval. Set the approval trust up first (Approval, on the host's page). Nothing was done.")
	case api.CodeUninstallDisabled:
		return apierr.New(409, name+"'s agent does not offer uninstall: its config.yaml needs uninstall.enabled: true. Nothing was done.")
	case api.CodeUninstallBusy:
		return apierr.New(409, "An uninstall was already started on "+name+". Nothing more was done.")
	case api.CodeHelperRequired:
		return apierr.New(409, name+" has no elevated helper installed, and on Windows it is the helper that checks the approval and carries the uninstall out. Nothing was done.")
	case api.CodeUpdaterBusy:
		return apierr.New(409, name+" is installing an update right now. Try again when it is done. Nothing was done.")
	}
	if status == 429 {
		return apierr.New(429, approval.Busy(name, code, retryAfter(data), false))
	}
	if msg, ok := approval.ErrorText(name, code, text, why); ok {
		st := 409
		if status == 403 {
			st = 403
		}
		return apierr.New(st, msg)
	}
	if status == 401 { // never our own 401 (that means "sign in again")
		return apierr.New(apierr.Upstream, "agent rejected the control token (check control_token_env)")
	}
	if text == "" {
		text = fmt.Sprintf("agent error %d", status)
	}
	if status >= 500 {
		return apierr.New(apierr.Upstream, name+": "+text)
	}
	return apierr.New(409, name+": "+text)
}
