package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// The Sessions page: every signed-in person sees where they are signed in (their own sessions) and can sign
// the other ones out; an admin also sees everyone's and can sign any of them out.
//
// Who may do what:
//
//   - Browser sessions only (an API token is refused, as on the Users page), and not a person who is still
//     waiting for access: they have the "waiting" view with its Sign out, and nothing else of the panel.
//   - A viewer has the page like an admin, for their own sessions. The list of everyone's sessions and
//     signing out another person's session are for admins.
//   - Reading the list needs the session and nothing more, like every other read. Whoever holds a copy of a
//     session cookie can therefore read that person's list. What they cannot do is end the other sessions:
//   - Ending a session that is not the one the request comes from always asks "Confirm it's you" for that one
//     action (stepUpEach): the request is kept, the person signs in again at Pocket ID, and the panel runs it
//     once. A confirmation made for something else does not count, also not inside its window. So a copied
//     cookie cannot sign the real owner out and keep the panel. Signing out the session one is using is the
//     menu's Sign out, as before.
//   - Every sign-out here is written to the audit log (sessions.revoke, sessions.revoke_others): who, from
//     which address, whose session, which browser and address that session had. No session id, no cookie.
//
// The page never gets a session id. A session is named by a handle (sessionCodec.handle): a keyed hash of the
// id, which is not a cookie and leads to none. A handle is looked up among the sessions the caller may end
// (their own; all of them for an admin), so one person's handle is nothing in another person's hands.

// sessionView is one session as the page gets it.
type sessionView struct {
	ID      string        `json:"id"` // the handle: names the session in a sign-out request
	Current bool          `json:"current"`
	User    *sessionOwner `json:"user,omitempty"` // whose it is; only in the admins' list of everyone's sessions
	// SignedIn and LastActive are null when the panel does not know them: a session from before this page
	// that was not used since.
	SignedIn   *time.Time `json:"signed_in"`
	LastActive *time.Time `json:"last_active"`
	// IP is the address of the session's latest request ("" = not known), SignedInIP the one it signed in from.
	// AlsoFrom are other addresses its requests came from since the panel started, latest first: the same
	// sign-in used from more than one place.
	IP         string        `json:"ip"`
	SignedInIP string        `json:"signed_in_ip"`
	AlsoFrom   []sessionAddr `json:"also_from"`
	Browser    string        `json:"browser"`    // "Firefox on Windows", from the User-Agent header (browserOf)
	UserAgent  string        `json:"user_agent"` // that header, cleaned and cut (adminstate.CleanUA); "" = not known
	// Location is empty: this version looks nothing up. It is where a later one puts "City, Country" for IP
	// (sessionLocation); the page shows it when it is not empty.
	Location string `json:"location"`
}

type sessionOwner struct {
	Sub   string `json:"sub"`
	Name  string `json:"name"`
	Email string `json:"email"`
	You   bool   `json:"you"`
}

type sessionAddr struct {
	IP string    `json:"ip"`
	At time.Time `json:"at"`
}

type sessionsPage struct {
	Sessions []sessionView `json:"sessions"`
	// what ends a session by itself, for the page's one line about it (session_idle_timeout, session_max_age)
	IdleTimeoutS int `json:"idle_timeout_s"`
	MaxAgeS      int `json:"max_age_s"`
}

func (s *Server) sessionRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/me/sessions", s.h(s.mySessions))
	m.HandleFunc("POST /api/me/sessions/revoke", s.h(s.revokeMySession))
	m.HandleFunc("POST /api/me/sessions/revoke-others", s.h(s.revokeMyOtherSessions))
	m.HandleFunc("GET /api/admin/sessions", s.h(s.adminSessions))
	m.HandleFunc("POST /api/admin/sessions/revoke", s.h(s.adminRevokeSession))
}

// sessionLocation is where an address is, for the Sessions page. This version has no such lookup (no network
// request, no database): always "".
func sessionLocation(string) string { return "" }

