package web

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/audit"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
)

// Which machine an approval is for: the web half of the panel's record of machine ids (internal/approval,
// binding.go; the poller keeps the record, poller/machines.go).

// helperTooOldPrefix starts the "error" of an approval block whose state is "unknown" because the RUNNING
// Windows helper is from before approvals while the machine has a trust file (the agent then refuses fan,
// power and update requests with approval_unavailable). Starting the helper does nothing: it runs. It must be
// restarted.
const helperTooOldPrefix = "the running fairy-agent-helper is older than this agent"

func helperTooOld(ap *model.Approval) bool {
	return ap != nil && ap.State == api.ApprovalUnknown && strings.HasPrefix(ap.Error, helperTooOldPrefix)
}

// unavailableWhy is what the agent's own approval block says about why it cannot check approvals
// (approval.ErrorText's why; "": the block does not say).
func (s *Server) unavailableWhy(agentID string) string {
	ap := s.Poller.ApprovalOf(agentID)
	switch {
	case ap == nil:
		return ""
	case helperTooOld(ap):
		return approval.WhyHelperOld
	case ap.State == api.ApprovalBroken:
		return approval.WhyBroken
	case ap.State == api.ApprovalUnknown && ap.EnforcedBy == api.EnforcedByHelper && !ap.LastKnown:
		return approval.WhyHelperDown
	}
	return ""
}

// retryAfter is the "retry_after_s" of an agent's 429 answer (0: none).
func retryAfter(data jsonx.M) int {
	if n := jsonx.IntP(data, "retry_after_s"); n != nil && *n >= 1 && *n <= 3600 {
		return *n
	}
	return 0
}

// machineAlarmText is a binding alarm in a few words, for the Notifications list.
func machineAlarmText(status string, with []string) string {
	switch status {
	case approval.BindNoRecord:
		return "has the check on with a machine id that is not on record: no approvals until an admin accepts it"
	case approval.BindChanged:
		return "shows another machine id than the one on record: no approvals until an admin accepts it"
	case approval.BindDuplicate:
		other := "another host"
		if len(with) > 0 {
			other = strings.Join(with, ", ")
		}
		return "has the same machine id as " + other + ": no approvals for either until that is resolved"
	case approval.BindVanished:
		return "had the check on the machine itself and shows none now"
	case approval.BindTooOld:
		return "its agent is too old for approvals that name the machine: install the current agent"
	}
	return "its approval check needs a look: " + status
}

// machineEvent is the poller's news about the record of machine ids (Poller.OnMachine): written to the audit
// log, and for an alarm (and its end) put on the Notifications list, which also pushes it.
func (s *Server) machineEvent(e poller.MachineEvent) {
	entry := func(action, target string, params map[string]any, outcome string) {
		s.writeAudit(audit.Entry{Actor: "Fairy", Action: action, Target: target, Source: "fairy", Params: params, Outcome: outcome})
	}
	if e.Kind == "store" {
		entry("approval.record", adminstateMachines, nil, "error: "+textsafe.Clip(e.Detail, 300))
		if s.Notes != nil {
			s.Notes.Add(notifications.Event{Kind: "approval_record", Type: "host", Severity: notifications.Warning, Host: "Fairy panel",
				Text: "the record of machine ids could not be written: " + textsafe.Clip(e.Detail, 200)})
		}
		return
	}
	name, sys := e.AgentID, e.AgentID
	if names := s.machineNames([]string{e.AgentID}); len(names) == 1 {
		name = names[0]
	}
	if x := s.Poller.RelaySystem(e.AgentID); x != nil {
		sys = x.ID
	}
	b := e.Binding
	key := "approval:" + e.AgentID
	switch e.Kind {
	case "gone":
		// the record of a host that is not in the configuration any more was dropped (audit 6, r4-x1-03)
		params := map[string]any{}
		if b.Record.MachineID != "" {
			params["on_record"], params["how"] = approval.ShortID(b.Record.MachineID), b.Record.How
		}
		entry("approval.record", e.AgentID, params, "dropped: the panel has no host with this id any more")
	case "alarm":
		params := map[string]any{"status": b.Status}
		if len(b.With) > 0 {
			params["with"] = b.With
		}
		if b.Record.MachineID != "" {
			params["on_record"] = approval.ShortID(b.Record.MachineID)
		}
		if b.Report.MachineID != "" {
			params["shown"] = approval.ShortID(b.Report.MachineID)
		}
		entry("approval.alarm", e.AgentID, params, "alarm: "+b.Status)
		if s.Notes != nil {
			s.Notes.Add(notifications.Event{Kind: "approval_alarm", Type: "host", Severity: notifications.Warning, SystemID: sys, Host: name,
				Text: machineAlarmText(b.Status, s.machineNames(b.With)), Key: key, Alarm: true})
		}
	case "clear":
		entry("approval.alarm", e.AgentID, nil, "cleared")
		if s.Notes != nil {
			s.Notes.Add(notifications.Event{Kind: "approval_ok", Type: "host", Severity: notifications.Info, SystemID: sys, Host: name,
				Text: machineClearText(e), Key: key})
		}
	}
}

