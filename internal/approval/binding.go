package approval

import (
	"crypto/rand"
	"encoding/base64"
	"io"
	"sort"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
)

// Which machine an approval is for (audit 5, r1-x1-01).
//
// A machine that checks approvals has a machine id in its trust file, and every approval names the id of the
// machine it is for. A machine cannot see other machines, so what the id is worth rests on the panel: the panel
// keeps, for each host, the id it has ON RECORD, and builds every approval with that one. It never takes the id
// from an answer that came with a challenge. An id comes on record in two ways only:
//
//   - the panel made it up itself when the check was switched on from here ("turn-on"), or
//   - an admin accepted, in the panel, exactly the id the host showed ("accepted").
//
// Never because a host shows it (audit 5, round 3, r3-x1-01). A machine id is no secret: a host that answers
// in bad faith can show the id of ANOTHER machine, and a panel that took the first id it saw on record
// ("first-sight", what an earlier build did) then made that other machine's approvals for the hostile host.
// A host that shows the check on while nothing is on record for it is "not on record" (BindNoRecord): it is
// asked for no challenge and handed no approval until an admin has accepted its id.
//
// From then on the record counts, not what the host says. Four things are an alarm, and while one of them
// holds for a host the panel asks it for no challenge and hands it no approval:
//
//   - the host shows the check on and nothing is on record for it ("no_record");
//   - the host shows another id than the one on record ("changed");
//   - two hosts have one id, on record or shown ("duplicate"): a machine made from a copy of another one's
//     disk, or one host answering in the name of another. A host that shows an id which is on record for
//     another host never gets it (Held). The host that has it on record is flagged too, until an admin says
//     that it keeps the id, because the machine behind it may be the other one (a Turn on that was handed on);
//   - a host that had the check on shows none any more, or says it is off ("vanished").
//
// This file is the part without files and HTTP: the record, what a host shows, and the decision.

// How a machine id came on record.
const (
	PinTurnOn   = "turn-on"
	PinAccepted = "accepted"
	// PinFirstSight is what a build before audit 5, round 3 wrote for an id it took from a host's first
	// answer. No id comes on record this way any more, and one that did counts as none (Record.Demote).
	PinFirstSight = "first-sight"
)

// Record is what the panel keeps about one machine that checks approvals (machines.json, by agent id).
type Record struct {
	// MachineID is the id on record ("": none, for a host that is known to have the check but has no id an
	// admin accepted or the panel issued).
	MachineID string
	How       string    // PinTurnOn, PinAccepted
	At        time.Time // when it came on record
	By        string    // who did it (the admin)
	// State is the approval state the machine last reported: required, broken, unknown, off, or "" (it
	// answered without the block). It is what the panel goes by while the machine is silent, also after a
	// restart: a machine with a record is never taken for one without the check.
	State   string
	StateAt time.Time
	// Holder: an admin said that this host is the one that keeps MachineID while another host shows the same
	// id. The other host stays flagged until it shows an id of its own.
	Holder bool
	// Unaccepted: the host showed the check on, with a machine id, while none was on record for it, and no
	// admin has accepted that id (or that the check is off) since. It names no id: nothing a host shows is kept
	// as if it were on record. It is what makes such a host that then shows no check an alarm ("vanished"),
	// like a host with an id on record: a machine that was once seen to require approvals never silently
	// becomes one without the check.
	Unaccepted bool
	// Alarm is the alarm that was last written to the audit log for this host ("": none, or it was cleared):
	// "no_record", "changed", "duplicate", "vanished", "too_old". It is kept with the record so that a restart of the panel
	// does not write every standing alarm again (audit 5, r2-b2-09): an alarm is written when it CHANGES.
	Alarm string
}

// Demote returns the record as it counts now. An id that came on record at first sight (an earlier build's
// machines.json) is not on record: the record keeps what the machine last said and that it showed the check,
// and loses the id, who "kept" it and how it came. Every other record is returned as it is.
func (r Record) Demote() Record {
	if r.How != PinFirstSight {
		return r
	}
	r.Unaccepted = r.Unaccepted || r.MachineID != ""
	r.MachineID, r.How, r.By, r.At, r.Holder = "", "", "", time.Time{}, false
	return r
}

