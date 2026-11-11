package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
)

// direct sends a request to the server as if it came from the TCP peer remote ("198.51.100.1:5555"), without
// a real connection, so the test chooses the peer and may send header values a real client cannot.
func (e *env) direct(method, path, remote string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = remote
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header[http.CanonicalHeaderKey(hdr[i])] = []string{hdr[i+1]}
	}
	rec := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(rec, req)
	return rec
}

// signInFrom is a whole sign-in at the fake identity provider as sub, whose callback comes from the TCP peer
// remote with hdr. It returns the session cookie.
func signInFrom(e *env, idp *fakeIDP, sub, remote string, hdr ...string) string {
	e.t.Helper()
	e.s.Cfg.OIDC.Issuer = idp.srv.URL
	idp.claims = map[string]any{"sub": sub, "groups": []string{"viewers", "controllers"}, "auth_time": time.Now().Unix()}
	q, rt := idp.login(e, "/auth/login?next=/", "")
	rec := e.direct("GET", "/auth/callback?code=c&state="+q.Get("state"), remote, append([]string{"Cookie", rt}, hdr...)...)
	if rec.Code != 303 {
		e.t.Fatalf("sign-in of %s: %d %s", sub, rec.Code, rec.Body)
	}
	return sessionCookie(e, rec.Header())
}

func sessionsOf(e *env, sub string) []adminstate.Session {
	e.t.Helper()
	var out []adminstate.Session
	for _, s := range must(e.s.Poller.Admin.Sessions()) {
		if s.Sub == sub {
			out = append(out, s)
		}
	}
	return out
}

// What a sign-in records for the Sessions page: the browser's User-Agent and the client address. The address
// is the one every other part of the panel uses (security.ClientIP): X-Forwarded-For counts when the peer is a
// trusted proxy (here: loopback, the default), and is never believed from any other peer.
func TestSignInRecordsBrowserAndAddress(t *testing.T) {
	e := setup(t)
	idp := newFakeIDP(t)
	// through the trusted proxy: the forwarded address
	ck := signInFrom(e, idp, "u2", "127.0.0.1:40000", "X-Forwarded-For", "203.0.113.9", "User-Agent", "Fakefox/9.9 (TestOS)")
	s := sessionsOf(e, "u2")
	if len(s) != 1 || s[0].IP != "203.0.113.9" || s[0].LoginIP != "203.0.113.9" || s[0].SeenIP != "203.0.113.9" ||
		s[0].UA != "Fakefox/9.9 (TestOS)" || s[0].LoginTS == 0 || s[0].SeenTS != s[0].LoginTS {
		t.Fatalf("a sign-in through the proxy: %+v", s)
	}
	// from a peer that is not a trusted proxy: its own address, whatever it claims in headers
	signInFrom(e, idp, "u3", "198.51.100.1:5555", "X-Forwarded-For", "192.0.2.66", "X-Real-IP", "192.0.2.66",
		"Forwarded", "for=192.0.2.66", "CF-Connecting-IP", "192.0.2.66", "User-Agent", "Fakefox/9.9 (TestOS)")
	if s := sessionsOf(e, "u3"); len(s) != 1 || s[0].IP != "198.51.100.1" || s[0].LoginIP != "198.51.100.1" || s[0].SeenIP != "198.51.100.1" {
		t.Fatalf("a sign-in from an untrusted peer: %+v", s)
	}
	// and through the proxy, a hop the client put in front is not chosen either
	signInFrom(e, idp, "u4", "127.0.0.1:40000", "X-Forwarded-For", "192.0.2.66, 203.0.113.10")
	if s := sessionsOf(e, "u4"); len(s) != 1 || s[0].IP != "203.0.113.10" {
		t.Fatalf("a client-sent hop: %+v", s)
	}
	raw, _ := os.ReadFile(e.s.Poller.Admin.SessionsPath)
	if strings.Contains(string(raw), "192.0.2.66") {
		t.Fatal("sessions.json holds an address an untrusted peer claimed")
	}

	// "Confirm it's you" from another address and browser: the session gets a new id and entry, which has the
	// confirming client's browser and address and keeps when and from where the person signed in
	first := s[0]
	e.s.Cfg.OIDC.Issuer = idp.srv.URL
	idp.claims = map[string]any{"sub": "u2", "groups": []string{"viewers", "controllers"}, "auth_time": time.Now().Unix()}
	q, rt := idp.login(e, "/auth/login?step_up=1&next=/", ck)
	rec := e.direct("GET", "/auth/callback?code=c&state="+q.Get("state"), "127.0.0.1:40000", "Cookie", rt,
		"X-Forwarded-For", "198.51.100.77", "User-Agent", "Otherfox/1.0")
	if rec.Code != 303 {
		t.Fatal("step-up", rec.Code, rec.Body)
	}
	s = sessionsOf(e, "u2")
	if len(s) != 1 || s[0].SID == first.SID || s[0].IP != "198.51.100.77" || s[0].UA != "Otherfox/1.0" ||
		s[0].LoginIP != "203.0.113.9" || s[0].LoginTS != first.LoginTS || s[0].SeenIP != "198.51.100.77" {
		t.Fatalf("after a confirmation: %+v (before: %+v)", s, first)
	}
}

