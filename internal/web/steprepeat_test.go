package web

import (
	"context"
	"io"
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

// noCookieSet fails when an answer carries any Set-Cookie header (not even one that clears a cookie).
func noCookieSet(t *testing.T, what string, h http.Header) {
	t.Helper()
	if n := len(h.Values("Set-Cookie")); n != 0 {
		t.Fatalf("%s: the answer sets %d cookie(s); a repeat must set none", what, n)
	}
}

// alreadySent checks the small page a repeat with the round trip's own cookie gets: 409, the two sentences,
// one link back, no script, no cookie.
func alreadySent(t *testing.T, what string, r resp, back string) {
	t.Helper()
	alreadyConfirmed(t, what, r, back, "The action was already sent. It is not sent again. Sign in again to go on.")
}

// alreadyConfirmed is alreadySent with the sentence the page must hold: what came of the kept action, as far as
// the first callback knows it (audit 6, r4-p1-01).
func alreadyConfirmed(t *testing.T, what string, r resp, back, sentence string) {
	t.Helper()
	body := string(r.raw)
	if r.code != 409 || !strings.Contains(r.hdr.Get("Content-Type"), "text/html") || !strings.Contains(body, "<h1>Already confirmed</h1>") ||
		!strings.Contains(body, sentence) {
		t.Fatalf("%s: %d %s", what, r.code, strings.TrimSpace(body))
	}
	if strings.Count(body, "<a ") != 1 || !strings.Contains(body, `href="`+back+`">Back to Fairy</a>`) || strings.Contains(body, "<script") {
		t.Fatalf("%s: the page must have one link back to %q and no script:\n%s", what, back, body)
	}
	noCookieSet(t, what, r.hdr)
}

// r3-p1-01 (audit 5, round 3; the auditor's TestR3P1_01), as decided after R7B-01. The answer to the callback
// of a "Confirm it's you" does not reach the browser. The kept shutdown HAS run. The browser asks again with
// what it has: the same address and the cookie with the old session id. Before r3-p1-01: "login failed". With
// the first fix: the new session's cookie, which a copy of the cookie got as well (R7B-01). Now: a page that
// says the action was already sent and to sign in again, no cookie of any kind, the shutdown not sent again,
// Pocket ID not asked again.
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

	for i := 0; i < 3; i++ { // however often
		alreadySent(t, "the reload with the old cookie", a.e.do("GET", callback, nil, "Cookie", rt), "/#/system/laptop")
	}
	if a.powerCalls() != 1 || a.idp.exchanges() != asked {
		t.Fatalf("the reload ran something again: shutdown requests %d, code exchanges %d (were 1 and %d)", a.powerCalls(), a.idp.exchanges(), asked)
	}
	// the round trip's cookie is not a session and gets nothing done
	if r := a.e.do("GET", "/api/me", nil, "Cookie", rt); r.code != 401 {
		t.Fatalf("the old session id must stay ended: %d", r.code)
	}
	if !strings.Contains(auditOutcomes(a.e.s.Cfg.AuditLog), "auth.step_up repeated callback of a confirmation that was accepted: no session handed out, nothing run again") {
		t.Error("the repeat is not in the audit log")
	}
	// the session the confirmation made is untouched by the repeats: whoever got the first answer goes on
	browser := sessionCookie(a.e, first.hdr)
	me := a.e.do("GET", "/api/me", nil, "Cookie", browser)
	pr, _ := me.body["pending_result"].(map[string]any)
	if me.code != 200 || pr == nil || pr["ok"] != true || pr["path"] != "/api/systems/laptop/power" || !strings.Contains(pr["message"].(string), "shutdown in") {
		t.Fatalf("the first answer's session must show that the shutdown was sent: /api/me %d, pending_result %v", me.code, me.body["pending_result"])
	}
	// a confirmation without a kept action: the page does not claim that an action was sent
	a.idp.mu.Lock()
	a.idp.tokenStatus, a.idp.tokenBody = 0, ""
	a.idp.mu.Unlock()
	a.sess = browser
	a.clk.advance(time.Hour)
	q, rt = a.idp.login(a.e, "/auth/login?step_up=1&next=/", a.sess)
	a.idp.claims["auth_time"] = a.clk.now().Unix()
	callback = "/auth/callback?code=c&state=" + q.Get("state")
	if r := a.e.do("GET", callback, nil, "Cookie", rt); r.code != 303 {
		t.Fatalf("a confirmation without an action: %d %s", r.code, r.raw)
	}
	r := a.e.do("GET", callback, nil, "Cookie", rt)
	if r.code != 409 || !strings.Contains(string(r.raw), "This confirmation was already used. Sign in again to go on.") || strings.Contains(string(r.raw), "already sent") {
		t.Fatalf("the repeat of a confirmation without an action: %d %s", r.code, r.raw)
	}
	noCookieSet(t, "the repeat of a confirmation without an action", r.hdr)
	if a.powerCalls() != 1 {
		t.Fatalf("shutdown requests: %d", a.powerCalls())
	}
}