// machineClearText says how an alarm ended. "In order again" is for a check that is on and matches the record.
// When an admin accepted what the machine shows, the line says what was accepted and by whom: after "Accept
// that its check is off" the check is OFF, and "in order again" read as if it were on.
func machineClearText(e poller.MachineEvent) string {
	by := textsafe.Clip(e.By, 80)
	switch e.Accepted {
	case "off":
		return "its check being off was accepted by " + by
	case "new":
		return "its new machine id was accepted by " + by
	case "id":
		return "its machine id was accepted by " + by
	}
	return "its approval check is in order again"
}

// adminstateMachines is the audit target of an entry about the record itself.
const adminstateMachines = "machines.json"

// approvalAccept is POST /api/admin/approval/accept/{id} (id: the agent's id) with what the page showed:
// {"machine_id": "<the id the host showed, in full>"} or {"off": true}. An admin accepts what the machine
// shows about its own check. It is the one way out of an alarm that the machine cannot end by itself, and,
// besides Turn on, the one way a machine id comes on record:
//
//   - the machine has the check on and nothing is on record for it (the check was set on the machine, or by
//     another panel; "not on record"): the id it shows is on record from now on. Nothing a host shows comes on
//     record without this (audit 5, round 3, r3-x1-01);
//   - the machine shows another machine id than the one on record (`fairy-agent helper-trust set
//     --new-machine-id`, or clear and set, on the machine): the id it shows is on record from now on;
//   - two hosts have one id: the host this is pressed for keeps the id, the other one stays flagged until it
//     shows an id of its own (the copy gets one with `--new-machine-id`);
//   - the machine had the check and shows none (`helper-trust clear` on the machine): the record is removed,
//     and the machine is one without the check until it shows one again.
//
// One id is accepted for one host only (audit 5, r2-b2-02): an id that another host keeps (it was accepted
// there, or it is on record there and not here) is refused here, 409, whoever sends the request; that host
// must get an id of its own on the machine first. And what is accepted is exactly what the dialog showed
// (audit 5, r2-x1-01): the request names it, and a host that shows something else when the request runs, also
// after the confirmation, is refused: "it changed while you were confirming".
//
// Admins in a browser only (no API token), CSRF, the kill switch, and always its own "Confirm it's you": it
// never rides on a confirmation given for something else. Nothing is sent to the machine.
func (s *Server) approvalAccept(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	u, err := c.controlAllowed(true)
	if err != nil {
		return err
	}
	var req acceptBody
	if err := c.decodeBody(&req); err != nil {
		return err
	}
	if (req.MachineID == "") != req.Off || len(req.MachineID) > 128 {
		return apierr.New(422, "say what is accepted, as the page showed it: machine_id (the id the machine shows), or off: true")
	}
	a, ok := s.Poller.AgentByID(c.r.PathValue("id"))
	if !ok {
		return apierr.New(404, "unknown machine")
	}
	name := s.machineNames([]string{a.ID})[0]
	want := poller.AcceptWant{MachineID: req.MachineID, Off: req.Off}
	refuse := func(before approval.Binding, err error) error {
		params := acceptParams(before)
		msg := name + ": " + err.Error()
		var held *poller.HeldError
		switch {
		case errors.As(err, &held):
			params["held_by"] = held.By
			msg = acceptHeldText(name, s.machineNames(held.By))
		case errors.Is(err, poller.ErrAcceptChanged):
			msg = name + ": nothing was accepted. What it shows is not what the dialog showed: it changed while you were confirming. Look at its Approval box and press the button again."
		}
		c.audit(u, "approval.accept", a.ID, params, "error 409: "+textsafe.Clip(err.Error(), 200))
		return apierr.New(409, msg)
	}
	// Before anybody is asked to confirm: an accept that would be refused for what the host shows, or for who
	// keeps its id. (AcceptMachine decides again, under the record's lock, when the request runs.)
	if b := s.Poller.BindingOf(a.ID); b.Status == approval.BindChanged || b.Status == approval.BindDuplicate || b.Status == approval.BindVanished || b.Status == approval.BindNoRecord {
		switch {
		case acceptShows(b) != want:
			return refuse(b, poller.ErrAcceptChanged)
		case len(b.Held) > 0:
			return refuse(b, &poller.HeldError{By: b.Held})
		}
	}
	if err := c.stepUpEach(u, "accepting what a machine shows about its own check"); err != nil {
		return err
	}
	if !s.control.allow(u.Sub) {
		return c.limited(u, s.control, "too many control requests")
	}
	what, before, err := s.Poller.AcceptMachine(a.ID, actorOf(u), want)
	if what == "" { // nothing was accepted
		return refuse(before, err)
	}
	params := acceptParams(before)
	params["accepted"] = what
	// one short line: "Confirm it's you" said before what this means (approvalAcceptDetail: "it becomes a machine without the check")
	msg := name + ": check off, accepted."
	if what == "id" {
		params["machine_id"] = approval.ShortID(before.Report.MachineID)
		msg = name + ": the machine id it shows is on record now."
		if after := s.Poller.BindingOf(a.ID); after.Known && after.Record.Holder {
			// what was done, in the words of what was done: it kept an id it had, or it was given the id two hosts show
			verb := " keeps the machine id. "
			if before.Record.MachineID != before.Report.MachineID {
				verb = " has the machine id on record now, and keeps it. "
			}
			switch others := s.machineNames(before.With); len(others) {
			case 0:
				msg = name + verb + "The other host stays flagged until it shows an id of its own (on it: fairy-agent helper-trust set --new-machine-id)."
			case 1:
				msg = name + verb + others[0] + " stays flagged until it shows an id of its own (on it: fairy-agent helper-trust set --new-machine-id)."
			default:
				msg = name + verb + andNames(others) + " stay flagged until each shows an id of its own (on each: fairy-agent helper-trust set --new-machine-id)."
			}
		}
	}
	outcome := "ok"
	if err != nil { // accepted, and the file could not be written
		outcome = "ok (kept in memory; machines.json could not be written: " + textsafe.Clip(err.Error(), 120) + ")"
		msg += " Fairy could not write that down (" + textsafe.Clip(err.Error(), 120) + "); it keeps it in memory and tries again."
	}
	c.audit(u, "approval.accept", a.ID, params, outcome)
	c.json(200, map[string]any{"ok": true, "accepted": what, "message": msg})
	return nil
}

