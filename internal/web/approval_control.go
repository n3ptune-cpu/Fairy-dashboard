package web

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/powerctl"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
)

// How a control request to a machine is confirmed (fans, shutdown / restart / sleep, the agent's update).
//
// Machines that send no "approval" block (agents from before the feature) and machines whose state is "off"
// are handled exactly as before: "Confirm it's you" with the panel's own window, then the control token.
// Nothing here asks them anything.
//
// A machine in any other state is asked for a challenge for the exact request (POST /approval/challenge):
//
//   - "needed: false" (inside the machine's own fan window, the one shutdown shortly after boot, or the state
//     went back to off): the request is confirmed and sent as before, without an approval.
//   - "needed: true": the request is kept with the plan (the machine's challenge and the canonical text of
//     the request) and the dialog asks for the approval (askApproval). After the round trip the panel replays
//     the kept request, the handler comes here again, and For hands out the approval for exactly the request
//     it then builds. A request that is not the approved one gets none and is refused here.
//   - anything else (the trust file is broken, the helper is not answering, the machine trusts another
//     Pocket ID than this panel asks): the request is refused with what the machine said, before anybody is
//     asked for a passkey.

// approvalTarget is one request to one machine, as a control route is about to send it.
type approvalTarget struct {
	agent config.Agent
	name  string // the machine's name for people
	req   api.ApprovalRequest
	// machineID is set for a Turn on only: the id the panel made up for the machine. Every other request is
	// bound to the id the panel has on record for the agent (challenge).
	machineID string
}

// approvalOutcome is what confirmControl decided for one target: the approval to send with it (nil: none), or
// why it cannot be sent at all.
type approvalOutcome struct {
	approval *api.Approval
	err      error
}

// approvalWanted reports whether the agent must be asked before a control request: it sends an approval
// block and its state is not "off"; or the panel's record says it did so when it last answered; or the panel's
// decision about which machine it is, is an alarm (bindingRefusal then says no before anybody is asked).
func (s *Server) approvalWanted(agentID string) bool {
	return s.Poller.MachineChecks(agentID)
}

// machineNames are the names of agents for a sentence.
func (s *Server) machineNames(agentIDs []string) []string {
	out := make([]string, 0, len(agentIDs))
	for _, id := range agentIDs {
		name := id
		if a, ok := s.Poller.AgentByID(id); ok {
			name = a.DisplayName()
			if x := s.Poller.RelaySystem(id); x != nil {
				name = x.Name
			}
		}
		out = append(out, name)
	}
	return out
}

// bindingRefusal is the panel's own "no" for a machine whose binding is an alarm (internal/approval,
// binding.go): the host shows another machine id than the one on record, two hosts have one id, a host that had
// the check shows none, its agent is too old to name the machine, or the record cannot be read. No challenge
// is asked for such a machine and no approval is handed to it. nil: nothing stands in the way.
func (s *Server) bindingRefusal(agentID, name string) error {
	b := s.Poller.BindingOf(agentID)
	if b.OK() {
		return nil
	}
	msg := approval.BindingText(name, b.Status, s.machineNames(b.With)) + " Nothing was sent."
	switch b.Status {
	case approval.BindChanged, approval.BindDuplicate, approval.BindVanished:
		msg += " An admin resolves it in the Approval box on the host's page."
	}
	return apierr.New(409, msg)
}

// wantsTarget reports whether a control route builds the approval target for the agent: the machine must be
// asked, or this is the replay after an approval (which then decides by its plan, not by the panel's view of
// the machine, which can be a poll behind).
func (c *reqCtx) wantsTarget(agentID string) bool {
	return c.approved() != nil || c.s.approvalWanted(agentID)
}

// confirmControl confirms a control request that is about to be sent to targets (see the top of this file)
// and returns, per target, the approval to send with it or why that target cannot be sent. An error means
// the whole request is refused for now: asked to confirm (401, kept), or not allowed.
//
// The caller has checked who is asking (controlAllowed); the rate limit comes after this, as before.
func (c *reqCtx) confirmControl(u *model.User, what string, targets []approvalTarget) ([]approvalOutcome, error) {
	return c.confirmTargets(u, what, targets, false)
}

