package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/powerctl"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// Machines that check passkey approvals themselves in an "Update unpowered hosts" run (README, "Machines with
// the check in a run"; the run's side is internal/wakerun, checked.go; the machine's is Fairy-agent v0.9.0).
//
// Two passkeys. The first starts the run, as always, and signs one ticket per machine that is off (it cannot
// be asked: approval.TicketBound) and answers the update challenge of each machine that is on. The second is
// asked for when the woken machines are up (the banner): it answers a fresh challenge of every waiting
// machine for exactly its update and its shutdown (run_go). The panel alone can make no such machine update
// or shut down: all it holds between the two passkeys is the ticket (runtickets.go), and all a ticket does is
// let a machine that the run woke switch itself off again.
//
// What this file sends, and when:
//
//   - POST /run/arm, once per machine, when the run says so (runOps.Arm; the run asks only for a machine its
//     own proof says it woke);
//   - POST /approval/challenge with the verb run_go, only on an admin's click on "Confirm" (wakeUpdateConfirm);
//   - POST /run/go, once per machine, in the replay of that click after the passkey (wuConfirmSend);
//   - the cancel of POST /power for "Keep on", and after every one of them a look at what the machine itself
//     says about its run (wakeUpdateKeepOn).

// runMaxDelay is the longest shutdown countdown a run's confirmation can name (api.RunShutdownDelayMaxS).
const runMaxDelay = api.RunShutdownDelayMaxS * time.Second

// selfOffNoticeS is the countdown of a machine's self-switch-off, which a ticket names.
const selfOffNoticeS = 120

// runTimings are the numbers the two passkeys of a run sign, from the configuration:
//
//   - valid_s: approval.TicketValidS, always.
//   - confirm wait: approval.run_approval_wait_s (default 600): how long the banner asks once the machines are up,
//     at most (the run ends its question earlier when a machine that waits would switch itself off before that).
//   - wait_s: how long an armed machine waits before it switches itself off: the longest another machine of
//     the run may still need to come up (wake_update.online_timeout_s), the confirm wait, and a minute; the
//     machines take at most an hour, and the confirm wait gives way to that.
//   - notice_s: selfOffNoticeS.
//   - within_s: how long a confirmed machine lets the run's one update and one shutdown through: 25 minutes
//     and the settle time, at most the 30 minutes a machine takes.
func (s *Server) runTimings() wakerun.Ticket {
	confirm := s.Cfg.Approval.RunApprovalWaitS
	if confirm < 60 || confirm > 3600 {
		confirm = 600
	}
	set := s.WakeRuns.Settings
	online := int(set.OnlineTimeout / time.Second)
	wait := online + confirm + 60
	if wait > api.RunWaitMaxS {
		wait = api.RunWaitMaxS
		confirm = max(60, min(confirm, wait-online-60))
	}
	return wakerun.Ticket{ValidS: approval.TicketValidS, WaitS: max(wait, api.RunWaitMinS), NoticeS: selfOffNoticeS,
		WithinS: min(api.RunWithinMaxS, 1500+int(set.Settle/time.Second)), ConfirmWaitS: confirm}
}

// wuShutdownDelay is the countdown of the shutdown a run asks the machine shown under id for.
func (s *Server) wuShutdownDelay(id string) time.Duration {
	if _, own := s.wuTimings(id); own > 0 {
		return own
	}
	return s.WakeRuns.Settings.ShutdownDelay
}

// wuChecked decides, for a machine that checks approvals itself, whether a run can take it (h.Checked) or why
// not (h.NoRun with its sentence). ap is what the machine says about its check, or what is on record.
func (s *Server) wuChecked(h *wuHost, x model.System, agentID string, ap *model.Approval) {
	no := func(short, why string) { h.NoRun, h.noRunShort, h.noRun = true, short, why }
	b := s.Poller.BindingOf(agentID)
	switch {
	case !b.OK():
		h.Approval = "alarm"
		no("", "is in no run: "+machineAlarmText(b.Status, s.machineNames(b.With))+" (see its page)")
		if b.Status == approval.BindNoStore {
			h.noRun = "is in no run: " + approval.BindingText(x.Name, b.Status, nil)
		}
	case ap.State != api.ApprovalRequired:
		no("its check of passkey approvals cannot be used right now",
			"is in no run: its check of passkey approvals cannot be used right now ("+orText(ap.State, api.ApprovalUnknown)+"), and a run needs the machine to check its ticket (see its page)")
	case ap.Runs != api.ApprovalRuns:
		// an agent up to v0.8.0 (or a Windows helper from before runs): refused, with the version that can
		no("its agent is older than Fairy-agent v0.9.0", wuNeedsNewAgent)
	case api.CheckMachineID(b.Record.MachineID) != nil:
		no("Fairy has no machine id on record for it", "is in no run: Fairy has no machine id on record for it, and a run's ticket is made for that id (see its page)")
	case len(s.Poller.SharedMachineID(agentID)) > 0:
		// A ticket answers no challenge: the machine id alone says which machine it is for. Two hosts with one
		// id: no ticket is signed for either, whoever was said to keep the id.
		no("another host has the same machine id", "is in no run: "+andNames(s.machineNames(s.Poller.SharedMachineID(agentID)))+
			" has the same machine id on record or shows it, and a run's ticket is tied to the machine id alone: no ticket is signed while two hosts share one (see their pages)")
	case !s.Cfg.ApprovalOn():
		no("this panel has no Pocket ID client for approvals", "is in no run: it checks passkey approvals itself, and this panel has no Pocket ID client for that (approval: in config.yaml)")
	default:
		h.Checked = true
	}
}