// The normal reload: the answer DID arrive, the browser holds the new session's cookie and asks the callback's
// address again. It is sent to the page it was sent to the first time, gets no cookie (it has one), the
// shutdown is not sent again, and it stays signed in with its confirmation.
func TestAReloadWithTheNewCookieStaysSignedIn(t *testing.T) {
	a := arm(t)
	q, rt := a.idp.confirmStart(a.e, a.id, "/#/system/laptop", a.sess)
	a.idp.claims["auth_time"] = a.clk.now().Unix()
	callback := "/auth/callback?code=c&state=" + q.Get("state")
	first := a.e.do("GET", callback, nil, "Cookie", rt)
	if first.code != 303 || a.powerCalls() != 1 {
		t.Fatalf("first callback: %d %s; shutdown requests: %d", first.code, first.raw, a.powerCalls())
	}
	a.idp.usedCode()
	asked := a.idp.exchanges()
	browser := sessionCookie(a.e, first.hdr)
	for i := 0; i < 3; i++ {
		back := a.e.do("GET", callback, nil, "Cookie", browser)
		if back.code != 303 || back.hdr.Get("Location") != "/#/system/laptop" {
			t.Fatalf("back to the callback with the new cookie: %d %q %s", back.code, back.hdr.Get("Location"), back.raw)
		}
		noCookieSet(t, "the reload with the new cookie", back.hdr)
	}
	if a.powerCalls() != 1 || a.idp.exchanges() != asked {
		t.Fatalf("the reload ran something again: shutdown requests %d, code exchanges %d (were 1 and %d)", a.powerCalls(), a.idp.exchanges(), asked)
	}
	me := a.e.do("GET", "/api/me", nil, "Cookie", browser)
	if pr, _ := me.body["pending_result"].(map[string]any); me.code != 200 || pr == nil || pr["ok"] != true {
		t.Fatalf("the browser must still be signed in and see the outcome: /api/me %d %v", me.code, me.body["pending_result"])
	}
	// its confirmation still counts: another control action runs without a new one
	name := a.e.do("GET", "/api/systems/laptop", nil, "Cookie", browser).body["name"].(string)
	if r := a.e.do("POST", "/api/systems/laptop/power", map[string]any{"action": "shutdown", "delay_s": 10, "confirm": name},
		"Cookie", browser, "X-Requested-With", "fairy", "Content-Type", "application/json"); r.code != 200 || a.powerCalls() != 2 {
		t.Fatalf("a control action right after: %d %s", r.code, r.raw)
	}
	// only exactly that callback: the new cookie with another code or another state is a stranger
	for _, other := range []string{"/auth/callback?code=other&state=" + q.Get("state"), "/auth/callback?state=" + q.Get("state"), "/auth/callback?code=c&state=other"} {
		if r := a.e.do("GET", other, nil, "Cookie", browser); r.code != 400 {
			t.Fatalf("%s with the new cookie: %d", other, r.code)
		}
	}
}