// acceptBody is what the page showed when "Accept" was pressed: exactly one of the two.
type acceptBody struct {
	MachineID string `json:"machine_id"`
	Off       bool   `json:"off"`
}

// acceptShows is what an accept of the host would accept now: the id it shows, or that it shows no check.
func acceptShows(b approval.Binding) poller.AcceptWant {
	if b.Report.Block && b.Report.State == api.ApprovalRequired && b.Report.MachineID != "" {
		return poller.AcceptWant{MachineID: b.Report.MachineID}
	}
	return poller.AcceptWant{Off: true}
}

func acceptParams(before approval.Binding) map[string]any {
	params := map[string]any{"was": before.Status}
	if before.Record.MachineID != "" {
		params["on_record_before"] = approval.ShortID(before.Record.MachineID)
	}
	if len(before.With) > 0 {
		params["with"] = before.With
	}
	return params
}

// acceptHeldText is the refusal of an accept on a host whose id another host keeps (by: their names), and
// what to do instead. The Approval box of such a host says the same and has no button.
func acceptHeldText(name string, by []string) string {
	return name + ": nothing was accepted. The machine id it shows is kept by " + andNames(by) + ", and one id is accepted for one host only. " +
		"Give " + name + " a new id on the machine itself (fairy-agent helper-trust set --new-machine-id), then accept the new id here."
}

