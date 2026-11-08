// Package adminstate: server-side state the admin CLI can change while the panel runs (no restart, no web login):
//
//	state.json     poll_overrides, control_disabled {by, at, reason}, power_control_disabled [system ids],
//	               sessions_revoked_before (epoch)
//	sessions.json  sessions {sid: {sub, name, email, ip, created, created_ts}}: a signed cookie is only valid while
//	               its sid is here
package adminstate

import (
	"errors"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/model"
	"github.com/n3ptune-cpu/Fairy/internal/statefile"
)

const SessionKeepS = 30 * 86400

type State struct {
	StatePath, SessionsPath string
	state, sessions         *statefile.Watched
}

func New(dataDir string) *State {
	sp, ss := filepath.Join(dataDir, "state.json"), filepath.Join(dataDir, "sessions.json")
	return &State{StatePath: sp, SessionsPath: ss, state: statefile.NewWatched(sp), sessions: statefile.NewWatched(ss)}
}

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

// ControlDisabled: the global kill switch (nil = control allowed).
func (a *State) ControlDisabled() *model.ControlState {
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

// StateVersion changes whenever state.json changes (for syncing poll overrides).
func (a *State) StateData() (statefile.Data, int) {
	d := a.state.Get()
	return d, a.state.Version
}

func (a *State) PowerDisabled(id string) bool {
	l, _ := a.state.Get()["power_control_disabled"].([]any)
	return slices.Contains(l, any(id))
}

func (a *State) PowerDisabledList() []string {
	l, _ := statefile.Read(a.StatePath)["power_control_disabled"].([]any)
	out := []string{}
	for _, v := range l {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func (a *State) SessionValid(sid string, iat float64) bool {
	if sid == "" {
		return false
	}
	ss, _ := a.sessions.Get()["sessions"].(map[string]any)
	_, known := ss[sid]
	if !known { // created a moment ago by another process? (cookies are signed, so this can't be spammed)
		ss, _ = a.sessions.Refresh()["sessions"].(map[string]any)
		_, known = ss[sid]
	}
	return known && iat > num(a.state.Get()["sessions_revoked_before"])
}

type Session struct {
	SID       string
	Sub       string
	Name      string
	Email     string
	IP        string
	Created   string
	CreatedTS float64
}

func (a *State) ListSessions() []Session {
	ss, _ := statefile.Read(a.SessionsPath)["sessions"].(map[string]any)
	out := []Session{}
	for sid, v := range ss {
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
	return out
}

// NewSession is what the panel records about a fresh login in sessions.json.
type NewSession struct {
	Sub   string
	Name  *string
	Email *string
	IP    string
}

// AddSession registers a new login (and drops registry entries older than SessionKeepS).
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
			if num(m["created_ts"]) < cutoff {
				delete(ss, k)
			}
		}
		rec := map[string]any{"sub": info.Sub, "name": info.Name, "email": info.Email, "ip": info.IP}
		rec["created"] = time.Now().UTC().Format(time.RFC3339Nano)
		rec["created_ts"] = now()
		ss[sid] = rec
		return nil
	})
	a.sessions.Refresh()
	return err
}

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

func (a *State) RevokeAllSessions() (int, error) {
	n := len(a.ListSessions())
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

// SetPoll: value is seconds (float64), "paused", or nil (back to default).
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
