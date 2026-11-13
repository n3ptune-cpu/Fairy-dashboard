package poller

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/normalise"
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
//   - A record counts only while its host is in the configuration (audit 6, r4-x1-03). A record whose host the
//     panel does not have at a start is set aside in memory (setAsideLocked): it keeps no machine id, and
//     nothing inherits it. It is NOT taken out of machines.json for that: a host can be missing at one start
//     without being gone (an added host whose tokens file was damaged, a list of added hosts that was moved
//     away, a typing mistake in config.yaml), and it has its record again when it is back (review of
//     passkey-k, RK-01). A record is deleted where the panel itself takes a host out or puts one in: a host
//     removed in the panel loses it at once (RemoveAgent), and a host added in the panel under an id starts
//     with nothing on record, whatever the file held for that id (AddAgent).
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
	// could not be written or read: Detail says why), "gone" (the record of AgentID was deleted because a host
	// was added in the panel under that id: the old record is not for it; Binding.Record is what was deleted).
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
	// gone are the records that were deleted when the file could be read again, because the panel itself had
	// added a host under their id meanwhile (forget): each is announced once (announceLocked).
	gone []MachineEvent
	// aside are the records whose host is not in the configuration of this start (setAsideLocked). They do not
	// count for anything and are in machines.json as they were.
	aside map[string]approval.Record
	// out are the events that were decided and are not sent yet, in the order they were decided
	// (queueMachinesLocked, emitMachines).
	out []MachineEvent
	// forget are the ids whose record the panel must delete as soon as the file can be read: a host was removed
	// or added in the panel under that id while machines.json could not be read (true: it was added).
	forget map[string]bool
}

// machineSettle is how long after the start the poller waits at most for every agent to be asked once before it
// announces changes of alarms (a paused agent is never asked).
const machineSettle = 45 * time.Second

func (p *Poller) loadMachines() {
	p.mach = machineStore{dirty: map[string]uint64{}, drop: map[string]uint64{}, last: map[string]string{},
		aside: map[string]approval.Record{}, forget: map[string]bool{}}
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
	p.setAsideLocked()
	p.demoteLocked()
	p.standingAlarmsLocked()
}

// setAsideLocked takes every record whose host the panel does not have out of the records that count (audit 6,
// r4-x1-03): the host was taken out of config.yaml or renamed there while the panel was down, or it is missing
// at this start for another reason. (A host removed IN the panel loses its record at once, RemoveAgent.) Such a
// record did two things it must not do. It counted as the keeper of its machine id, so the same machine under
// its new name could never be accepted: "kept by" a host that has no page and no button. And a host that was
// added under the old id later had the old machine's id on record from its first answer, without a Turn on and
// without an admin's accept.
//
// The record is set aside in memory and nothing is written: it keeps no id, it is no host's record, and
// machines.json holds it as before. It used to be deleted from the file at this point, for good (review of
// passkey-k, RK-01): one start at which a host was not in the merged configuration was enough, and the host did
// not have to be gone. An added host whose tokens file could not be read at that start is skipped, a missing
// or emptied list of added hosts is "no hosts", an id can be mistyped in config.yaml for one start, and a
// second "fairy serve" with another config file on the same data folder reads the same record. After each of
// them every such machine was "Not on record" and needed an accept with the id compared on the machine.
// Now a host that is back at a later start, under the same id, has its record again. The second half of the
// finding is closed where the panel itself puts a host in: AddAgent deletes whatever is there for the id.
// (Somebody who puts a host back into config.yaml under an old id gets the old record with it; whoever can do
// that can edit machines.json.)
//
// A line in the log says it; the audit log gets an entry only when a record is deleted. Caller holds p.mu, or
// nobody else has the poller yet.
func (p *Poller) setAsideLocked() {
	var ids []string
	for id := range p.mach.records {
		if _, ok := p.AgentByID(id); !ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		r := p.mach.records[id]
		slog.Warn("the record of a machine's check is set aside: the panel has no host with this id at this start (taken out of config.yaml, renamed, "+
			"or an added host that was skipped). It does not count while that is so and stays in machines.json: the host has it again when it is back under this id. "+
			"A host that shows the check under another id is \"not on record\" until an admin accepts its machine id",
			"agent", id, "machine_id", approval.ShortID(r.MachineID), "pinned", r.How)
		delete(p.mach.records, id)
		delete(p.mach.dirty, id)
		p.mach.aside[id] = r
	}
}

