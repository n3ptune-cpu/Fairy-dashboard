package web

import (
	"crypto/sha256"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// NEW-1. A kept action belongs to the client that asked for it. These tests are the six properties of the fix:
// (a) bound to the creating client and released only by that client's step-up, (b) never named by a link,
// (c) nothing is kept for a client the session was not established for, (d) the confirmation names the action,
// (e) the same in step_up_per_action mode, (f) the legitimate flow still works.

// thief is a copy of the session cookie on another address and in another browser.
var thief = append(append([]string{}, otherAddr...), otherBrowser...)

func (a *armed) asThief(method, path string, body any, who []string) resp {
	return a.e.do(method, path, body, append([]string{"Cookie", a.sess, "X-Requested-With", "fairy",
		"Content-Type", "application/json"}, who...)...)
}

func (a *armed) shutdownBody() map[string]any {
	name := a.e.do("GET", "/api/systems/laptop", nil, "Cookie", a.sess).body["name"].(string)
	return map[string]any{"action": "shutdown", "delay_s": 10, "confirm": name}
}

// (c), (b), (e): the reviewer's three attacks. The thief's refused request is not kept, it gets no id, and
// whatever sign-in the admin then completes (the old link form with any id, a plain step-up link, a link from
// another site) runs nothing. Proven for a shutdown, for making the thief's own account admin, and in
// per-action mode; and for a copy that differs from the admin's client in the address only or the browser only.
func TestCookieCopyCannotPlantAKeptAction(t *testing.T) {
	for _, perAction := range []bool{false, true} {
		for who, hdr := range map[string][]string{"another address and browser": thief, "another address": otherAddr, "another browser": otherBrowser} {
			a := signedIn(t)
			a.e.s.Cfg.StepUpPerAction = perAction
			a.e.signIn(model.User{Sub: "mallory", Name: model.P("Mallory"), Email: model.P("m@example.com"), Groups: []string{"viewers"}})
			for _, req := range []struct {
				method, path string
				body         any
			}{
				{"POST", "/api/systems/laptop/power", a.shutdownBody()},
				{"PUT", "/api/admin/users/mallory", map[string]any{"role": "admin"}},
			} {
				r := a.asThief(req.method, req.path, req.body, hdr)
				d, _ := r.body["detail"].(map[string]any)
				if !stepUpRequired(r) || d["pending"] != false || d["pending_id"] != nil || d["action"] != nil || d["not_kept"] != notKeptClient {
					t.Fatalf("%s, per-action %v: the copy's refused request: %d %s", who, perAction, r.code, r.raw)
				}
				if id := keptID(a.e, a.sess); id != "" {
					t.Fatalf("%s: the panel kept the copy's request (%s)", who, id)
				}
			}
			// the admin follows a link, as the attack had it: with an id, from another site; and completes the sign-in
			for _, link := range []string{"&pending=AAAAAAAAAAAAAAAAAAAAAAAA&next=/", "&next=/"} {
				lr := a.e.do("GET", "/auth/login?step_up=1"+link, nil, "Cookie", a.sess, "Sec-Fetch-Site", "cross-site",
					"Sec-Fetch-Mode", "navigate", "Referer", "https://evil.example/")
				if lr.code != 303 {
					t.Fatal(lr.code, string(lr.raw))
				}
				if r := a.stepUp(link, false); r.code != 303 {
					t.Fatal(r.code, string(r.raw))
				}
			}
			if n := a.powerCalls(); n != 0 {
				t.Fatalf("%s, per-action %v: the planted shutdown ran (%d)", who, perAction, n)
			}
			if role := a.e.s.Access.Load().Users["mallory"].Role; role != access.RolePending {
				t.Fatalf("%s, per-action %v: the planted role change ran: mallory is %q", who, perAction, role)
			}
			if me := a.e.do("GET", "/api/me", nil, "Cookie", a.sess); me.body["pending_result"] != nil {
				t.Fatal("a result of something that must not have run", string(me.raw))
			}
		}
	}
}

// (c): the copy's refused request also leaves alone what the session's own client has waiting, so the admin
// confirms the action the dialog shows, not one that replaced it.
func TestCookieCopyDoesNotReplaceTheAdminsKeptAction(t *testing.T) {
	a := arm(t) // the admin's shutdown waits
	a.e.signIn(model.User{Sub: "mallory", Name: model.P("Mallory"), Email: model.P("m@example.com"), Groups: []string{"viewers"}})
	if r := a.asThief("PUT", "/api/admin/users/mallory", map[string]any{"role": "admin"}, thief); !stepUpRequired(r) {
		t.Fatal(r.code, string(r.raw))
	}
	if got := keptID(a.e, a.sess); got != a.id {
		t.Fatalf("the admin's kept action changed: %q, was %q", got, a.id)
	}
	if r := a.confirm(a.id, false); r.code != 303 || a.powerCalls() != 1 {
		t.Fatal("the admin's own action must run", r.code, a.powerCalls())
	}
	if role := a.e.s.Access.Load().Users["mallory"].Role; role != access.RolePending {
		t.Fatal("mallory is", role)
	}
}

// (a): the kept action can be confirmed only by the client that asked for it. A copy of the cookie that knows
// the id is refused and the action is forgotten; and a confirmation that the right client started but that
// comes back from another client runs nothing.
func TestKeptActionIsBoundToTheClientThatAskedForIt(t *testing.T) {
	for who, hdr := range map[string][]string{"another address and browser": thief, "another address": otherAddr, "another browser": otherBrowser} {
		a := arm(t)
		if _, _, r := a.idp.tryConfirmStart(a.e, a.id, "/", a.sess, hdr...); r.code != 409 {
			t.Fatalf("%s confirmed the admin's kept action: %d %s", who, r.code, r.raw)
		}
		if keptID(a.e, a.sess) != "" {
			t.Fatalf("%s: asking for it from another client must forget it", who)
		}
		if r := a.confirm(a.id, false); r.code != 409 || a.powerCalls() != 0 {
			t.Fatal("forgotten means forgotten", r.code, a.powerCalls())
		}

		// started by the right client, finished by another one (it holds a copy of the round-trip cookie)
		a = arm(t)
		q, rt := a.idp.confirmStart(a.e, a.id, "/", a.sess)
		a.idp.claims["auth_time"] = a.clk.now().Unix()
		r := a.e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, append([]string{"Cookie", rt}, hdr...)...)
		if r.code != 303 || a.powerCalls() != 0 {
			t.Fatalf("%s came back with the confirmation and the action ran: %d, %d calls", who, r.code, a.powerCalls())
		}
		if !strings.Contains(auditOutcomes(a.e.s.Cfg.AuditLog), "the action that waited was not run") {
			t.Fatal("the audit log must say that the kept action was not run:\n" + auditOutcomes(a.e.s.Cfg.AuditLog))
		}
		if keptID(a.e, sessionCookie(a.e, r.hdr)) != "" || keptID(a.e, a.sess) != "" {
			t.Fatal("still kept")
		}
	}
}

