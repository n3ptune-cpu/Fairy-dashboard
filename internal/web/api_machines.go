package web

import (
	"strings"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/audit"
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

// machineAlarmText is a binding alarm in a few words, for the Notifications list.
func machineAlarmText(status string, with []string) string {
	switch status {
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
	case "pinned":
		entry("approval.machine_id", e.AgentID, map[string]any{"how": b.Record.How, "machine_id": approval.ShortID(b.Record.MachineID)}, "on record")
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
				Text: "its approval check is in order again", Key: key})
		}
	}
}

// adminstateMachines is the audit target of an entry about the record itself.
const adminstateMachines = "machines.json"

// approvalAccept is POST /api/admin/approval/accept/{id} (id: the agent's id): an admin accepts what the
// machine shows NOW about its own check, after it was changed on the machine. It is the one way out of an alarm
// that the machine cannot end by itself:
//
//   - the machine shows another machine id than the one on record (`fairy-agent helper-trust set
//     --new-machine-id`, or clear and set, on the machine): the id it shows is on record from now on;
//   - two hosts have one id: the host this is pressed for keeps the id, the other one stays flagged until it
//     shows an id of its own (the copy gets one with `--new-machine-id`);
//   - the machine had the check and shows none (`helper-trust clear` on the machine): the record is removed,
//     and the machine is one without the check until it shows one again.
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
	if err := c.stepUpEach(u, "accepting what a machine shows about its own check"); err != nil {
		return err
	}
	if !s.control.allow(u.Sub) {
		return c.limited(u, s.control, "too many control requests")
	}
	id := c.r.PathValue("id")
	a, ok := s.Poller.AgentByID(id)
	if !ok {
		return apierr.New(404, "unknown machine")
	}
	name := s.machineNames([]string{a.ID})[0]
	what, before, err := s.Poller.AcceptMachine(a.ID, actorOf(u))
	params := map[string]any{"was": before.Status}
	if before.Record.MachineID != "" {
		params["on_record_before"] = approval.ShortID(before.Record.MachineID)
	}
	if len(before.With) > 0 {
		params["with"] = before.With
	}
	if what == "" { // nothing was accepted
		c.audit(u, "approval.accept", a.ID, params, "error 409: "+textsafe.Clip(err.Error(), 200))
		return apierr.New(409, name+": "+err.Error())
	}
	params["accepted"] = what
	msg := name + ": its check is off, and Fairy treats it as a machine without the check from now on."
	if what == "id" {
		params["machine_id"] = approval.ShortID(before.Report.MachineID)
		msg = name + ": the machine id it shows is on record now."
		if after := s.Poller.BindingOf(a.ID); after.Known && after.Record.Holder {
			msg = name + " keeps the machine id. The other host stays flagged until it shows an id of its own (on it: fairy-agent helper-trust set --new-machine-id)."
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

// approvalAcceptDetail says, in the dialog that asks to confirm, what exactly would be accepted.
func (c *reqCtx) approvalAcceptDetail(*model.User) string {
	b := c.s.Poller.BindingOf(c.r.PathValue("id"))
	switch {
	case b.Report.Block && b.Report.MachineID != "" && b.Record.MachineID != "" && b.Record.MachineID != b.Report.MachineID:
		return "it shows the machine id " + approval.ShortID(b.Report.MachineID) + "; on record is " + approval.ShortID(b.Record.MachineID)
	case b.Status == approval.BindDuplicate && len(b.With) > 0:
		return "it keeps its machine id; " + strings.Join(c.s.machineNames(b.With), ", ") + " must get a new one"
	case b.Status == approval.BindVanished:
		return "it shows no check any more: it becomes a machine without the check"
	}
	return ""
}