// forgetPendingLocked deletes the records the panel was to delete while machines.json could not be read
// (forgetMachineLocked): of a host that was removed in the panel, and of an id a host was added under. Caller
// holds p.mu.
func (p *Poller) forgetPendingLocked() {
	ids := make([]string, 0, len(p.mach.forget))
	for id := range p.mach.forget {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		added := p.mach.forget[id]
		delete(p.mach.forget, id)
		if r, ok := p.dropRecordLocked(id); ok && added {
			p.mach.gone = append(p.mach.gone, MachineEvent{AgentID: id, Kind: "gone", Binding: approval.Binding{Record: r, Known: true}})
		}
	}
}

// dropRecordLocked deletes the record of id, in memory (counted or set aside) and, with the next write, in the
// file. It returns what was there. Caller holds p.mu.
func (p *Poller) dropRecordLocked(id string) (approval.Record, bool) {
	r, ok := p.mach.records[id]
	if !ok {
		if r, ok = p.mach.aside[id]; !ok {
			return approval.Record{}, false
		}
	}
	delete(p.mach.records, id)
	delete(p.mach.aside, id)
	delete(p.mach.dirty, id)
	p.mach.seq++
	p.mach.drop[id] = p.mach.seq
	return r, true
}

// MachinesLoaded writes what reading the record at the start changed (ids taken off the record: demoteLocked).
// The server calls it once, when OnMachine is set. Without it that waits for the first answer of an agent.
func (p *Poller) MachinesLoaded() {
	p.mu.Lock()
	p.queueMachinesLocked(p.mach.gone)
	p.mach.gone = nil
	p.mu.Unlock()
	p.machinesChanged()
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
			slog.Warn("a machine id that came on record at first sight (an earlier build) is not on record any more: an admin accepts the id the machine shows (Approval, on its page)",
				"agent", id, "machine_id", approval.ShortID(r.MachineID))
		} else {
			slog.Warn("a machine id whose record does not say that this panel issued it at Turn on or that an admin accepted it is not on record any more: an admin accepts the id the machine shows (Approval, on its page)",
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
		p.forgetPendingLocked()
		p.setAsideLocked()
		p.demoteLocked()
		p.standingAlarmsLocked()
	}
}