// approvalAcceptDetail says, in the dialog that asks to confirm, what exactly would be accepted: the id the
// request names (the one the page showed), not whatever the host shows by now. An id is shown in the form the
// Approval box has (approval.CompareID: the first eight and the last six characters), so that what the admin
// compares with the machine's own looks the same in both places. The audit log keeps the first six.
func (c *reqCtx) approvalAcceptDetail(*model.User) string {
	b := c.s.Poller.BindingOf(c.r.PathValue("id"))
	var req acceptBody
	_ = json.Unmarshal(c.body, &req)
	switch {
	case req.MachineID != "" && b.Record.MachineID == req.MachineID && len(b.With) > 0:
		return "it keeps its machine id " + approval.CompareID(req.MachineID) + "; " + andNames(c.s.machineNames(b.With)) + " must get a new one"
	case req.MachineID != "" && b.Record.MachineID != "":
		d := "it shows the machine id " + approval.CompareID(req.MachineID) + "; on record is " + approval.CompareID(b.Record.MachineID)
		if others := c.s.machineNames(b.With); len(others) > 0 {
			// Another host shows this new id too, and has it on record no more than this host has (a host that
			// keeps it would have refused this accept before the dialog). The admin must know before the
			// accept that there are two (second review of passkey-g, G-03). Who may accept is as it was.
			verb := " shows"
			if len(others) > 1 {
				verb = " show"
			}
			d += "; " + andNames(others) + verb + " the same id"
		}
		return d
	case req.MachineID != "" && len(b.With) > 0:
		// nothing is on record for it, and another host without a record shows the same id: the first accept decides
		return "it shows the machine id " + approval.CompareID(req.MachineID) + ", which is not on record; " + andNames(c.s.machineNames(b.With)) + " shows the same id and must get a new one"
	case req.MachineID != "":
		return "it shows the machine id " + approval.CompareID(req.MachineID) + ", which is not on record"
	case req.Off:
		return "it shows no check any more: it becomes a machine without the check"
	}
	return ""
}

// acceptCompare is the last thing an admin reads before a machine id comes on record or is kept: the dialog's
// own line under what would be accepted (second review of passkey-g, G-02). The same instruction is behind the
// info button of the Approval box; an admin who reads the dialogs only must not accept blind.
const acceptCompare = "Compare it first with the machine's own: on the machine, fairy-agent helper-status, line \"machine id:\"."

// approvalAcceptItems is that line, for the three accepts of an id (not on record, a new id, this host keeps
// it). It is a line of its own, not a part of the one-line description: that one is cut (safeTarget,
// maxActionText) and names hosts, and this sentence must be there in full whatever the hosts are called. An
// accept of "its check is off" has no id to compare.
func (c *reqCtx) approvalAcceptItems(*model.User) []string {
	var req acceptBody
	if json.Unmarshal(c.body, &req) != nil || req.MachineID == "" {
		return nil
	}
	return []string{acceptCompare}
}
