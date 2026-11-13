package web

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Audit 6, r4-p1-02. When the groups are read from userinfo, every claim of the userinfo answer replaced the ID
// token's: auth_time, email_verified, everything but sub. The ID token is signed and made for this sign-in;
// userinfo is an unsigned answer. Only what userinfo is asked for is taken now: the groups, and the texts that
// are shown where the ID token has none.

// The freshness of a "Confirm it's you" comes from the ID token alone. The ID token says "signed in ten minutes
// ago" (Pocket ID did not ask again); userinfo brings the groups and an auth_time of this second.
func TestUserinfoDoesNotMakeAConfirmationFresh(t *testing.T) {
	a := arm(t)
	q, rt := a.idp.confirmStart(a.e, a.id, "/", a.sess)
	a.idp.mu.Lock()
	a.idp.claims = map[string]any{"auth_time": a.clk.now().Add(-10 * time.Minute).Unix()}
	a.idp.userinfo = map[string]any{"sub": "u1", "groups": []string{"viewers", "controllers"}, "auth_time": a.clk.now().Unix()}
	a.idp.mu.Unlock()
	cb := a.e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt)
	if cb.code != 400 || !strings.Contains(string(cb.raw), "Pocket ID did not ask you to sign in again") || a.powerCalls() != 0 {
		t.Errorf("an old auth_time in the ID token and a new one in userinfo: callback %d, the kept shutdown was sent %d time(s); want 400 and none\n%s",
			cb.code, a.powerCalls(), cb.raw)
	}
	// the control: the ID token itself says "just now", the groups come from userinfo: accepted, the action runs
	b := arm(t)
	q, rt = b.idp.confirmStart(b.e, b.id, "/", b.sess)
	b.idp.mu.Lock()
	b.idp.claims = map[string]any{"auth_time": b.clk.now().Unix()}
	b.idp.userinfo = map[string]any{"sub": "u1", "groups": []string{"viewers", "controllers"}, "auth_time": b.clk.now().Add(-time.Hour).Unix()}
	b.idp.mu.Unlock()
	if cb := b.e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt); cb.code != 303 || b.powerCalls() != 1 {
		t.Errorf("a new auth_time in the ID token (and an old one in userinfo): callback %d, shutdown requests %d; want 303 and 1", cb.code, b.powerCalls())
	}
}

// Whether an e-mail address counts (allowed_emails) is the ID token's word. Userinfo's email_verified, and an
// address that only userinfo gives, do not let an account in.
func TestUserinfoDoesNotVerifyAnEmailAddress(t *testing.T) {
	for _, c := range []struct {
		name     string
		id, info map[string]any
		want     int
	}{
		{"the ID token says the address is not verified, userinfo says it is",
			map[string]any{"email": "ann@example.com", "email_verified": false}, map[string]any{"email_verified": true}, 403},
		{"the address is only in userinfo, with email_verified",
			map[string]any{"email": nil}, map[string]any{"email": "ann@example.com", "email_verified": true}, 403},
		{"the ID token has no address but says verified, the address is only in userinfo",
			map[string]any{"email": nil, "email_verified": true}, map[string]any{"email": "ann@example.com"}, 403},
		{"the control: the ID token has the verified address, userinfo the groups",
			map[string]any{"email": "ann@example.com", "email_verified": true}, map[string]any{"email": "eve@example.com", "email_verified": false}, 303},
	} {
		e := setup(t)
		idp := newFakeIDP(t)
		e.s.Cfg.OIDC.Issuer = idp.srv.URL
		e.s.Cfg.OIDC.AllowedEmails = []string{"ann@example.com"}
		idp.claims = c.id // no groups in the ID token: userinfo is asked
		idp.userinfo = map[string]any{"sub": "u1", "groups": []string{"viewers"}}
		for k, v := range c.info {
			idp.userinfo[k] = v
		}
		q, rt := idp.login(e, "/auth/login?next=/", "")
		if r := e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt); r.code != c.want {
			t.Errorf("%s: callback %d, want %d", c.name, r.code, c.want)
		}
	}
}

// What is shown may come from userinfo, where the ID token has none; what the ID token says stays.
func TestUserinfoGivesTheGroupsAndTheShownTexts(t *testing.T) {
	e := setup(t)
	idp := newFakeIDP(t)
	e.s.Cfg.OIDC.Issuer = idp.srv.URL
	idp.claims = map[string]any{"name": "Ann from the ID token", "email": nil} // no address in the ID token
	idp.userinfo = map[string]any{"sub": "u1", "groups": []string{"viewers"}, "name": "Ann from userinfo", "email": "ann@example.com", "email_verified": true,
		"preferred_username": "ann"}
	q, rt := idp.login(e, "/auth/login?next=/", "")
	r := e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt)
	if r.code != 303 {
		t.Fatalf("callback: %d %s", r.code, r.raw)
	}
	me := e.do("GET", "/api/me", nil, "Cookie", sessionCookie(e, r.hdr))
	if me.body["name"] != "Ann from the ID token" || me.body["email"] != "ann@example.com" || me.body["preferred_username"] != "ann" || me.body["email_verified"] == true {
		t.Errorf("/api/me: name %v, email %v, preferred_username %v, email_verified %v; want the ID token's name, userinfo's address (not verified) and user name",
			me.body["name"], me.body["email"], me.body["preferred_username"], me.body["email_verified"])
	}
	if gs, _ := me.body["groups"].([]any); len(gs) != 1 || gs[0] != "viewers" {
		t.Errorf("/api/me: groups %v, want the ones from userinfo", me.body["groups"])
	}
}

// A userinfo answer about another account: nothing of it is used, and the sign-in is refused also for an
// account that needs no group (here: no allowed groups are set, so every account of the client may sign in).
// It used to be ignored, and that account signed in.
func TestUserinfoAboutAnotherAccountRefusesTheSignIn(t *testing.T) {
	e := setup(t)
	idp := newFakeIDP(t)
	e.s.Cfg.OIDC.Issuer = idp.srv.URL
	e.s.Cfg.OIDC.AllowedGroups = nil
	idp.claims = map[string]any{}
	idp.userinfo = map[string]any{"sub": "someone-else", "groups": []string{"viewers", "controllers"}}
	q, rt := idp.login(e, "/auth/login?next=/", "")
	r := e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt)
	if r.code != 502 || cookieAfter(e, rt, r.hdr) != "" {
		t.Errorf("userinfo about another account, no allowed groups set: callback %d, a session cookie: %v; want 502 and none", r.code, cookieAfter(e, rt, r.hdr) != "")
	}
	// the audit entry says what happened (review of fix8, RF8-10: it said "the groups could not be read from
	// Pocket ID (userinfo)", which is another failure)
	outcome := ""
	for _, a := range e.auditEntries("", "auth.login") {
		outcome = fmt.Sprint(a["outcome"])
	}
	if outcome != "failed: the sign-in answer of Pocket ID (userinfo) was for another account; not decided, no session ended" {
		t.Errorf("the audit entry of a userinfo answer about another account: %q", outcome)
	}
	// the control: userinfo about this account
	idp.userinfo = map[string]any{"sub": "u1", "groups": []string{"viewers"}}
	q, rt = idp.login(e, "/auth/login?next=/", "")
	if r := e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt); r.code != 303 {
		t.Errorf("userinfo about this account: callback %d, want 303", r.code)
	}
}
