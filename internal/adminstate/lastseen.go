package adminstate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
)

// last-seen.json, in the data folder: when each agent last gave the panel a good answer, so that a machine
// that is off when the panel starts is shown with the time it was last seen and not with "never".
//
//	{ "agents": { "<agent id>": "2026-11-13T16:25:07Z" } }
//
// It is for showing only: nothing that decides whether a machine is on or off reads it (poller, lastseen.go).
//
// A file of its own, not a key of state.json like the agents' last versions, for two reasons. It is written
// about once a minute while a machine answers, and state.json is the file that holds the kill switch and the
// session cut-off: every write of it is a lock, a rewrite and a reload by every reader. And state.json fails
// closed (a damaged one stops the panel at its start), which is right for a kill switch and wrong for a time
// that is only shown: THIS file never stops anything. A file that cannot be read, is empty, is not what the
// panel writes, is a link or belongs to another user is not used and not written (statefile: it is never
// overwritten, like every other state file); the panel starts and runs, and shows "never" as it did before
// the file existed. A plain file that is damaged is moved out of the way when the panel starts, so that the
// times are kept again from then on (SetAgentSeenAside). An entry that is not of the form is skipped. The file is written like the other state
// files (under its lock, read again, put in place of the old file in one step, mode 0600) and never read
// through a symbolic link (statefile.ReadFile).
//
// It stays small: an entry per agent, taken out when the agent is removed in the panel, and at most
// MaxAgentSeen entries (the newest are kept).

// LastSeenFile is the file's name in the data folder.
const LastSeenFile = "last-seen.json"

// MaxAgentSeen is the most entries the file holds. A panel has an entry per agent; a file with more was not
// written by this panel, and none of its entries is used.
const MaxAgentSeen = 1000

// LastSeenAside is the name a last-seen.json that cannot be used is moved to, in the same folder.
const LastSeenAside = LastSeenFile + ".bad"

func (a *State) lastSeenPath() string { return filepath.Join(filepath.Dir(a.StatePath), LastSeenFile) }

// SetAgentSeenAside moves a last-seen.json that cannot be used out of the way, to last-seen.json.bad in the
// same folder, so that the panel can start the file over; it returns the new name. It is for the one case of
// a PLAIN file that is damaged (empty, cut off, other content, far too large): such a file was never
// overwritten and never repaired itself, so the times were not kept until somebody deleted it by hand (review
// of build 14, F10). Unlike every other state file this one holds nothing that is lost with it: the times
// come back with the next answers.
//
// Only a plain file is moved, and it is moved as it is (a rename): never read, never followed. A symbolic
// link, a folder, a pipe at that name is an error and stays where it is. One older last-seen.json.bad is
// replaced, if that is a plain file too; anything else of that name is in the way, and then nothing is moved.
func (a *State) SetAgentSeenAside() (string, error) {
	path := a.lastSeenPath()
	aside := filepath.Join(filepath.Dir(path), LastSeenAside)
	st, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a plain file: it is left alone", path)
	}
	if old, err := os.Lstat(aside); err == nil && !old.Mode().IsRegular() {
		return "", fmt.Errorf("%s is in the way and is not a plain file: nothing is moved", aside)
	}
	if err := os.Rename(path, aside); err != nil {
		return "", err
	}
	return aside, nil
}

// AgentSeen returns the kept times, by agent id. The error says why the file cannot be used (the result is
// then empty); a file that does not exist is no error. An entry that is not of the form (no RFC 3339 time, a
// year outside 2000 to 9999, an id that is no id) is skipped.
func (a *State) AgentSeen() (map[string]time.Time, error) {
	out := map[string]time.Time{}
	d, err := statefile.Read(a.lastSeenPath())
	if err != nil {
		return out, err
	}
	m, _ := d["agents"].(map[string]any)
	if len(m) > MaxAgentSeen {
		return out, fmt.Errorf("%s holds %d entries, more than the %d this panel ever writes: none is used", a.lastSeenPath(), len(m), MaxAgentSeen)
	}
	for id, v := range m {
		if t, ok := seenTime(id, v); ok {
			out[id] = t
		}
	}
	return out, nil
}

func seenTime(id string, v any) (time.Time, bool) {
	s, _ := v.(string)
	if id == "" || len(id) > 128 || len(s) < 20 || len(s) > 40 {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil || t.Year() < 2000 || t.Year() > 9999 {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// SetAgentSeen writes the times in set and takes the agents in drop out (they were removed), in one write of
// the file. Entries that are not of the form go with it, and of more than MaxAgentSeen the oldest. Nothing is
// written when nothing changes.
func (a *State) SetAgentSeen(set map[string]time.Time, drop []string) error {
	_, err := statefile.Update(a.lastSeenPath(), func(d statefile.Data) error {
		m, _ := d["agents"].(map[string]any)
		changed := false
		for _, id := range drop {
			if _, ok := m[id]; ok {
				delete(m, id)
				changed = true
			}
		}
		for id, at := range set {
			v := at.UTC().Format(time.RFC3339)
			if _, ok := seenTime(id, v); !ok {
				continue
			}
			if m == nil {
				m = map[string]any{}
			}
			if m[id] != v {
				m[id] = v
				changed = true
			}
		}
		if !changed {
			return errNoChange
		}
		type entry struct {
			id string
			at time.Time
		}
		var all []entry
		for id, v := range m {
			t, ok := seenTime(id, v)
			if !ok {
				delete(m, id)
				continue
			}
			all = append(all, entry{id, t})
		}
		if len(all) > MaxAgentSeen {
			sort.Slice(all, func(i, j int) bool {
				return all[i].at.After(all[j].at) || (all[i].at.Equal(all[j].at) && all[i].id < all[j].id)
			})
			for _, e := range all[MaxAgentSeen:] {
				delete(m, e.id)
			}
		}
		for k := range d { // the file holds this one key
			delete(d, k)
		}
		d["agents"] = m
		return nil
	})
	if errors.Is(err, errNoChange) {
		return nil
	}
	return err
}
