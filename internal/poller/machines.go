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
//   - A host that shows the check with a machine id and has none on record gets that id on record ("first
//     sight").
//   - What a host last said about its check is kept, so that a host that is silent, also after a restart of the
//     panel, is still known to be one that checks approvals.
//   - Every time something changes, each host's binding is decided again (approval.Evaluate), and a host whose
//     binding became an alarm, or stopped being one, is announced through OnMachine.
//
// The records are read once when the poller is made and kept in memory; every change is written through. While
// the file cannot be read, nothing is on record and every agent's binding says so (approval.BindNoStore): the
// panel then asks no machine for an approval and sends none a fan, power or update request.

// MachineEvent is something about a machine's record that people must hear of (OnMachine).
type MachineEvent struct {
	AgentID string
	// Kind: "pinned" (an id came on record at first sight), "alarm" (the binding became Binding.Status),
	// "clear" (it is in order again), "store" (machines.json could not be written or read: Detail says why).
	Kind    string
	Binding approval.Binding
	Detail  string
}

// machineStore is the poller's part of the record. Guarded by Poller.mu.
type machineStore struct {
	records map[string]approval.Record
	err     error             // machines.json cannot be read
	dirty   map[string]uint64 // agent id -> the change that still has to be written
	drop    map[string]uint64 // agent id -> its record still has to be removed from the file
	seq     uint64
	last    map[string]string // agent id -> the binding status last announced
	writeEr string            // the last write error that was logged
}

func (p *Poller) loadMachines() {
	p.mach = machineStore{dirty: map[string]uint64{}, drop: map[string]uint64{}, last: map[string]string{}}
	recs, err := p.Admin.Machines()
	if err != nil {
		slog.Error("the record of machine ids cannot be read: no machine is asked for an approval until it can", "error", err)
		p.mach.records, p.mach.err = map[string]approval.Record{}, err
		return
	}
	p.mach.records = recs
}

// reloadMachinesLocked tries again to read a file that could not be read. Caller holds p.mu.
func (p *Poller) reloadMachinesLocked() {
	if p.mach.err == nil {
		return
	}
	if recs, err := p.Admin.Machines(); err == nil {
		slog.Warn("the record of machine ids can be read again")
		p.mach.records, p.mach.err = recs, nil
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
	changed, first := false, false
	if ap != nil && state == api.ApprovalRequired && ap.Proto == api.ApprovalProto && ap.MachineID != "" && rec.MachineID == "" {
		rec.MachineID, rec.How, rec.At, rec.By, rec.Holder = ap.MachineID, approval.PinFirstSight, now.UTC(), "Fairy", false
		changed, first = true, true
	}
	noCheck := ap == nil || state == api.ApprovalOff
	switch {
	case !known && !changed && noCheck:
		return nil // a machine without the check, and nothing on record: nothing to keep
	case known && rec.MachineID == "" && noCheck:
		// it had the check without an id the panel could take (an old test build) and has none now
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
		if first {
			evs = append(evs, MachineEvent{AgentID: id, Kind: "pinned", Binding: approval.Binding{Record: rec, Known: true}})
		}
	}
	return evs
}

// announceLocked compares every agent's binding with the one last announced. Caller holds p.mu.
func (p *Poller) announceLocked() (evs []MachineEvent) {
	reps := p.reportsLocked()
	seen := map[string]bool{}
	for _, a := range p.Agents() {
		seen[a.ID] = true
		b := p.bindingLocked(a.ID, reps)
		st := b.Status
		if st == approval.BindNoRecord || st == approval.BindNoStore { // not a machine's doing: the store event says it
			st = approval.BindOK
		}
		if p.mach.last[a.ID] == st {
			continue
		}
		p.mach.last[a.ID] = st
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
func (p *Poller) flushMachines() error {
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
// check was switched on from here (how: approval.PinTurnOn). It returns an error when the record could not be
// written; the id is on record in memory all the same.
func (p *Poller) PinMachine(agentID, machineID, how, by string) error {
	if err := api.CheckMachineID(machineID); err != nil {
		return err
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
var ErrNothingToAccept = errors.New("the machine shows nothing to accept right now: it must be answering, and show another machine id than the one on record, the same id as another host, or no check at all")

// AcceptMachine is an admin's "accept what this machine shows now" (by: who):
//
//   - it shows the check with a machine id: that id is on record from now on (approval.PinAccepted). If another
//     host has the same id, this host is the one that keeps it (Record.Holder) and the other stays flagged
//     until it shows an id of its own;
//   - it shows no check, or says it is off, and had an id on record: the record is removed. The machine is
//     then one without the check, until it shows one again.
//
// It returns what was accepted: "id" or "off", and the binding from before.
func (p *Poller) AcceptMachine(agentID, by string) (what string, before approval.Binding, err error) {
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
	case before.Status != approval.BindChanged && before.Status != approval.BindDuplicate && before.Status != approval.BindVanished:
		p.mu.Unlock()
		return "", before, ErrNothingToAccept
	case rep.Block && rep.State == api.ApprovalRequired && rep.Proto == api.ApprovalProto && api.CheckMachineID(rep.MachineID) == nil:
		what = "id"
		rec := approval.Record{MachineID: rep.MachineID, How: approval.PinAccepted, At: now.UTC(), By: by, State: rep.State, StateAt: now.UTC()}
		for other, o := range p.mach.records {
			if other == agentID || o.MachineID != rep.MachineID {
				continue
			}
			rec.Holder = true // another host has this id on record: this one keeps it, by the admin's word
			if o.Holder {
				o.Holder = false
				p.mach.records[other] = o
				p.mach.seq++
				p.mach.dirty[other] = p.mach.seq
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
		a.Binding = b.Status
		return &a
	}
	if !b.Checks() {
		return nil
	}
	a := &model.Approval{State: b.Record.State, Keys: []model.ApprovalKey{}, AgentChecks: []string{}, Binding: b.Status, MachineID: b.Record.MachineID}
	if !b.Report.Heard {
		a.LastKnown = true // what it said before it went silent (or before the panel restarted)
	}
	if a.State == "" || b.Report.Heard { // it answers and shows no block: what the record says is not its state now
		a.State = api.ApprovalUnknown
	}
	return a
}
