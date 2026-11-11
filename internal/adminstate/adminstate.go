// Package adminstate holds the server-side state the admin CLI can change while the panel runs (no restart, no web
// login):
//
//	state.json     poll_overrides, control_disabled {by, at, reason}, power_control_disabled [system ids],
//	               sessions_revoked_before (epoch)
//	sessions.json  sessions {sid: {sub, name, email, ip, created, created_ts, gen}}: a signed cookie is only valid
//	               while its sid is here, for the account (sub) it names. For the Sessions page an entry also
//	               has ua, login_ts, login_ip, seen_ts and seen_ip (see Session); an entry written before
//	               they existed lacks them and is read as "not known".
package adminstate

import (
	"errors"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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

// Session is one registered login, as listed by `fairy admin sessions list` and on the Sessions page.
//
// IP, Created and CreatedTS are of the entry: the address and time this session id was registered, which is the
// sign-in, or the last "Confirm it's you" (a confirmation gives the session a new id and a new entry). The
// fields after them are what the Sessions page needs besides. An entry written before the panel recorded them
// has none of them: "" and 0 mean "not known", and the page says so.
type Session struct {
	SID       string
	Sub       string
	Name      string
	Email     string
	IP        string
	Created   string
	CreatedTS float64

	UA      string  // the browser's User-Agent header at the sign-in, cleaned and cut (CleanUA)
	LoginTS float64 // when the person signed in (Unix seconds); it stays when a confirmation renews the id
	LoginIP string  // the address they signed in from; stays likewise
	SeenTS  float64 // when the session was last really used, written at most once a minute (TouchSession)
	SeenIP  string  // the address of that use
}

// SignedIn is when the person signed in for this session: LoginTS, or for an entry from before LoginTS was
// recorded the time the entry was made (which is the sign-in, or a later confirmation). 0: not known.
func (s Session) SignedIn() float64 {
	if s.LoginTS > 0 {
		return s.LoginTS
	}
	return s.CreatedTS
}

// SignedInIP is the address of that sign-in, with the same fallback.
func (s Session) SignedInIP() string {
	if s.LoginIP != "" {
		return s.LoginIP
	}
	return s.IP
}

// sessionsOf reads the sessions out of a sessions.json object, oldest entry first.
func sessionsOf(d statefile.Data) []Session {
	ss, _ := d["sessions"].(map[string]any)
	out := []Session{}
	for sid, v := range ss {
		if !currentGen(v) {
			continue // left by an older release: not a session any more (SessionGen)
		}
		m, _ := v.(map[string]any)
		s := Session{SID: sid, CreatedTS: num(m["created_ts"]), LoginTS: num(m["login_ts"]), SeenTS: num(m["seen_ts"])}
		s.Sub, _ = m["sub"].(string)
		s.Name, _ = m["name"].(string)
		s.Email, _ = m["email"].(string)
		s.IP, _ = m["ip"].(string)
		s.Created, _ = m["created"].(string)
		s.UA, _ = m["ua"].(string)
		s.LoginIP, _ = m["login_ip"].(string)
		s.SeenIP, _ = m["seen_ip"].(string)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedTS != out[j].CreatedTS {
			return out[i].CreatedTS < out[j].CreatedTS
		}
		return out[i].SID < out[j].SID
	})
	return out
}

// ListSessions returns the registered sessions, oldest first (read from the file: the admin CLI).
func (a *State) ListSessions() ([]Session, error) {
	d, err := statefile.Read(a.SessionsPath)
	if err != nil {
		return nil, err
	}
	return sessionsOf(d), nil
}

// Sessions returns the registered sessions, oldest first, from the running panel's view of the file (the one
// SessionValid decides by). While sessions.json cannot be read there are none, and the error says why.
func (a *State) Sessions() ([]Session, error) {
	if err := a.sessions.Err(); err != nil {
		return nil, err
	}
	return sessionsOf(a.sessions.Get()), nil
}

// MaxUA is how many bytes of a User-Agent header are kept (CleanUA), and maxAddr how many of an address. With
// them an entry of sessions.json has a fixed largest size, whatever a browser sends: the file is bounded by
// that and by the number of entries (MaxSessionsPerAccount, MaxSessionsTotal).
const (
	MaxUA   = 256
	maxAddr = 64
)

