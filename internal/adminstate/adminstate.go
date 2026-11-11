// Package adminstate holds the server-side state the admin CLI can change while the panel runs (no restart, no web
// login):
//
//	state.json     poll_overrides, control_disabled {by, at, reason}, power_control_disabled [system ids],
//	               sessions_revoked_before (epoch)
//	sessions.json  sessions {sid: {sub, name, email, ip, created, created_ts}}: a signed cookie is only valid while
//	               its sid is here, for the account (sub) it names
package adminstate

import (
	"errors"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
)

// SessionKeepS is how long (s) a session stays in sessions.json before it is pruned.
const SessionKeepS = 30 * 86400

// The limits of the session registry (T-04). Every sign-in with an empty cookie jar registers a session, any
// account the identity provider lets in can do that as often as it likes, and an entry stays for
// SessionKeepS. Without a limit the file, the time every sign-in takes to rewrite it and the memory to hold
// it grew with the number of sign-ins.
//
//   - MaxSessionsPerAccount: one account's sessions. A person has a session per browser and device, and one
//     more for every sign-in after the cookies were cleared; 50 is a new one every day of a month and then
//     some, far more than anyone uses at a time. The sign-in that goes over it drops that account's oldest.
//   - MaxSessionsTotal: all accounts together (40 accounts at their own limit). When it is reached, the
//     account with the most sessions gives up its oldest, so an account with a few keeps them.
//
// A session that is dropped is signed out: its cookie is answered 401 and the page shows the sign-in.
const (
	MaxSessionsPerAccount = 50
	MaxSessionsTotal      = 2000
)

// State gives typed access to state.json and sessions.json in the data directory.
type State struct {
	StatePath, SessionsPath string
	state, sessions         *statefile.Watched
}

// New returns the state files of dataDir.
func New(dataDir string) *State {
	sp, ss := filepath.Join(dataDir, "state.json"), filepath.Join(dataDir, "sessions.json")
	return &State{StatePath: sp, SessionsPath: ss, state: statefile.NewWatched(sp), sessions: statefile.NewWatched(ss)}
}

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

// ControlDisabled returns the global kill switch (nil = control allowed). While state.json is unreadable or
// corrupt, control counts as disabled (fail closed).
func (a *State) ControlDisabled() *model.ControlState {
	if err := a.state.Err(); err != nil {
		return &model.ControlState{By: "fairy", Reason: "state.json is unreadable or corrupt (see the server log)"}
	}
	v, ok := a.state.Get()["control_disabled"].(map[string]any)
	if !ok {
		return nil
	}
	cs := &model.ControlState{}
	cs.By, _ = v["by"].(string)
	cs.Reason, _ = v["reason"].(string)
	if s, ok := v["at"].(string); ok {
		cs.At, _ = time.Parse(time.RFC3339Nano, s)
	}
	return cs
}

// StateData returns state.json and a version number that changes whenever the file changes (for syncing poll
// overrides).
func (a *State) StateData() (statefile.Data, int) {
	d := a.state.Get()
	return d, a.state.Version
}

// PowerDisabled reports whether an administrator turned power control off for this system (always true while
// state.json is unreadable).
func (a *State) PowerDisabled(id string) bool {
	if a.state.Err() != nil {
		return true
	}
	l, _ := a.state.Get()["power_control_disabled"].([]any)
	return slices.Contains(l, any(id))
}

