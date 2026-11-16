package web

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The user name (preferred_username) is what the panel names a person by. Userinfo was asked only when the
// groups were missing from the ID token, so a provider that puts the groups into the ID token and the user name
// into userinfo alone gave the panel no user name: everybody was "(address hidden)". Userinfo is asked now also
// when the ID token has no user name (and the scope "profile" is asked for). What comes from it is a text that
// is shown, and nothing else: these tests hold the user name, and what must not change with it.

// signInWith signs the account u1 in at a provider whose ID token and userinfo answer are these.
func signInWith(t *testing.T, idToken, userinfo map[string]any) (e *env, idp *fakeIDP, r resp) {
	t.Helper()
	e = setup(t)
	idp = newFakeIDP(t)
	e.s.Cfg.OIDC.Issuer = idp.srv.URL
	idp.claims, idp.userinfo = idToken, userinfo
	q, rt := idp.login(e, "/auth/login?next=/", "")
	return e, idp, e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt)
}

// The groups are in the ID token, the user name only in userinfo: the account has the user name, cleaned like
// one from the ID token (no escape sequence, no zero width space), in its session and in users.json. Nothing
// else of that answer is used: the groups stay the ID token's, the address is not verified by userinfo's word,
// and the subject is the ID token's.
func TestUsernameComesFromUserinfoWhenTheIDTokenHasNone(t *testing.T) {
	e, idp, r := signInWith(t,
		map[string]any{"groups": []string{"viewers"}, "preferred_username": nil, "email": "n@example.com", "email_verified": false},
		map[string]any{"sub": "u1", "preferred_username": " Juni​per\x1b[31m\n", "groups": []string{"viewers", "controllers"}, "email": "other@example.com", "email_verified": true,
			"auth_time": time.Now().Add(time.Hour).Unix(), "name": "Real Name"})
	if r.code != 303 {
		t.Fatalf("callback: %d %s", r.code, r.raw)
	}
	me := e.do("GET", "/api/me", nil, "Cookie", sessionCookie(e, r.hdr))
	if me.body["preferred_username"] != "Juniper" || me.body["shown_as"] != "Juniper" {
		t.Errorf("/api/me: preferred_username %q, shown_as %q; want the user name of userinfo, cleaned", me.body["preferred_username"], me.body["shown_as"])
	}
	if u := e.s.Access.Load().Users["u1"]; u.Username != "Juniper" || u.Email != "n@example.com" || u.Verified() {
		t.Errorf("users.json: %+v", u)
	}
	if gs := fmt.Sprint(me.body["groups"]); gs != "[viewers]" {
		t.Errorf("the groups are %s: userinfo's replaced the ID token's", gs)
	}
	if me.body["sub"] != "u1" || me.body["email"] != "n@example.com" || me.body["email_verified"] == true || me.body["name"] != "Real Name" || me.body["can_control"] == true {
		t.Errorf("/api/me: %s", me.raw)
	}
	if idp.userinfoAsked != 1 {
		t.Errorf("userinfo was asked %d times, want once", idp.userinfoAsked)
	}
}

// An ID token with an empty user name has none: userinfo's is taken.
func TestAnEmptyUsernameInTheIDTokenIsNone(t *testing.T) {
	e, _, r := signInWith(t, map[string]any{"groups": []string{"viewers"}, "preferred_username": ""}, map[string]any{"sub": "u1", "preferred_username": "Juniper"})
	if r.code != 303 || e.s.Access.Load().Users["u1"].Username != "Juniper" {
		t.Errorf("callback %d, users.json %+v", r.code, e.s.Access.Load().Users["u1"])
	}
}