// CleanUA makes a User-Agent header storable and showable: bytes that are not UTF-8 become U+FFFD, control
// characters and the characters that change the direction or the line of a text are dropped, and the result
// is cut to MaxUA bytes at a character boundary. The header is whatever the client sent; nothing is decided
// by it, it is only shown.
func CleanUA(ua string) string {
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(ua, "\uFFFD") {
		if unicode.IsControl(r) || r == 0x2028 || r == 0x2029 || (r >= 0x202a && r <= 0x202e) ||
			(r >= 0x2066 && r <= 0x2069) || r == 0x200e || r == 0x200f || r == 0x061c || r == 0xfeff {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > MaxUA {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// cleanAddr bounds an address for sessions.json. The panel passes what security.ClientIP made of the request:
// a parsed address, or the peer as the listener named it.
func cleanAddr(ip string) string {
	if len(ip) > maxAddr {
		ip = ip[:maxAddr]
	}
	return CleanUA(ip)
}

// NewSession is what the panel records about a fresh login in sessions.json.
type NewSession struct {
	Sub   string
	Name  *string
	Email *string
	IP    string
	UA    string // the request's User-Agent header, as sent (AddSession cleans and cuts it)
	// Replaces is the session id this one continues: a "Confirm it's you" gives a session a new id. The new
	// entry then keeps when and from where the person signed in (login_ts, login_ip) from that entry, when it
	// is one of the same account. "" for a sign-in.
	Replaces string
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
		ip, at := cleanAddr(info.IP), now()
		rec := map[string]any{"sub": info.Sub, "name": info.Name, "email": info.Email, "ip": ip, "gen": SessionGen}
		rec["created"] = time.Now().UTC().Format(time.RFC3339Nano)
		rec["created_ts"] = at
		// for the Sessions page: the browser, the sign-in (kept across a renewed id) and the last use
		rec["ua"] = CleanUA(info.UA)
		rec["login_ts"], rec["login_ip"] = at, ip
		if old, ok := ss[info.Replaces].(map[string]any); ok && info.Replaces != "" && subjectOf(old) == info.Sub {
			for _, k := range []string{"created_ts", "login_ts"} { // an entry from before login_ts: when it was made
				if v := num(old[k]); v > 0 {
					rec["login_ts"] = v
				}
			}
			for _, k := range []string{"ip", "login_ip"} {
				if v, _ := old[k].(string); v != "" {
					rec["login_ip"] = cleanAddr(v)
				}
			}
		}
		rec["seen_ts"], rec["seen_ip"] = at, ip
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

// SessionListed reports whether sessions.json has an entry for sid, of any generation; ok is false while the
// file cannot be read, when nothing can be said about sid.
func (a *State) SessionListed(sid string) (listed, ok bool) {
	if a.sessions.Err() != nil {
		return false, false
	}
	ss, _ := a.sessions.Get()["sessions"].(map[string]any)
	_, listed = ss[sid]
	return listed, true
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

// errNoChange ends a statefile.Update that has nothing to write.
var errNoChange = errors.New("nothing to change")

// TouchSession records that sid was really used at ts (Unix seconds) from ip. The panel calls it at most once
// a minute per session. A session that is not registered (any more) is left alone, and nothing is written.
func (a *State) TouchSession(sid string, ts float64, ip string) error {
	if sid == "" {
		return nil
	}
	_, err := statefile.Update(a.SessionsPath, func(d statefile.Data) error {
		ss, _ := d["sessions"].(map[string]any)
		rec, ok := ss[sid].(map[string]any)
		if !ok || !currentGen(rec) {
			return errNoChange
		}
		rec["seen_ts"], rec["seen_ip"] = ts, cleanAddr(ip)
		return nil
	})
	if errors.Is(err, errNoChange) {
		return nil
	}
	a.sessions.Refresh()
	return err
}

// RemoveSessions forgets several sessions in one write (the Sessions page's "Sign out everywhere else") and
// returns how many of them the file listed.
func (a *State) RemoveSessions(sids []string) (int, error) {
	n := 0
	if len(sids) == 0 {
		return 0, nil
	}
	_, err := statefile.Update(a.SessionsPath, func(d statefile.Data) error {
		n = 0
		ss, _ := d["sessions"].(map[string]any)
		for _, sid := range sids {
			if _, ok := ss[sid]; ok && sid != "" {
				delete(ss, sid)
				n++
			}
		}
		return nil
	})
	a.sessions.Refresh()
	if err != nil {
		return 0, err
	}
	return n, nil
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