// PowerDisabledList returns the systems whose power control is off (read straight from disk).
func (a *State) PowerDisabledList() ([]string, error) {
	d, err := statefile.Read(a.StatePath)
	if err != nil {
		return nil, err
	}
	l, _ := d["power_control_disabled"].([]any)
	out := []string{}
	for _, v := range l {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// SessionValid reports whether a cookie's session id is still registered, was registered for this account
// (sub, the subject in the cookie) and was issued (iat, Unix seconds) after the last revoke-all. While
// sessions.json or state.json is unreadable no session is valid (fail closed).
//
// The subject is part of the question (T-22): a session id belongs to the account that signed in for it. The
// cookie is signed, so its subject cannot be changed without FAIRY_SESSION_SECRET; with this check a secret
// that leaked still gives nobody another account's session, only their own.
func (a *State) SessionValid(sid, sub string, iat float64) bool {
	if sid == "" || sub == "" || a.state.Err() != nil || a.sessions.Err() != nil {
		return false
	}
	ss, _ := a.sessions.Get()["sessions"].(map[string]any)
	rec, known := ss[sid]
	if !known { // created a moment ago by another process? (cookies are signed, so this can't be spammed)
		ss, _ = a.sessions.Refresh()["sessions"].(map[string]any)
		rec, known = ss[sid]
	}
	return known && currentGen(rec) && subjectOf(rec) == sub && iat > num(a.state.Get()["sessions_revoked_before"])
}

// subjectOf is the account a sessions.json entry was registered for ("" when the entry has none).
func subjectOf(rec any) string {
	m, _ := rec.(map[string]any)
	sub, _ := m["sub"].(string)
	return sub
}

// SessionGen is the generation of the sessions this release makes. An entry of sessions.json without it was
// made by a release before the session cookie changed (its name, its signing key, what the panel binds a
// session to): such a session is not valid any more, is not listed, and its entry is dropped with the next
// sign-in. Everyone signs in once after that upgrade; roles, sites and tokens are in other files.
const SessionGen = 2

func currentGen(rec any) bool {
	m, _ := rec.(map[string]any)
	return num(m["gen"]) == SessionGen
}

// Session is one registered login, as listed by `fairy admin sessions list`.
type Session struct {
	SID       string
	Sub       string
	Name      string
	Email     string
	IP        string
	Created   string
	CreatedTS float64
}

// ListSessions returns the registered sessions, oldest first.
func (a *State) ListSessions() ([]Session, error) {
	d, err := statefile.Read(a.SessionsPath)
	if err != nil {
		return nil, err
	}
	ss, _ := d["sessions"].(map[string]any)
	out := []Session{}
	for sid, v := range ss {
		if !currentGen(v) {
			continue // left by an older release: not a session any more (SessionGen)
		}
		m, _ := v.(map[string]any)
		s := Session{SID: sid, CreatedTS: num(m["created_ts"])}
		s.Sub, _ = m["sub"].(string)
		s.Name, _ = m["name"].(string)
		s.Email, _ = m["email"].(string)
		s.IP, _ = m["ip"].(string)
		s.Created, _ = m["created"].(string)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedTS < out[j].CreatedTS })
	return out, nil
}

// NewSession is what the panel records about a fresh login in sessions.json.
type NewSession struct {
	Sub   string
	Name  *string
	Email *string
	IP    string
}

// AddSession registers a new login (and drops registry entries older than SessionKeepS, those an older
// release left: see SessionGen, and what is over the registry's limits: see MaxSessionsPerAccount).
func (a *State) AddSession(sid string, info NewSession) error {
	_, err := statefile.Update(a.SessionsPath, func(d statefile.Data) error {
		ss, _ := d["sessions"].(map[string]any)
		if ss == nil {
			ss = map[string]any{}
			d["sessions"] = ss
		}
		cutoff := now() - SessionKeepS
		for k, v := range ss {
			m, _ := v.(map[string]any)
			if num(m["created_ts"]) < cutoff || !currentGen(v) {
				delete(ss, k)
			}
		}
		rec := map[string]any{"sub": info.Sub, "name": info.Name, "email": info.Email, "ip": info.IP, "gen": SessionGen}
		rec["created"] = time.Now().UTC().Format(time.RFC3339Nano)
		rec["created_ts"] = now()
		ss[sid] = rec
		trimSessions(ss, sid)
		return nil
	})
	a.sessions.Refresh()
	return err
}

// trimSessions drops what is over the registry's limits from ss: of every account its oldest sessions beyond
// MaxSessionsPerAccount, then, while there are more than MaxSessionsTotal, the oldest session of the account
// that has the most. keep, the session that is being added, is never dropped.
func trimSessions(ss map[string]any, keep string) {
	type entry struct {
		sid string
		ts  float64
	}
	by := map[string][]entry{} // account -> its sessions, oldest first
	for sid, v := range ss {
		if sid == keep {
			continue
		}
		m, _ := v.(map[string]any)
		sub := subjectOf(v)
		by[sub] = append(by[sub], entry{sid, num(m["created_ts"])})
	}
	room := MaxSessionsPerAccount
	for sub, l := range by {
		sort.Slice(l, func(i, j int) bool {
			if l[i].ts != l[j].ts {
				return l[i].ts < l[j].ts
			}
			return l[i].sid < l[j].sid
		})
		limit := room
		if sub == subjectOf(ss[keep]) {
			limit-- // the new session is one of this account's
		}
		for len(l) > limit {
			delete(ss, l[0].sid)
			l = l[1:]
		}
		by[sub] = l
	}
	for len(ss) > MaxSessionsTotal {
		most := ""
		for sub, l := range by {
			if len(l) > len(by[most]) || (len(l) == len(by[most]) && len(l) > 0 && sub < most) {
				most = sub
			}
		}
		if len(by[most]) == 0 {
			return
		}
		delete(ss, by[most][0].sid)
		by[most] = by[most][1:]
	}
}

// SessionAccount returns the account (subject) a session id is registered for, "" when it is not registered
// (any more) or the registry cannot be read.
func (a *State) SessionAccount(sid string) string {
	if sid == "" || a.sessions.Err() != nil {
		return ""
	}
	ss, _ := a.sessions.Get()["sessions"].(map[string]any)
	rec, known := ss[sid]
	if !known || !currentGen(rec) {
		return ""
	}
	return subjectOf(rec)
}

// RemoveSession forgets one session (logout, idle timeout); its cookie stops working.
func (a *State) RemoveSession(sid string) error {
	if sid == "" {
		return nil
	}
	_, err := statefile.Update(a.SessionsPath, func(d statefile.Data) error {
		if ss, ok := d["sessions"].(map[string]any); ok {
			delete(ss, sid)
		}
		return nil
	})
	a.sessions.Refresh()
	return err
}

// RevokeSession removes one session by sid prefix (CLI). Returns the number removed.
func (a *State) RevokeSession(prefix string) (int, error) {
	if len(prefix) < 6 {
		return 0, errors.New("give at least 6 characters of the session id")
	}
	n := 0
	_, err := statefile.Update(a.SessionsPath, func(d statefile.Data) error {
		ss, _ := d["sessions"].(map[string]any)
		for k := range ss {
			if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
				delete(ss, k)
				n++
			}
		}
		return nil
	})
	a.sessions.Refresh()
	return n, err
}

