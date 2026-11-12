package web

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
)

// usedCode makes the stand-in identity provider behave like a real one from now on: a code is taken once.
func (f *fakeIDP) usedCode() {
	f.mu.Lock()
	f.tokenStatus, f.tokenBody = 400, `{"error":"invalid_grant"}`
	f.mu.Unlock()
}

func (f *fakeIDP) exchanges() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenRequests
}

// r3-p1-01 (audit 5, round 3; the auditor's TestR3P1_01). The answer to the callback of a "Confirm it's you"
// does not reach the browser. The kept shutdown HAS run. The browser asks again with what it has: the same
// address and the cookie with the old session id. Before: "login failed", the browser signed out, and after
// the next sign-in nothing said that the shutdown was sent. Now: the same redirect, the cookie of the session
// the confirmation made, the shutdown not sent again, and the page shows its outcome.
func TestARepeatedStepUpCallbackIsAnsweredAgain(t *testing.T) {
	a := arm(t)
	q, rt := a.idp.confirmStart(a.e, a.id, "/#/system/laptop", a.sess)
	a.idp.claims["auth_time"] = a.clk.now().Unix()
	callback := "/auth/callback?code=c&state=" + q.Get("state")
	first := a.e.do("GET", callback, nil, "Cookie", rt) // its answer is lost
	if first.code != 303 || a.powerCalls() != 1 {
		t.Fatalf("first callback: %d %s; shutdown requests: %d", first.code, first.raw, a.powerCalls())
	}
	a.idp.usedCode()
	asked := a.idp.exchanges()
	a.clk.advance(20 * time.Second)

	reload := a.e.do("GET", callback, nil, "Cookie", rt) // the browser's reload: the same cookie
	if reload.code != 303 || reload.hdr.Get("Location") != "/#/system/laptop" {
		t.Fatalf("the reload must land where the first answer would have: %d %s (Location %q)", reload.code, strings.TrimSpace(string(reload.raw)), reload.hdr.Get("Location"))
	}
	if a.powerCalls() != 1 || a.idp.exchanges() != asked {
		t.Fatalf("the reload ran something again: shutdown requests %d, code exchanges %d (were 1 and %d)", a.powerCalls(), a.idp.exchanges(), asked)
	}
	browser := cookieAfter(a.e, rt, reload.hdr)
	if browser == "" || browser == rt || sidOf(a.e, browser) != sidOf(a.e, sessionCookie(a.e, first.hdr)) {
		t.Fatal("the reload must hand the browser the session the confirmation made (and no other)")
	}
	me := a.e.do("GET", "/api/me", nil, "Cookie", browser)
	pr, _ := me.body["pending_result"].(map[string]any)
	if me.code != 200 || pr == nil || pr["ok"] != true || pr["path"] != "/api/systems/laptop/power" || !strings.Contains(pr["message"].(string), "shutdown in") {
		t.Fatalf("the page must show that the shutdown was sent: /api/me %d, pending_result %v", me.code, me.body["pending_result"])
	}
	// the round trip's cookie is still not a session: only this callback answers it
	if r := a.e.do("GET", "/api/me", nil, "Cookie", rt); r.code != 401 {
		t.Fatalf("the old session id must stay ended: %d", r.code)
	}
	// the confirmation counts for the session the reload handed out (another control action runs without a new one)
	name := a.e.do("GET", "/api/systems/laptop", nil, "Cookie", browser).body["name"].(string)
	if r := a.e.do("POST", "/api/systems/laptop/power", map[string]any{"action": "shutdown", "delay_s": 10, "confirm": name},
		"Cookie", browser, "X-Requested-With", "fairy", "Content-Type", "application/json"); r.code != 200 {
		t.Fatalf("a control action right after: %d %s", r.code, r.raw)
	}
	if !strings.Contains(auditOutcomes(a.e.s.Cfg.AuditLog), "auth.step_up repeated callback of a confirmation that was accepted: answered again, nothing run again") {
		t.Error("the repeat is not in the audit log")
	}
	// now that the new cookie is in use, the old one gets nothing any more: the strict way, as before
	if r := a.e.do("GET", callback, nil, "Cookie", rt); r.code != 400 || len(r.hdr.Values("Set-Cookie")) != 0 && cookieAfter(a.e, "x", r.hdr) != "x" {
		t.Fatalf("a repeat with the old cookie after the new one was used: %d %s %q", r.code, r.raw, r.hdr.Values("Set-Cookie"))
	}
	// the person goes back to the callback's address with the new cookie: the same page, nothing run, no new cookie
	back := a.e.do("GET", callback, nil, "Cookie", browser)
	if back.code != 303 || back.hdr.Get("Location") != "/#/system/laptop" || a.powerCalls() != 2 || cookieAfter(a.e, browser, back.hdr) != browser {
		t.Fatalf("back to the callback with the new cookie: %d %q, shutdown requests %d", back.code, back.hdr.Get("Location"), a.powerCalls())
	}
	if r := a.e.do("GET", "/api/me", nil, "Cookie", browser); r.code != 200 {
		t.Fatalf("the browser must still be signed in: %d", r.code)
	}
}