// wuTicketWords is the dialog's line for a machine of the wake list that checks approvals itself: what the
// passkey of the start signs for it, with the numbers.
func (s *Server) wuTicketWords(h wuHost, tm wakerun.Ticket) string {
	to := "update to " + s.latest().Version
	if !runVersionOK(s.latest().Version) {
		to = "no update (Fairy does not know the newest signed release)"
	}
	// short: the dialog cuts a line at 240 characters, and the numbers come first
	return fmt.Sprintf("wake; signs its ticket: once up it switches itself off after %d s (notice %d s) unless a second passkey confirms: "+
		"%s, shut down in %d s. Found on: nothing is sent", tm.WaitS, tm.NoticeS, to, int(s.wuShutdownDelay(h.ID)/time.Second))
}

// runVersionOK reports whether v is a version a run's confirmation can name (the machines' rule for "to").
func runVersionOK(v string) bool {
	to, run, d, w := v, "0000000000000000", api.RunShutdownDelayMinS, api.RunWithinMinS
	_, err := api.ApprovalRequest{Verb: api.ApproveRunGo, Run: &run, To: &to, ShutdownDelayS: &d, WithinS: &w}.Canonical()
	return err == nil && v != api.RunNoUpdate
}

// runStart is what the kept start of a run with machines that check approvals carries next to its plan: the
// run's id (the tickets name it, so it is made before the dialog), the numbers, and which agent gets which
// entry. In memory with the kept action.
type runStart struct {
	id      string
	list    string // wakerun.ListHash of the request the plan was made for
	numbers wakerun.Ticket
	arm     map[string]api.ApprovalRequest // agent id -> its ticket's request
	update  map[string]api.ApprovalRequest // agent id -> apply_update, for a machine that is on
}

// wuStartPlan makes the approval plan of a start: a ticket for every machine of the wake list that checks
// approvals itself, and the answer to its own update challenge for every such machine that is on. Any refusal
// ends the start before anybody is asked for a passkey.
func (s *Server) wuStartPlan(c *reqCtx, u *model.User, b wuStartBody, hosts map[string]wuHost) (*approvalPlan, error) {
	if !s.Cfg.ApprovalOn() {
		return nil, apierr.New(409, approvalNotConfigured)
	}
	if !s.approvalRL.allow(u.Sub) {
		return nil, c.limited(u, s.approvalRL, "too many control requests")
	}
	rs := &runStart{id: wakerun.NewID(), list: wakerun.ListHash(b.Hosts, b.UpdateOnly, b.UpdateIfOn), numbers: s.runTimings(),
		arm: map[string]api.ApprovalRequest{}, update: map[string]api.ApprovalRequest{}}
	tk := approval.Ticket{Run: rs.id, ValidS: rs.numbers.ValidS, WaitS: rs.numbers.WaitS, NoticeS: rs.numbers.NoticeS}
	var bound []approval.Bound
	for i, id := range slices.Concat(b.Hosts, b.UpdateOnly) {
		h := hosts[id]
		if !h.Checked {
			continue
		}
		a, ok := s.Poller.AgentByID(h.agentID)
		if !ok {
			return nil, apierr.New(409, h.Name+" is no longer in the panel. Nothing was started.")
		}
		if err := s.bindingRefusal(a.ID, h.Name); err != nil {
			return nil, err
		}
		if i < len(b.Hosts) {
			bd, err := approval.TicketBound(tk, a.ID, h.Name, s.Poller.BindingOf(a.ID).Record.MachineID, nil)
			if err != nil {
				return nil, apierr.New(409, "no run was started: "+textsafe.Clip(err.Error(), 200))
			}
			bound, rs.arm[a.ID] = append(bound, bd), tk.Request()
			continue
		}
		// it is on: it is asked now, and the passkey of the start answers its own challenge
		t := approvalTarget{agent: a, name: h.Name, req: api.ApprovalRequest{Verb: api.ApproveApplyUpdate}}
		ch, reason, err := c.challenge(t)
		if err != nil {
			return nil, err
		}
		if ch == nil {
			s.Poller.Scheduler.Kick("agent:" + a.ID)
			return nil, apierr.New(409, h.Name+" says that an update needs no approval right now ("+textsafe.Clip(orText(reason, "no reason given"), 40)+
				"), which is not what Fairy has for it. Nothing was started: look at its page, then start the run again.")
		}
		bound, rs.update[a.ID] = append(bound, *ch), t.req
	}
	plan, err := approval.NewPlan(bound)
	if err != nil { // two tickets for one machine id, among others: nothing is signed
		return nil, apierr.New(409, "no run was started: "+textsafe.Clip(err.Error(), 200))
	}
	ap := newApprovalPlan(plan)
	ap.run = rs
	ap.message = "Confirm it's you: this passkey starts the run. For a machine that checks approvals itself and is off it signs a ticket, good for " +
		spanWords(time.Duration(rs.numbers.ValidS)*time.Second) + ": once the run has woken the machine, the ticket lets it switch itself off again if nobody confirms. " +
		"Its update and its shutdown need a second passkey when the machines are up (up to " + spanWords(time.Duration(rs.numbers.ConfirmWaitS)*time.Second) + " to give it; the strip counts the time)."
	return ap, nil
}