// (a), on the store itself: every comparison counts. With any one of them removed, one of these cases fails.
func TestArmAndTakeCompareIDClientRoundAndAge(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	me := client{ip: "100.64.0.1", ua: sha256.Sum256([]byte("browser"))}
	otherIP := client{ip: "100.64.0.2", ua: me.ua}
	otherUA := client{ip: me.ip, ua: sha256.Sum256([]byte("another browser"))}
	fresh := func() *liveSessions {
		l := newLiveSessions()
		l.keepPending("sid", &pendingAction{ID: "the-id", At: now, Client: me}, now)
		return l
	}
	kept := func(l *liveSessions) bool { return l.m["sid"].pending != nil }

	for name, arm := range map[string]func(l *liveSessions) *pendingAction{
		"wrong id":       func(l *liveSessions) *pendingAction { return l.armPending("sid", "another-id", me, "round", now) },
		"empty id":       func(l *liveSessions) *pendingAction { return l.armPending("sid", "", me, "round", now) },
		"a prefix of it": func(l *liveSessions) *pendingAction { return l.armPending("sid", "the-", me, "round", now) },
		"other address":  func(l *liveSessions) *pendingAction { return l.armPending("sid", "the-id", otherIP, "round", now) },
		"other browser":  func(l *liveSessions) *pendingAction { return l.armPending("sid", "the-id", otherUA, "round", now) },
		"no round":       func(l *liveSessions) *pendingAction { return l.armPending("sid", "the-id", me, "", now) },
		"too old": func(l *liveSessions) *pendingAction {
			return l.armPending("sid", "the-id", me, "round", now.Add(pendingMaxAge+time.Second))
		},
		"another session":   func(l *liveSessions) *pendingAction { return l.armPending("sid2", "the-id", me, "round", now) },
		"no session at all": func(l *liveSessions) *pendingAction { return l.armPending("", "the-id", me, "round", now) },
	} {
		l := fresh()
		if p := arm(l); p != nil {
			t.Errorf("armPending, %s: armed", name)
		}
		if strings.Contains(name, "session") {
			continue // nothing of this session was asked for: its action stays
		}
		if kept(l) {
			t.Errorf("armPending, %s: the action must be forgotten", name)
		}
	}
	l := fresh()
	if p := l.armPending("sid", "the-id", me, "round", now); p == nil || p.Round != "round" || !kept(l) {
		t.Fatal("the right id from the right client arms it")
	}

	for name, take := range map[string]func(l *liveSessions) (*pendingAction, bool){
		"another round trip": func(l *liveSessions) (*pendingAction, bool) { return l.takePending("sid", "round2", me, now) },
		"no round":           func(l *liveSessions) (*pendingAction, bool) { return l.takePending("sid", "", me, now) },
		"other address":      func(l *liveSessions) (*pendingAction, bool) { return l.takePending("sid", "round", otherIP, now) },
		"other browser":      func(l *liveSessions) (*pendingAction, bool) { return l.takePending("sid", "round", otherUA, now) },
		"too old": func(l *liveSessions) (*pendingAction, bool) {
			return l.takePending("sid", "round", me, now.Add(pendingMaxAge+time.Second))
		},
	} {
		l := fresh()
		l.armPending("sid", "the-id", me, "round", now)
		p, foreign := take(l)
		if p != nil || kept(l) {
			t.Errorf("takePending, %s: handed out (%v) or still kept (%v)", name, p != nil, kept(l))
		}
		if foreign != strings.HasPrefix(name, "other") {
			t.Errorf("takePending, %s: foreign = %v", name, foreign)
		}
	}
	// never armed: a round trip cannot take it, whatever its state is (also the empty one)
	for _, round := range []string{"", "round"} {
		l = fresh()
		if p, _ := l.takePending("sid", round, me, now); p != nil || kept(l) {
			t.Errorf("an action that was never armed was handed out to round %q", round)
		}
	}
	l = fresh()
	l.armPending("sid", "the-id", me, "round", now)
	if p, foreign := l.takePending("sid", "round", me, now); p == nil || foreign || kept(l) {
		t.Fatal("the round trip it was armed for, from its client, takes it once")
	}
	if p, _ := l.takePending("sid", "round", me, now); p != nil {
		t.Fatal("taken twice")
	}
}