// What a repeat does NOT get. Each case is one confirmation whose answer is lost, and one more request.
func TestARepeatedStepUpCallbackStaysStrict(t *testing.T) {
	type lost struct {
		a        *armed
		rt, call string
	}
	lose := func(t *testing.T) lost {
		a := arm(t)
		q, rt := a.idp.confirmStart(a.e, a.id, "/", a.sess)
		a.idp.claims["auth_time"] = a.clk.now().Unix()
		l := lost{a: a, rt: rt, call: "/auth/callback?code=c&state=" + q.Get("state")}
		if first := a.e.do("GET", l.call, nil, "Cookie", rt); first.code != 303 || a.powerCalls() != 1 {
			t.Fatalf("first callback: %d %s", first.code, first.raw)
		}
		a.idp.usedCode()
		return l
	}
	refused := func(t *testing.T, l lost, r resp) {
		t.Helper()
		if r.code != 400 || !strings.Contains(string(r.raw), "Go back and try again") || l.a.powerCalls() != 1 {
			t.Fatalf("must be refused as before: %d %s; shutdown requests %d", r.code, strings.TrimSpace(string(r.raw)), l.a.powerCalls())
		}
		for _, ck := range (&http.Response{Header: r.hdr}).Cookies() {
			if ck.Name == l.a.e.s.codec.name() && ck.Value != "" {
				t.Fatalf("a refused repeat was handed a session cookie")
			}
		}
	}
	t.Run("another browser (User-Agent) with a copy of the cookie", func(t *testing.T) {
		l := lose(t)
		refused(t, l, l.a.e.do("GET", l.call, nil, "Cookie", l.rt, "User-Agent", "curl/8"))
	})
	t.Run("another address with a copy of the cookie", func(t *testing.T) {
		l := lose(t)
		refused(t, l, l.a.e.do("GET", l.call, nil, "Cookie", l.rt, "X-Forwarded-For", "203.0.113.50"))
	})
	t.Run("another state", func(t *testing.T) {
		l := lose(t)
		refused(t, l, l.a.e.do("GET", "/auth/callback?code=c&state=another", nil, "Cookie", l.rt))
	})
	t.Run("more than two minutes later", func(t *testing.T) {
		l := lose(t)
		l.a.clk.advance(stepRepeatFor + time.Second)
		refused(t, l, l.a.e.do("GET", l.call, nil, "Cookie", l.rt))
	})
	t.Run("without the cookie", func(t *testing.T) {
		l := lose(t)
		refused(t, l, l.a.e.do("GET", l.call, nil))
	})
	t.Run("the session of the confirmation was signed out in between", func(t *testing.T) {
		l := lose(t)
		all, _ := l.a.e.s.Poller.Admin.Sessions()
		for _, x := range all {
			_ = l.a.e.s.dropSession(x.SID, "test")
		}
		refused(t, l, l.a.e.do("GET", l.call, nil, "Cookie", l.rt))
	})
	t.Run("a confirmation that was not accepted (Pocket ID did not ask again) leaves nothing to repeat", func(t *testing.T) {
		a := arm(t)
		q, rt := a.idp.confirmStart(a.e, a.id, "/", a.sess)
		a.idp.claims["auth_time"] = a.clk.now().Add(-10 * time.Minute).Unix()
		call := "/auth/callback?code=c&state=" + q.Get("state")
		if first := a.e.do("GET", call, nil, "Cookie", rt); first.code != 400 || a.powerCalls() != 0 {
			t.Fatalf("first callback: %d", first.code)
		}
		a.idp.usedCode()
		if r := a.e.do("GET", call, nil, "Cookie", rt); r.code != 400 || a.powerCalls() != 0 || !strings.Contains(string(r.raw), "Go back and try again") {
			t.Fatalf("repeat of a refused confirmation: %d %s", r.code, r.raw)
		}
		if r := a.e.do("GET", "/api/me", nil, "Cookie", a.sess); r.code != 200 {
			t.Fatalf("the session it did not confirm goes on, as before: %d", r.code)
		}
	})
	t.Run("a plain sign-in is not remembered", func(t *testing.T) {
		e := setup(t)
		idp := newFakeIDP(t)
		e.s.Cfg.OIDC.Issuer = idp.srv.URL
		idp.claims = map[string]any{"groups": []string{"viewers", "controllers"}, "auth_time": time.Now().Unix()}
		q, rt := idp.login(e, "/auth/login?next=/", "")
		call := "/auth/callback?code=c&state=" + q.Get("state")
		if r := e.do("GET", call, nil, "Cookie", rt); r.code != 303 {
			t.Fatalf("sign-in: %d", r.code)
		}
		idp.usedCode()
		if r := e.do("GET", call, nil, "Cookie", rt); r.code != 400 {
			t.Fatalf("repeat of a sign-in callback: %d %s", r.code, r.raw)
		}
		if n := len(e.s.codec.live.steps); n != 0 {
			t.Fatalf("%d round trips remembered", n)
		}
	})
}

