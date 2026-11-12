package poller

import (
	"errors"
	"log/slog"
	"sort"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// The panel's record of the machines that check passkey approvals themselves (internal/approval, binding.go;
// the file is adminstate's machines.json). The poller keeps it because the poller is what hears the machines:
// every answer of an agent is compared with the record here, before anything else uses it.
//
//   - Nothing a host shows comes on record by itself (audit 5, round 3, r3-x1-01). An id is on record because
//     the panel issued it at a Turn on (PinMachine) or because an admin accepted exactly the id the host showed
//     (AcceptMachine). A host that shows the check on while it has no id on record is "not on record"
//     (approval.BindNoRecord); that it showed the check is kept (Record.Unaccepted), the id is not.
//   - A record whose id did not come one of these two ways counts as none (approval.Record.Demote): one an
//     earlier build made at first sight, and one that does not say how its id came.
//   - What a host last said about its check is kept, so that a host that is silent, also after a restart of the
//     panel, is still known to be one that checks approvals.
//   - A record lives as long as its host is in the configuration. A host removed in the panel loses it at once
//     (RemoveAgent); a host that was taken out of config.yaml, or renamed there, loses it when the panel starts
//     (dropGoneLocked; audit 6, r4-x1-03). A record never counts for a host that is gone, and a host that comes
//     under an id later starts with nothing on record.
//   - Every time something changes, each host's binding is decided again (approval.Evaluate), and a host whose
//     binding became an alarm, or stopped being one, is announced through OnMachine.
//
// The records are read once when the poller is made and kept in memory; every change is written through. While
// the file cannot be read, nothing is on record and every agent's binding says so (approval.BindNoStore): the
// panel then asks no machine for an approval and sends none a fan, power or update request.

// MachineEvent is something about a machine's record that people must hear of (OnMachine).
type MachineEvent struct {
	AgentID string
	// Kind: "alarm" (the binding became Binding.Status), "clear" (it is in order again), "store" (machines.json
	// could not be written or read: Detail says why), "gone" (the record of AgentID was dropped because the
	// panel has no such host any more; Binding.Record is what was dropped).
	Kind    string
	Binding approval.Binding
	Detail  string
	// Accepted and By are set for a "clear" that is the end of an alarm because an admin (By) accepted what the
	// machine shows: "off" (its check is off, and it is a machine without the check now), "new" (another id
	// than the one that was on record) or "id" (the id it shows, where none was on record, or the one it
	// keeps). Empty: the alarm ended by itself, and the check is on and matches the record.
	Accepted string
	By       string
}

// machineStore is the poller's part of the record. Guarded by Poller.mu.
type machineStore struct {
	records map[string]approval.Record
	err     error             // machines.json cannot be read
	dirty   map[string]uint64 // agent id -> the change that still has to be written
	drop    map[string]uint64 // agent id -> its record still has to be removed from the file
	seq     uint64
	last    map[string]string // agent id -> the binding status last announced (for a host with a record: its Record.Alarm)
	writeEr string            // the last write error that was logged
	// settleBy: until every agent was asked once since the start (or this time has come), no change of an alarm
	// is announced. What one host shows can make another host's alarm, so before all of them were heard a
	// standing alarm can look cleared, or like another one, for a moment.
	settleBy time.Time
	// gone are the records that were dropped when the file was read, because their host is not in the
	// configuration any more: each is announced once (MachinesLoaded, announceLocked).
	gone []MachineEvent
}

// machineSettle is how long after the start the poller waits at most for every agent to be asked once before it
// announces changes of alarms (a paused agent is never asked).
const machineSettle = 45 * time.Second

func (p *Poller) loadMachines() {
	p.mach = machineStore{dirty: map[string]uint64{}, drop: map[string]uint64{}, last: map[string]string{}}
	if now, _ := p.now(); !now.IsZero() {
		p.mach.settleBy = now.Add(machineSettle)
	}
	recs, err := p.Admin.Machines()
	if err != nil {
		slog.Error("the record of machine ids cannot be read: no machine is asked for an approval until it can", "error", err)
		p.mach.records, p.mach.err = map[string]approval.Record{}, err
		return
	}
	p.mach.records = recs
	p.dropGoneLocked()
	p.demoteLocked()
	p.standingAlarmsLocked()
}

// dropGoneLocked drops every record whose host the panel does not have (audit 6, r4-x1-03): it was taken out of
// config.yaml, or renamed there, while the panel was down. (A host removed IN the panel loses its record at
// once, RemoveAgent.) Such a record did two things it must not do. It counted as the keeper of its machine id,
// so the same machine under its new name could never be accepted: "kept by" a host that has no page and no
// button. And a host that was added under the old id later had the old machine's id on record from its first
// answer, without a Turn on and without an admin's accept.
//
// So the record goes, from memory now and from the file with the next write, with a line in the log and an
// entry in the audit log (MachineEvent "gone"). The host under its new name is "not on record", and one accept
// of the id it shows, behind "Confirm it's you", puts it on record again. Caller holds p.mu, or nobody else has
// the poller yet.
func (p *Poller) dropGoneLocked() {
	var ids []string
	for id := range p.mach.records {
		if _, ok := p.AgentByID(id); !ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		r := p.mach.records[id]
		slog.Warn("the record of a machine's check is dropped: the panel has no host with this id any more (taken out of config.yaml, or renamed). "+
			"A host that shows the check under another id is \"not on record\" until an admin accepts its machine id",
			"agent", id, "machine_id", approval.ShortID(r.MachineID), "pinned", r.How)
		delete(p.mach.records, id)
		delete(p.mach.dirty, id)
		p.mach.seq++
		p.mach.drop[id] = p.mach.seq
		p.mach.gone = append(p.mach.gone, MachineEvent{AgentID: id, Kind: "gone", Binding: approval.Binding{Record: r, Known: true}})
	}
}

// MachinesLoaded announces the records that were dropped when the record was read at the start, and writes what
// reading it changed (those records, and ids taken off the record: demoteLocked). The server calls it once,
// when OnMachine is set. Without it both wait for the first answer of an agent.
func (p *Poller) MachinesLoaded() {
	p.mu.Lock()
	evs := p.mach.gone
	p.mach.gone = nil
	p.mu.Unlock()
	p.machinesChanged(evs)
}

// demoteLocked takes every id off the record that did not come on record through a Turn on or an admin's
// accept (approval.Record.Demote: an allow-list of the two values this panel writes), in memory and, with the
// next write, in the file: an id that an earlier build took at first sight, and an id whose record does not say
// how it came or says something else. Such a host is "not on record" from now on, until an admin accepts the
// id it shows. Caller holds p.mu, or nobody else has the poller yet.
func (p *Poller) demoteLocked() {
	for id, r := range p.mach.records {
		d := r.Demote()
		if d == r {
			continue
		}
		if r.How == approval.PinFirstSight {
			slog.Warn("a machine id that came on record at first sight (an earlier build) is not on record any more: an admin accepts the id the machine shows in its Approval box",
				"agent", id, "machine_id", approval.ShortID(r.MachineID))
		} else {
			slog.Warn("a machine id whose record does not say that this panel issued it at Turn on or that an admin accepted it is not on record any more: an admin accepts the id the machine shows in its Approval box",
				"agent", id, "machine_id", approval.ShortID(r.MachineID), "pinned", r.How)
		}
		p.mach.records[id] = d
		p.mach.seq++
		p.mach.dirty[id] = p.mach.seq
	}
}

// standingAlarmsLocked takes the alarms the records say were written before this start as the ones last
// announced: they hold until they change. Caller holds p.mu, or nobody else has the poller yet.
func (p *Poller) standingAlarmsLocked() {
	for id, r := range p.mach.records {
		if _, ok := p.mach.last[id]; !ok && r.Alarm != "" {
			p.mach.last[id] = r.Alarm
		}
	}
}

// machinesSettledLocked reports whether changes of alarms may be announced: every agent was asked at least
// once since the start, or the start is machineSettle ago. Caller holds p.mu.
func (p *Poller) machinesSettledLocked() bool {
	if now, _ := p.now(); !now.Before(p.mach.settleBy) {
		return true
	}
	for _, a := range p.Agents() {
		_, answered := p.agentInfo[a.ID]
		if _, failed := p.agentFailures[a.ID]; !answered && !failed {
			return false
		}
	}
	return true
}

// reloadMachinesLocked tries again to read a file that could not be read. Caller holds p.mu.
func (p *Poller) reloadMachinesLocked() {
	if p.mach.err == nil {
		return
	}
	if recs, err := p.Admin.Machines(); err == nil {
		slog.Warn("the record of machine ids can be read again")
		p.mach.records, p.mach.err = recs, nil
		p.dropGoneLocked()
		p.demoteLocked()
		p.standingAlarmsLocked()
	}
}

// reportLocked is what the agent says now. Caller holds p.mu.
func (p *Poller) reportLocked(id string) approval.Report {
	info, heard := p.agentInfo[id]
	r := approval.Report{Heard: heard, Asked: info.AskedAt}
	if s, ok := p.systems[id]; ok && s.Approval != nil {
		r.Block, r.State, r.Proto = true, s.Approval.State, s.Approval.Proto
		if s.Approval.State == api.ApprovalRequired {
			r.MachineID = s.Approval.MachineID
		}
	}
	return r
}

func (p *Poller) reportsLocked() map[string]approval.Report {
	out := make(map[string]approval.Report, len(p.systems))
	for _, a := range p.Agents() {
		if r := p.reportLocked(a.ID); r.Heard || r.Block {
			out[a.ID] = r
		}
	}
	return out
}

// bindingLocked decides for one agent. Caller holds p.mu.
func (p *Poller) bindingLocked(id string, reps map[string]approval.Report) approval.Binding {
	if p.mach.err != nil {
		return approval.Binding{Status: approval.BindNoStore, Report: p.reportLocked(id)}
	}
	if reps == nil {
		reps = p.reportsLocked()
	}
	return approval.Evaluate(id, p.mach.records, reps)
}

// BindingOf is the decision about the agent's machine, from the record and from what every agent says now: see
// approval.Binding. It is asked before every approval and before every fan, power or update request.
func (p *Poller) BindingOf(agentID string) approval.Binding {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bindingLocked(agentID, nil)
}

// noteMachineLocked takes what a good answer of the agent says about its check (ap; nil: no block) into the
// record. Caller holds p.mu; the events are sent, and the file is written, after the lock is released
// (machinesChanged).
func (p *Poller) noteMachineLocked(id string, ap *model.Approval, now time.Time) (evs []MachineEvent) {
	p.reloadMachinesLocked()
	if p.mach.err != nil {
		return nil
	}
	rec, known := p.mach.records[id]
	state := ""
	if ap != nil {
		state = ap.State
	}
	changed := false
	if ap != nil && state == api.ApprovalRequired && ap.Proto == api.ApprovalProto && ap.MachineID != "" && rec.MachineID == "" && !rec.Unaccepted {
		// It shows the check on, with an id, and nothing is on record for it. The id is NOT taken on record:
		// it is only what this host says, and it may be another machine's. What is kept is that the host
		// showed the check, so that it is "not on record" now and an alarm if the check goes.
		rec.Unaccepted = true
		changed = true
	}
	noCheck := ap == nil || state == api.ApprovalOff
	switch {
	case !known && !changed && noCheck:
		return nil // a machine without the check, and nothing on record: nothing to keep
	case known && rec.MachineID == "" && !rec.Unaccepted && noCheck:
		// it had the check without an id anybody could accept (an old test build) and has none now
		delete(p.mach.records, id)
		delete(p.mach.dirty, id)
		p.mach.seq++
		p.mach.drop[id] = p.mach.seq
		return nil
	case rec.State != state || !known:
		rec.State, rec.StateAt = state, now.UTC()
		changed = true
	}
	if changed {
		p.mach.records[id] = rec
		p.mach.seq++
		p.mach.dirty[id] = p.mach.seq
		delete(p.mach.drop, id)
	}
	return nil
}

// announceLocked compares every agent's binding with the one last announced, and announces what changed: an
// alarm when it begins or becomes another one, its end when it ends. What was announced is kept with the
// host's record (Record.Alarm), so a restart of the panel announces nothing that still holds. Caller holds p.mu.
func (p *Poller) announceLocked() (evs []MachineEvent) {
	gone := p.mach.gone // records dropped when the file could be read again (dropGoneLocked): said once
	p.mach.gone = nil
	defer func() { evs = append(gone, evs...) }()
	settled := p.machinesSettledLocked()
	reps := p.reportsLocked()
	seen := map[string]bool{}
	for _, a := range p.Agents() {
		seen[a.ID] = true
		b := p.bindingLocked(a.ID, reps)
		st := b.Status
		if st == approval.BindNoStore { // not a machine's doing: the store event says it
			st = approval.BindOK
		}
		if p.mach.last[a.ID] == st || !settled {
			continue
		}
		p.mach.last[a.ID] = st
		if rec, ok := p.mach.records[a.ID]; ok && rec.Alarm != st && p.mach.err == nil {
			rec.Alarm = st
			p.mach.records[a.ID] = rec
			p.mach.seq++
			p.mach.dirty[a.ID] = p.mach.seq
			delete(p.mach.drop, a.ID)
		}
		kind := "alarm"
		if st == approval.BindOK {
			kind = "clear"
		}
		evs = append(evs, MachineEvent{AgentID: a.ID, Kind: kind, Binding: b})
	}
	for id := range p.mach.last {
		if !seen[id] {
			delete(p.mach.last, id)
		}
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].AgentID < evs[j].AgentID })
	return evs
}