// Report is what a host says now (its latest answer since the panel started).
type Report struct {
	Heard     bool   // it answered at least once since the panel started
	Block     bool   // that answer had the approval block
	State     string // the block's state
	MachineID string // the block's machine_id ("" when it has none or it is no id)
	Proto     int
	// Asked is when the panel sent the request this answer is to. An answer that was asked for before the id
	// came on record says nothing about the machine since: right after a Turn on the last answer still says
	// "off", and that is not a check that vanished.
	Asked time.Time
}

// What Evaluate says about a host. BindOK: nothing stands in the way of an approval for it.
const (
	BindOK        = ""
	BindChanged   = "changed"
	BindDuplicate = "duplicate"
	BindVanished  = "vanished"
	BindTooOld    = "too_old"
	BindNoRecord  = "no_record"  // not on record: it shows the check on, and no id was issued by the panel or accepted by an admin
	BindNoStore   = "unreadable" // machines.json cannot be read: the panel does not know what is on record
)

// Binding is the decision for one host.
type Binding struct {
	Status string
	Record Record
	Known  bool // a record exists
	Report Report
	// With are the other hosts (agent ids, sorted) that have the same id, for BindDuplicate; and for
	// BindNoRecord the other hosts without a record that show the same id (the first accept decides).
	With []string
	// Held are those of With that keep the id this host shares with them, so that it cannot be accepted for
	// this host too (audit 5, r2-b2-02): one id is accepted for one host only. A host keeps an id when it has
	// it on record by an admin's word (Record.Holder), or when it has it on record and this host does not
	// (this host shows an id that is another host's). Two hosts that both show an id which is on record for
	// neither are both "not on record": Held is empty for both, and the first accept decides.
	Held []string
}

// OK reports whether an approval may be asked for and handed to the host.
func (b Binding) OK() bool { return b.Status == BindOK }

// Checks reports whether the panel must treat the host as a machine that checks approvals itself: it says so
// now, or it has a record that says it did when the panel last heard from it. Such a machine is never sent a
// fan, power or update request on the strength of the panel's own confirmation alone, and is in no run.
func (b Binding) Checks() bool {
	if b.Status != BindOK {
		return true
	}
	pinned := b.Known && b.Record.MachineID != ""
	if b.Report.Block && (!pinned || !b.Report.Asked.Before(b.Record.At)) {
		return b.Report.State != api.ApprovalOff
	}
	// (a host that was seen with the check and never accepted counts like one with an id on record: also when
	// the last thing it said, before it went silent or the panel restarted, was "off")
	return pinned || (b.Known && (b.Record.Unaccepted || (b.Record.State != api.ApprovalOff && b.Record.State != "")))
}

// ids are the ids a host stands for: the one on record and the one it shows.
func ids(rec Record, known bool, rep Report) (out []string) {
	if known && rec.MachineID != "" {
		out = append(out, rec.MachineID)
	}
	if rep.Block && rep.State == api.ApprovalRequired && rep.MachineID != "" && (len(out) == 0 || out[0] != rep.MachineID) {
		out = append(out, rep.MachineID)
	}
	return out
}

