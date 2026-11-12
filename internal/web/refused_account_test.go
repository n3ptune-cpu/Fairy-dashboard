package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// r3-p1-12 (audit 5, round 3; the auditor's TestR3P1_12). At a "Confirm it's you" Pocket ID says, with a
// verified ID token, that the account is not in an allowed group any more. The panel answered "not allowed" and
// cleared this browser's cookie, but the session stayed registered as admin: the cookie as it was before (a
// copy, the other tab) and the account's other sessions went on working. Now the account's sessions end on the
// server at that moment, and the audit entry says how many.
func TestNotAllowedAtAConfirmationEndsTheAccountsSessions(t *testing.T) {
	a := arm(t)
	before := a.sess                                                                                // the admin's cookie, as any copy of it
	second := a.e.staleCookie(model.User{Sub: "u1", Groups: []string{"viewers", "controllers"}}, 0) // the same account, another browser
	other := a.e.cookie(model.User{Sub: "u7", Name: model.P("Vee"), Groups: []string{"viewers"}}, 0)
	for name, ck := range map[string]string{"the session": before, "the account's other session": second, "another account's session": other} {
		if r := a.e.do("GET", "/api/me", nil, "Cookie", ck); r.code != 200 {
			t.Fatalf("setup: %s must work: %d", name, r.code)
		}
	}
	q, rt := a.idp.confirmStart(a.e, a.id, "/", a.sess)
	a.idp.claims = map[string]any{"groups": []string{"strangers"}, "auth_time": a.clk.now().Unix()} // taken out of the allowed group
	cb := a.e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt)
	if cb.code != 403 || a.powerCalls() != 0 {
		t.Fatalf("callback: %d %s; shutdown requests: %d", cb.code, cb.raw, a.powerCalls())
	}
	if cookieAfter(a.e, rt, cb.hdr) != "" {
		t.Error("the browser's cookie must be cleared, as before")
	}
	for name, ck := range map[string]string{"the session's cookie as it was before": before, "the round trip's cookie": rt, "the account's other session": second} {
		if me := a.e.do("GET", "/api/me", nil, "Cookie", ck); me.code != 401 {
			t.Errorf("%s still works after \"not allowed\": /api/me %d (role %v)", name, me.code, me.body["role"])
		}
		if sys := a.e.do("GET", "/api/systems", nil, "Cookie", ck); sys.code != 401 {
			t.Errorf("%s still reads the systems after \"not allowed\": %d", name, sys.code)
		}
	}
	if r := a.e.do("GET", "/api/me", nil, "Cookie", other); r.code != 200 {
		t.Errorf("another account's session must not be touched: %d", r.code)
	}
	var entry map[string]any
	for _, x := range a.e.auditEntries("u1", "auth.login") {
		if x["outcome"] == "denied: not in an allowed group" {
			entry = x
		}
	}
	if p, _ := entry["params"].(map[string]any); entry == nil || p["sessions_revoked"] != float64(2) {
		t.Errorf("the audit entry must say that the account's two sessions were ended: %v", entry)
	}
}

// The same refusal at a plain sign-in. With no session in the browser nothing is ended; in a browser that holds
// somebody else's session (a shared PC), that one session ends with its cookie, and nothing else of that person.
func TestNotAllowedAtASignInEndsOnlyWhatTheBrowserHeld(t *testing.T) {
	e := setup(t)
	idp := newFakeIDP(t)
	e.s.Cfg.OIDC.Issuer = idp.srv.URL
	vee := model.User{Sub: "u7", Name: model.P("Vee"), Groups: []string{"viewers"}}
	here, elsewhere := e.cookie(vee, 0), e.cookie(vee, 0)
	idp.claims = map[string]any{"groups": []string{"strangers"}, "auth_time": e.s.now().Unix()} // account u1, in no allowed group

	q, rt := idp.login(e, "/auth/login?next=/", "") // a browser without a session
	if cb := e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt); cb.code != 403 {
		t.Fatalf("callback: %d %s", cb.code, cb.raw)
	}
	for _, ck := range []string{here, elsewhere} {
		if r := e.do("GET", "/api/me", nil, "Cookie", ck); r.code != 200 {
			t.Fatalf("a refused sign-in in another browser ended a session: %d", r.code)
		}
	}
	for _, x := range e.auditEntries("u1", "auth.login") {
		if p, _ := x["params"].(map[string]any); p["sessions_revoked"] != nil {
			t.Errorf("nothing was ended, the entry must not say otherwise: %v", x)
		}
	}

	q, rt = idp.login(e, "/auth/login?next=/", here) // the shared browser: Vee's session is in it
	cb := e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt)
	if cb.code != 403 || cookieAfter(e, rt, cb.hdr) != "" {
		t.Fatalf("callback: %d %s", cb.code, cb.raw)
	}
	if r := e.do("GET", "/api/me", nil, "Cookie", here); r.code != 401 {
		t.Errorf("the session whose cookie the answer cleared is still valid on the server: %d", r.code)
	}
	if r := e.do("GET", "/api/me", nil, "Cookie", elsewhere); r.code != 200 {
		t.Errorf("Vee's session in another browser must not be touched: %d", r.code)
	}
}