// machinesChanged writes what is not written yet and sends the events. Never called with p.mu held.
func (p *Poller) machinesChanged(evs []MachineEvent) {
	if err := p.flushMachines(); err != nil {
		evs = append(evs, MachineEvent{Kind: "store", Detail: err.Error()})
	}
	p.emitMachines(evs)
}

func (p *Poller) emitMachines(evs []MachineEvent) {
	if p.OnMachine != nil {
		for _, e := range evs {
			p.OnMachine(e)
		}
	}
}

// flushMachines writes the records that changed since the last write. A failure is returned once per error
// text and tried again at the next change or poll: the record in memory stays the one that counts.
//
// One write at a time (p.machWrite), and each takes its copy of the records while it holds that lock: so the
// writes reach the file in the order of their copies, and the file never goes back to an older record. Without
// it two answers of one poll could write in the opposite order, and the older copy stayed in the file.
func (p *Poller) flushMachines() error {
	p.machWrite.Lock()
	defer p.machWrite.Unlock()
	p.mu.Lock()
	set, setSeq := map[string]approval.Record{}, map[string]uint64{}
	for id, seq := range p.mach.dirty {
		set[id], setSeq[id] = p.mach.records[id], seq
	}
	var drop []string
	dropSeq := map[string]uint64{}
	for id, seq := range p.mach.drop {
		drop = append(drop, id)
		dropSeq[id] = seq
	}
	p.mu.Unlock()
	if len(set) == 0 && len(drop) == 0 {
		return nil
	}
	err := p.Admin.SetMachines(set, drop)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		if msg := err.Error(); msg != p.mach.writeEr {
			p.mach.writeEr = msg
			slog.Error("the record of machine ids could not be written (it is kept in memory and tried again)", "error", err)
			return err
		}
		return nil
	}
	p.mach.writeEr = ""
	for id, seq := range setSeq {
		if p.mach.dirty[id] == seq {
			delete(p.mach.dirty, id)
		}
	}
	for id, seq := range dropSeq {
		if p.mach.drop[id] == seq {
			delete(p.mach.drop, id)
		}
	}
	return nil
}