// wuStartApproved checks the replay of a start against the plan the passkey approved: the same list, and for
// every machine that checks approvals itself now exactly the entry that was signed for it, made for the
// machine id that is on record for it now.
func (s *Server) wuStartApproved(g *approvalGrant, b wuStartBody, hosts map[string]wuHost) (*runStart, error) {
	changed := func(name string) error {
		return apierr.New(409, "The machines of this run are not as they were when you confirmed ("+name+"). Nothing was started: press the button again.")
	}
	rs := g.plan.run
	if rs == nil || rs.list != wakerun.ListHash(b.Hosts, b.UpdateOnly, b.UpdateIfOn) {
		return nil, apierr.New(409, "This is not the request that was approved. Nothing was started: press the button again.")
	}
	arm, upd := 0, 0
	for i, id := range slices.Concat(b.Hosts, b.UpdateOnly) {
		h := hosts[id]
		if !h.Checked {
			continue
		}
		set, n := rs.arm, &arm
		if i >= len(b.Hosts) {
			set, n = rs.update, &upd
		}
		req, ok := set[h.agentID]
		if !ok || !g.plan.plan.Covers(h.agentID, s.Poller.BindingOf(h.agentID).Record.MachineID, req) {
			return nil, changed(h.Name)
		}
		*n++
	}
	if arm != len(rs.arm) || upd != len(rs.update) {
		return nil, changed("a machine no longer checks approvals itself")
	}
	return rs, nil
}

// wuConfirmStart is the confirmation of a start. Without a machine that checks approvals itself it is what it
// always was: "Confirm it's you", every time. With one, the passkey is the machines' own approval: the request
// is kept with its plan, and the replay after the round trip gets the plan's runStart and the approval.
func (s *Server) wuConfirmStart(c *reqCtx, u *model.User, b wuStartBody, hosts map[string]wuHost) (*runStart, *approvalGrant, error) {
	checked := false
	for _, h := range hosts {
		checked = checked || h.Checked
	}
	g := c.approved()
	switch {
	case !checked && g == nil:
		return nil, nil, c.stepUpEach(u, "runs that wake, update and switch off machines")
	case !checked:
		return nil, nil, apierr.New(409, "The machines of this run are not as they were when you confirmed (none checks approvals itself any more). Nothing was started: press the button again.")
	case g == nil && c.resumed():
		return nil, nil, apierr.New(409, "A machine of this run checks passkey approvals itself now (none did when you pressed the button). Nothing was started: press the button again.")
	case g == nil:
		plan, err := s.wuStartPlan(c, u, b, hosts)
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, c.askApproval(u, plan)
	}
	rs, err := s.wuStartApproved(g, b, hosts)
	return rs, g, err
}