// (b): the id is read from nowhere but the body of the dialog's own POST. A link never arms a kept action,
// whatever it carries and wherever it comes from, and the POST needs the CSRF header, a session, the same
// origin, and refuses API tokens.
func TestNoLinkCanNameAKeptAction(t *testing.T) {
	for name, hdr := range map[string][]string{
		"a link on another site": {"Sec-Fetch-Site", "cross-site", "Sec-Fetch-Mode", "navigate", "Referer", "https://evil.example/"},
		"a link on the panel":    {"Sec-Fetch-Site", "same-origin", "Sec-Fetch-Mode", "navigate"},
		"a typed address":        {"Sec-Fetch-Site", "none"},
		"an old browser":         nil,
	} {
		for _, form := range []string{"&pending=%s&next=/", "&pending_id=%s&next=/", "&next=/&id=%s"} {
			a := arm(t)
			link := strings.Replace(form, "%s", a.id, 1)
			q, rt := a.idp.login(a.e, "/auth/login?step_up=1"+link, a.sess)
			if keptID(a.e, a.sess) != "" {
				t.Fatalf("%s (%s): starting a sign-in by link must forget the kept action", name, form)
			}
			a.idp.claims["auth_time"] = a.clk.now().Unix()
			r := a.e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, append([]string{"Cookie", rt}, hdr...)...)
			if r.code != 303 || a.powerCalls() != 0 {
				t.Fatalf("%s (%s): %d, %d power calls", name, form, r.code, a.powerCalls())
			}
		}
	}
	// the POST: the id in the address instead of the body is not read
	a := arm(t)
	if r := a.e.do("POST", stepUpPath+"?pending_id="+a.id, map[string]any{"next": "/"}, "Cookie", a.sess, "X-Requested-With", "fairy",
		"Content-Type", "application/json"); r.code != 409 || keptID(a.e, a.sess) != "" {
		t.Fatal("an id in the address:", r.code, string(r.raw))
	}
	// and it is a write like every other
	body := func(a *armed) map[string]any { return map[string]any{"pending_id": a.id, "next": "/"} }
	a = arm(t)
	for name, hdr := range map[string][]string{
		"no CSRF header":      {"Cookie", a.sess},
		"another origin":      {"Cookie", a.sess, "X-Requested-With", "fairy", "Origin", "https://evil.example"},
		"cross-site":          {"Cookie", a.sess, "X-Requested-With", "fairy", "Sec-Fetch-Site", "cross-site"},
		"same-site, not same": {"Cookie", a.sess, "X-Requested-With", "fairy", "Sec-Fetch-Site", "same-site"},
	} {
		if r := a.e.do("POST", stepUpPath, body(a), append(hdr, "Content-Type", "application/json")...); r.code != 403 {
			t.Errorf("%s: %d %s", name, r.code, r.raw)
		}
		if keptID(a.e, a.sess) != a.id {
			t.Fatalf("%s: a refused request changed what is kept", name)
		}
	}
	if r := a.e.do("POST", stepUpPath, body(a), "X-Requested-With", "fairy", "Content-Type", "application/json"); r.code != 401 {
		t.Error("no session:", r.code)
	}
	tok, _, _ := a.e.s.Tokens.Create("ops", "system", "control", "cli:test", 0)
	if r := a.e.do("POST", stepUpPath, body(a), "Authorization", "Bearer "+tok, "Content-Type", "application/json"); r.code != 403 {
		t.Error("API token:", r.code)
	}
	if r := a.e.do("GET", stepUpPath+"?pending_id="+a.id, nil, "Cookie", a.sess); r.code != 405 && r.code != 404 {
		t.Error("GET:", r.code)
	}
	if keptID(a.e, a.sess) != a.id || a.powerCalls() != 0 {
		t.Fatal("refused requests changed what is kept, or something ran")
	}
	// the answer of a confirmation carries no id and no secret, only where to sign in and what will run
	_, _, r := a.idp.tryConfirmStart(a.e, a.id, "/", a.sess)
	if r.code != 200 || len(r.body) != 2 || r.body["action"] != "power action on laptop (action: shutdown)" || strings.Contains(string(r.raw), a.id) {
		t.Fatal(r.code, string(r.raw))
	}
}