// R7B-04. Pocket ID does not put the groups into the ID token, so the panel asks userinfo for them at every
// sign-in and confirmation. When that request FAILS (userinfo answers 500, something that is not JSON, 404,
// the connection breaks, or an answer about another subject), the account had no groups, counted as "not
// allowed", and since r3-p1-12 that ended every session of the account. A failed request is "could not
// check": the confirmation fails with a page that says so and this browser's cookie is cleared, as before
// r3-p1-12, but nothing is ended on the server. Only an answer that was read and really lacks the allowed
// group ends the account's sessions.
func TestGroupsThatCouldNotBeReadEndNoSession(t *testing.T) {
	for _, how := range []string{"500", "garbage", "cut", "404", "another subject"} {
		t.Run("userinfo: "+how, func(t *testing.T) {
			a := arm(t)
			before := a.sess
			second := a.e.staleCookie(model.User{Sub: "u1", Groups: []string{"viewers", "controllers"}}, 0) // the same account, another browser
			q, rt := a.idp.confirmStart(a.e, a.id, "/#/system/laptop", a.sess)
			a.idp.mu.Lock()
			a.idp.claims = map[string]any{"auth_time": a.clk.now().Unix()} // no groups in the ID token: they come from userinfo
			switch how {
			case "404":
			case "another subject":
				a.idp.userinfo = map[string]any{"sub": "someone-else", "groups": []string{"viewers", "controllers"}}
			default:
				a.idp.userinfoFail = how
			}
			a.idp.mu.Unlock()
			cb := a.e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt)
			refusalPage(t, "the confirmation", cb, 502, cbConfirm, cbNoGroups, "/#/system/laptop")
			if a.powerCalls() != 0 {
				t.Fatalf("the kept shutdown ran: %d", a.powerCalls())
			}
			if cookieAfter(a.e, rt, cb.hdr) != "" {
				t.Error("this browser's cookie must be cleared, as it was before r3-p1-12")
			}
			for name, ck := range map[string]string{"the session as it was before": before, "the account's other session": second} {
				if me := a.e.do("GET", "/api/me", nil, "Cookie", ck); me.code != 200 {
					t.Errorf("R7B-04: %s was ended because userinfo could not be read: /api/me %d", name, me.code)
				}
			}
			all, _ := a.e.s.Poller.Admin.Sessions()
			if len(all) != 2 {
				t.Errorf("R7B-04: %d sessions are registered, want the 2 there were", len(all))
			}
			var entry map[string]any
			for _, x := range a.e.auditEntries("u1", "auth.login") {
				if strings.HasPrefix(fmt.Sprint(x["outcome"]), "failed: the groups could not be read from Pocket ID (userinfo)") {
					entry = x
				}
				if strings.HasPrefix(fmt.Sprint(x["outcome"]), "denied") {
					t.Errorf("the audit log says the account was refused: %v", x)
				}
			}
			if p, _ := entry["params"].(map[string]any); entry == nil || p["sessions_revoked"] != nil {
				t.Errorf("the audit entry must say that the groups could not be read, and name no ended session: %v", entry)
			}
			// the same at a plain sign-in: a page, no session
			a.idp.mu.Lock()
			a.idp.claims = map[string]any{"auth_time": a.clk.now().Unix()}
			a.idp.mu.Unlock()
			q, rt = a.idp.login(a.e, "/auth/login?next=/", "")
			cb = a.e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt)
			refusalPage(t, "the sign-in", cb, 502, cbSignIn, cbNoGroups, "/")
			if cookieAfter(a.e, rt, cb.hdr) != "" {
				t.Error("a sign-in that could not be checked must leave no cookie")
			}
		})
	}

	// userinfo answers, about this account, and the allowed group is not in it: that IS "not allowed", and it
	// ends the account's sessions (r3-p1-12 holds for groups from userinfo too).
	for _, groups := range []any{[]string{"strangers"}, []string{}, nil} {
		a := arm(t)
		before := a.sess
		second := a.e.staleCookie(model.User{Sub: "u1", Groups: []string{"viewers", "controllers"}}, 0)
		q, rt := a.idp.confirmStart(a.e, a.id, "/", a.sess)
		a.idp.mu.Lock()
		a.idp.claims = map[string]any{"auth_time": a.clk.now().Unix()}
		a.idp.userinfo = map[string]any{"sub": "u1"}
		if groups != nil {
			a.idp.userinfo["groups"] = groups
		}
		a.idp.mu.Unlock()
		cb := a.e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt)
		refusalPage(t, "the confirmation", cb, 403, cbNotAllowed, cbRefused, "/")
		if a.powerCalls() != 0 || cookieAfter(a.e, rt, cb.hdr) != "" {
			t.Fatalf("userinfo groups %v: shutdown requests %d, or the cookie was not cleared", groups, a.powerCalls())
		}
		for name, ck := range map[string]string{"the session as it was before": before, "the account's other session": second} {
			if me := a.e.do("GET", "/api/me", nil, "Cookie", ck); me.code != 401 {
				t.Errorf("userinfo groups %v: %s still works after \"not allowed\": /api/me %d", groups, name, me.code)
			}
		}
		var entry map[string]any
		for _, x := range a.e.auditEntries("u1", "auth.login") {
			if x["outcome"] == "denied: not in an allowed group" {
				entry = x
			}
		}
		if p, _ := entry["params"].(map[string]any); entry == nil || p["sessions_revoked"] != float64(2) {
			t.Errorf("userinfo groups %v: the audit entry must say that the account's two sessions were ended: %v", groups, entry)
		}
	}
}

// cookieAfter applies an answer's Set-Cookie headers to the one session cookie a browser holds ("" = none).
func cookieAfter(e *env, have string, h http.Header) string {
	for _, ck := range (&http.Response{Header: h}).Cookies() {
		if ck.Name != e.s.codec.name() {
			continue
		}
		if ck.MaxAge < 0 || ck.Value == "" {
			have = ""
		} else {
			have = ck.Name + "=" + ck.Value
		}
	}
	return have
}