// R7B-01 (the reviewer's TestReviewB4_CopyOfCookieWithoutCodeGetsTheConfirmedSession). A COPY of the round
// trip's cookie, used from the same address with the same User-Agent after the real browser's confirmation.
// The state is readable in the cookie (signed, not encrypted), so the copy needs no authorization code. It was
// handed the new session with the fresh passkey confirmation and ran a second shutdown with no question. It
// gets no cookie now, with the code or without it.
func TestACopyOfTheRoundTripCookieGetsNoSession(t *testing.T) {
	a := arm(t)
	name := a.e.do("GET", "/api/systems/laptop", nil, "Cookie", a.sess).body["name"].(string)
	q, rt := a.idp.confirmStart(a.e, a.id, "/", a.sess)
	a.idp.claims["auth_time"] = a.clk.now().Unix()
	first := a.e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt) // the real browser
	if first.code != 303 || a.powerCalls() != 1 {
		t.Fatalf("first callback: %d", first.code)
	}
	a.idp.usedCode()
	real := sessionCookie(a.e, first.hdr) // (not used yet: the copy is quicker than the real browser)
	for _, c := range []struct{ what, url string }{
		{"the copy without the code", "/auth/callback?state=" + q.Get("state")},
		{"the copy with another code", "/auth/callback?code=x&state=" + q.Get("state")},
		{"the copy with the code", "/auth/callback?code=c&state=" + q.Get("state")},
	} {
		cp := a.e.do("GET", c.url, nil, "Cookie", rt)
		thief := cookieAfter(a.e, rt, cp.hdr)
		if cp.code == 303 || thief != rt {
			t.Errorf("R7B-01: %s: status %d, handed the new session's cookie: %v", c.what, cp.code, thief != "" && sidOf(a.e, thief) == sidOf(a.e, real))
		}
		if n := len(cp.hdr.Values("Set-Cookie")); n != 0 {
			t.Errorf("%s: the answer sets %d cookie(s)", c.what, n)
		}
		// what the copy holds runs nothing
		r := a.e.do("POST", "/api/systems/laptop/power", map[string]any{"action": "shutdown", "delay_s": 10, "confirm": name},
			"Cookie", thief, "X-Requested-With", "fairy", "Content-Type", "application/json")
		if r.code != 401 || a.powerCalls() != 1 {
			t.Errorf("R7B-01: %s runs a control action without any passkey: status %d, shutdown requests now %d", c.what, r.code, a.powerCalls())
		}
	}
	// only exactly that callback is recognised as a repeat: without the code it is a stranger's ("login failed")
	if r := a.e.do("GET", "/auth/callback?state="+q.Get("state"), nil, "Cookie", rt); r.code != 400 || !strings.Contains(string(r.raw), "Go back and try again") {
		t.Errorf("the copy without the code must go the strict way: %d %s", r.code, strings.TrimSpace(string(r.raw)))
	}
	alreadySent(t, "the copy with the code", a.e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt), "/")
	if r := a.e.do("GET", "/api/me", nil, "Cookie", real); r.code != 200 {
		t.Fatalf("the real browser must not be touched by the copies: %d", r.code)
	}
}

