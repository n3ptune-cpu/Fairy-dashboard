package web

import (
	"sync/atomic"
	"testing"
	"time"
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

// A session that is not used for session_idle_timeout is refused; one that is older than session_max_age is
// refused however much it was used.
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