// The reviewer's test for the comparison in takePending: a step-up round trip that was started WITHOUT naming
// an action must not run one that was kept afterwards (second tab); and a round trip that was started for one
// action must not run another one that replaced it.
func TestRoundTripRunsOnlyTheActionItWasStartedFor(t *testing.T) {
	a := signedIn(t)
	q, rt := a.idp.login(a.e, "/auth/login?step_up=1&next=/", a.sess) // tab A: a step-up that names nothing
	if id := a.shutdown(); id == "" {                                 // tab B: an action is refused and kept
		t.Fatal("not kept")
	}
	a.idp.claims["auth_time"] = a.clk.now().Unix()
	if r := a.e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt); r.code != 303 {
		t.Fatal(r.code, string(r.raw))
	}
	if n := a.powerCalls(); n != 0 {
		t.Fatalf("a step-up that named no action ran the one kept later (%d)", n)
	}

	a = signedIn(t)
	r := a.e.poll(a.sess) // tab A: a polling change is refused, kept, and its confirmation starts
	q, rt = a.idp.confirmStart(a.e, pendingID(r), "/", a.sess)
	if id := a.shutdown(); id == "" { // tab B: the shutdown replaces it
		t.Fatal("not kept")
	}
	a.idp.claims["auth_time"] = a.clk.now().Unix()
	if r := a.e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt); r.code != 303 {
		t.Fatal(r.code, string(r.raw))
	}
	if n := a.powerCalls(); n != 0 || keptID(a.e, a.sess) != "" {
		t.Fatalf("the confirmation of one action ran another (%d), or left it armed", n)
	}
}