// confirmTargets is confirmControl. only: targets is everything this request sends (a single action, not a
// batch that may also go to machines that are not asked): when its one machine refuses the question, that
// refusal is the answer and nobody is asked to confirm a request that cannot be sent.
func (c *reqCtx) confirmTargets(u *model.User, what string, targets []approvalTarget, only bool) ([]approvalOutcome, error) {
	out := make([]approvalOutcome, len(targets))
	if g := c.approved(); g != nil {
		// The panel's own replay, right after the approval for this kept request was verified. Each request
		// gets the approval only if it is exactly one the approval was given for.
		for i, t := range targets {
			// the binding is decided again now, with what every machine said meanwhile: an approval is never
			// handed to a host that became an alarm while the person was at Pocket ID
			if err := c.s.bindingRefusal(t.agent.ID, t.name); err != nil {
				out[i].err = err
				continue
			}
			ap, err := g.For(t.agent.ID, c.s.Poller.BindingOf(t.agent.ID).Record.MachineID, t.req)
			switch {
			case err == nil:
				out[i].approval = ap
			case !c.s.approvalWanted(t.agent.ID):
				// nothing checks there, and no approval was asked for it: sent as before, on the strength of
				// this confirmation (a batch that also goes to machines without the check)
			case g.plan.refused[t.agent.ID] != nil:
				// it could not be asked before the approval (its check cannot be used): that reason stands
				out[i].err = g.plan.refused[t.agent.ID]
			default:
				out[i].err = apierr.New(409, "This is not the request that was approved for "+t.name+", so it was not sent. Nothing was done: press the button again.")
			}
		}
		return out, nil
	}
	var ask []int
	stopped := 0
	for i, t := range targets {
		if err := c.s.bindingRefusal(t.agent.ID, t.name); err != nil {
			out[i].err = err // not asked, not sent: see bindingRefusal
			stopped++
			continue
		}
		if c.s.approvalWanted(t.agent.ID) {
			ask = append(ask, i)
		}
	}
	if stopped == len(targets) && stopped > 0 {
		return out, nil // nothing can be sent at all: nobody is asked to confirm for nothing
	}
	if len(ask) == 0 {
		return out, c.stepUp(u, what)
	}
	if u.Auth == "token" {
		// A token cannot be sent to Pocket ID. What the machine would answer anyway is said here, without
		// spending its request budget; a machine that is only "broken" or "unknown" answers for itself.
		for _, i := range ask {
			if ap := c.s.Poller.ApprovalOf(targets[i].agent.ID); ap != nil && ap.State == api.ApprovalRequired {
				out[i].err = apierr.New(403, targets[i].name+" checks a passkey approval itself before it does this: an API token cannot give one. Use the panel in a browser.")
			}
		}
		return out, nil
	}
	if !c.s.approvalRL.allow(u.Sub) {
		return nil, c.limited(u, c.s.approvalRL, "too many control requests")
	}
	var bound []approval.Bound
	inWindow := 0 // targets the machine's own fan window covers, opened by this session's approval a moment ago
	for _, i := range ask {
		t := targets[i]
		ch, reason, err := c.challenge(t)
		switch {
		case err != nil:
			out[i].err = err
		case ch != nil:
			bound = append(bound, *ch)
		case reason == "fan_window" && c.s.codec.live.fanNoted(c.session().SID, t.agent.ID, c.client(), c.s.now()):
			inWindow++
		}
	}
	if len(bound) == 0 {
		// nothing needs an approval right now: every asked machine said so, or cannot be sent to at all
		if only && len(targets) == 1 && out[0].err != nil {
			return out, nil // the one machine's refusal is the answer; nobody is asked to confirm for nothing
		}
		if only && inWindow == len(targets) {
			// The machine's fan window: this session's approval of a fan change on it was accepted seconds
			// ago, and the machine itself says further fan changes need none for now. The panel's own
			// confirmation would send the person to Pocket ID again for every step of the slider.
			return out, nil
		}
		return out, c.stepUp(u, what)
	}
	plan, err := approval.NewPlan(bound)
	if err != nil {
		return nil, apierr.New(apierr.Upstream, "the machines' challenges cannot be used: "+textsafe.Clip(err.Error(), 200))
	}
	ap := newApprovalPlan(plan)
	if len(targets) > 1 { // a batch: the dialog lists what the one approval covers, and what it does not
		for _, b := range bound {
			ap.items = append(ap.items, b.Name+": "+requestWords(b.Request)+" (checked on the machine)")
		}
		for i, t := range targets {
			if out[i].err != nil {
				ap.items = append(ap.items, t.name+": NOT sent ("+shortWhy(out[i].err)+")")
				if ap.refused == nil {
					ap.refused = map[string]error{}
				}
				ap.refused[t.agent.ID] = out[i].err
			}
		}
	}
	return nil, c.askApproval(u, ap)
}

