package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/nsxservices/fairy/internal/model"
)

const cookieName = "fairy_session"

// sessionData is stored client-side in a signed (HMAC-SHA256), HttpOnly cookie. It holds no secrets: the
// server-side registry (sessions.json) decides whether a sid is still valid, so sessions are revocable.
type sessionData struct {
	SID      string      `json:"sid,omitempty"`
	User     *model.User `json:"user,omitempty"`
	IAT      int64       `json:"iat,omitempty"`
	Seen     int64       `json:"seen,omitempty"`
	AuthTime float64     `json:"auth_time,omitempty"`
	// login round trip
	Next     string `json:"next,omitempty"`
	StepUp   bool   `json:"step_up,omitempty"`
	State    string `json:"state,omitempty"`
	Nonce    string `json:"nonce,omitempty"`
	Verifier string `json:"verifier,omitempty"`
	Started  int64  `json:"started,omitempty"`
}

type sessionCodec struct {
	key    []byte
	maxAge int
	secure bool
}

func newCodec(secret string, maxAge int, secure bool) *sessionCodec {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("fairy-session-cookie-v1"))
	return &sessionCodec{key: m.Sum(nil), maxAge: maxAge, secure: secure}
}

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
	if json.Unmarshal(raw, &out) != nil || time.Since(time.Unix(out.T, 0)) > time.Duration(c.maxAge)*time.Second {
		return s
	}
	return &out.D
}

func (c *sessionCodec) write(w http.ResponseWriter, s *sessionData) {
	if s == nil || (*s == sessionData{}) {
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
			Secure: c.secure, SameSite: http.SameSiteLaxMode})
		return
	}
	raw, _ := json.Marshal(struct {
		D *sessionData `json:"d"`
		T int64        `json:"t"`
	}{s, time.Now().Unix()})
	payload := base64.RawURLEncoding.EncodeToString(raw)
	// SameSite=Lax (not Strict): the OIDC callback is a top-level navigation from Pocket ID and must carry the
	// cookie with state/nonce/PKCE verifier. Writes are protected by the CSRF header + Origin checks.
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: payload + "." + c.sign(payload), Path: "/",
		MaxAge: c.maxAge, HttpOnly: true, Secure: c.secure, SameSite: http.SameSiteLaxMode})
}