// Userinfo is not asked when the ID token has what the panel needs, and not for a user name the panel did not
// ask the provider for (no scope "profile"). The ID token's user name is never replaced.
func TestUserinfoIsAskedOnlyForWhatIsMissing(t *testing.T) {
	for _, c := range []struct {
		name     string
		id       map[string]any
		scopes   []string
		asked    int
		username string
	}{
		{"groups and user name in the ID token", map[string]any{"groups": []string{"viewers"}, "preferred_username": "from-the-token"}, nil, 0, "from-the-token"},
		{"no user name in the ID token", map[string]any{"groups": []string{"viewers"}, "preferred_username": nil}, nil, 1, "from-userinfo"},
		{"no groups in the ID token", map[string]any{"preferred_username": "from-the-token"}, nil, 1, "from-the-token"},
		{"neither", map[string]any{"preferred_username": nil}, nil, 1, "from-userinfo"},
		{"no user name, and the scope profile is not asked for", map[string]any{"groups": []string{"viewers"}, "preferred_username": nil}, []string{"openid", "email", "groups"}, 0, ""},
		{"no groups, and the scope groups is not asked for", map[string]any{"groups": nil, "preferred_username": "from-the-token"}, []string{"openid", "profile", "email"}, 0, "from-the-token"},
	} {
		e := setup(t)
		idp := newFakeIDP(t)
		e.s.Cfg.OIDC.Issuer = idp.srv.URL
		e.s.Cfg.OIDC.AllowedGroups = nil
		if c.scopes != nil {
			e.s.Cfg.OIDC.Scopes = c.scopes
		}
		idp.claims = c.id
		idp.userinfo = map[string]any{"sub": "u1", "preferred_username": "from-userinfo", "groups": []string{"viewers"}}
		q, rt := idp.login(e, "/auth/login?next=/", "")
		r := e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt)
		if r.code != 303 || idp.userinfoAsked != c.asked || e.s.Access.Load().Users["u1"].Username != c.username {
			t.Errorf("%s: callback %d, userinfo asked %d times (want %d), user name %q (want %q)", c.name, r.code, idp.userinfoAsked, c.asked, e.s.Access.Load().Users["u1"].Username, c.username)
		}
	}
}

// A userinfo request that is made for the user name alone and fails does not fail the sign-in: the account
// signs in as it did before the panel asked, without a user name, and the log says so without the account's
// address. (When the groups have to come from userinfo a failure refuses the sign-in, as before:
// TestGroupsFromUserinfoThatCannotBeRead and the tests of refused_account_test.go.)
func TestUserinfoForTheUsernameAloneMayFail(t *testing.T) {
	for _, how := range []string{"500", "garbage", "cut", "404", "no subject"} {
		logs := captureLog(t)
		e := setup(t)
		idp := newFakeIDP(t)
		e.s.Cfg.OIDC.Issuer = idp.srv.URL
		idp.claims = map[string]any{"groups": []string{"viewers"}, "preferred_username": nil, "email": "n@example.com", "email_verified": true}
		switch how {
		case "404": // (no userinfo answer set)
		case "no subject":
			idp.userinfo = map[string]any{"preferred_username": "Juniper"}
		default:
			idp.userinfo, idp.userinfoFail = map[string]any{"sub": "u1", "preferred_username": "Juniper"}, how
		}
		q, rt := idp.login(e, "/auth/login?next=/", "")
		r := e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt)
		if r.code != 303 {
			t.Errorf("userinfo %s: callback %d, want the sign-in to go on: %s", how, r.code, r.raw)
			continue
		}
		me := e.do("GET", "/api/me", nil, "Cookie", sessionCookie(e, r.hdr))
		if me.code != 200 || me.body["preferred_username"] != nil || me.body["shown_as"] != "(address hidden)" || fmt.Sprint(me.body["groups"]) != "[viewers]" {
			t.Errorf("userinfo %s: /api/me %d %s", how, me.code, me.raw)
		}
		if idp.userinfoAsked == 0 {
			t.Errorf("userinfo %s: not asked", how)
		}
		out := logs.take()
		if !strings.Contains(out, "OIDC userinfo") || !strings.Contains(out, "the sign-in goes on without a user name") || !strings.Contains(out, "sub=u1") {
			t.Errorf("userinfo %s: the log does not say what happened:\n%s", how, out)
		}
		if strings.Contains(out, "n@example.com") || strings.Contains(out, "userinfo is down") || strings.Contains(out, "not json") {
			t.Errorf("userinfo %s: the log has the account's address or the answer's body:\n%s", how, out)
		}
	}
}