// R7B-01, second half (the reviewer's TestReviewB4_CopyThatWaitsIsAnsweredTogetherWithTheRealBrowser). A copy
// that asked while the real callback was still at work (the kept action runs) was parked on the same record
// and released with the real answer: both then held the confirmed session. A repeat is not parked for the
// action any more and never gets a cookie.
func TestACopyThatAsksWhileTheCallbackWorksGetsNoSession(t *testing.T) {
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
	type answer struct {
		r   *http.Response
		err error
	}
	get := func(url string) chan answer {
		ch := make(chan answer, 1)
		go func() {
			req, _ := http.NewRequestWithContext(context.Background(), "GET", e.srv.URL+url, nil)
			req.Header.Set("Cookie", rt)
			r, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
			if err == nil {
				r.Body.Close()
			}
			ch <- answer{r, err}
		}()
		return ch
	}
	real := get("/auth/callback?code=c&state=" + q.Get("state"))
	select {
	case <-slow.waiting: // the real callback is at the machine
	case <-time.After(10 * time.Second):
		t.Fatal("the kept shutdown was not sent")
	}
	a.idp.usedCode()
	released := false
	release := func() {
		if !released {
			released = true
			close(slow.release)
		}
	}
	defer release()
	for _, c := range []struct{ what, url string }{
		{"the copy without the code", "/auth/callback?state=" + q.Get("state")},
		{"the copy with the code", "/auth/callback?code=c&state=" + q.Get("state")},
	} {
		cp := get(c.url)
		var ca answer
		select {
		case ca = <-cp:
		case <-time.After(3 * time.Second):
			t.Errorf("R7B-01: %s is parked until the real callback has finished", c.what)
			release()
			ca = <-cp
		}
		if ca.err != nil {
			t.Fatal(ca.err)
		}
		if thief := cookieAfter(e, rt, ca.r.Header); ca.r.StatusCode == 303 || thief != rt || len(ca.r.Header.Values("Set-Cookie")) != 0 {
			t.Errorf("R7B-01: %s, asking while the real callback was at work: status %d, holds a new session cookie: %v", c.what, ca.r.StatusCode, thief != rt && thief != "")
		}
	}
	release() // the machine answers
	ra := <-real
	if ra.err != nil {
		t.Fatal(ra.err)
	}
	browser := cookieAfter(e, rt, ra.r.Header)
	if ra.r.StatusCode != 303 || browser == "" || browser == rt || a.powerCalls() != 1 {
		t.Fatalf("the real browser: %d, shutdown requests %d", ra.r.StatusCode, a.powerCalls())
	}
	if r := e.do("GET", "/api/me", nil, "Cookie", browser); r.code != 200 {
		t.Fatalf("real browser: %d", r.code)
	}
	if r := e.do("GET", "/api/me", nil, "Cookie", rt); r.code != 401 {
		t.Errorf("R7B-01: what the copy holds is a session: /api/me %d", r.code)
	}
}