// (d): the refusal, the confirmation's answer and the audit log name the action in the panel's own words (from
// the route, its parameters and a few named body fields), not in the words the page sent. Nothing of the body
// that is not on the list appears (the typed confirmation, a delay).
func TestConfirmationNamesTheAction(t *testing.T) {
	a := signedIn(t)
	body := a.shutdownBody()
	r := a.e.do("POST", "/api/systems/laptop/power", body, "Cookie", a.sess, "X-Requested-With", "fairy",
		"Content-Type", "application/json", "X-Fairy-Action", "Look%20at%20kittens")
	d, _ := r.body["detail"].(map[string]any)
	const want = "power action on laptop (action: shutdown)"
	if !stepUpRequired(r) || d["action"] != want {
		t.Fatal("the refusal must say what would run:", string(r.raw))
	}
	id := pendingID(r)
	if cb := a.confirm(id, false); cb.code != 303 || a.powerCalls() != 1 {
		t.Fatal(cb.code, a.powerCalls())
	}
	raw, _ := os.ReadFile(a.e.s.Cfg.AuditLog)
	log := string(raw)
	for _, w := range []string{`"outcome":"redirected to Pocket ID"`, `"for":"` + want + `"`, `"runs":"` + want + `"`} {
		if !strings.Contains(log, w) {
			t.Errorf("audit log lacks %s", w)
		}
	}
	if strings.Contains(log, "kittens") {
		t.Error("the page's label is not what the audit log names the action by")
	}
	if id == "" || strings.Contains(log, id) {
		t.Error("the kept action's id is in the audit log")
	}
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, `"action":"auth.step_up"`) && (strings.Contains(line, "confirm") || strings.Contains(line, "delay_s")) {
			t.Errorf("a step-up entry carries more of the request than its description: %s", line)
		}
	}

	// the description: every route with a text is a real route, parameters are filled in and cleaned, only the
	// listed body fields appear, and a route without a text is named by method and path
	for pat := range actionText {
		method, path, _ := strings.Cut(pat, " ")
		req := httptest.NewRequest(method, wildcardRe.ReplaceAllString(path, "x"), nil)
		if _, got := a.e.s.mux.Handler(req); got != pat {
			t.Errorf("actionText: %q is not a registered route (matched %q)", pat, got)
		}
	}
	req := httptest.NewRequest("DELETE", "/api/admin/users/x", nil)
	req.Pattern = "DELETE /api/admin/users/{sub}"
	for sub, wantText := range map[string]string{
		"u1":                    "remove user u1",
		"a\nb\x1b[31m\u202e":    "remove user a%0Ab%1B[31m%E2%80%AE",
		"{sub}":                 "remove user {sub}",
		strings.Repeat("x", 90): "remove user " + strings.Repeat("x", 80),
	} {
		if got := describeAction(req, map[string]string{"sub": sub}, nil); got != wantText {
			t.Errorf("describeAction(%q) = %q, want %q", sub, got, wantText)
		}
	}
	req = httptest.NewRequest("PUT", "/api/admin/users/u7", nil)
	req.Pattern = "PUT /api/admin/users/{sub}"
	got := describeAction(req, map[string]string{"sub": "u7"}, []byte(`{"role":"admin","confirm":"TYPED","token":"fy_secret","sites":["a"],"name":7}`))
	if got != "change user u7 (role: admin)" {
		t.Errorf("body fields: %q", got)
	}
	req = httptest.NewRequest("POST", "/api/future/thing", nil)
	req.Pattern = "POST /api/future/{x}"
	if got := describeAction(req, map[string]string{"x": "thing"}, []byte(`not json`)); got != "POST /api/future/thing" {
		t.Errorf("a route without a text: %q", got)
	}
	if got := describeAction(req, nil, []byte(`{"name":"`+strings.Repeat("n", 500)+`"}`)); len(got) > maxActionText {
		t.Errorf("description of %d bytes", len(got))
	}
}