// PinMachine takes machineID on record for the agent, as the id the panel itself gave the machine when the
// check was switched on from here (how: approval.PinTurnOn; nothing else is taken). It returns an error when
// the record could not be written; the id is on record in memory all the same.
func (p *Poller) PinMachine(agentID, machineID, how, by string) error {
	if err := api.CheckMachineID(machineID); err != nil {
		return err
	}
	if how != approval.PinTurnOn {
		return errors.New("a machine id comes on record through Turn on or through an admin's accept only")
	}
	p.mu.Lock()
	p.reloadMachinesLocked()
	if p.mach.err != nil {
		err := p.mach.err
		p.mu.Unlock()
		return err
	}
	now, _ := p.now()
	p.mach.records[agentID] = approval.Record{MachineID: machineID, How: how, At: now.UTC(), By: by, State: api.ApprovalRequired, StateAt: now.UTC()}
	p.mach.seq++
	p.mach.dirty[agentID] = p.mach.seq
	delete(p.mach.drop, agentID)
	evs := p.announceLocked()
	p.version++
	p.mu.Unlock()
	err := p.flushMachines()
	p.emitMachines(evs)
	return err
}

// ErrNothingToAccept: the machine does not show anything now that an admin could accept.
var ErrNothingToAccept = errors.New("the machine shows nothing to accept right now: it must be answering, and show a machine id that is not on record, another one than the one on record, the same id as another host, or no check at all")