// What stays strict (the reviewer's TestReviewB4_AddressAloneAndOtherClientsGetNothing): the callback's address
// alone, the address with the cookie from before "Continue", another browser, another address.
func TestTheCallbackAddressAloneAndOtherClientsGetNothing(t *testing.T) {
	a := arm(t)
	q, rt := a.idp.confirmStart(a.e, a.id, "/", a.sess)
	a.idp.claims["auth_time"] = a.clk.now().Unix()
	call := "/auth/callback?code=c&state=" + q.Get("state")
	first := a.e.do("GET", call, nil, "Cookie", rt)
	if first.code != 303 {
		t.Fatal(first.code)
	}
	a.idp.usedCode()
	nothing := func(r resp) {
		t.Helper()
		noCookieSet(t, "a stranger's callback", r.hdr)
		if r.code != 400 || !strings.Contains(string(r.raw), "Go back and try again") {
			t.Fatalf("must be refused the strict way: %d %s", r.code, strings.TrimSpace(string(r.raw)))
		}
	}
	nothing(a.e.do("GET", call, nil))                                                  // the stolen address, no cookie
	nothing(a.e.do("GET", call, nil, "Cookie", a.sess))                                // the address with the cookie from BEFORE the round trip (no state in it)
	nothing(a.e.do("GET", call, nil, "Cookie", rt, "User-Agent", "Mozilla/5.0 other")) // copy, another browser
	nothing(a.e.do("GET", call, nil, "Cookie", rt, "X-Forwarded-For", "203.0.113.50")) // copy, another address
	if a.powerCalls() != 1 {
		t.Fatalf("the kept action ran %d times", a.powerCalls())
	}
	// the repeat itself never runs the action again, however often
	for i := 0; i < 5; i++ {
		a.e.do("GET", call, nil, "Cookie", rt)
	}
	if a.powerCalls() != 1 {
		t.Fatalf("the kept action ran %d times after repeats", a.powerCalls())
	}
	if !strings.Contains(auditOutcomes(a.e.s.Cfg.AuditLog), "repeated callback") {
		t.Error("the repeat is not audited")
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
		noCookieSet(t, "a refused repeat", r.hdr)
	}
	t.Run("another browser (User-Agent) with a copy of the cookie", func(t *testing.T) {
		l := lose(t)
		refused(t, l, l.a.e.do("GET", l.call, nil, "Cookie", l.rt, "User-Agent", "curl/8"))
	})
	t.Run("another address with a copy of the cookie", func(t *testing.T) {
		l := lose(t)
		refused(t, l, l.a.e.do("GET", l.call, nil, "Cookie", l.rt, "X-Forwarded-For", "203.0.113.50"))
	})
	t.Run("another code", func(t *testing.T) {
		l := lose(t)
		refused(t, l, l.a.e.do("GET", "/auth/callback?code=another&state="+strings.TrimPrefix(l.call, "/auth/callback?code=c&state="), nil, "Cookie", l.rt))
	})
	t.Run("no code", func(t *testing.T) {
		l := lose(t)
		refused(t, l, l.a.e.do("GET", "/auth/callback?state="+strings.TrimPrefix(l.call, "/auth/callback?code=c&state="), nil, "Cookie", l.rt))
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
		// (it was "refused" while a repeat could be handed that session; the page hands out nothing, so it
		// does not ask whether the session still lives)
		alreadySent(t, "the repeat", l.a.e.do("GET", l.call, nil, "Cookie", l.rt), "/")
		if l.a.powerCalls() != 1 {
			t.Fatalf("shutdown requests %d", l.a.powerCalls())
		}
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
// the machine is not stopped half-way. The reload is answered at once, with the page and without a cookie
// (it used to wait for the first and get its session: R7B-01), and the shutdown is sent once.
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
	var got answer
	select {
	case got = <-second:
	case <-time.After(5 * time.Second):
		close(slow.release)
		t.Fatal("the reload was parked until the first callback had finished")
	}
	if got.err != nil {
		close(slow.release)
		t.Fatal(got.err)
	}
	raw, _ := io.ReadAll(got.r.Body)
	got.r.Body.Close()
	// (audit 6, r4-p1-01: it said "The action was already sent." here, while the request was still at the machine)
	alreadyConfirmed(t, "the reload while the first callback is at work", resp{code: got.r.StatusCode, raw: raw, hdr: got.r.Header}, "/",
		"The action is on its way. It is not sent again. Sign in again to go on.")
	close(slow.release) // the machine answers
	for end := time.Now().Add(10 * time.Second); a.powerCalls() != 1 && time.Now().Before(end); {
		time.Sleep(20 * time.Millisecond)
	}
	slow.mu.Lock()
	aborted := slow.aborted
	slow.mu.Unlock()
	if aborted != 0 || a.powerCalls() != 1 {
		t.Fatalf("the request to the machine was stopped half-way by the reload (%d), or not sent once (%d)", aborted, a.powerCalls())
	}
	if r := e.do("GET", "/api/me", nil, "Cookie", rt); r.code != 401 {
		t.Fatalf("the round trip's cookie is a session: %d", r.code)
	}
	if !strings.Contains(auditOutcomes(e.s.Cfg.AuditLog), "repeated callback of a confirmation that was accepted: no session handed out") {
		t.Error("the repeat is not in the audit log")
	}
}