// RevokeUserSessions removes every session of one subject (when the user is removed). Returns the number removed.
func (a *State) RevokeUserSessions(sub string) (int, error) {
	n := 0
	_, err := statefile.Update(a.SessionsPath, func(d statefile.Data) error {
		ss, _ := d["sessions"].(map[string]any)
		for k, v := range ss {
			if m, _ := v.(map[string]any); m["sub"] == sub {
				delete(ss, k)
				n++
			}
		}
		return nil
	})
	a.sessions.Refresh()
	return n, err
}

// RevokeAllSessions removes every session and invalidates every cookie issued before now.
func (a *State) RevokeAllSessions() (int, error) {
	list, err := a.ListSessions()
	if err != nil {
		return 0, err
	}
	n := len(list)
	if _, err := statefile.Update(a.SessionsPath, func(d statefile.Data) error { d["sessions"] = map[string]any{}; return nil }); err != nil {
		return 0, err
	}
	if _, err := statefile.Update(a.StatePath, func(d statefile.Data) error { d["sessions_revoked_before"] = now(); return nil }); err != nil {
		return 0, err
	}
	a.sessions.Refresh()
	a.state.Refresh()
	return n, nil
}

// SetControlDisabled turns the global kill switch on (with who and why) or off.
func (a *State) SetControlDisabled(disabled bool, by, reason string) error {
	_, err := statefile.Update(a.StatePath, func(d statefile.Data) error {
		if disabled {
			d["control_disabled"] = map[string]any{"by": by, "at": time.Now().UTC().Format(time.RFC3339Nano), "reason": reason}
		} else {
			delete(d, "control_disabled")
		}
		return nil
	})
	a.state.Refresh()
	return err
}

// SetPowerDisabled turns power control off (or back on) for one system.
func (a *State) SetPowerDisabled(id string, disabled bool) error {
	_, err := statefile.Update(a.StatePath, func(d statefile.Data) error {
		cur := map[string]bool{}
		l, _ := d["power_control_disabled"].([]any)
		for _, v := range l {
			if s, ok := v.(string); ok {
				cur[s] = true
			}
		}
		if disabled {
			cur[id] = true
		} else {
			delete(cur, id)
		}
		out := make([]string, 0, len(cur))
		for k := range cur {
			out = append(out, k)
		}
		sort.Strings(out)
		d["power_control_disabled"] = out
		return nil
	})
	a.state.Refresh()
	return err
}

// SetPoll stores a polling override: value is seconds (float64), "paused", or nil (back to default).
func (a *State) SetPoll(id string, value any) error {
	_, err := statefile.Update(a.StatePath, func(d statefile.Data) error {
		po, _ := d["poll_overrides"].(map[string]any)
		if po == nil {
			po = map[string]any{}
			d["poll_overrides"] = po
		}
		if value == nil {
			delete(po, id)
		} else {
			po[id] = value
		}
		return nil
	})
	a.state.Refresh()
	return err
}
