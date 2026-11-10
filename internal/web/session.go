package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// The session cookie's name. With the __Host- prefix a browser only accepts (and only sends) the cookie when it
// was set by this very host over HTTPS with Path=/ and no Domain, so a site on a sibling host name cannot plant
// one for the panel. Browsers refuse a __Host- cookie that is not Secure, so the plain name remains for the
// FAIRY_INSECURE_COOKIES test mode (local http) only. A cookie under the plain name is never read in normal
// mode, and a cookie value from before this change is not valid under any name (cookieKeyLabel): sessions from
// before this change sign in once more.
const (
	hostCookieName  = "__Host-fairy_session"
	plainCookieName = "fairy_session"
)

// name is the cookie name in use.
func (c *sessionCodec) name() string {
	if c.secure {
		return hostCookieName
	}
	return plainCookieName
}

// expireOld tells the browser to drop a cookie left under the plain name by an older version (normal mode only;
// in the test mode the plain name is the one in use).
func (c *sessionCodec) expireOld(w http.ResponseWriter) {
	if c.secure {
		http.SetCookie(w, &http.Cookie{Name: plainCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
			Secure: true, SameSite: http.SameSiteLaxMode})
	}
}

// sessionData is stored client-side in a signed (HMAC-SHA256), HttpOnly cookie. It holds no secrets: the
// server-side registry (sessions.json) decides whether a sid is still valid, so sessions are revocable.
//
// When the user last authenticated at Pocket ID (the step-up window) is deliberately NOT here: it is kept on the
// server per session id and bound to the client that confirmed it (sessionstate.go), so a copy of the cookie
// does not inherit the window. An "auth_time" in a cookie from an older version is ignored.
type sessionData struct {
	SID  string      `json:"sid,omitempty"`
	User *model.User `json:"user,omitempty"`
	IAT  int64       `json:"iat,omitempty"`
	Seen int64       `json:"seen,omitempty"`
	// login round trip
	Next     string `json:"next,omitempty"`
	StepUp   bool   `json:"step_up,omitempty"`
	State    string `json:"state,omitempty"`
	Nonce    string `json:"nonce,omitempty"`
	Verifier string `json:"verifier,omitempty"`
	Started  int64  `json:"started,omitempty"`
	// Client marks the client this session was established for: the address and browser its sign-in, or its
	// last "Confirm it's you", came back to (clientMark: a keyed hash, so the cookie does not show them). Only
	// that client's refused control request is kept for a step-up (pending.go). The mark is in the cookie so
	// that it survives a restart; the cookie is signed, and a copy of it used from another address or browser
	// carries the mark of the client it was copied from, which that client does not match.
	Client string `json:"client,omitempty"`
	// PendingResult is the outcome of the kept control action the panel ran after a step-up, shown once by
	// /api/me. The action itself, and which round trip it is armed for, are on the server only (pending.go).
	PendingResult *model.PendingResult `json:"pending_result,omitempty"`

	// expiredSID is set by decode instead of everything else when the cookie is genuine but older than maxAge:
	// the session id it carried, so the session can be removed from sessions.json. Never written.
	expiredSID string
}

// cookieKeyLabel derives the cookie's signing key from the session secret. It changed with the cookie's name
// (NEW-6): the name alone did not end the old sessions, because a cookie value of the old release, sent under
// the new name, still carried a valid signature and its session was still in sessions.json. With another key
// no cookie made before the upgrade verifies, whatever name it is sent under; the session registry refuses
// those sessions as well (adminstate.SessionGen). Everyone signs in once, as the guide says.
const cookieKeyLabel = "fairy-session-cookie-v2"

type sessionCodec struct {
	key    []byte
	maxAge int
	secure bool
	live   *liveSessions // per-session state that is not in the cookie (sessionstate.go)
	// now is the clock of everything that decides how old a session, a sign-in or a kept action is (the cookie,
	// sessionUser, authAge, the login round trip, the pending action). Tests replace it; see Server.now.
	now func() time.Time
}

func newCodec(secret string, maxAge config.Seconds, secure bool) *sessionCodec {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(cookieKeyLabel))
	return &sessionCodec{key: m.Sum(nil), maxAge: int(maxAge), secure: secure, live: newLiveSessions(), now: time.Now}
}

// now is the server's session clock (time.Now, except in tests). It lives on the codec, which every session
// decision already goes through.
func (s *Server) now() time.Time { return s.codec.now() }

// nowSec is now as Unix seconds with a fraction, the unit of auth_time.
func (s *Server) nowSec() float64 { return float64(s.now().UnixNano()) / 1e9 }

// clientMark is the session cookie's mark for a client (sessionData.Client): a keyed hash of its address and
// browser, so the cookie tells neither.
func (c *sessionCodec) clientMark(cl client) string {
	m := hmac.New(sha256.New, c.key)
	m.Write([]byte("client\x00" + cl.ip + "\x00"))
	m.Write(cl.ua[:])
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:16])
}

// established reports whether this request comes from the client its session was established for (see
// sessionData.Client). Demo mode has one simulated user and no sessions.
func (c *reqCtx) established() bool {
	if c.s.DemoUser != nil {
		return true
	}
	m := c.session().Client
	return m != "" && hmac.Equal([]byte(m), []byte(c.s.codec.clientMark(c.client())))
}

func (c *sessionCodec) sign(payload string) string {
	m := hmac.New(sha256.New, c.key)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// decode returns the session (empty when missing, tampered or older than maxAge; for a genuine cookie that is
// too old only expiredSID is set).
func (c *sessionCodec) decode(r *http.Request) *sessionData {
	s := &sessionData{}
	ck, err := r.Cookie(c.name())
	if err != nil || len(ck.Value) > 8192 {
		return s
	}
	payload, sig, ok := strings.Cut(ck.Value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(c.sign(payload))) {
		return s
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return s
	}
	var out struct {
		D sessionData `json:"d"`
		T int64       `json:"t"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return s
	}
	if c.now().Sub(time.Unix(out.T, 0)) > time.Duration(c.maxAge)*time.Second {
		s.expiredSID = out.D.SID
		return s
	}
	return &out.D
}

func (c *sessionCodec) write(w http.ResponseWriter, s *sessionData) {
	if s == nil || s.empty() {
		http.SetCookie(w, &http.Cookie{Name: c.name(), Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
			Secure: c.secure, SameSite: http.SameSiteLaxMode})
		c.expireOld(w)
		return
	}
	raw, _ := json.Marshal(struct {
		D *sessionData `json:"d"`
		T int64        `json:"t"`
	}{s, c.now().Unix()})
	payload := base64.RawURLEncoding.EncodeToString(raw)
	// SameSite=Lax (not Strict): the OIDC callback is a top-level navigation from Pocket ID and must carry the
	// cookie with state/nonce/PKCE verifier. Writes are protected by the CSRF header + Origin checks.
	// No Domain attribute, Path=/ and Secure: what the __Host- prefix requires.
	http.SetCookie(w, &http.Cookie{Name: c.name(), Value: payload + "." + c.sign(payload), Path: "/",
		MaxAge: c.maxAge, HttpOnly: true, Secure: c.secure, SameSite: http.SameSiteLaxMode})
	c.expireOld(w)
}

func (s *sessionData) empty() bool {
	return s.SID == "" && s.User == nil && s.IAT == 0 && s.Seen == 0 && s.Next == "" && !s.StepUp &&
		s.State == "" && s.Nonce == "" && s.Verifier == "" && s.Started == 0 && s.PendingResult == nil
}