// requestWords says a request a machine is asked to approve in words for the dialog.
func requestWords(r api.ApprovalRequest) string {
	switch r.Verb {
	case api.ApproveApplyUpdate:
		return "update of its agent"
	case api.ApprovePower:
		return fmt.Sprintf("%s in %d s", *r.Action, *r.DelayS)
	case api.ApproveFanMode:
		return "fan mode " + *r.Mode
	case api.ApproveFanSet:
		return fmt.Sprintf("fan zone %d to %d%%", *r.Zone, *r.Percent)
	}
	return r.Verb
}

// one is confirmControl for a request with at most one target: the approval to send (nil: none).
func (c *reqCtx) confirmOne(u *model.User, what string, target *approvalTarget) (*api.Approval, error) {
	if target == nil {
		if c.approved() != nil { // the replay after an approval, for a request that no machine is asked about any more
			return nil, nil
		}
		return nil, c.stepUp(u, what)
	}
	out, err := c.confirmTargets(u, what, []approvalTarget{*target}, true)
	if err != nil {
		return nil, err
	}
	return out[0].approval, out[0].err
}

// challenge asks the machine for its challenge for exactly t.req. It returns the bound request when an
// approval is needed, nil and the machine's reason when it says none is needed right now ("off", "fan_window",
// "boot_shutdown"), or why the request cannot be sent.
// The bound request names the machine id the panel has ON RECORD for the agent (for a Turn on: the id the
// panel made up, t.machineID). Nothing of the machine's answer goes into that id: the answer may be another
// machine's, handed over in this host's name, and that other machine refuses an approval made for this id.
// What the machine answers is compared with what the panel asked and with the panel's own configuration: a
// machine that answers for another request, or that trusts another Pocket ID or client than the panel will
// ask, is not sent anything.
func (c *reqCtx) challenge(t approvalTarget) (*approval.Bound, string, error) {
	canon, err := t.req.Canonical()
	if err != nil {
		return nil, "", apierr.New(422, "this request cannot be approved: "+err.Error())
	}
	status, data, err := c.s.Poller.AgentRequest(c.r.Context(), t.agent, "POST", api.PathApprovalChallenge, t.req)
	if err != nil {
		return nil, "", apierr.New(502, "agent unreachable: "+devices.Describe(err))
	}
	if status != 200 {
		return nil, "", challengeRefusal(t.name, status, data, c.s.unavailableWhy(t.agent.ID))
	}
	var ch api.ChallengeResponse
	raw, _ := json.Marshal(data)
	if err := json.Unmarshal(raw, &ch); err != nil || ch.Request != canon {
		return nil, "", apierr.New(apierr.Upstream, t.name+" answered the question about an approval for another request than the one asked. Nothing was sent.")
	}
	if !ch.Needed {
		return nil, ch.Reason, nil
	}
	if _, err := api.DecodeChallenge(ch.Challenge); err != nil {
		return nil, "", apierr.New(apierr.Upstream, t.name+" sent a challenge that cannot be used. Nothing was sent.")
	}
	if t.req.Verb != api.ApproveTrustSet { // a machine without trust has no issuer yet (the first-time set)
		if !c.s.Cfg.ApprovalOn() {
			return nil, "", apierr.New(409, approvalNotConfigured)
		}
		if ch.Issuer != c.s.Cfg.ApprovalIssuer() || ch.ClientID != c.s.Cfg.Approval.ClientID {
			return nil, "", apierr.New(409, fmt.Sprintf("%s accepts approvals from another Pocket ID or client (%s, client %s) than this panel asks (%s, client %s). "+
				"An approval from here would be refused, so nothing was sent. It is changed only on the machine: fairy-agent helper-trust show",
				t.name, textsafe.Clip(ch.Issuer, 120), textsafe.Clip(ch.ClientID, 64), c.s.Cfg.ApprovalIssuer(), c.s.Cfg.Approval.ClientID))
		}
	}
	id := t.machineID
	if t.req.Verb != api.ApproveTrustSet {
		// decided once more, after the machine's answer: the record may have changed while it was asked
		if err := c.s.bindingRefusal(t.agent.ID, t.name); err != nil {
			return nil, "", err
		}
		id = c.s.Poller.BindingOf(t.agent.ID).Record.MachineID
	}
	if api.CheckMachineID(id) != nil {
		return nil, "", apierr.New(409, approval.BindingText(t.name, approval.BindNoRecord, nil)+" Nothing was sent.")
	}
	return &approval.Bound{Target: approval.Target{AgentID: t.agent.ID, Name: t.name, Request: t.req}, Canonical: canon, Challenge: ch.Challenge, MachineID: id}, "", nil
}

