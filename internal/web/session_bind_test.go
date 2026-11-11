package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// forgeSubject does what someone can do who knows FAIRY_SESSION_SECRET and has a session of their own: it
// rewrites the user inside their own cookie to another account's subject and signs the result. The session id,
// and with it the entry in sessions.json and the step-up window, stays the forger's own.
func forgeSubject(secret, ownCookie, victimSub string) string {
	name, val, _ := strings.Cut(ownCookie, "=")
	payload, _, _ := strings.Cut(val, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	var c struct {
		D map[string]any `json:"d"`
		T int64          `json:"t"`
	}
	_ = json.Unmarshal(raw, &c)
	c.D["user"] = map[string]any{"sub": victimSub, "name": "forged", "groups": []string{}}
	c.D["iat"], c.D["seen"], c.T = time.Now().Unix(), time.Now().Unix(), time.Now().Unix()
	out, _ := json.Marshal(c)
	p := base64.RawURLEncoding.EncodeToString(out)
	k := hmac.New(sha256.New, []byte(secret))
	k.Write([]byte(cookieKeyLabel))
	m := hmac.New(sha256.New, k.Sum(nil))
	m.Write([]byte(p))
	return name + "=" + p + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// T-22 (with T-05): a session id belongs to the account it was issued to. A cookie that carries a registered
// session id with another account's subject, correctly signed because the session secret leaked, is no
// session: not for reading, not for the step-up window of that session id, not for its kept action. The
// accounts' own cookies keep working.
func TestSessionIDIsTiedToItsAccount(t *testing.T) {
	e := setup(t)
	secret := strings.Repeat("s", 48) // the test panel's secret (setup), known to the forger
	admin := e.cookie(controller(), 0)
	own := e.cookie(viewer, 0) // the forger's own session, signed in a moment ago: its step-up window is open
	csrf := []string{"X-Requested-With", "fairy"}
	with := func(ck string, more ...string) []string { return append([]string{"Cookie", ck}, more...) }

	if r := e.do("GET", "/api/me", nil, with(own)...); r.code != 200 || r.body["sub"] != "u1" || r.body["role"] == "admin" {
		t.Fatalf("the viewer's own cookie: %d %s", r.code, r.raw)
	}
	forged := forgeSubject(secret, own, "u2")
	if r := e.do("GET", "/api/me", nil, with(forged)...); r.code != 401 {
		t.Fatalf("a cookie with the viewer's session id and the admin's subject is a session: %d %s", r.code, r.raw)
	}
	if r := e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 5}, with(forged, csrf...)...); r.code != 401 {
		t.Fatalf("control action with the forged cookie: %d %s", r.code, r.raw)
	}
	if r := e.do("PUT", "/api/admin/users/u1", map[string]any{"role": "admin"}, with(forged, csrf...)...); r.code != 401 {
		t.Fatalf("role change with the forged cookie: %d %s", r.code, r.raw)
	}
	if r := e.do("POST", "/api/admin/panel-update", nil, with(forged, csrf...)...); r.code != 401 {
		t.Fatalf("panel update with the forged cookie: %d %s", r.code, r.raw)
	}
	for _, p := range []string{"/api/admin/users", "/api/audit", "/api/systems", "/api/stream"} {
		if r := e.do("GET", p, nil, with(forged)...); r.code != 401 {
			t.Fatalf("GET %s with the forged cookie: %d", p, r.code)
		}
	}
	if r := e.do("DELETE", "/api/me/pending", nil, with(forged, csrf...)...); r.code != 401 {
		t.Fatalf("cancelling a kept action with the forged cookie: %d", r.code)
	}
	if role := e.s.Access.Load().Users["u1"].Role; role == "admin" {
		t.Fatal("the viewer made themself admin")
	}
	// the other way round: the admin's session id under the viewer's subject is no session either
	if r := e.do("GET", "/api/me", nil, with(forgeSubject(secret, admin, "u1"))...); r.code != 401 {
		t.Fatalf("the admin's session id with the viewer's subject: %d", r.code)
	}
	// the forged requests ended nobody's session
	if r := e.do("GET", "/api/me", nil, with(own)...); r.code != 200 || r.body["role"] == "admin" || r.body["sub"] != "u1" {
		t.Fatalf("the viewer's own cookie afterwards: %d %s", r.code, r.raw)
	}
	if r := e.do("GET", "/api/me", nil, with(admin)...); r.code != 200 || r.body["role"] != "admin" {
		t.Fatalf("the admin's own cookie afterwards: %d %s", r.code, r.raw)
	}
	if r := e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 5}, with(admin, csrf...)...); r.code != 200 {
		t.Fatalf("the admin's own control action: %d %s", r.code, r.raw)
	}
}