// handle is the name the Sessions page has for the session sid: the first 16 bytes of HMAC-SHA256 over the id,
// with a key derived from the session secret for this purpose alone. It cannot be turned back into the id, is
// not a cookie (the cookie's signature has another key and covers more than the id), and cannot be guessed
// (128 bits). It changes when the session id does ("Confirm it's you") and when the secret does.
func (c *sessionCodec) handle(sid string) string {
	m := hmac.New(sha256.New, c.handleKey)
	m.Write([]byte(sid))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:16])
}

// handleKeyLabel derives the key of the handles from the session secret (see cookieKeyLabel).
const handleKeyLabel = "fairy-session-handle-v1"

// knownSession is a registered session with what the panel's memory knows about its use.
type knownSession struct {
	adminstate.Session
	seen  int64      // the last real use, Unix seconds: from memory, else from sessions.json; 0 = not known
	addrs []seenAddr // the addresses its requests came from since the panel started, latest first
}

// lastAddr is the address of the session's latest request: from memory, else the one written with its last
// use; "" when neither is known.
func (k knownSession) lastAddr() string {
	if len(k.addrs) > 0 {
		return k.addrs[0].ip
	}
	return k.SeenIP
}

// sessions returns the registered sessions of sub ("" = of everyone) that can still be used. Left out are the
// ones whose cookie the panel would refuse: ended in memory, signed in longer ago than session_max_age, or not
// used for session_idle_timeout. (sessions.json keeps such an entry until its browser comes back or the entry
// is 30 days old.) The last use is the one in the panel's memory. For a session the panel has not seen since
// it started it is the one written to sessions.json, which can be up to a minute older than the cookie's own
// copy (both are refreshed once a minute, seenPersist): such a session is left out only when it is two more
// minutes over the timeout. A session from before the Sessions page that was not used since has no last use
// at all: it is listed, as "not known", until it is used or too old.
func (s *Server) sessions(sub string) []knownSession {
	all, err := s.Poller.Admin.Sessions()
	if err != nil {
		return nil
	}
	now := s.now().Unix()
	var out []knownSession
	for _, x := range all {
		if (sub != "" && x.Sub != sub) || s.codec.live.isEnded(x.SID) {
			continue
		}
		if at := x.SignedIn(); at > 0 && float64(now)-at > float64(s.Cfg.SessionMaxAge) {
			continue
		}
		k := knownSession{Session: x}
		k.seen, k.addrs = s.codec.live.usage(x.SID)
		if k.seen > 0 && now-k.seen > int64(s.Cfg.SessionIdleTimeout) {
			continue
		}
		if k.seen == 0 {
			if k.seen = int64(x.SeenTS); k.seen > 0 && now-k.seen > int64(s.Cfg.SessionIdleTimeout)+2*seenPersist {
				continue
			}
		}
		out = append(out, k)
	}
	return out
}

// findSession returns the session the handle names among those of sub ("" = of everyone).
func (s *Server) findSession(handle, sub string) (knownSession, bool) {
	if handle == "" || len(handle) > 64 {
		return knownSession{}, false
	}
	for _, k := range s.sessions(sub) {
		if hmac.Equal([]byte(s.codec.handle(k.SID)), []byte(handle)) {
			return k, true
		}
	}
	return knownSession{}, false
}

func unixTime(sec float64) *time.Time {
	if sec <= 0 {
		return nil
	}
	t := time.Unix(int64(sec), 0).UTC()
	return &t
}

// ownerLabel is how a session's account is named in the audit log and in "Confirm it's you": the e-mail
// address or name recorded at its sign-in, else the account id.
func ownerLabel(x adminstate.Session) string {
	for _, v := range []string{x.Email, x.Name} {
		if v != "" {
			return v
		}
	}
	return x.Sub
}