// challengeRefusal is the machine's "no" to the question about an approval, in the panel's words where the
// code is one of the protocol's.
func challengeRefusal(name string, status int, data jsonx.M, why string) error {
	code, text := jsonx.S(data, "code"), textsafe.Clip(jsonx.S(data, "error"), maxAgentError)
	if msg, ok := approval.ErrorText(name, code, text, why); ok {
		return apierr.New(409, msg)
	}
	switch {
	case status == 401:
		return apierr.New(apierr.Upstream, "agent rejected the control token (check control_token_env)")
	case status == 503 && (code == api.ReasonHelperStopped || code == api.ReasonHelperError):
		return apierr.New(409, "the elevated helper on "+name+" is not answering, and it is the one that checks approvals there: start the helper first (Elevated helper, next to the host name)")
	case status == 429:
		// its budget of questions, or its list of open ones (too_many_challenges): said with the machine's own
		// number of seconds. The panel does not ask again by itself.
		return apierr.New(429, approval.Busy(name, code, retryAfter(data), true))
	case status == 404 && code == "":
		// state said "ask", the agent has no such route: an agent from before the feature answers here
		return apierr.New(409, name+" says it checks approvals, but its agent does not know how to be asked (it answered \"not found\"). Nothing was sent.")
	}
	if text == "" {
		text = fmt.Sprintf("agent error %d", status)
	}
	st := 409
	if status == 400 || status == 422 {
		st = 400
	}
	return apierr.New(st, text)
}

// isStepUp reports whether err is the answer "confirm first" (401 step_up_required): the request was not
// refused, it waits.
func isStepUp(err error) bool {
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 401 {
		return false
	}
	d, _ := ae.Detail.(map[string]any)
	return d["code"] == "step_up_required"
}

// approvalRefusal rewrites a machine's refusal of a power request when its code is one of the approval
// protocol's (approval_required, approval_refused, approval_unavailable): the page then says what it means
// and what to do, around the machine's own words. Any other error is returned as it is.
func approvalRefusal(name string, err error, why string) error {
	var ref *powerctl.AgentRefusal
	if !errors.As(err, &ref) {
		return err
	}
	if ref.Status == 429 { // the machine's request budget: when to ask again, in its own seconds
		return apierr.New(429, approval.Busy(name, ref.Code, ref.RetryAfterS, false))
	}
	text, _ := ref.Err.Detail.(string)
	if msg, ok := approval.ErrorText(name, ref.Code, text, why); ok {
		return apierr.New(ref.Err.Status, msg)
	}
	return err
}

// agentRefusal is a machine's refusal with one of the approval protocol's codes, as agentCall returns it: the
// error the page gets, and the code for a caller that acts on it.
type agentRefusal struct {
	err  *apierr.Error
	code string
}

func (e *agentRefusal) Error() string { return e.err.Error() }
func (e *agentRefusal) Unwrap() error { return e.err }

// refusalCode is the code a machine gave with its refusal ("" when err is none, or has none).
func refusalCode(err error) string {
	var a *agentRefusal
	if errors.As(err, &a) {
		return a.code
	}
	var p *powerctl.AgentRefusal
	if errors.As(err, &p) {
		return p.Code
	}
	return ""
}

// askAfterRefusal is for a request the panel sent without asking the machine first, because the machine's last
// answer said that it checks nothing, and which the machine then refused with "approval_required" (the check
// was switched on a moment ago, on the machine or from the panel). Instead of an error, the machine is asked
// now and the dialog for its approval is shown, as if the panel had known. It returns that answer (401, the
// request is kept), the machine's refusal of the question, or nil when this request cannot be asked for (a
// replay, an API token) or the machine says no approval is needed after all.
func (c *reqCtx) askAfterRefusal(u *model.User, t approvalTarget) error {
	if c.resumed() || u.Auth == "token" || !c.s.approvalRL.allow(u.Sub) {
		return nil
	}
	c.s.Poller.Scheduler.Kick("agent:" + t.agent.ID) // the page's view of the machine, and the panel's record, catch up
	if err := c.s.bindingRefusal(t.agent.ID, t.name); err != nil {
		return err
	}
	ch, _, err := c.challenge(t)
	if err != nil {
		return err
	}
	if ch == nil {
		return nil
	}
	plan, err := approval.NewPlan([]approval.Bound{*ch})
	if err != nil {
		return nil
	}
	c.s.Poller.Scheduler.Kick("agent:" + t.agent.ID) // the page's view of the machine catches up
	return c.askApproval(u, newApprovalPlan(plan))
}