// (f): the session's own client is still known after a restart (the mark is in the signed cookie), so the
// first control action after a panel update is kept and runs after the sign-in, as before. A cookie without
// the mark (it cannot exist after this release: every session is new) keeps nothing.
func TestOwnClientIsStillKnownAfterARestart(t *testing.T) {
	a := signedIn(t)
	a.e.s.codec.live = newLiveSessions() // what a restart leaves
	id := a.shutdown()
	if id == "" {
		t.Fatal("the session's own client must still be kept after a restart")
	}
	if r := a.asThief("POST", "/api/systems/laptop/power", a.shutdownBody(), thief); pendingID(r) != "" {
		t.Fatal("a copy of the cookie is kept after a restart")
	}
	if r := a.confirm(id, false); r.code != 303 || a.powerCalls() != 1 {
		t.Fatal(r.code, a.powerCalls())
	}

	e := setup(t)
	u := controller()
	e.signIn(u)
	sid := randToken(18)
	if err := e.s.Poller.Admin.AddSession(sid, adminstate.NewSession{Sub: u.Sub}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	e.s.codec.write(rec, &sessionData{SID: sid, User: &u, IAT: e.s.now().Unix(), Seen: e.s.now().Unix()}) // no mark
	ck := strings.Split(rec.Header().Get("Set-Cookie"), ";")[0]
	r := e.poll(ck)
	if d, _ := r.body["detail"].(map[string]any); !stepUpRequired(r) || d["pending"] != false || d["not_kept"] != notKeptClient {
		t.Fatal("a session without a client mark:", r.code, string(r.raw))
	}
}

// (f): the owner's own browser after its address changed (phone on mobile data, VPN). Nothing is kept for the
// new address; one plain sign-in from there makes it the session's client, and the old address is then the
// stranger. In per-action mode the action is then asked for once more, alone.
func TestClientThatMovedSignsInOnceAndIsThenKept(t *testing.T) {
	for _, perAction := range []bool{false, true} {
		a := signedIn(t)
		a.e.s.Cfg.StepUpPerAction = perAction
		r := a.asThief("POST", "/api/systems/laptop/power", a.shutdownBody(), otherAddr)
		if d, _ := r.body["detail"].(map[string]any); !stepUpRequired(r) || d["not_kept"] != notKeptClient {
			t.Fatal(r.code, string(r.raw))
		}
		// the dialog's plain "Continue" (a link): sign in again from the new address
		q, rt := a.idp.login(a.e, "/auth/login?step_up=1&next=/", a.sess)
		a.idp.claims["auth_time"] = a.clk.now().Unix()
		cb := a.e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, append([]string{"Cookie", rt}, otherAddr...)...)
		if cb.code != 303 {
			t.Fatal(cb.code, string(cb.raw))
		}
		a.sess = sessionCookie(a.e, cb.hdr)
		r = a.asThief("POST", "/api/systems/laptop/power", a.shutdownBody(), otherAddr) // "Resume" sends it again
		if !perAction {
			if r.code != 200 || a.powerCalls() != 1 {
				t.Fatal("inside the window from the new address:", r.code, string(r.raw))
			}
		} else {
			id := pendingID(r)
			if !stepUpRequired(r) || id == "" {
				t.Fatal("per-action: the new address is the session's client now, so the action is kept:", r.code, string(r.raw))
			}
			if cb := a.confirm(id, false, otherAddr...); cb.code != 303 || a.powerCalls() != 1 {
				t.Fatal(cb.code, a.powerCalls())
			}
		}
		// the address the session came from before is now the one nothing is kept for
		if r := a.e.poll(a.sess); pendingID(r) != "" || !stepUpRequired(r) {
			t.Fatal("the old address:", r.code, string(r.raw))
		}
	}
}