// views turns sessions into what the page gets, the caller's own session first, then by account, each
// account's sessions by last use (latest first). current is the caller's session id, me their account.
func (s *Server) views(list []knownSession, current, me string, withUser bool) []sessionView {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if (a.SID == current) != (b.SID == current) {
			return a.SID == current
		}
		if a.Sub != b.Sub {
			if (a.Sub == me) != (b.Sub == me) {
				return a.Sub == me
			}
			if la, lb := strings.ToLower(ownerLabel(a.Session)), strings.ToLower(ownerLabel(b.Session)); la != lb {
				return la < lb
			}
			return a.Sub < b.Sub
		}
		if a.seen != b.seen {
			return a.seen > b.seen
		}
		return a.SignedIn() > b.SignedIn()
	})
	out := []sessionView{}
	for _, k := range list {
		v := sessionView{ID: s.codec.handle(k.SID), Current: k.SID == current, SignedIn: unixTime(k.SignedIn()),
			LastActive: unixTime(float64(k.seen)), IP: k.lastAddr(), SignedInIP: k.SignedInIP(), AlsoFrom: []sessionAddr{},
			Browser: browserOf(k.UA), UserAgent: k.UA}
		v.Location = sessionLocation(v.IP)
		for i, a := range k.addrs {
			if i > 0 {
				v.AlsoFrom = append(v.AlsoFrom, sessionAddr{IP: a.ip, At: time.Unix(a.at, 0).UTC()})
			}
		}
		if withUser {
			v.User = &sessionOwner{Sub: k.Sub, Name: k.Name, Email: k.Email, You: k.Sub == me}
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) page(views []sessionView) sessionsPage {
	return sessionsPage{Sessions: views, IdleTimeoutS: int(s.Cfg.SessionIdleTimeout), MaxAgeS: int(s.Cfg.SessionMaxAge)}
}

// sessionsUser is who may use the Sessions page: a browser session (no API token) of a person with a role.
// write: the request changes something (CSRF header, same origin).
func (c *reqCtx) sessionsUser(write bool) (*model.User, error) {
	u, err := c.requireSession()
	if err != nil {
		return nil, err
	}
	if write {
		if err := c.checkCSRF(u); err != nil {
			return nil, err
		}
	}
	if _, g, _ := c.grant(); g.Pending() {
		return nil, apierr.New(403, pendingDetail)
	}
	return u, nil
}

// sessionsAdmin is sessionsUser for the two admin routes; a refusal is audited like every refused admin page.
func (c *reqCtx) sessionsAdmin(write bool) (*model.User, error) {
	u, err := c.sessionsUser(write)
	if err != nil {
		return nil, err
	}
	if _, g, _ := c.grant(); !g.Admin() {
		return nil, c.deny(u, 403, "everyone's sessions are for admins only (your own are on the Sessions page)", "sessions of all users denied")
	}
	return u, nil
}

// mySessions: GET /api/me/sessions, the caller's own sessions. The demo has no sign-in and no sessions.
func (s *Server) mySessions(c *reqCtx) error {
	u, err := c.sessionsUser(false)
	if err != nil {
		return err
	}
	if s.DemoUser != nil {
		c.json(200, s.page([]sessionView{}))
		return nil
	}
	c.json(200, s.page(s.views(s.sessions(u.Sub), c.session().SID, u.Sub, false)))
	return nil
}

// adminSessions: GET /api/admin/sessions, everyone's sessions, each with its account (admins).
func (s *Server) adminSessions(c *reqCtx) error {
	u, err := c.sessionsAdmin(false)
	if err != nil {
		return err
	}
	if s.DemoUser != nil {
		c.json(200, s.page([]sessionView{}))
		return nil
	}
	c.json(200, s.page(s.views(s.sessions(""), c.session().SID, u.Sub, true)))
	return nil
}

// revokeReq names the session to sign out by its handle (sessionView.ID).
type revokeReq struct {
	Session string `json:"session"`
}

// revokeMySession: POST /api/me/sessions/revoke {"session": handle}, one of the caller's own other sessions.
func (s *Server) revokeMySession(c *reqCtx) error {
	if _, err := c.requireSession(); err != nil { // before the body is read
		return err
	}
	var body revokeReq
	if err := c.decodeBody(&body); err != nil {
		return err
	}
	u, err := c.sessionsUser(true)
	if err != nil {
		return err
	}
	return s.revokeSession(c, u, body.Session, u.Sub)
}

// adminRevokeSession: POST /api/admin/sessions/revoke {"session": handle}, any session but the caller's own
// current one (admins).
func (s *Server) adminRevokeSession(c *reqCtx) error {
	if _, err := c.requireSession(); err != nil { // before the body is read
		return err
	}
	var body revokeReq
	if err := c.decodeBody(&body); err != nil {
		return err
	}
	u, err := c.sessionsAdmin(true)
	if err != nil {
		return err
	}
	return s.revokeSession(c, u, body.Session, "")
}

const sessionsNotWritable = "failed: sessions.json not writable; ended in memory only"

// revokeSession signs out the session handle names, looked up among those of sub ("" = of everyone). Before
// anything is asked: the session must exist and must not be the caller's own. Then "Confirm it's you", for
// this action alone.
func (s *Server) revokeSession(c *reqCtx, u *model.User, handle, sub string) error {
	if s.DemoUser != nil {
		return apierr.New(409, "The demo has no sign-in, so there is no session to sign out.")
	}
	t, ok := s.findSession(handle, sub)
	if !ok {
		return apierr.New(404, "That session is not there any more: it was signed out or has ended. Nothing was done.")
	}
	if t.SID == c.session().SID {
		return apierr.New(409, "That is the session you are using right now. To end it, use Sign out in the menu.")
	}
	if err := c.stepUpEach(u, "sign-outs of other sessions"); err != nil {
		return err
	}
	if !s.adminRL.allow(u.Sub) {
		return apierr.New(429, "too many changes; slow down")
	}
	what, target := sessionText(t), ownerLabel(t.Session)
	params := map[string]any{"sub": t.Sub, "own": t.Sub == u.Sub, "browser": browserOf(t.UA), "ip": t.lastAddr()}
	if err := s.dropSession(t.SID, "a session signed out on the Sessions page"); err != nil {
		c.audit(u, "sessions.revoke", target, params, sessionsNotWritable)
		return apierr.New(503, "That session is signed out, but the sign-out could not be recorded: the panel cannot write "+
			"sessions.json. It stays out until the panel restarts; an administrator should look at the server log.")
	}
	c.audit(u, "sessions.revoke", target, params, "ok")
	msg := "Signed out: " + what
	if t.Sub != u.Sub {
		msg = "Signed out " + target + ": " + what
	}
	c.json(200, map[string]any{"ok": true, "message": msg})
	return nil
}

// revokeMyOtherSessions: POST /api/me/sessions/revoke-others, every session of the caller's account except the
// one the request comes from. With no other session that could be used there is nothing to confirm and
// nothing is done.
func (s *Server) revokeMyOtherSessions(c *reqCtx) error {
	u, err := c.sessionsUser(true)
	if err != nil {
		return err
	}
	if s.DemoUser != nil {
		return apierr.New(409, "The demo has no sign-in, so there is no session to sign out.")
	}
	others := func() (usable int, sids []string) {
		current := c.session().SID
		for _, k := range s.sessions(u.Sub) {
			if k.SID != current {
				usable++
			}
		}
		// entries of this account that cannot be used any more go too (they are not on the page)
		all, _ := s.Poller.Admin.Sessions()
		for _, x := range all {
			if x.Sub == u.Sub && x.SID != current {
				sids = append(sids, x.SID)
			}
		}
		return usable, sids
	}
	n, _ := others()
	if n == 0 {
		c.json(200, map[string]any{"ok": true, "count": 0, "message": "You are signed in nowhere else."})
		return nil
	}
	if err := c.stepUpEach(u, "sign-outs of other sessions"); err != nil {
		return err
	}
	if !s.adminRL.allow(u.Sub) {
		return apierr.New(429, "too many changes; slow down")
	}
	n, sids := others() // the session of this request has a new id since the confirmation
	params := map[string]any{"count": n}
	if err := s.dropSessions(sids, "a session signed out on the Sessions page"); err != nil {
		c.audit(u, "sessions.revoke_others", "", params, sessionsNotWritable)
		return apierr.New(503, "Your other sessions are signed out, but that could not be recorded: the panel cannot write "+
			"sessions.json. They stay out until the panel restarts; an administrator should look at the server log.")
	}
	c.audit(u, "sessions.revoke_others", "", params, "ok")
	c.json(200, map[string]any{"ok": true, "count": n, "message": fmt.Sprintf("Signed out everywhere else (%d %s).", n, plural(n, "session"))})
	return nil
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// dropSessions ends several sessions with one write of sessions.json. When that write fails, each is ended
// like a single one (dropSession: in memory, with the log line), and the error is returned.
func (s *Server) dropSessions(sids []string, what string) error {
	if _, err := s.Poller.Admin.RemoveSessions(sids); err == nil {
		for _, sid := range sids {
			s.codec.live.forget(sid)
		}
		return nil
	}
	var first error
	for _, sid := range sids {
		if err := s.dropSession(sid, what); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// sessionText describes a session for the person who is about to end it, or just did: its browser and the
// address it was last used from ("Firefox on Windows, 203.0.113.7").
func sessionText(k knownSession) string {
	ip := k.lastAddr()
	if ip == "" {
		ip = k.SignedInIP()
	}
	if ip == "" {
		return browserOf(k.UA)
	}
	return browserOf(k.UA) + ", " + ip
}

// sessionDetail is what "Confirm it's you" and the step-up's audit entries add to the description of a kept
// sign-out (actionDetail): which session it is, in the panel's own words, so the confirmation is for that
// session and not for whatever the page called it. The handle itself is never shown or logged.
func (c *reqCtx) sessionDetail(u *model.User) string {
	var b revokeReq
	if json.Unmarshal(c.body, &b) != nil {
		return ""
	}
	sub := u.Sub
	if c.r.Pattern == "POST /api/admin/sessions/revoke" {
		sub = ""
	}
	t, ok := c.s.findSession(b.Session, sub)
	if !ok {
		return ""
	}
	if t.Sub != u.Sub {
		return "of " + truncate(ownerLabel(t.Session), 60) + ": " + sessionText(t)
	}
	return sessionText(t)
}

// browserOf makes "Firefox on Windows" out of a User-Agent header: a best effort for the usual browsers, for
// people to recognise their own devices by. It is only shown. What it cannot tell is "Unknown browser".
func browserOf(ua string) string {
	has := func(subs ...string) bool {
		for _, x := range subs {
			if strings.Contains(ua, x) {
				return true
			}
		}
		return false
	}
	browser := ""
	switch { // the order matters: Edge and Opera also say Chrome, Chrome also says Safari
	case has("Edg/", "EdgA/", "EdgiOS/", "Edge/"):
		browser = "Edge"
	case has("OPR/", "Opera"):
		browser = "Opera"
	case has("SamsungBrowser/"):
		browser = "Samsung Internet"
	case has("Firefox/", "FxiOS/"):
		browser = "Firefox"
	case has("HeadlessChrome/"):
		browser = "Chrome (headless)"
	case has("Chrome/", "CriOS/", "Chromium/"):
		browser = "Chrome"
	case has("Safari/") && has("Version/"):
		browser = "Safari"
	case strings.HasPrefix(ua, "curl/"):
		browser = "curl"
	case strings.HasPrefix(ua, "Go-http-client/"):
		browser = "Go program"
	case strings.HasPrefix(ua, "python-requests/"), strings.HasPrefix(ua, "Python-urllib/"):
		browser = "Python program"
	}
	system := ""
	switch { // Android also says Linux, an iPhone also says "like Mac OS X"
	case has("Windows NT", "Windows Phone"):
		system = "Windows"
	case has("Android"):
		system = "Android"
	case has("iPhone", "iPod"):
		system = "iPhone"
	case has("iPad"):
		system = "iPad"
	case has("Macintosh", "Mac OS X"):
		system = "macOS"
	case has("CrOS"):
		system = "ChromeOS"
	case has("Linux", "X11"):
		system = "Linux"
	}
	switch {
	case browser == "" && system == "":
		return "Unknown browser"
	case browser == "":
		return "Unknown browser on " + system
	case system == "":
		return browser
	}
	return browser + " on " + system
}
