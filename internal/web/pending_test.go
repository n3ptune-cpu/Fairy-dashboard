package web

import (
	"strings"
	"testing"
	"time"
)

// armed is a signed-in admin (through the fake identity provider) whose shutdown of "laptop" was refused for a
// step-up and is kept by the panel.
type armed struct {
	e    *env
	idp  *fakeIDP
	clk  *testClock
	sess string // the session cookie
	id   string // the kept action's id
}

// signedIn is an admin who signed in through the fake identity provider an hour ago (on Pocket ID's clock), so
// the next control action needs a step-up.
func signedIn(t *testing.T) *armed {
	t.Helper()
	e := setup(t)
	a := &armed{e: e, idp: newFakeIDP(t), clk: e.fakeClock()}
	e.s.Cfg.OIDC.Issuer = a.idp.srv.URL
	a.idp.claims = map[string]any{"groups": []string{"viewers", "controllers"}, "auth_time": time.Now().Add(-time.Hour).Unix()}
	q, rt := a.idp.login(e, "/auth/login?next=/", "")
	a.sess = sessionCookie(e, e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt).hdr)
	return a
}

func arm(t *testing.T) *armed {
	t.Helper()
	a := signedIn(t)
	a.id = a.shutdown()
	if a.id == "" || a.powerCalls() != 0 {
		t.Fatal("the shutdown must be refused for a step-up and kept")
	}
	return a
}

// shutdown asks for the shutdown and returns the kept action's id ("" when it ran or was not kept).
func (a *armed) shutdown() string {
	name := a.e.do("GET", "/api/systems/laptop", nil, "Cookie", a.sess).body["name"].(string)
	r := a.e.do("POST", "/api/systems/laptop/power", map[string]any{"action": "shutdown", "delay_s": 10, "confirm": name},
		"Cookie", a.sess, "X-Requested-With", "fairy", "Content-Type", "application/json")
	if r.code != 401 {
		return ""
	}
	return pendingID(r)
}

func (a *armed) powerCalls() int {
	a.e.b.Mu.Lock()
	defer a.e.b.Mu.Unlock()
	n := 0
	for _, x := range a.e.b.Requests {
		if strings.HasPrefix(x, "laptop POST /power") {
			n++
		}
	}
	return n
}

// stepUp runs a whole step-up round trip started with the given /auth/login query and returns the callback's
// answer. The identity provider says the user signed in just now (on the server's clock) unless stale.
func (a *armed) stepUp(query string, stale bool) resp {
	a.e.t.Helper()
	q, rt := a.idp.login(a.e, "/auth/login?step_up=1"+query, a.sess)
	at := a.clk.now()
	if stale {
		at = at.Add(-10 * time.Minute)
	}
	a.idp.claims["auth_time"] = at.Unix()
	r := a.e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt)
	if r.code == 303 {
		a.sess = sessionCookie(a.e, r.hdr)
	}
	return r
}

// L1: the kept action runs only for the step-up that names it. A step-up started any other way (a link on
// another site: /auth/login?step_up=1) runs nothing and disarms it, so it cannot run later either.
func TestForeignStepUpLinkRunsNothing(t *testing.T) {
	a := arm(t)
	if r := a.stepUp("&next=/", false); r.code != 303 || a.powerCalls() != 0 {
		t.Fatal("a step-up that does not name the kept action ran it", r.code, a.powerCalls())
	}
	if me := a.e.do("GET", "/api/me", nil, "Cookie", a.sess); me.body["pending_result"] != nil {
		t.Fatal("nothing ran, so there is no result", string(me.raw))
	}
	if r := a.stepUp("&pending="+a.id+"&next=/", false); r.code != 303 || a.powerCalls() != 0 {
		t.Fatal("the foreign step-up must have disarmed the action", r.code, a.powerCalls())
	}

	// a wrong id is the same: nothing runs, and the action is gone
	a = arm(t)
	if r := a.stepUp("&pending=AAAAAAAAAAAAAAAAAAAAAAAA&next=/", false); r.code != 303 || a.powerCalls() != 0 {
		t.Fatal("wrong pending id", r.code, a.powerCalls())
	}
	if got := a.e.s.codec.live.pendingID(sidOf(a.e, a.sess), a.e.s.now()); got != "" {
		t.Fatal("still armed after a step-up for another id")
	}

	// a plain re-login started from the armed session (not a step-up at all) disarms too
	a = arm(t)
	_, _ = a.idp.login(a.e, "/auth/login?next=/", a.sess)
	if got := a.e.s.codec.live.pendingID(sidOf(a.e, a.sess), a.e.s.now()); got != "" {
		t.Fatal("still armed after another sign-in started")
	}
}