// Demo mode (the browser check runs in it): "Continue" runs the kept action and says which page to show; a
// link runs nothing.
func TestDemoConfirmRunsTheKeptActionAndALinkDoesNot(t *testing.T) {
	e := setup(t)
	u := person("demo-alice")
	u.Auth = "demo"
	e.s.DemoUser = &u
	e.s.demoRoutes()
	if _, _, err := e.s.Access.Seen(u.Sub, "", "", access.RolePending, "demo"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.Access.SetRole(u.Sub, access.RoleAdmin, nil, "demo", true); err != nil {
		t.Fatal(err)
	}
	e.s.demoAuthTime = time.Now().Add(-time.Hour)
	demoKept := func() bool {
		l := e.s.codec.live
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.m[demoKey] != nil && l.m[demoKey].pending != nil
	}
	r := e.poll("")
	id := pendingID(r)
	if !stepUpRequired(r) || id == "" {
		t.Fatal(r.code, string(r.raw))
	}
	if l := e.do("GET", "/auth/login?step_up=1&pending="+id+"&next=/", nil); l.code != 303 {
		t.Fatal(l.code)
	}
	if me := e.do("GET", "/api/me", nil); me.body["pending_result"] != nil || demoKept() {
		t.Fatal("a link ran or kept the action in demo mode", string(me.raw))
	}
	e.s.demoAuthTime = time.Now().Add(-time.Hour)
	r = e.poll("")
	id = pendingID(r)
	c := e.do("POST", stepUpPath, map[string]any{"pending_id": id, "next": "/#/system/laptop"}, "X-Requested-With", "fairy",
		"Content-Type", "application/json")
	if c.code != 200 || c.body["url"] != "/#/system/laptop" || c.body["action"] != "polling of laptop" {
		t.Fatal(c.code, string(c.raw))
	}
	var ck []string
	if v := c.hdr.Get("Set-Cookie"); v != "" {
		ck = []string{"Cookie", strings.Split(v, ";")[0]}
	}
	me := e.do("GET", "/api/me", nil, ck...)
	if pr, _ := me.body["pending_result"].(map[string]any); pr == nil || pr["ok"] != true {
		t.Fatal("the demo confirmation must run the action:", string(me.raw))
	}
}

// Parallel requests around a confirmation: one kept action runs at most once and nothing races (run with
// -race). The fake identity provider answers every token request, so the replayed callbacks that lose sign in
// again; a real provider refuses a code twice.
func TestParallelCallbacksRunTheKeptActionOnce(t *testing.T) {
	a := arm(t)
	q, rt := a.idp.confirmStart(a.e, a.id, "/", a.sess)
	a.idp.claims["auth_time"] = a.clk.now().Unix()
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 3 {
			case 0:
				a.e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt)
			case 1: // (a control request here would be refused and kept in place of the shutdown: that is meant)
				a.e.do("GET", "/api/systems", nil, "Cookie", a.sess)
			default:
				a.e.do("GET", "/api/me", nil, "Cookie", a.sess, "X-Fairy-Active", "1")
			}
		}(i)
	}
	wg.Wait()
	// At most once. It can be zero here: the callback that took the action can find the session already given
	// a new id by a replay that finished first, and then nothing runs (it never runs by another path).
	if n := a.powerCalls(); n > 1 {
		t.Fatalf("the kept shutdown ran %d times", n)
	}
}

