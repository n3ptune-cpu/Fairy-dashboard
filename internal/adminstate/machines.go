package adminstate

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
)

// machines.json, in the data folder: what the panel has on record about the machines that check passkey
// approvals themselves (internal/approval, binding.go). It is a file of its own, not a key of state.json: it is
// what an approval's tie to its machine rests on, nothing but the panel writes it, and a panel from before it
// leaves it alone.
//
//	{ "machines": { "<agent id>": {
//	      "machine_id": "<22-64 characters>",       the id on record ("" or absent: none)
//	      "pinned":     "turn-on" | "accepted"       (anything else, or nothing, with a machine_id: the panel
//	                                                 takes the id off the record when it starts,
//	                                                 approval.Record.Demote; "first-sight" is an earlier build's)
//	      "pinned_at":  "<RFC 3339>",
//	      "pinned_by":  "<who: the admin>",
//	      "state":      "required" | "broken" | "unknown" | "off" | "",   what the machine last said
//	      "state_at":   "<RFC 3339>",
//	      "holder":     true                         (optional) an admin said this host keeps an id two hosts show
//	      "unaccepted": true                         (optional) it showed the check on, with an id nobody accepted
//	      "alarm":      "changed" | "duplicate" | ... (optional) the alarm last written to the audit log for it
//	} } }
//
// Written like the other state files: under the file's lock, read again, changed, and put in place of the old
// file in one step, mode 0600. It fails closed: a file that cannot be read or is not what it should be is an
// error, never "no records", and it is not overwritten.

// MachinesFile is the file's name in the data folder.
const MachinesFile = "machines.json"

// MachinesPath is where the records of dataDir are.
func MachinesPath(dataDir string) string { return filepath.Join(dataDir, MachinesFile) }

func (a *State) machinesPath() string { return filepath.Join(filepath.Dir(a.StatePath), MachinesFile) }

// maxRecordText bounds the texts of a record that is read (a name, a state).
const maxRecordText = 200

// Machines reads every record. An error means the file cannot be used: the caller must not go on as if there
// were none.
func (a *State) Machines() (map[string]approval.Record, error) {
	d, err := statefile.Read(a.machinesPath())
	if err != nil {
		return nil, err
	}
	return parseMachines(a.machinesPath(), d)
}

func parseMachines(path string, d statefile.Data) (map[string]approval.Record, error) {
	out := map[string]approval.Record{}
	raw, ok := d["machines"]
	if !ok {
		return out, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New(path + ": \"machines\" is not an object: Fairy will not use or overwrite the file")
	}
	for id, v := range m {
		e, ok := v.(map[string]any)
		if !ok || id == "" || len(id) > 64 {
			return nil, errors.New(path + ": the entry of " + clipRecord(id) + " is not a record: Fairy will not use or overwrite the file")
		}
		r := approval.Record{}
		r.MachineID, _ = e["machine_id"].(string)
		if r.MachineID != "" && api.CheckMachineID(r.MachineID) != nil {
			return nil, errors.New(path + ": the machine id of " + clipRecord(id) + " is none: Fairy will not use or overwrite the file")
		}
		r.How, _ = e["pinned"].(string)
		r.By, _ = e["pinned_by"].(string)
		r.State, _ = e["state"].(string)
		r.How, r.By, r.State = clipRecord(r.How), clipRecord(r.By), clipRecord(r.State)
		if s, _ := e["pinned_at"].(string); s != "" {
			r.At, _ = time.Parse(time.RFC3339, s)
		}
		if s, _ := e["state_at"].(string); s != "" {
			r.StateAt, _ = time.Parse(time.RFC3339, s)
		}
		r.Holder, _ = e["holder"].(bool)
		r.Unaccepted, _ = e["unaccepted"].(bool)
		r.Alarm, _ = e["alarm"].(string)
		r.Alarm = clipRecord(r.Alarm)
		out[id] = r
	}
	return out, nil
}

func clipRecord(s string) string {
	if len(s) > maxRecordText {
		return s[:maxRecordText]
	}
	return s
}

// SetMachines writes the records of set and removes the ones of drop, in one step. Nothing else in the file
// changes.
func (a *State) SetMachines(set map[string]approval.Record, drop []string) error {
	if len(set) == 0 && len(drop) == 0 {
		return nil
	}
	_, err := statefile.Update(a.machinesPath(), func(d statefile.Data) error {
		if _, err := parseMachines(a.machinesPath(), d); err != nil {
			return err // a file that cannot be read as records is not written over
		}
		m, _ := d["machines"].(map[string]any)
		if m == nil {
			m = map[string]any{}
			d["machines"] = m
		}
		for _, id := range drop {
			delete(m, id)
		}
		for id, r := range set {
			e := map[string]any{"state": r.State}
			if r.MachineID != "" {
				e["machine_id"], e["pinned"], e["pinned_by"] = r.MachineID, r.How, r.By
				if !r.At.IsZero() {
					e["pinned_at"] = r.At.UTC().Format(time.RFC3339)
				}
			}
			if !r.StateAt.IsZero() {
				e["state_at"] = r.StateAt.UTC().Format(time.RFC3339)
			}
			if r.Holder {
				e["holder"] = true
			}
			if r.Unaccepted && r.MachineID == "" {
				e["unaccepted"] = true
			}
			if r.Alarm != "" {
				e["alarm"] = r.Alarm
			}
			m[id] = e
		}
		return nil
	})
	return err
}
