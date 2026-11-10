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
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
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
// and any write the panel accepts. API tokens have no idle timeout.
func TestActivityExtendsTheSession(t *testing.T) {
	for name, use := range map[string]func(e *env, ck string) resp{
		"marked read": func(e *env, ck string) resp { return e.do("GET", "/api/me", nil, "Cookie", ck, "X-Fairy-Active", "1") },
		"activity signal": func(e *env, ck string) resp {
			return e.do("POST", "/api/me/active", nil, "Cookie", ck, "X-Requested-With", "fairy")
		},
		"write": func(e *env, ck string) resp {
			return e.do("POST", "/api/notifications/read", map[string]any{"all": true}, "Cookie", ck, "X-Requested-With", "fairy")
		},
	} {
		e := setup(t)
		clk := e.fakeClock()
		idle := time.Duration(e.s.Cfg.SessionIdleTimeout) * time.Second
		ck := e.cookie(controller(), 0)
		for i := 0; i < 4; i++ { // twice the timeout in all, used every half timeout
			clk.advance(idle / 2)
			r := use(e, ck)
			if r.code != 200 && r.code != 204 {
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

// L8: live-update streams are limited per user (or API token) and for the whole panel. One more is answered 429
// with Retry-After, and a closed stream frees its slot.
func TestStreamLimits(t *testing.T) {
	e := setup(t)
	admin := controller()
	ck := e.cookie(admin, 0)
	var cancels []context.CancelFunc
	for i := 0; i < maxStreamsPerUser; i++ {
		code, _, cancel := e.holdStream("Cookie", ck)
		if code != 200 {
			t.Fatalf("stream %d: %d", i+1, code)
		}
		cancels = append(cancels, cancel)
	}
	code, hdr, _ := e.holdStream("Cookie", ck)
	if code != 429 || hdr.Get("Retry-After") == "" || !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") {
		t.Fatal("the ninth stream of one user must be refused with 429 and Retry-After:", code, hdr.Get("Retry-After"))
	}
	if code, _, _ := e.holdStream("Cookie", e.staleCookie(admin, 0)); code != 429 {
		t.Fatal("the limit is per user, not per session: another session of the same user", code)
	}
	// an API token has its own slots
	raw, _, _ := e.s.Tokens.Create("ha", "system", "read", "cli:test", 0)
	for i := 0; i < maxStreamsPerUser; i++ {
		if code, _, _ := e.holdStream("Authorization", "Bearer "+raw); code != 200 {
			t.Fatalf("token stream %d: %d", i+1, code)
		}
	}
	if code, _, _ := e.holdStream("Authorization", "Bearer "+raw); code != 429 {
		t.Fatal("the ninth stream of one token", code)
	}
	// ordinary requests are not affected
	if r := e.do("GET", "/api/me", nil, "Cookie", ck); r.code != 200 {
		t.Fatal(r.code)
	}
	// closing one frees its slot
	cancels[0]()
	freed := false
	for i := 0; i < 50 && !freed; i++ {
		time.Sleep(100 * time.Millisecond)
		code, _, cancel := e.holdStream("Cookie", ck)
		freed = code == 200
		if !freed {
			cancel()
		}
	}
	if !freed {
		t.Fatal("a closed stream did not free its slot")
	}

	// the cap for the whole panel
	e = setup(t)
	e.s.codec.live.streams.total = 3
	for i, name := range []string{"a", "b", "c", "d"} {
		raw, _, _ := e.s.Tokens.Create(name, "system", "read", "cli:test", 0)
		code, hdr, _ := e.holdStream("Authorization", "Bearer "+raw)
		if want := map[bool]int{true: 200, false: 429}[i < 3]; code != want || (want == 429 && hdr.Get("Retry-After") == "") {
			t.Fatalf("stream %d of the whole panel: %d, want %d", i+1, code, want)
		}
	}
}

// holdStream opens a stream and keeps it open until cancel is called or the test ends.
func (e *env) holdStream(hdr ...string) (int, http.Header, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	e.t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/api/stream", nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	go func() { defer r.Body.Close(); _, _ = io.Copy(io.Discard, r.Body) }()
	return r.StatusCode, r.Header, cancel
}

// L9: the session cookie is called __Host-fairy_session (Secure, Path=/, no Domain), which a sibling site cannot
// plant. A cookie under the old name is ignored and expired; only the FAIRY_INSECURE_COOKIES test mode keeps
// the plain name.
func TestSessionCookieName(t *testing.T) {
	e := setup(t)
	clk := e.fakeClock()
	ck := e.cookie(viewer, 0)
	if !strings.HasPrefix(ck, "__Host-fairy_session=") {
		t.Fatal("cookie name:", ck[:30])
	}
	if r := e.do("GET", "/api/me", nil, "Cookie", ck); r.code != 200 {
		t.Fatal("round trip under the __Host- name", r.code)
	}
	// the same signed value under the old name (a session from before the change, or a planted cookie)
	old := "fairy_session=" + strings.TrimPrefix(ck, "__Host-fairy_session=")
	if r := e.do("GET", "/api/me", nil, "Cookie", old); r.code != 401 {
		t.Fatal("a cookie under the old name must be ignored", r.code)
	}
	// both present: only the __Host- one counts, whatever the other says
	planted := "fairy_session=" + strings.TrimPrefix(e.cookie(controller(), 0), "__Host-fairy_session=")
	if r := e.do("GET", "/api/me", nil, "Cookie", planted+"; "+ck); r.code != 200 || r.body["sub"] != viewer.Sub {
		t.Fatal("a planted old-name cookie changed who is signed in", r.code, string(r.raw))
	}
	// what a response that sets the cookie looks like
	clk.advance(2 * time.Minute)
	r := e.do("GET", "/api/me", nil, "Cookie", ck, "X-Fairy-Active", "1")
	var set, expired string
	for _, c := range r.hdr.Values("Set-Cookie") {
		if strings.HasPrefix(c, "__Host-fairy_session=") {
			set = c
		}
		if strings.HasPrefix(c, "fairy_session=;") {
			expired = c
		}
	}
	for _, want := range []string{"Path=/", "Secure", "HttpOnly", "SameSite=Lax"} {
		if !strings.Contains(set, want) {
			t.Errorf("Set-Cookie lacks %s: %s", want, set)
		}
	}
	if set == "" || strings.Contains(strings.ToLower(set), "domain=") {
		t.Error("a __Host- cookie must not carry a Domain:", set)
	}
	if !strings.Contains(expired, "Max-Age=0") {
		t.Error("the old name must be expired on a response that sets the new one:", r.hdr.Values("Set-Cookie"))
	}
	if h := r.hdr.Get("Strict-Transport-Security"); h != "max-age=63072000" {
		t.Error("HSTS must stay without includeSubDomains:", h)
	}

	// FAIRY_INSECURE_COOKIES (local http testing): the plain name, not Secure
	insecure := newCodec(strings.Repeat("s", 48), 3600, false)
	rec := httptest.NewRecorder()
	insecure.write(rec, &sessionData{SID: "x", User: &viewer, IAT: time.Now().Unix()})
	got := rec.Header().Values("Set-Cookie")
	if len(got) != 1 || !strings.HasPrefix(got[0], "fairy_session=") || strings.Contains(got[0], "Secure") {
		t.Fatal("insecure test mode:", got)
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Cookie", strings.Split(got[0], ";")[0])
	if insecure.decode(req).SID != "x" {
		t.Fatal("round trip under the plain name in the test mode")
	}
	secure := newCodec(strings.Repeat("s", 48), 3600, true)
	if secure.decode(req).SID != "" {
		t.Fatal("the plain name must not be read in normal mode")
	}
}

// NEW-5: a request the panel refuses is not use of the session. Before, every non-GET request with the cookie
// moved "last used" before anything was checked, so a form post from a sibling host (it carries a SameSite=Lax
// cookie, is answered 403 for the missing header) kept an unattended session alive for ever.
func TestRefusedRequestsAreNotUse(t *testing.T) {
	csrf := []string{"X-Requested-With", "fairy"}
	for name, send := range map[string]func(e *env, ck string) resp{
		"a write without the CSRF header": func(e *env, ck string) resp { return e.do("POST", "/api/me/active", nil, "Cookie", ck) },
		"a form post from a sibling host": func(e *env, ck string) resp {
			return e.do("POST", "/api/me/active", nil, "Cookie", ck, "Content-Type", "application/x-www-form-urlencoded",
				"Origin", "https://other.example.com", "Sec-Fetch-Site", "same-site")
		},
		"a write from another origin": func(e *env, ck string) resp {
			return e.do("POST", "/api/me/active", nil, append([]string{"Cookie", ck, "Origin", "https://evil.example"}, csrf...)...)
		},
		"a write marked cross-site": func(e *env, ck string) resp {
			return e.do("POST", "/api/me/active", nil, append([]string{"Cookie", ck, "Sec-Fetch-Site", "cross-site"}, csrf...)...)
		},
		"a write marked same-site": func(e *env, ck string) resp {
			return e.do("POST", "/api/me/active", nil, append([]string{"Cookie", ck, "Sec-Fetch-Site", "same-site"}, csrf...)...)
		},
		"a sign-out without the CSRF header": func(e *env, ck string) resp { return e.do("POST", "/auth/logout", nil, "Cookie", ck) },
		"a write to a route that does not exist": func(e *env, ck string) resp {
			return e.do("POST", "/api/nope", nil, append([]string{"Cookie", ck}, csrf...)...)
		},
		"a write the user's role does not allow": func(e *env, ck string) resp { return e.poll(ck) }, // 403: not an admin
		"a write with a body the panel refuses": func(e *env, ck string) resp {
			return e.do("POST", "/api/notifications/read", map[string]any{"nope": 1}, append([]string{"Cookie", ck}, csrf...)...)
		},
		"a marked read from another site": func(e *env, ck string) resp {
			return e.do("GET", "/api/me", nil, "Cookie", ck, "X-Fairy-Active", "1", "Sec-Fetch-Site", "cross-site")
		},
		"a marked read that is refused": func(e *env, ck string) resp {
			return e.do("GET", "/api/audit", nil, "Cookie", ck, "X-Fairy-Active", "1") // 403: admins only
		},
		"a marked read of something that is not there": func(e *env, ck string) resp {
			return e.do("GET", "/api/systems/nope", nil, "Cookie", ck, "X-Fairy-Active", "1")
		},
	} {
		e := setup(t)
		clk := e.fakeClock()
		ck := e.cookie(model.User{Sub: "u7", Name: model.P("Vee"), Groups: []string{"viewers"}}, 0)
		if _, _, err := e.s.Access.SetRole("u7", "viewer", nil, "test", false); err != nil {
			t.Fatal(err)
		}
		idle := time.Duration(e.s.Cfg.SessionIdleTimeout) * time.Second
		for i := 0; i < 3; i++ {
			clk.advance(idle / 3)
			if i == 2 {
				clk.advance(time.Minute) // now the whole timeout has passed since the sign-in
			}
			r := send(e, ck)
			answered := name == "a marked read from another site" // it is answered (a read), it only does not count
			if i < 2 && (r.code == 401 || (r.code < 400) != answered) {
				t.Fatalf("%s: answered %d, the test wants a refusal of a live session", name, r.code)
			}
			if i < 2 && strings.Contains(strings.Join(r.hdr.Values("Set-Cookie"), ";"), e.s.codec.name()+"=ey") {
				t.Fatalf("%s: a refused request refreshed the cookie", name)
			}
		}
		if r := e.do("GET", "/api/me", nil, "Cookie", ck); r.code != 401 {
			t.Errorf("%s kept an unattended session alive past session_idle_timeout: %d", name, r.code)
		}
	}

	// a control request that is asked to confirm is not use either; the confirmation is (the callback)
	a := signedIn(t)
	idle := time.Duration(a.e.s.Cfg.SessionIdleTimeout) * time.Second
	a.clk.advance(idle - time.Minute)
	id := a.shutdown()
	if id == "" {
		t.Fatal("not kept")
	}
	a.clk.advance(2 * time.Minute)
	if r := a.e.do("GET", "/api/me", nil, "Cookie", a.sess); r.code != 401 {
		t.Fatal("a request answered step_up_required counted as use:", r.code)
	}
	a = signedIn(t)
	a.clk.advance(idle - time.Minute)
	id = a.shutdown()
	if r := a.confirm(id, false); r.code != 303 {
		t.Fatal(r.code, string(r.raw))
	}
	a.clk.advance(idle - time.Minute)
	if r := a.e.do("GET", "/api/me", nil, "Cookie", a.sess); r.code != 200 {
		t.Fatal("confirming at Pocket ID is use:", r.code)
	}
}
