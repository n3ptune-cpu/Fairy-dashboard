package web

import (
	"strings"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
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