// slowPower holds every shutdown request to the laptop until release is closed, and says when one is waiting.
type slowPower struct {
	poller.Backend
	waiting chan struct{}
	release chan struct{}
	mu      sync.Mutex
	aborted int // requests whose context ended while they waited
}

func (s *slowPower) AgentRequest(ctx context.Context, a config.Agent, method, path string, body any) (int, jsonx.M, error) {
	if a.ID == "laptop" && method == "POST" && path == "/power" {
		s.waiting <- struct{}{}
		select {
		case <-s.release:
		case <-ctx.Done():
			s.mu.Lock()
			s.aborted++
			s.mu.Unlock()
			return 0, nil, ctx.Err()
		}
	}
	return s.Backend.AgentRequest(ctx, a, method, path, body)
}

// The auditor's first case: the person reloads a callback that takes long because the machine answers slowly.
// The browser drops the first request and sends the second while the first is still at work. The request to
// the machine is not stopped half-way, the reload waits for it and gets the session and the outcome.
func TestAReloadWhileTheKeptActionRuns(t *testing.T) {
	slow := &slowPower{waiting: make(chan struct{}, 4), release: make(chan struct{})}
	e := setupWith(t, func(b poller.Backend) poller.Backend { slow.Backend = b; return slow })
	a := &armed{e: e, idp: newFakeIDP(t), clk: e.fakeClock()}
	e.s.Cfg.OIDC.Issuer = a.idp.srv.URL
	a.idp.claims = map[string]any{"groups": []string{"viewers", "controllers"}, "auth_time": time.Now().Add(-time.Hour).Unix()}
	q0, rt0 := a.idp.login(e, "/auth/login?next=/", "")
	a.sess = sessionCookie(e, e.do("GET", "/auth/callback?code=c&state="+q0.Get("state"), nil, "Cookie", rt0).hdr)
	if a.id = a.shutdown(); a.id == "" {
		t.Fatal("the shutdown must be kept")
	}
	q, rt := a.idp.confirmStart(e, a.id, "/", a.sess)
	a.idp.claims["auth_time"] = a.clk.now().Unix()
	call := e.srv.URL + "/auth/callback?code=c&state=" + q.Get("state")
	get := func(ctx context.Context) (*http.Response, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", call, nil)
		req.Header.Set("Cookie", rt)
		return (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	}
	ctx1, drop := context.WithCancel(context.Background())
	firstGone := make(chan struct{})
	go func() {
		defer close(firstGone)
		if r, err := get(ctx1); err == nil {
			r.Body.Close()
		}
	}()
	select {
	case <-slow.waiting: // the first callback is at the machine
	case <-time.After(10 * time.Second):
		t.Fatal("the kept shutdown was not sent")
	}
	a.idp.usedCode()
	drop() // the reload: the browser drops the first request ...
	<-firstGone
	type answer struct {
		r   *http.Response
		err error
	}
	second := make(chan answer, 1)
	go func() { r, err := get(context.Background()); second <- answer{r, err} }() // ... and sends it again
	select {
	case got := <-second:
		t.Fatalf("the reload was answered while the first callback was still at work: %v %v", got.r, got.err)
	case <-time.After(300 * time.Millisecond):
	}
	close(slow.release) // the machine answers
	var got answer
	select {
	case got = <-second:
	case <-time.After(10 * time.Second):
		t.Fatal("the reload got no answer")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	got.r.Body.Close()
	slow.mu.Lock()
	aborted := slow.aborted
	slow.mu.Unlock()
	if aborted != 0 {
		t.Fatalf("the request to the machine was stopped half-way by the reload (%d)", aborted)
	}
	browser := cookieAfter(e, rt, got.r.Header)
	if got.r.StatusCode != 303 || browser == "" || browser == rt || a.powerCalls() != 1 {
		t.Fatalf("the reload: %d, shutdown requests %d", got.r.StatusCode, a.powerCalls())
	}
	me := e.do("GET", "/api/me", nil, "Cookie", browser)
	if pr, _ := me.body["pending_result"].(map[string]any); me.code != 200 || pr == nil || pr["ok"] != true {
		t.Fatalf("the outcome of the shutdown: /api/me %d %v", me.code, me.body["pending_result"])
	}
}