// The User-Agent is the client's text. A hostile one signs in like any other, and what is kept of it is
// bounded and has no control characters; a session without the header has an empty one.
func TestHostileUserAgentIsCleanedAndCut(t *testing.T) {
	e := setup(t)
	idp := newFakeIDP(t)
	hostile := "<img src=x onerror=alert(1)>\x00\x1b[31m\r\nSet-Cookie: x=1\u202e\xff\xfe" + strings.Repeat("A", 9000)
	signInFrom(e, idp, "u2", "127.0.0.1:40000", "User-Agent", hostile)
	s := sessionsOf(e, "u2")
	if len(s) != 1 {
		t.Fatalf("%+v", s)
	}
	ua := s[0].UA
	if len(ua) != adminstate.MaxUA || !strings.HasPrefix(ua, "<img src=x onerror=alert(1)>[31mSet-Cookie: x=1\ufffdAAAA") ||
		strings.ContainsAny(ua, "\x00\x1b\r\n\u202e") || !utf8.ValidString(ua) {
		t.Fatalf("kept: %d bytes %q", len(ua), ua)
	}
	signInFrom(e, idp, "u3", "127.0.0.1:40000")
	if s := sessionsOf(e, "u3"); len(s) != 1 || s[0].UA != "" {
		t.Fatalf("no User-Agent: %+v", s)
	}
}

// A session's last use goes to sessions.json with the address it came from, at most once a minute, and only
// for real use: a read the page did not mark and a refused request write nothing.
func TestLastUseIsSavedOnceAMinute(t *testing.T) {
	e := setup(t)
	clk := e.fakeClock()
	ck := e.cookie(controller(), 0)
	sid := sidOf(e, ck)
	read := func() adminstate.Session {
		for _, s := range must(e.s.Poller.Admin.Sessions()) {
			if s.SID == sid {
				return s
			}
		}
		t.Fatal("the session is gone")
		return adminstate.Session{}
	}
	active := []string{"Cookie", ck, "X-Fairy-Active", "1"}
	at0 := read().SeenTS
	clk.advance(10 * time.Second)
	if r := e.do("GET", "/api/me", nil, "Cookie", ck); r.code != 200 || read().SeenTS != at0 {
		t.Fatal("a read the page did not mark is not use", r.code)
	}
	if r := e.do("POST", "/api/me/active", nil, "Cookie", ck); r.code != 403 || read().SeenTS != at0 {
		t.Fatal("a refused write is not use", r.code)
	}
	if r := e.do("GET", "/api/me", nil, append(active, "X-Forwarded-For", "203.0.113.9")...); r.code != 200 {
		t.Fatal(r.code)
	}
	one := read()
	if one.SeenTS != float64(clk.now().Unix()) || one.SeenIP != "203.0.113.9" || one.IP != "" || one.LoginIP != "" {
		t.Fatalf("the first use: %+v (now %d)", one, clk.now().Unix())
	}
	clk.advance(30 * time.Second) // inside the minute: kept in memory, not written
	if r := e.do("GET", "/api/me", nil, append(active, "X-Forwarded-For", "198.51.100.2")...); r.code != 200 || read() != one {
		t.Fatalf("a second use inside a minute was written: %+v", read())
	}
	clk.advance(31 * time.Second)
	if r := e.do("POST", "/api/me/active", nil, append(active, "X-Requested-With", "fairy", "X-Forwarded-For", "198.51.100.2")...); r.code != 204 {
		t.Fatal(r.code)
	}
	if two := read(); two.SeenTS != float64(clk.now().Unix()) || two.SeenIP != "198.51.100.2" {
		t.Fatalf("a use a minute later: %+v", two)
	}
	// the panel's memory has the last use to the second, and the addresses the session was used from
	seen, addrs := e.s.codec.live.usage(sid)
	if seen != clk.now().Unix() || len(addrs) != 3 || addrs[0].ip != "198.51.100.2" || addrs[1].ip != "203.0.113.9" || addrs[2].ip != "127.0.0.1" {
		t.Fatalf("in memory: %d %+v", seen, addrs)
	}
	// an address list has an end: the latest one and three before it
	for _, ip := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.1", "192.0.2.4", "192.0.2.5"} {
		e.do("GET", "/api/me", nil, "Cookie", ck, "X-Forwarded-For", ip)
	}
	if _, addrs = e.s.codec.live.usage(sid); len(addrs) != maxAddrs || addrs[0].ip != "192.0.2.5" || addrs[1].ip != "192.0.2.4" ||
		addrs[2].ip != "192.0.2.1" || addrs[3].ip != "192.0.2.3" {
		t.Fatalf("after many addresses: %+v", addrs)
	}
	// a session that ended leaves nothing behind
	if r := e.do("POST", "/auth/logout", nil, "Cookie", ck, "X-Requested-With", "fairy"); r.code != 303 {
		t.Fatal(r.code)
	}
	if seen, addrs := e.s.codec.live.usage(sid); seen != 0 || addrs != nil {
		t.Fatal("a signed-out session is still remembered")
	}
}