// ErrAcceptChanged: the machine does not show what the admin was shown when asked to confirm.
var ErrAcceptChanged = errors.New("what the machine shows is not what the dialog showed: it changed while you were confirming")

// HeldError: the machine id the host shows is kept by another host (By: agent ids), and one id is accepted
// for one host only (approval.Binding.Held).
type HeldError struct{ By []string }

func (e *HeldError) Error() string {
	return "the machine id it shows is on record for another host; one id is accepted for one host only"
}

// AcceptWant is what the admin was shown and confirmed: exactly one of the machine id the host showed in the
// dialog, in full (MachineID), or that it shows no check (Off).
type AcceptWant struct {
	MachineID string
	Off       bool
}

// AcceptMachine is an admin's "accept what this machine shows" (by: who; want: what the admin was shown):
//
//   - it shows the check with a machine id (one that is not on record for it: it has none yet, or another
//     one; or the one it has, which another host shows too): that id is on record from now on
//     (approval.PinAccepted). If another host shows the same id, this host is the one that keeps it
//     (Record.Holder) and the other stays flagged until it shows an id of its own;
//   - it shows no check, or says it is off, and had an id on record: the record is removed. The machine is
//     then one without the check, until it shows one again.
//
// What is accepted is what the admin was shown, never what the host shows by the time the confirmation is
// back (audit 5, r2-x1-01): a host that shows something else now is refused with ErrAcceptChanged. And an id
// that another host keeps is not accepted for this one (HeldError; audit 5, r2-b2-02): the press on the copy
// used to take the id away from the original.
//
// It returns what was accepted: "id" or "off", and the binding from before.
func (p *Poller) AcceptMachine(agentID, by string, want AcceptWant) (what string, before approval.Binding, err error) {
	p.mu.Lock()
	p.reloadMachinesLocked()
	if p.mach.err != nil {
		err := p.mach.err
		p.mu.Unlock()
		return "", approval.Binding{}, err
	}
	reps := p.reportsLocked()
	before = p.bindingLocked(agentID, reps)
	rep := before.Report
	now, _ := p.now()
	switch {
	case before.Status != approval.BindChanged && before.Status != approval.BindDuplicate && before.Status != approval.BindVanished && before.Status != approval.BindNoRecord:
		p.mu.Unlock()
		return "", before, ErrNothingToAccept
	case rep.Block && rep.State == api.ApprovalRequired && rep.Proto == api.ApprovalProto && api.CheckMachineID(rep.MachineID) == nil:
		if want.Off || want.MachineID != rep.MachineID {
			p.mu.Unlock()
			return "", before, ErrAcceptChanged
		}
		if len(before.Held) > 0 {
			p.mu.Unlock()
			return "", before, &HeldError{By: before.Held}
		}
		what = "id"
		rec := approval.Record{MachineID: rep.MachineID, How: approval.PinAccepted, At: now.UTC(), By: by, State: rep.State, StateAt: now.UTC()}
		for other, o := range p.mach.records {
			if other != agentID && o.MachineID == rep.MachineID {
				rec.Holder = true // another host has this id on record too: this one keeps it, by the admin's word
			}
		}
		for other, r := range reps {
			if other != agentID && r.Block && r.State == api.ApprovalRequired && r.MachineID == rep.MachineID {
				rec.Holder = true
			}
		}
		p.mach.records[agentID] = rec
		p.mach.seq++
		p.mach.dirty[agentID] = p.mach.seq
	case rep.Heard && (!rep.Block || rep.State == api.ApprovalOff):
		if !want.Off || want.MachineID != "" {
			p.mu.Unlock()
			return "", before, ErrAcceptChanged
		}
		what = "off"
		delete(p.mach.records, agentID)
		delete(p.mach.dirty, agentID)
		p.mach.seq++
		p.mach.drop[agentID] = p.mach.seq
	default:
		p.mu.Unlock()
		return "", before, ErrNothingToAccept
	}
	evs := p.announceLocked()
	for i := range evs { // the end of this host's alarm is the admin's doing: said as that, not as "in order again"
		if evs[i].AgentID == agentID && evs[i].Kind == "clear" {
			evs[i].Accepted, evs[i].By = what, by
			if what == "id" && before.Status == approval.BindChanged {
				evs[i].Accepted = "new"
			}
		}
	}
	p.version++
	p.mu.Unlock()
	err = p.flushMachines()
	p.emitMachines(evs)
	return what, before, err
}