// Arm hands a machine that checks approvals itself the ticket of the run's start (wakerun.Ops): one request,
// with the approval the tickets hand out once. Every way this can end is one of three for the run: armed (the
// machine said so, for this run, with the ticket's wait), "no answer" (it may have arrived: the machine's own
// word decides), or not armed with the reason.
func (o runOps) Arm(ctx context.Context, id, run string) (time.Duration, error) {
	s := o.s
	gone := func(why string) (time.Duration, error) {
		return 0, &wakerun.OpError{Code: wakerun.CodeTicketGone, Text: why}
	}
	a, ok := s.Poller.AgentFor(id)
	x, found := o.system(id)
	if !ok || !found {
		return gone("the machine is no longer in the panel")
	}
	if off := powerctl.ControlOff(s.Poller, x.ID); off != "" {
		return gone(off)
	}
	if err := s.bindingRefusal(a.ID, x.Name); err != nil {
		return gone(shortWhy(err))
	}
	ap, why := s.tickets.take(run, "arm", a.ID, s.Poller.BindingOf(a.ID).Record.MachineID)
	if ap == nil {
		return gone(why)
	}
	status, data, err := powerctl.RunCall(ctx, s.Poller, a, api.PathRunArm, api.RunArm{Approval: ap})
	var lost *powerctl.Unanswered
	switch {
	case errors.As(err, &lost):
		return 0, &wakerun.OpError{Code: wakerun.CodeNoAnswer, Text: orText(lost.Short, "no answer")}
	case err != nil: // it never left
		return gone(shortWhy(err))
	case status >= 500 && jsonx.S(data, "code") != api.CodeRunUnavailable && jsonx.S(data, "code") != api.CodeApprovalUnavailable:
		// an error of the machine's that is not one of the two "nothing was armed": it may have got as far as arming
		return 0, &wakerun.OpError{Code: wakerun.CodeNoAnswer, Text: fmt.Sprintf("http %d from its agent", status)}
	case status != 200:
		// A "no" is the answer to this request. What is on the machine is what its approval block shows (a copy
		// of the request may have arrived first): it is asked now, and the run goes by that (wakerun, armedMaybe).
		s.Poller.Scheduler.Kick("agent:" + a.ID)
		reason, words := runRefusal(status, data)
		return 0, &wakerun.OpError{Code: wakerun.CodeRunRefused, Reason: reason, Text: words}
	}
	var ans api.RunAnswer
	raw, _ := json.Marshal(data)
	want := 0
	for _, e := range ap.Entries {
		if r, err := api.ParseRunRequest(e.Request); err == nil && r.Verb == api.ApproveRunArm && r.WaitS != nil {
			want = *r.WaitS
		}
	}
	if json.Unmarshal(raw, &ans) != nil || ans.Run != run || ans.State != api.RunArmed || ans.OffInS != want {
		// "200" for something else than this ticket: not taken for armed, and not for "not armed" either
		return 0, &wakerun.OpError{Code: wakerun.CodeNoAnswer, Text: "its agent answered, but not that it is armed for this run with the ticket's wait"}
	}
	s.Poller.Scheduler.Kick("agent:" + a.ID)
	return time.Duration(ans.OffInS) * time.Second, nil
}

// runReasonWords are the machines' reasons for refusing a ticket or a confirmation, in the panel's words.
var runReasonWords = map[string]string{
	api.RunReasonNotFresh:      "it had been up for ten minutes or longer, and a ticket arms only a machine that has just started",
	api.RunReasonStartedBefore: "it was already running when the run was started",
	api.RunReasonArmedThisBoot: "it has taken a ticket since it started, or a run was cancelled on it since then: it takes one again after its next start",
	api.RunReasonRunUsed:       "a ticket of this run was accepted on it before",
	api.RunReasonPowerPending:  "a shutdown, restart or sleep is pending on it",
	api.RunReasonLimits:        "the countdown is shorter than this machine allows (its power_control.min_delay_s)",
	api.RunReasonOtherRun:      "another run is armed or confirmed on it",
	api.RunReasonConfirmed:     "it says this run is confirmed already",
	api.RunReasonTooLate:       "its wait was over: it is switching itself off",
	api.RunReasonOver:          "this run is over on it: it was cancelled or finished there, or the machine was started again",
	api.RunReasonClockBack:     "its clock was set back, and a ticket is judged by the clock",
	api.RunReasonBeforeRecord:  "its run record was made anew after the ticket was signed",
	"clock":                    "a time in the approval does not fit its clock: the approval is too old for it, or its clock is wrong",
	"auth_time":                "the passkey check is older than it allows",
	"expired":                  "the approval is too old for it",
	"user":                     "you are not on its list of people who may approve",
	"nonce":                    "the approval was not made for this machine's id",
}

// runRefusal says a machine's "no" to POST /run/arm or /run/go: its word for the rule, and the sentence part.
func runRefusal(status int, data jsonx.M) (reason, words string) {
	code, reason := jsonx.S(data, "code"), jsonx.S(data, "reason")
	if w, ok := runReasonWords[reason]; ok && (code == api.CodeRunRefused || code == api.CodeApprovalRefused) {
		return reason, w
	}
	switch {
	case status == 401:
		return "control_token", "its agent rejected the panel's control token"
	case status == 429:
		return "busy", "it has had its control requests for this minute"
	case code == api.CodeApprovalOff:
		return code, "it says it has no check of approvals any more"
	case code == api.CodeRunUnavailable:
		return code, "it cannot read or write its run record, so it arms and confirms nothing"
	case code == api.CodeHelperTooOld || status == 404:
		return orText(code, "not_found"), "its agent, or its running helper, does not take runs (or power control is off on it)"
	}
	said := textsafe.Clip(jsonx.S(data, "error"), 160)
	if said == "" {
		said = fmt.Sprintf("agent error %d", status)
	}
	return orText(reason, orText(code, "refused")), "it says: " + said
}

// runConfirm is what the kept confirmation of a run carries next to its plan: the run and, per agent, the
// machine and the exact run_go request its challenge was asked for.
type runConfirm struct {
	run   string
	hosts map[string]runGoHost
}

type runGoHost struct {
	hostID, name string
	body         api.RunGo // without the approval
}