// clampMachineLocked makes sure the record of the agent is not newer than the panel's clock: asked is the
// moment the panel is about to ask the agent, and a record whose id came on record "after" that moment is
// counted as of that moment. Caller holds p.mu.
//
// The decision "the check is gone" (approval.BindVanished) takes an answer only when it was asked at or after
// the time the id came on record: an answer from before a Turn on says nothing about the machine after it. With
// the panel's clock BEHIND the record's time (a record written while the clock was ahead, a host restored to
// an older time) no answer ever was, for as long as the clock stayed behind: a machine whose check was cleared
// read "required", "already on", its accept answered "nothing to accept", and every request was refused with
// "press Approval on the host's page", where there was nothing to press (review of passkey-k, RK-03). A
// record cannot be newer than now, so its time is taken down to the moment of this question, in memory: the
// answer that follows counts, the alarm and its button are there with it. The file is not written for that
// alone (the next write of this record carries the time), and a clock that is right again changes nothing:
// the record is then older than every answer, as every record is. A record that is not newer than the clock
// is never touched, so an answer that was asked before an id came on record still says nothing about it.
func (p *Poller) clampMachineLocked(id string, asked time.Time) {
	if r, ok := p.mach.records[id]; ok && !asked.IsZero() && r.At.After(asked) {
		r.At = asked.UTC()
		p.mach.records[id] = r
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
	gone := p.mach.gone // records deleted when the file could be read again (forgetPendingLocked): said once
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

// queueMachinesLocked takes events for OnMachine in the order they were decided. Caller holds p.mu, which is
// what makes the order of the queue the order of the decisions; they are sent after the lock is released
// (machinesChanged, emitMachines).
func (p *Poller) queueMachinesLocked(evs []MachineEvent) {
	p.mach.out = append(p.mach.out, evs...)
}

// machinesChanged writes what is not written yet and sends the events that wait. Never called with p.mu held.
func (p *Poller) machinesChanged() {
	if err := p.flushMachines(); err != nil {
		p.emitMachines(MachineEvent{Kind: "store", Detail: err.Error()})
		return
	}
	p.emitMachines()
}

// emitMachines sends the events that wait (and then more, which are no decision about a machine: a write that
// failed) to OnMachine, in the order they were decided. Never called with p.mu held.
//
// The order is the point. Events are decided under p.mu and sent without it, and two answers of one poll are
// two goroutines: each used to send what it had decided itself, and the one that decided first could send
// last. The audit log and the Notifications list then ended with an alarm that had already been replaced
// ("Check gone" written after "One machine id on two hosts", which was the standing one), or with "cleared"
// under an alarm that stood. Now a decision goes into one queue while p.mu is held, and whoever sends takes
// the whole queue: one sender at a time (machEmit), so what a later decision's goroutine sends first is the
// earlier decision.
func (p *Poller) emitMachines(more ...MachineEvent) {
	p.machEmit.Lock()
	defer p.machEmit.Unlock()
	p.mu.Lock()
	evs := append(p.mach.out, more...)
	p.mach.out = nil
	p.mu.Unlock()
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
	p.queueMachinesLocked(p.announceLocked())
	p.version++
	p.mu.Unlock()
	err := p.flushMachines()
	p.emitMachines()
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
	p.queueMachinesLocked(evs)
	p.version++
	p.mu.Unlock()
	err = p.flushMachines()
	p.emitMachines()
	return what, before, err
}

// ApprovalNow asks the agent for its /metrics now and returns what it says about its own check (nil: it answers
// without the block). ok is false when it gave no answer. Like ProbeAgent it does not change what the poller
// knows; a caller that wants that kicks the agent's poll. It is for the one moment the panel must know before it
// says anything final: a Turn on whose request got no answer (the trust may be written all the same).
func (p *Poller) ApprovalNow(ctx context.Context, agentID string) (ap *model.Approval, ok bool) {
	a, found := p.AgentByID(agentID)
	if !found {
		return nil, false
	}
	timeout := time.Duration(p.Cfg.RequestTimeout * float64(time.Second))
	cctx, cancel := context.WithTimeout(ctx, timeout+500*time.Millisecond)
	defer cancel()
	payload, err := p.Backend.FetchAgentMetrics(cctx, a, timeout)
	if err != nil {
		return nil, false
	}
	return normalise.AgentApproval(payload), true
}

// forgetMachineLocked deletes the record of an agent id because the panel itself took that host out
// (RemoveAgent) or put a host in under that id (AddAgent, added true): whatever is there for the id, counted or
// set aside, is not for the host that comes next. It returns what was deleted. While machines.json cannot be
// read, the id is remembered and its record deleted when it can (forgetPendingLocked). Caller holds p.mu; the
// caller calls machinesChanged afterwards.
func (p *Poller) forgetMachineLocked(id string, added bool) (approval.Record, bool) {
	delete(p.mach.last, id)
	if p.mach.err != nil {
		p.mach.forget[id] = added || p.mach.forget[id]
		return approval.Record{}, false
	}
	return p.dropRecordLocked(id)
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
		a.BindingWithIDs, a.BindingHeldIDs = b.With, b.Held
		return &a
	}
	if !b.Checks() {
		return nil
	}
	a := &model.Approval{State: b.Record.State, Keys: []model.ApprovalKey{}, AgentChecks: []string{}, Binding: b.Status, MachineID: b.Record.MachineID,
		BindingWith: p.hostNamesLocked(b.With), BindingHeld: p.hostNamesLocked(b.Held), BindingWithIDs: b.With, BindingHeldIDs: b.Held}
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