// L1: the kept action is single-use on the server. A second copy of the round-trip cookie (or a replay of the
// callback) cannot run it again.
func TestKeptActionRunsOnceEvenWithACookieCopy(t *testing.T) {
	a := arm(t)
	q, rt := a.idp.login(a.e, "/auth/login?step_up=1&pending="+a.id+"&next=/", a.sess)
	a.idp.claims["auth_time"] = a.clk.now().Unix()
	cb := "/auth/callback?code=c&state=" + q.Get("state")
	if r := a.e.do("GET", cb, nil, "Cookie", rt); r.code != 303 || a.powerCalls() != 1 {
		t.Fatal("the named step-up must run the action once", r.code, a.powerCalls())
	}
	_ = a.e.do("GET", cb, nil, "Cookie", rt) // the copy: same cookie, same callback (the fake provider accepts the code again)
	if a.powerCalls() != 1 {
		t.Fatal("an older copy of the cookie ran the kept action again:", a.powerCalls())
	}
}

// L1: a confirmation that fails leaves nothing armed.
func TestFailedConfirmationDisarms(t *testing.T) {
	a := arm(t)
	if r := a.stepUp("&pending="+a.id+"&next=/", true); r.code != 400 || a.powerCalls() != 0 {
		t.Fatal("a stale auth_time must fail the step-up", r.code)
	}
	if r := a.stepUp("&pending="+a.id+"&next=/", false); r.code != 303 || a.powerCalls() != 0 {
		t.Fatal("the failed confirmation must have disarmed the action", r.code, a.powerCalls())
	}

	// a callback that fails before any check of the sign-in (wrong state) disarms too
	a = arm(t)
	_, rt := a.idp.login(a.e, "/auth/login?step_up=1&pending="+a.id+"&next=/", a.sess)
	if r := a.e.do("GET", "/auth/callback?code=c&state=wrong", nil, "Cookie", rt); r.code != 400 {
		t.Fatal(r.code)
	}
	if got := a.e.s.codec.live.pendingID(sidOf(a.e, a.sess), a.e.s.now()); got != "" {
		t.Fatal("still armed after a failed callback")
	}
}

// L1: a kept action is gone after three minutes.
func TestKeptActionExpires(t *testing.T) {
	a := arm(t)
	a.clk.advance(pendingMaxAge - 10*time.Second)
	if r := a.stepUp("&pending="+a.id+"&next=/", false); r.code != 303 || a.powerCalls() != 1 {
		t.Fatal("inside three minutes the action runs", r.code, a.powerCalls())
	}
	a = arm(t)
	a.clk.advance(pendingMaxAge + time.Second)
	if r := a.stepUp("&pending="+a.id+"&next=/", false); r.code != 303 || a.powerCalls() != 0 {
		t.Fatal("after three minutes nothing may run", r.code, a.powerCalls())
	}
}

// L1: a step-up returns to the start page (plus the fragment of the page the dialog was on), nothing else.
func TestStepUpReturnsToTheStartPageOnly(t *testing.T) {
	a := arm(t)
	for next, want := range map[string]string{"/%23/system/laptop": "/#/system/laptop", "/": "/", "/docs": "/", "/api/me": "/",
		"/auth/logout": "/", "//evil.example": "/", "https://evil.example/": "/"} {
		if r := a.stepUp("&next="+next, false); r.code != 303 || r.hdr.Get("Location") != want {
			t.Errorf("next=%s: %d %q, want %q", next, r.code, r.hdr.Get("Location"), want)
		}
	}
	// an ordinary sign-in keeps the wider rule (any same-site path)
	a.idp.claims["auth_time"] = a.clk.now().Unix()
	q, rt := a.idp.login(a.e, "/auth/login?next=/docs", "")
	if r := a.e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt); r.hdr.Get("Location") != "/docs" {
		t.Fatal(r.hdr.Get("Location"))
	}
}

// L1: the kept action is in memory only. A restarted panel (a new Server on the same data) has forgotten it.
func TestKeptActionIsNotInTheCookie(t *testing.T) {
	a := arm(t)
	req := a.e.do("GET", "/api/me", nil, "Cookie", a.sess)
	if req.code != 200 || strings.Contains(a.sess, a.id) {
		t.Fatal("cookie carries the pending id")
	}
	a.e.s.codec.live = newLiveSessions() // what a restart leaves
	if r := a.stepUp("&pending="+a.id+"&next=/", false); r.code != 303 || a.powerCalls() != 0 {
		t.Fatal("after a restart nothing may run", r.code, a.powerCalls())
	}
}
