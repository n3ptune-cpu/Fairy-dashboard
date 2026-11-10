package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

const cookieName = "fairy_session"

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
	// PendingID names the kept control action this step-up round trip was started for (see pending.go: the
	// action itself is on the server, never in the cookie). PendingResult is its outcome, shown once by /api/me.
	PendingID     string               `json:"pending_id,omitempty"`
	PendingResult *model.PendingResult `json:"pending_result,omitempty"`
}

type sessionCodec struct {
	key    []byte
	maxAge int
	secure bool
	live   *liveSessions // per-session state that is not in the cookie (sessionstate.go)
	// now is the clock of everything that decides how old a session, a sign-in or a kept action is (the cookie,
	// sessionUser, authAge, the login round trip, the pending action). Tests replace it; see Server.now.
	now func() time.Time
}

func newCodec(secret string, maxAge int, secure bool) *sessionCodec {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("fairy-session-cookie-v1"))
	return &sessionCodec{key: m.Sum(nil), maxAge: maxAge, secure: secure, live: newLiveSessions(), now: time.Now}
}

// now is the server's session clock (time.Now, except in tests). It lives on the codec, which every session
// decision already goes through.
func (s *Server) now() time.Time { return s.codec.now() }

// nowSec is now as Unix seconds with a fraction, the unit of auth_time.
func (s *Server) nowSec() float64 { return float64(s.now().UnixNano()) / 1e9 }

func (c *sessionCodec) sign(payload string) string {
	m := hmac.New(sha256.New, c.key)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// decode returns the session (empty when missing, tampered or older than maxAge).
func (c *sessionCodec) decode(r *http.Request) *sessionData {
	s := &sessionData{}
	ck, err := r.Cookie(cookieName)
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
	if json.Unmarshal(raw, &out) != nil || c.now().Sub(time.Unix(out.T, 0)) > time.Duration(c.maxAge)*time.Second {
		return s
	}
	return &out.D
}

func (c *sessionCodec) write(w http.ResponseWriter, s *sessionData) {
	if s == nil || s.empty() {
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
			Secure: c.secure, SameSite: http.SameSiteLaxMode})
		return
	}
	raw, _ := json.Marshal(struct {
		D *sessionData `json:"d"`
		T int64        `json:"t"`
	}{s, c.now().Unix()})
	payload := base64.RawURLEncoding.EncodeToString(raw)
	// SameSite=Lax (not Strict): the OIDC callback is a top-level navigation from Pocket ID and must carry the
	// cookie with state/nonce/PKCE verifier. Writes are protected by the CSRF header + Origin checks.
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: payload + "." + c.sign(payload), Path: "/",
		MaxAge: c.maxAge, HttpOnly: true, Secure: c.secure, SameSite: http.SameSiteLaxMode})
}

func (s *sessionData) empty() bool {
	return s.SID == "" && s.User == nil && s.IAT == 0 && s.Seen == 0 && s.Next == "" && !s.StepUp &&
		s.State == "" && s.Nonce == "" && s.Verifier == "" && s.Started == 0 && s.PendingID == "" && s.PendingResult == nil
}
