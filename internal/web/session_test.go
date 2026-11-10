package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// testClock moves the server's session clock (Server.now) without waiting: now = real time + skew.
type testClock struct{ skew atomic.Int64 }

func (c *testClock) advance(d time.Duration) { c.skew.Add(int64(d)) }
func (c *testClock) now() time.Time          { return time.Now().Add(time.Duration(c.skew.Load())) }

// fakeClock replaces the session clock of e's server. Call it before the first request.
func (e *env) fakeClock() *testClock {
	c := &testClock{}
	e.s.codec.now = c.now
	return c
}

// A session that is not used for session_idle_timeout is refused (an unmarked read is not use: see
// TestBackgroundTrafficDoesNotExtendTheSession); one that is older than session_max_age is refused however much
// it was used.
func TestSessionIdleAndMaxAgeExpiry(t *testing.T) {
	e := setup(t)
	clk := e.fakeClock()
	idle := time.Duration(e.s.Cfg.SessionIdleTimeout) * time.Second
	ck := e.cookie(viewer, 0)
	me := func(cookie string) int { return e.do("GET", "/api/me", nil, "Cookie", cookie).code }
	if c := me(ck); c != 200 {
		t.Fatal("fresh session", c)
	}
	clk.advance(idle - time.Minute)
	if c := me(ck); c != 200 {
		t.Fatal("inside the idle timeout", c)
	}
	clk.advance(2 * time.Minute)
	if c := me(ck); c != 401 {
		t.Fatal("idle for longer than session_idle_timeout must be refused", c)
	}

	// max age: idle timeout out of the way
	e = setup(t)
	clk = e.fakeClock()
	e.s.Cfg.SessionIdleTimeout = e.s.Cfg.SessionMaxAge
	maxAge := time.Duration(e.s.Cfg.SessionMaxAge) * time.Second
	ck = e.cookie(viewer, 0)
	clk.advance(maxAge - time.Minute)
	if c := me(ck); c != 200 {
		t.Fatal("inside session_max_age", c)
	}
	clk.advance(2 * time.Minute)
	if c := me(ck); c != 401 {
		t.Fatal("older than session_max_age must be refused", c)
	}
}

// openStream opens /api/stream with the given headers and returns its status and a channel that is closed when
// the server ends the stream.
func (e *env) openStream(hdr ...string) (int, <-chan struct{}) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	e.t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/api/stream", nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer r.Body.Close()
		_, _ = io.Copy(io.Discard, r.Body)
	}()
	return r.StatusCode, done
}

func ended(done <-chan struct{}, within time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(within):
		return false
	}
}

// S-a: the idle timeout measures real use. The page's background traffic (the once-a-minute updates poll, other
// unmarked reads, an open live stream) does not keep a session alive, however often it comes.
func TestBackgroundTrafficDoesNotExtendTheSession(t *testing.T) {
	e := setup(t)
	clk := e.fakeClock()
	idle := time.Duration(e.s.Cfg.SessionIdleTimeout) * time.Second
	ck := e.cookie(controller(), 0)
	code, done := e.openStream("Cookie", ck)
	if code != 200 {
		t.Fatal("stream", code)
	}
	for i := 0; i < 5; i++ { // an unattended tab: polls for 10/12 of the timeout
		clk.advance(idle / 6)
		for _, p := range []string{"/api/admin/updates", "/api/me", "/api/systems"} {
			r := e.do("GET", p, nil, "Cookie", ck)
			if r.code != 200 {
				t.Fatalf("poll %d %s: %d", i, p, r.code)
			}
			if len(r.hdr.Values("Set-Cookie")) != 0 {
				t.Fatalf("a background read must not refresh the cookie (%s)", p)
			}
		}
	}
	if ended(done, 1500*time.Millisecond) {
		t.Fatal("the stream ended before the idle timeout")
	}
	clk.advance(idle/6 + time.Minute) // nobody touched the page for the whole timeout
	if !ended(done, 5*time.Second) {
		t.Fatal("the live stream must end when the session idles out")
	}
	if n := len(must(e.s.Poller.Admin.ListSessions())); n != 0 {
		t.Fatal("the idle session must leave sessions.json, have", n)
	}
	r := e.do("GET", "/api/admin/updates", nil, "Cookie", ck)
	if r.code != 401 || !strings.Contains(strings.Join(r.hdr.Values("Set-Cookie"), ";"), "Max-Age=0") {
		t.Fatal("after the idle timeout the poll is refused and the cookie cleared", r.code, r.hdr.Values("Set-Cookie"))
	}
	if code, _ := e.openStream("Cookie", ck); code != 401 {
		t.Fatal("the stream's reconnect must be answered 401", code)
	}
}