func (s *Server) wakeUpdateConfirm(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	if len(c.body) > 0 {
		var b struct{}
		if err := c.decodeBody(&b); err != nil {
			return err
		}
	}
	u, err := c.controlAllowed(true)
	if err != nil {
		return err
	}
	id := c.r.PathValue("id")
	if g := c.approved(); g != nil {
		return s.wuConfirmSend(c, u, id, g)
	}
	if c.resumed() {
		return apierr.New(409, "The run's confirmation is the machines' own passkey approval. Nothing was sent: press Confirm again.")
	}
	// From here on: an admin's click. The machines are asked for their challenges now, and only now.
	waiting, err := s.WakeRuns.Waiting(id)
	if err != nil {
		return apierr.New(409, err.Error())
	}
	if !s.Cfg.ApprovalOn() {
		return apierr.New(409, approvalNotConfigured)
	}
	if !s.approvalRL.allow(u.Sub) {
		return c.limited(u, s.approvalRL, "too many control requests")
	}
	rc := &runConfirm{run: id, hosts: map[string]runGoHost{}}
	var bound []approval.Bound
	var lines, notSent []string
	refused := map[string]error{}
	for _, w := range waiting {
		no := func(agentID string, err error) {
			lines = append(lines, w.Name+": NOT sent ("+shortWhy(err)+")")
			notSent = append(notSent, w.Name+" ("+shortWhy(err)+")")
			if agentID != "" {
				refused[agentID] = err
			}
		}
		a, ok := s.Poller.AgentFor(w.ID)
		if !ok {
			no("", apierr.New(409, "it is no longer in the panel"))
			continue
		}
		run, to, delay, within := id, w.To, w.ShutdownDelayS, w.WithinS
		t := approvalTarget{agent: a, name: w.Name, req: api.ApprovalRequest{Verb: api.ApproveRunGo, Run: &run, To: &to, ShutdownDelayS: &delay, WithinS: &within}}
		if err := s.bindingRefusal(a.ID, w.Name); err != nil {
			no(a.ID, err)
			continue
		}
		ch, reason, err := c.challenge(t)
		switch {
		case err != nil:
			no(a.ID, err)
		case ch == nil:
			no(a.ID, apierr.New(409, "it says that this needs no approval ("+textsafe.Clip(orText(reason, "no reason given"), 40)+"), which a machine with the check does not say"))
		default:
			bound = append(bound, *ch)
			rc.hosts[a.ID] = runGoHost{hostID: w.ID, name: w.Name, body: api.RunGo{Run: id, To: to, ShutdownDelayS: &delay, WithinS: &within}}
			lines = append(lines, w.Name+": "+runGoWords(to, delay, within)+" (checked on the machine)")
		}
	}
	if len(bound) == 0 {
		// nobody is asked for a passkey that no machine would take; and nothing asks again by itself
		c.audit(u, "wakeupdate.confirm", id, map[string]any{"run": id}, "error 409: no machine could be asked: "+strings.Join(notSent, "; "))
		return apierr.New(409, "The confirmation was not asked for, because no machine of the run takes one now: "+strings.Join(notSent, "; ")+". Nothing was sent.")
	}
	plan, err := approval.NewPlan(bound)
	if err != nil {
		return apierr.New(apierr.Upstream, "the machines' challenges cannot be used: "+textsafe.Clip(err.Error(), 200))
	}
	ap := newApprovalPlan(plan)
	ap.confirm, ap.items, ap.refused = rc, lines, refused
	ap.message = "Confirm it's you: one passkey for the machines this run woke. Each checks it itself and then lets the run's one update and one shutdown through."
	return c.askApproval(u, ap)
}

// runGoWords says a run_go request for the dialog, with its numbers.
func runGoWords(to string, delayS, withinS int) string {
	upd := "update to " + to
	if to == api.RunNoUpdate {
		upd = "no update"
	}
	return fmt.Sprintf("%s, then shut down in %d s (good for %s)", upd, delayS, spanWords(time.Duration(withinS)*time.Second))
}