// NEW-7: the one-time join code goes to the client that confirmed the Add host, while that confirmation still
// counts. The same cookie from another address or browser used to take it (and the admin's page then got 404).
func TestJoinCodeIsRevealedOnlyToTheClientThatConfirmed(t *testing.T) {
	for _, perAction := range []bool{false, true} {
		a := signedIn(t)
		e := a.e
		e.s.Cfg.StepUpPerAction = perAction
		e.s.Enroll.PanelAddr = "100.64.0.10"
		e.s.Enroll.Interval = time.Hour
		add := func(name string) string {
			r := e.do("POST", "/api/admin/hosts", map[string]any{"name": name, "os": "linux"}, "Cookie", a.sess,
				"X-Requested-With", "fairy", "Content-Type", "application/json")
			if !stepUpRequired(r) {
				t.Fatal(r.code, string(r.raw))
			}
			if cb := a.confirm(pendingID(r), false); cb.code != 303 {
				t.Fatal(cb.code, string(cb.raw))
			}
			pr, _ := e.do("GET", "/api/me", nil, "Cookie", a.sess).body["pending_result"].(map[string]any)
			id, _ := pr["ref"].(string)
			if id == "" {
				t.Fatal("no Add host was made", pr)
			}
			return id
		}
		id := add("SRV-WINPC")
		for who, hdr := range map[string][]string{"another address and browser": thief, "another address": otherAddr, "another browser": otherBrowser} {
			// the copy can see that an Add host waits (it is the same session) ...
			list := e.do("GET", "/api/admin/hosts", nil, append([]string{"Cookie", a.sess}, hdr...)...)
			if enr, _ := list.body["enrollments"].([]any); list.code != 200 || len(enr) != 1 {
				t.Fatal(list.code, string(list.raw))
			}
			// ... but not take its code
			g := e.do("GET", "/api/admin/enrollments/"+id+"?reveal=1", nil, append([]string{"Cookie", a.sess}, hdr...)...)
			if g.code != 403 || g.body["code"] != nil || g.body["command"] != nil {
				t.Fatalf("per-action %v, %s took the join code: %d %s", perAction, who, g.code, g.raw)
			}
		}
		own := e.do("GET", "/api/admin/enrollments/"+id+"?reveal=1", nil, "Cookie", a.sess)
		if code, _ := own.body["code"].(string); own.code != 200 || code == "" {
			t.Fatalf("per-action %v: the confirming browser must still get the code after the refusals: %d %s", perAction, own.code, own.raw)
		}
		if again := e.do("GET", "/api/admin/enrollments/"+id+"?reveal=1", nil, "Cookie", a.sess); again.code != 404 {
			t.Fatal("the code is shown once:", again.code)
		}
		e.s.Enroll.Cancel(id)

		// the confirming browser itself, once the confirmation no longer counts
		long := time.Duration(max(e.s.Cfg.StepUpWindowS, perActionGrace)+5) * time.Second
		a.clk.advance(long) // so that the next Add host is asked to confirm again
		id = add("SRV-TOWER2")
		a.clk.advance(time.Duration(max(e.s.Cfg.StepUpWindowS, perActionGrace)+5) * time.Second)
		if g := e.do("GET", "/api/admin/enrollments/"+id+"?reveal=1", nil, "Cookie", a.sess); g.code != 403 || g.body["code"] != nil {
			t.Fatalf("per-action %v: revealed after the window: %d %s", perAction, g.code, g.raw)
		}
		e.s.Enroll.Cancel(id)
	}

	// another admin, freshly signed in from the very same address and browser, is not who it is held for
	a := signedIn(t)
	e := a.e
	e.s.Enroll.PanelAddr = "100.64.0.10"
	e.s.Enroll.Interval = time.Hour
	r := e.do("POST", "/api/admin/hosts", map[string]any{"name": "SRV-WINPC", "os": "linux"}, "Cookie", a.sess,
		"X-Requested-With", "fairy", "Content-Type", "application/json")
	if cb := a.confirm(pendingID(r), false); cb.code != 303 || len(e.s.Enroll.List()) != 1 {
		t.Fatal(cb.code)
	}
	id := e.s.Enroll.List()[0].ID
	second := model.User{Sub: "admin2", Name: model.P("Second"), Groups: []string{"viewers"}}
	ck := e.cookie(second, 0)
	if _, _, err := e.s.Access.SetRole("admin2", access.RoleAdmin, nil, "test", true); err != nil {
		t.Fatal(err)
	}
	if g := e.do("GET", "/api/admin/enrollments/"+id+"?reveal=1", nil, "Cookie", ck); g.code != 404 || g.body["code"] != nil {
		t.Fatalf("another admin took the code: %d %s", g.code, g.raw)
	}
	if g := e.do("GET", "/api/admin/enrollments/"+id+"?reveal=1", nil, "Cookie", a.sess); g.code != 200 {
		t.Fatal("the admin who confirmed:", g.code, string(g.raw))
	}
}