// S-b: what the UI's sign-out handling relies on. A session that ended gets 401 with a JSON body from the
// stream and from every read (never a redirect, never an event stream), and that 401 is not the step-up kind,
// so the page shows the sign-in view instead of "Confirm it's you" or another retry.
func TestEndedSessionGets401JSONEverywhere(t *testing.T) {
	e := setup(t)
	ck := e.cookie(controller(), 0)
	if _, err := e.s.Poller.Admin.RevokeAllSessions(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	for _, p := range []string{"/api/stream", "/api/stream?watch=laptop", "/api/me", "/api/admin/updates", "/api/systems"} {
		r := e.do("GET", p, nil, "Cookie", ck, "Accept", "text/event-stream")
		d, _ := r.body["detail"].(string)
		if r.code != 401 || !strings.HasPrefix(r.hdr.Get("Content-Type"), "application/json") || d != "not authenticated" {
			t.Errorf("%s: %d %s %s", p, r.code, r.hdr.Get("Content-Type"), r.raw)
		}
	}
	if r := e.poll(ck); r.code != 401 || stepUpRequired(r) {
		t.Fatal("a write of an ended session is a plain 401, not a step-up", r.code, string(r.raw))
	}
}

// S-a: real use keeps the session: a request the UI marks as caused by input, the explicit activity signal,
// and any write. API tokens have no idle timeout.
func TestActivityExtendsTheSession(t *testing.T) {
	for name, use := range map[string]func(e *env, ck string) resp{
		"marked read": func(e *env, ck string) resp { return e.do("GET", "/api/me", nil, "Cookie", ck, "X-Fairy-Active", "1") },
		"activity signal": func(e *env, ck string) resp {
			return e.do("POST", "/api/me/active", nil, "Cookie", ck, "X-Requested-With", "fairy")
		},
		"write": func(e *env, ck string) resp { return e.poll(ck) },
	} {
		e := setup(t)
		clk := e.fakeClock()
		idle := time.Duration(e.s.Cfg.SessionIdleTimeout) * time.Second
		ck := e.cookie(controller(), 0)
		for i := 0; i < 4; i++ { // twice the timeout in all, used every half timeout
			clk.advance(idle / 2)
			r := use(e, ck)
			if r.code != 200 && r.code != 204 && !stepUpRequired(r) {
				t.Fatalf("%s, round %d: %d %s", name, i, r.code, r.raw)
			}
			if i == 0 && !strings.Contains(strings.Join(r.hdr.Values("Set-Cookie"), ";"), "=") {
				t.Fatalf("%s: the cookie's copy of last-used must be refreshed", name)
			}
		}
		clk.advance(idle - time.Minute)
		if r := e.do("GET", "/api/me", nil, "Cookie", ck); r.code != 200 {
			t.Fatalf("%s: session lost although it was used: %d", name, r.code)
		}
		clk.advance(2 * time.Minute)
		if r := e.do("GET", "/api/me", nil, "Cookie", ck); r.code != 401 {
			t.Fatalf("%s: the timeout counts from the last real use: %d", name, r.code)
		}
	}

	e := setup(t)
	clk := e.fakeClock()
	ck := e.cookie(viewer, 0)
	if r := e.do("POST", "/api/me/active", nil, "Cookie", ck); r.code != 403 {
		t.Fatal("the activity signal needs the CSRF header", r.code)
	}
	if r := e.do("POST", "/api/me/active", nil); r.code != 401 {
		t.Fatal("the activity signal needs a sign-in", r.code)
	}
	raw, _, _ := e.s.Tokens.Create("ha", "system", "read", "cli:test", 0)
	clk.advance(3 * time.Duration(e.s.Cfg.SessionIdleTimeout) * time.Second)
	if r := e.do("GET", "/api/systems", nil, "Authorization", "Bearer "+raw); r.code != 200 {
		t.Fatal("API tokens are not affected by the idle timeout", r.code)
	}
}

// S-a: "last used" lives on the server. After a restart it is seeded from the cookie's own copy, so a session
// that was idle before the restart is still idle after it.
func TestIdleTimeoutSurvivesRestart(t *testing.T) {
	e := setup(t)
	clk := e.fakeClock()
	idle := time.Duration(e.s.Cfg.SessionIdleTimeout) * time.Second
	restart := func() *env {
		s2 := New(e.s.Cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, e.s.Poller, nil, "test")
		s2.codec.now = clk.now
		srv := httptest.NewServer(s2.Handler())
		t.Cleanup(srv.Close)
		return &env{t: t, s: s2, srv: srv, b: e.b}
	}
	used, unused := e.cookie(viewer, 0), e.cookie(controller(), 0)
	clk.advance(idle - time.Hour)
	r := e.do("GET", "/api/me", nil, "Cookie", used, "X-Fairy-Active", "1")
	if r.code != 200 {
		t.Fatal(r.code)
	}
	used = sessionCookie(e, r.hdr) // the browser keeps the refreshed cookie
	clk.advance(2 * time.Hour)
	e2 := restart()
	if r := e2.do("GET", "/api/me", nil, "Cookie", used); r.code != 200 {
		t.Fatal("used an hour before the restart: still signed in", r.code)
	}
	if r := e2.do("GET", "/api/me", nil, "Cookie", unused); r.code != 401 {
		t.Fatal("idle since before the restart: signed out", r.code)
	}
}

// S-d: a session that reaches session_max_age ends like an idle one: 401, the cookie is cleared and the session
// leaves sessions.json. Both for a cookie that was never refreshed and for one refreshed a moment ago.
func TestMaxAgeEndsTheSessionCleanly(t *testing.T) {
	cleared := func(r resp) bool {
		return r.code == 401 && strings.Contains(strings.Join(r.hdr.Values("Set-Cookie"), ";"), "Max-Age=0")
	}
	// never refreshed: the cookie itself is as old as the session
	e := setup(t)
	clk := e.fakeClock()
	e.s.Cfg.SessionIdleTimeout = e.s.Cfg.SessionMaxAge
	maxAge := time.Duration(e.s.Cfg.SessionMaxAge) * time.Second
	ck := e.cookie(viewer, 0)
	clk.advance(maxAge + time.Minute)
	if r := e.do("GET", "/api/me", nil, "Cookie", ck); !cleared(r) {
		t.Fatal("max age, old cookie: want 401 and a cleared cookie", r.code, r.hdr.Values("Set-Cookie"))
	}
	if n := len(must(e.s.Poller.Admin.ListSessions())); n != 0 {
		t.Fatal("the session must leave sessions.json, have", n)
	}

	// used the whole time: the cookie is fresh, the sign-in is not
	e = setup(t)
	clk = e.fakeClock()
	idle := time.Duration(e.s.Cfg.SessionIdleTimeout) * time.Second
	ck = e.cookie(viewer, 0)
	for at := time.Duration(0); at+idle/2 < maxAge; at += idle / 2 {
		clk.advance(idle / 2)
		r := e.do("GET", "/api/me", nil, "Cookie", ck, "X-Fairy-Active", "1")
		if r.code != 200 {
			t.Fatalf("after %v: %d", at+idle/2, r.code)
		}
		ck = sessionCookie(e, r.hdr)
	}
	clk.advance(idle/2 + time.Minute)
	if r := e.do("GET", "/api/me", nil, "Cookie", ck, "X-Fairy-Active", "1"); !cleared(r) {
		t.Fatal("max age, fresh cookie: want 401 and a cleared cookie", r.code, r.hdr.Values("Set-Cookie"))
	}
	if n := len(must(e.s.Poller.Admin.ListSessions())); n != 0 {
		t.Fatal("the session must leave sessions.json, have", n)
	}
}