// wuConfirmSend is the replay of a confirmation after the passkey: the run is told first (no second one is
// begun while this one is on its way, and none after a machine has accepted one), then every machine of the
// plan that can still be confirmed gets its POST /run/go, once, and the run is told what each answered. The
// run is confirmed when the first machine accepts. If none does, the run goes on asking (wakerun, tryOver).
func (s *Server) wuConfirmSend(c *reqCtx, u *model.User, id string, g *approvalGrant) error {
	rc := g.plan.confirm
	if rc == nil || rc.run != id {
		return apierr.New(409, "This is not the request that was approved. Nothing was sent: press Confirm again.")
	}
	var hostIDs []string
	for _, h := range rc.hosts {
		hostIDs = append(hostIDs, h.hostID)
	}
	sort.Strings(hostIDs)
	by := actorOf(u)
	begun, err := s.WakeRuns.BeginConfirm(id, by, hostIDs)
	if err != nil {
		c.audit(u, "wakeupdate.confirm", id, map[string]any{"run": id}, "error 409: "+err.Error())
		return apierr.New(409, err.Error()+" Nothing was sent.")
	}
	// Not cut short by the browser going away; each request has the agent call's own time limit, and all of
	// them together this one.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.r.Context()), 40*time.Second)
	defer cancel()
	type outcome struct {
		name string
		res  wakerun.ConfirmResult
	}
	results := map[string]outcome{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for agentID, h := range rc.hosts {
		if !slices.Contains(begun, h.hostID) {
			mu.Lock()
			results[h.hostID] = outcome{h.name, wakerun.ConfirmResult{NotAsked: true, Text: "it could no longer be confirmed: too close to the moment it switches itself off, or it has ended"}}
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := s.runGo(ctx, agentID, h, g)
			s.WakeRuns.Confirmed(id, h.hostID, res)
			mu.Lock()
			results[h.hostID] = outcome{h.name, res}
			mu.Unlock()
		}()
	}
	wg.Wait()
	// the machines that wait and were not part of it: said in their rows, so nobody takes them for confirmed
	// (once the run is confirmed: after a confirmation no machine accepted they can still be asked)
	active, _ := s.WakeRuns.Snapshot()
	if active != nil && active.ID == id && active.Approval == "given" {
		for _, h := range active.Hosts {
			if _, part := results[h.ID]; part || !h.Checked || h.State != wakerun.StApproval || h.Confirm != "" {
				continue
			}
			why := "it was not up, or could not be asked, when the confirmation was given"
			if a, ok := s.Poller.AgentFor(h.ID); ok && g.plan.refused[a.ID] != nil {
				why = shortWhy(g.plan.refused[a.ID])
			}
			res := wakerun.ConfirmResult{NotAsked: true, Text: why}
			s.WakeRuns.Confirmed(id, h.ID, res)
			results[h.ID] = outcome{h.Name, res}
		}
	}
	ids := slices.Sorted(maps.Keys(results))
	sort.SliceStable(ids, func(i, j int) bool { return results[ids[i]].name < results[ids[j]].name })
	var lines []string
	okN := 0
	words := map[string]any{}
	for _, hid := range ids {
		o := results[hid]
		switch {
		case o.res.OK:
			okN++
			lines, words[hid] = append(lines, o.name+": confirmed"), "confirmed"
		case o.res.NotAsked:
			lines, words[hid] = append(lines, o.name+": NOT sent ("+o.res.Text+")"), "not sent"
		case o.res.Unanswered:
			lines, words[hid] = append(lines, o.name+": no answer ("+o.res.Text+"): it may have arrived, and it is not sent again"), "no answer"
		default:
			lines, words[hid] = append(lines, o.name+": NOT accepted ("+o.res.Text+")"), "refused: "+orText(o.res.Reason, "refused")
		}
	}
	msg := fmt.Sprintf("confirmed on %d of %d machine%s", okN, len(ids), map[bool]string{true: "", false: "s"}[len(ids) == 1])
	if _, err := s.WakeRuns.Waiting(id); okN == 0 && err == nil && active != nil && active.ID == id && active.Approval == "needed" {
		// the run's one confirmation was not used up, and a machine can still be asked: said, with until when
		msg = "No machine accepted this confirmation (by " + by + "). The run is not confirmed and still asks"
		if active.ConfirmBy != nil {
			msg += " until " + active.ConfirmBy.UTC().Format("15:04 UTC")
		}
		msg += ": an admin whose passkey the machines take can confirm."
	}
	c.audit(u, "wakeupdate.confirm", id, map[string]any{"run": id, "machines": words, "approver": g.sub}, msg)
	c.json(200, map[string]any{"ok": true, "message": msg, "lines": lines, "all_ok": okN == len(ids)})
	return nil
}

// runGo sends one machine its confirmation.
func (s *Server) runGo(ctx context.Context, agentID string, h runGoHost, g *approvalGrant) wakerun.ConfirmResult {
	notSent := func(why string) wakerun.ConfirmResult {
		return wakerun.ConfirmResult{Reason: "not_sent", Text: "nothing was sent: " + why}
	}
	a, ok := s.Poller.AgentByID(agentID)
	if !ok {
		return notSent("it is no longer in the panel")
	}
	if err := s.bindingRefusal(agentID, h.name); err != nil {
		return notSent(shortWhy(err))
	}
	ap, err := g.For(agentID, s.Poller.BindingOf(agentID).Record.MachineID, h.body.Request())
	if err != nil {
		return notSent("this is not the request that was approved for it")
	}
	body := h.body
	body.Approval = ap
	status, data, err := powerctl.RunCall(ctx, s.Poller, a, api.PathRunGo, body)
	s.Poller.Scheduler.Kick("agent:" + agentID)
	var lost *powerctl.Unanswered
	switch {
	case errors.As(err, &lost):
		return wakerun.ConfirmResult{Unanswered: true, Text: orText(lost.Short, "no answer")}
	case err != nil:
		return notSent(shortWhy(err))
	case status >= 500 && jsonx.S(data, "code") != api.CodeRunUnavailable && jsonx.S(data, "code") != api.CodeApprovalUnavailable:
		return wakerun.ConfirmResult{Unanswered: true, Text: fmt.Sprintf("http %d from its agent", status)}
	case status != 200:
		reason, words := runRefusal(status, data)
		return wakerun.ConfirmResult{Reason: reason, Text: words}
	}
	var ans api.RunAnswer
	raw, _ := json.Marshal(data)
	if json.Unmarshal(raw, &ans) != nil || ans.Run != h.body.Run || ans.State != api.RunConfirmed || h.body.WithinS == nil || ans.WithinS != *h.body.WithinS {
		return wakerun.ConfirmResult{Unanswered: true, Text: "its agent answered, but not that this run is confirmed"}
	}
	return wakerun.ConfirmResult{OK: true}
}

// runWaiting is what the banner shows (GET /api/admin/wake-update/waiting, and the stream's run_wait event
// without the fields that count down): the run that asks for its second confirmation, and the machines that
// may switch themselves off by a ticket, also of a run that is over or was interrupted by a restart.
type runWaiting struct {
	Run       string     `json:"run,omitempty"` // the run that asks for a confirmation now ("": none)
	By        string     `json:"by,omitempty"`  // who started it
	ConfirmBy *time.Time `json:"confirm_by,omitempty"`
	LeftS     int        `json:"left_s,omitempty"` // seconds left to confirm, on the run's steady clock (the one its question ends by)
	Hosts     []string   `json:"hosts,omitempty"`  // the names of the machines that wait
	Tried     string     `json:"tried,omitempty"`  // who gave a confirmation that no machine accepted (the run still asks)
	// Armed: machines that may switch themselves off, with the time as far as the panel knows it.
	Armed []runArmed `json:"armed,omitempty"`
	Now   time.Time  `json:"now"`
}

type runArmed struct {
	Run       string     `json:"run"`
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	At        *time.Time `json:"at,omitempty"`
	Latest    bool       `json:"latest,omitempty"`     // At is the latest it can be (its arming got no answer)
	ByMachine bool       `json:"by_machine,omitempty"` // At is the machine's own word, by its own clock
	Sure      bool       `json:"sure"`                 // false: its arming got no answer; it may not be armed at all
	Active    bool       `json:"active,omitempty"`     // of the run that is going
}

func (s *Server) runWaitingNow() runWaiting {
	now := s.Poller.Clock().UTC()
	out := runWaiting{Now: now}
	if a, _ := s.WakeRuns.Snapshot(); a != nil && a.Approval == "needed" {
		if waiting, err := s.WakeRuns.Waiting(a.ID); err == nil {
			out.Run, out.By, out.ConfirmBy, out.Tried = a.ID, a.By, a.ConfirmBy, a.Tried
			if left, ok := s.WakeRuns.ConfirmLeft(a.ID); ok {
				out.LeftS = int(left / time.Second)
			}
			for _, w := range waiting {
				out.Hosts = append(out.Hosts, w.Name)
			}
		}
	}
	for _, h := range s.WakeRuns.MayGoOff(now) {
		if h.Latest && h.At != nil { // counted from two clocks: without the jitter of that, for the stream's comparison
			at := h.At.Truncate(10 * time.Second)
			h.At = &at
		}
		out.Armed = append(out.Armed, runArmed{Run: h.RunID, ID: h.ID, Name: h.Name, At: h.At, Latest: h.Latest, ByMachine: h.ByMachine, Sure: h.Sure, Active: h.Active})
	}
	return out
}

func (s *Server) wakeUpdateWaiting(c *reqCtx) error {
	if _, err := c.requireAdmin("\"Update unpowered hosts\""); err != nil {
		return err
	}
	c.json(200, s.runWaitingNow())
	return nil
}

// runLive reports whether what a machine says about its run means that the run still acts on it: it may
// switch itself off (armed, counting down), or it lets an update and a shutdown through (confirmed). Only
// "cancelled", "done" or no run at all mean that nothing of a run is left on it.
func runLive(ap *model.Approval) (live bool, state string) {
	if ap == nil || ap.Run == nil {
		return false, ""
	}
	switch ap.Run.State {
	case api.RunArmed, api.RunOffPending, api.RunConfirmed:
		return true, ap.Run.State
	}
	return false, ap.Run.State
}

// keepOnTries: "Keep on" sends its cancel at most this often in one request.
const keepOnTries = 2

// wakeUpdateKeepOn is "Keep on" for a machine that a run's ticket armed: the cancel of the Power menu (the
// agent ends its run with it), and after every cancel a look at what the machine itself says. Only its word
// "cancelled" (or "done", or no run at all) is reported as "stays on"; a cancel that was answered while the
// machine still shows the run is sent once more, and what is still open then is said with the time. It does
// not need a run that is going: after a restart of the panel the machines of an interrupted run are kept on
// the same way. Like every cancel it passes the kill switch.
func (s *Server) wakeUpdateKeepOn(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	if len(c.body) > 0 {
		var b struct{}
		if err := c.decodeBody(&b); err != nil {
			return err
		}
	}
	u, err := c.stopGuard(s.stopRL)
	if err != nil {
		return err
	}
	id := c.r.PathValue("id")
	x, err := c.find(id)
	if err != nil {
		return err
	}
	a, ok := s.Poller.AgentFor(x.ID)
	if !ok {
		return apierr.New(409, "this is not a machine with an agent of its own")
	}
	if s.WakeRuns.Arming(x.ID) {
		// a cancel now finds no run and ends nothing, and the ticket arrives a moment later: not sent, and said
		return apierr.New(409, x.Name+" is being handed the run's ticket this very moment: nothing was cancelled. Press Keep on again in a few seconds.")
	}
	mayGo := ""
	var late *wakerun.ArmedHost // its arming got no answer, and the ticket can still reach it
	for _, h := range s.WakeRuns.MayGoOff(s.Poller.Clock()) {
		if h.ID == x.ID && h.At != nil {
			mayGo = h.At.UTC().Format("15:04 UTC")
			if h.Latest {
				mayGo += " at the latest"
			}
		}
		if h.ID == x.ID && !h.Sure && h.TicketUntil != nil {
			late = &h
		}
	}
	itMayGo := func() string {
		if mayGo == "" {
			return x.Name + " may switch itself off."
		}
		return x.Name + " may switch itself off at " + mayGo + "."
	}
	done := func(status int, outcome, msg string, stays bool) error {
		c.audit(u, "wakeupdate.keep_on", x.ID, map[string]any{"host": x.Name}, outcome)
		s.Poller.Scheduler.Kick("agent:" + a.ID)
		if status != 200 {
			return apierr.New(status, msg)
		}
		c.json(200, map[string]any{"ok": true, "stays_on": stays, "message": msg})
		return nil
	}
	said := ""
	for try := 1; try <= keepOnTries; try++ {
		req := powerctl.Request{Action: "cancel", PendingUnseen: true}
		err := req.Validate()
		if err == nil {
			_, err = powerctl.Execute(c.r.Context(), s.Poller, x, req, actorOf(u))
		}
		if err != nil { // every answer that is not the agent's "ok" is "not cancelled"
			return done(apierr.Upstream, "error: not cancelled: "+shortWhy(err), "NOT cancelled ("+shortWhy(err)+"). "+itMayGo()+" Try again, or stop it at the machine.", false)
		}
		ap, answered := s.Poller.ApprovalNow(c.r.Context(), a.ID)
		if !answered {
			return done(apierr.Upstream, "error: cancel answered, the machine's word about its run not read",
				"The cancel was answered, but "+x.Name+" did not answer when Fairy asked what it shows now: not known whether it stays on. "+itMayGo()+" Try again.", false)
		}
		live, state := runLive(ap)
		if !live && late != nil {
			// A cancel that finds no run ends nothing, and the ticket can still arrive: this is not "stays ON".
			until := late.TicketUntil.UTC().Format("15:04 UTC")
			lead := "Not known until " + until + " whether " + x.Name + " stays on: its agent shows no run now, but the run's ticket, whose request got no answer, may still reach it. "
			if late.Active && s.WakeRuns.KeepOn(x.ID, actorOf(u)) {
				return done(200, "not known until "+until+": the ticket may still arrive; the run keeps cancelling", lead+"The panel keeps cancelling until then, and the run's report says what came of it.", false)
			}
			return done(200, "not known until "+until+": the ticket may still arrive", lead+"The run it was in is not going any more, so nothing repeats the cancel: press Keep on again after "+until+", or cancel at the machine.", false)
		}
		if !live {
			how := "it shows no run any more"
			if state != "" {
				how = "it shows its run as " + state
			}
			return done(200, "ok: "+how, x.Name+" stays ON: its switch-off was called off ("+how+").", true)
		}
		said = state
	}
	return done(apierr.Upstream, "error: cancel answered, the machine still shows its run as "+said,
		"NOT cancelled: "+x.Name+" answered the cancel and still shows its run as "+said+". "+itMayGo()+" Try again, or stop it at the machine.", false)
}

// helperStopRefusal is why the helper of the machine x is not stopped from the panel right now ("": nothing
// stands in the way): the helper is what counts a run's wait and switches the PC off again, and what lets a
// confirmed run's update and shutdown through. While a run is live on the machine, by its own word or by the
// run's, it is not stopped.
func (s *Server) helperStopRefusal(x model.System) string {
	live, state := runLive(x.Approval)
	armed := false
	for _, h := range s.WakeRuns.MayGoOff(s.Poller.Clock()) {
		armed = armed || h.ID == x.ID
	}
	arming := s.WakeRuns.Arming(x.ID) // from the moment the run decides to arm it: its ticket is on its way
	if !live && !armed && !arming {
		return ""
	}
	switch {
	case state != "":
	case arming && !armed:
		state = "its ticket is on its way to it"
	default:
		state = "armed, or may be"
	}
	return x.Name + " is in a run of \"Update unpowered hosts\" (" + state + "), and its helper is what counts the run's wait and switches this PC off again: " +
		"it is not stopped while that is so. Keep the machine on first (Keep on, Updates page), or wait until the run is over on it. Nothing was sent."
}