// Evaluate decides for the host id, from every host's record and report. A host without a record and without a
// report is BindOK: nothing is known that would ask for an approval.
func Evaluate(id string, recs map[string]Record, reps map[string]Report) Binding {
	rec, known := recs[id]
	rep := reps[id]
	b := Binding{Record: rec, Known: known, Report: rep}
	requires := rep.Block && rep.State == api.ApprovalRequired
	pinned := known && rec.MachineID != ""
	changed := pinned && requires && rep.MachineID != "" && rep.MachineID != rec.MachineID
	// shared: the other hosts that have the id m, on record or shown
	shared := func(m string) (with []string) {
		for other := range union(recs, reps) {
			if other == id {
				continue
			}
			orec, oknown := recs[other]
			for _, x := range ids(orec, oknown, reps[other]) {
				// the admin's word: this host keeps the id on record, the other one must get its own
				if x == m && !(pinned && rec.Holder && m == rec.MachineID && !(oknown && orec.Holder)) {
					with = appendOnce(with, other)
				}
			}
		}
		return with
	}
	if changed {
		// what counts is the id it shows now: the one on record is about to be replaced, or the host is not
		// the machine on record
		b.With = shared(rep.MachineID)
	} else {
		for _, m := range ids(rec, known, rep) {
			for _, o := range shared(m) {
				b.With = appendOnce(b.With, o)
			}
		}
	}
	sort.Strings(b.With)
	mine := ids(rec, known, rep)
	if changed {
		mine = []string{rep.MachineID}
	}
	for _, o := range b.With {
		orec, ok := recs[o]
		for _, m := range mine {
			if ok && orec.MachineID == m && (orec.Holder || !pinned || rec.MachineID != m) {
				b.Held = appendOnce(b.Held, o)
			}
		}
	}
	gone := rep.Heard && (!rep.Block || rep.State == api.ApprovalOff)
	switch {
	case len(b.With) > 0 && (pinned || len(b.Held) > 0):
		// (a host without a record that shares the id it shows only with other hosts without a record is not
		// a duplicate of anything on record: it is "not on record" like them, below)
		b.Status = BindDuplicate
	case requires && (rep.Proto != api.ApprovalProto || rep.MachineID == ""):
		b.Status = BindTooOld
	case changed:
		b.Status = BindChanged
	case pinned && gone && !rep.Asked.Before(rec.At):
		b.Status = BindVanished
	case !pinned && known && rec.Unaccepted && gone:
		b.Status = BindVanished
	case requires && !pinned:
		b.Status = BindNoRecord
	}
	return b
}

func union(recs map[string]Record, reps map[string]Report) map[string]struct{} {
	out := make(map[string]struct{}, len(recs)+len(reps))
	for k := range recs {
		out[k] = struct{}{}
	}
	for k := range reps {
		out[k] = struct{}{}
	}
	return out
}

func appendOnce(l []string, s string) []string {
	for _, x := range l {
		if x == s {
			return l
		}
	}
	return append(l, s)
}

// NewMachineID makes up a machine id for a Turn on: 16 random bytes as 22 characters of base64url, the form a
// machine makes up itself (rnd nil: crypto/rand).
func NewMachineID(rnd io.Reader) (string, error) {
	if rnd == nil {
		rnd = rand.Reader
	}
	var b [16]byte
	if _, err := io.ReadFull(rnd, b[:]); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(b[:])
	return id, api.CheckMachineID(id)
}

// ShortID is a machine id as the audit log and a sentence show it: enough to tell two apart, not the id.
func ShortID(id string) string {
	if len(id) > 8 {
		return id[:6] + "…"
	}
	return id
}

// BindingText is one sentence for people about a host whose binding is not OK ("" for BindOK). name is the
// host's name; with are the names of the hosts that share its id.
func BindingText(name, status string, with []string) string {
	switch status {
	case BindChanged:
		return name + " shows another machine id than the one Fairy has on record for it. Until an admin accepts the new id, Fairy asks it for no approval."
	case BindDuplicate:
		other := "another host"
		if len(with) > 0 {
			other = andList(with)
		}
		return name + " and " + other + " have the same machine id, so each would accept an approval made for the other. Until that is resolved, Fairy asks neither for an approval."
	case BindVanished:
		return name + " had the check on and now shows none. Fairy sends it no fan, power or update request until the check is back or an admin accepts that it is off."
	case BindTooOld:
		return name + " runs an agent whose approvals do not name the machine (the test build of 2026-11-11). Install the current agent on it; Fairy asks it for no approval."
	case BindNoRecord:
		return name + " checks approvals itself, and its machine id is not on record: an admin accepts it in the Approval box on its page first."
	case BindNoStore:
		return "Fairy cannot read its record of machine ids (machines.json in its data folder; the panel's log says why), so it asks no machine for an approval."
	}
	return ""
}

// HeldText is the sentence for a host that shows a machine id another host keeps (Binding.Held; held: their
// names): it is not one of two equals any more, it is the one that needs an id of its own.
func HeldText(name string, held []string) string {
	return name + " has the same machine id as " + andList(held) + ", which keeps it: one id is accepted for one host only. Until " + name +
		" has an id of its own (on the machine: fairy-agent helper-trust set --new-machine-id, then accept the new id on its page), Fairy asks it for no approval."
}

func andList(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	out := names[0]
	for i := 1; i < len(names)-1; i++ {
		out += ", " + names[i]
	}
	return out + " and " + names[len(names)-1]
}