// forgetMachineLocked drops the record of an agent that was removed from the panel. Caller holds p.mu; the
// caller calls machinesChanged afterwards.
func (p *Poller) forgetMachineLocked(id string) {
	delete(p.mach.last, id)
	if _, ok := p.mach.records[id]; !ok {
		return
	}
	delete(p.mach.records, id)
	delete(p.mach.dirty, id)
	p.mach.seq++
	p.mach.drop[id] = p.mach.seq
}

// shownApprovalLocked is the agent's approval block as the pages get it: what the agent said (live, nil: no
// block), with the panel's decision about its binding; or, for an agent that says nothing about a check the
// panel has on record, a block made from the record, so that such a machine never looks like one without the
// check. Caller holds p.mu.
func (p *Poller) shownApprovalLocked(id string, live *model.Approval, reps map[string]approval.Report) *model.Approval {
	b := p.bindingLocked(id, reps)
	if live != nil {
		a := *live
		a.Binding, a.BindingWith, a.BindingHeld = b.Status, p.hostNamesLocked(b.With), p.hostNamesLocked(b.Held)
		return &a
	}
	if !b.Checks() {
		return nil
	}
	a := &model.Approval{State: b.Record.State, Keys: []model.ApprovalKey{}, AgentChecks: []string{}, Binding: b.Status, MachineID: b.Record.MachineID,
		BindingWith: p.hostNamesLocked(b.With), BindingHeld: p.hostNamesLocked(b.Held)}
	if !b.Report.Heard {
		a.LastKnown = true // what it said before it went silent (or before the panel restarted)
	}
	if a.State == "" || b.Report.Heard { // it answers and shows no block: what the record says is not its state now
		a.State = api.ApprovalUnknown
	}
	return a
}

// hostNamesLocked are the names the pages show for the agents ids (nil for none). Caller holds p.mu.
func (p *Poller) hostNamesLocked(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		name := id
		if a, ok := p.AgentByID(id); ok {
			name = a.DisplayName()
			if s, ok := p.systems[id]; ok && s.Name != "" {
				name = s.Name
			} else if s, ok := p.systems[p.linkOfLocked(a)]; ok && s.Name != "" {
				name = s.Name
			}
		}
		out = append(out, name)
	}
	return out
}