// A userinfo answer about another account is refused whatever it was asked for: nothing of it is used, the
// subject stays the ID token's, and the sign-in does not go on.
func TestUserinfoAboutAnotherAccountIsRefusedAlsoForTheUsername(t *testing.T) {
	e, idp, r := signInWith(t, map[string]any{"groups": []string{"viewers"}, "preferred_username": nil},
		map[string]any{"sub": "someone-else", "preferred_username": "Juniper"})
	if r.code != 502 || cookieAfter(e, "", r.hdr) != "" {
		t.Errorf("userinfo about another account: callback %d, want 502 and no session", r.code)
	}
	if u, ok := e.s.Access.Load().Users["someone-else"]; ok {
		t.Errorf("the account of the userinfo answer was recorded: %+v", u)
	}
	if u, ok := e.s.Access.Load().Users["u1"]; ok && u.Username != "" {
		t.Errorf("the user name of another account's answer was kept: %+v", u)
	}
	if idp.userinfoAsked != 1 {
		t.Errorf("userinfo asked %d times", idp.userinfoAsked)
	}
}

// The freshness of a "Confirm it's you" is the ID token's word also when userinfo is asked for the user name:
// the ID token says "signed in ten minutes ago" and has the groups; userinfo brings the user name and an
// auth_time of this second.
func TestUserinfoForTheUsernameDoesNotMakeAConfirmationFresh(t *testing.T) {
	a := arm(t)
	q, rt := a.idp.confirmStart(a.e, a.id, "/", a.sess)
	a.idp.mu.Lock()
	a.idp.claims = map[string]any{"auth_time": a.clk.now().Add(-10 * time.Minute).Unix(), "groups": []string{"viewers", "controllers"}, "preferred_username": nil}
	a.idp.userinfo = map[string]any{"sub": "u1", "preferred_username": "Juniper", "auth_time": a.clk.now().Unix()}
	a.idp.mu.Unlock()
	cb := a.e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt)
	if cb.code != 400 || !strings.Contains(string(cb.raw), "Pocket ID did not ask you to sign in again") || a.powerCalls() != 0 {
		t.Errorf("an old auth_time in the ID token and a new one in userinfo: callback %d, the kept shutdown was sent %d time(s); want 400 and none", cb.code, a.powerCalls())
	}
}

// What the ID token says about the groups is said, also when userinfo, asked for the user name, does not
// answer: an account whose ID token has groups and none that is allowed is refused as "not in an allowed
// group" (403, and its sessions end), not as "the groups could not be read" (502, nothing ended). Userinfo was
// not asked for the groups.
func TestUserinfoForTheUsernameAloneThatFailsDoesNotUndecideTheGroups(t *testing.T) {
	for _, how := range []string{"500", "404"} {
		e := setup(t)
		idp := newFakeIDP(t)
		e.s.Cfg.OIDC.Issuer = idp.srv.URL
		idp.claims = map[string]any{"groups": []string{"strangers"}, "preferred_username": nil}
		if how != "404" {
			idp.userinfo, idp.userinfoFail = map[string]any{"sub": "u1", "preferred_username": "Juniper", "groups": []string{"viewers"}}, how
		}
		q, rt := idp.login(e, "/auth/login?next=/", "")
		r := e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt)
		outcome := ""
		for _, a := range e.auditEntries("", "auth.login") {
			outcome = fmt.Sprint(a["outcome"])
		}
		if r.code != 403 || outcome != "denied: not in an allowed group" || idp.userinfoAsked != 1 {
			t.Errorf("userinfo %s, the ID token's groups are not allowed: callback %d, audit %q, userinfo asked %d times; want 403 and the denial", how, r.code, outcome, idp.userinfoAsked)
		}
	}
	// ... and userinfo's groups do not let such an account in when it does answer
	e, _, r := signInWith(t, map[string]any{"groups": []string{"strangers"}, "preferred_username": nil}, map[string]any{"sub": "u1", "preferred_username": "Juniper", "groups": []string{"viewers", "controllers"}})
	if r.code != 403 {
		t.Errorf("the ID token's groups are not allowed, userinfo's are: callback %d, want 403", r.code)
	}
	if _, ok := e.s.Access.Load().Users["u1"]; ok {
		t.Error("an account that was refused is in users.json")
	}
}
