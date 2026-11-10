package web

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// poll is a harmless control action (step-up protected): change one host's polling interval.
func (e *env) poll(cookie string, hdr ...string) resp {
	return e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 5},
		append([]string{"Cookie", cookie, "X-Requested-With", "fairy"}, hdr...)...)
}

func stepUpRequired(r resp) bool {
	d, _ := r.body["detail"].(map[string]any)
	return r.code == 401 && d["code"] == "step_up_required"
}

// The second client of these tests: the same cookie, presented from another address (X-Forwarded-For is
// believed from the test's loopback connection, as from nginx) or from another browser.
var (
	otherAddr    = []string{"X-Forwarded-For", "203.0.113.9"}
	otherBrowser = []string{"User-Agent", "Mozilla/5.0 (thief)"}
)

// S-c: the step-up window belongs to the client that confirmed the sign-in. The same cookie presented from
// another address or another browser is asked to confirm, while the first client is still inside the window.
func TestStepUpWindowIsBoundToTheConfirmingClient(t *testing.T) {
	a := signedIn(t)
	if r := a.e.poll(a.sess); !stepUpRequired(r) {
		t.Fatal("a sign-in from an hour ago needs a step-up", r.code, string(r.raw))
	}
	if r := a.stepUp("&next=/", false); r.code != 303 {
		t.Fatal(r.code, string(r.raw))
	}
	if r := a.e.poll(a.sess); r.code != 200 {
		t.Fatal("the confirming client is inside the window", r.code, string(r.raw))
	}
	for name, hdr := range map[string][]string{"another address": otherAddr, "another browser": otherBrowser} {
		r := a.e.poll(a.sess, hdr...)
		if !stepUpRequired(r) {
			t.Fatalf("the same cookie from %s must be asked to confirm, got %d %s", name, r.code, r.raw)
		}
		// the copy can still read (it is the same session), but is told its sign-in is old
		me := a.e.do("GET", "/api/me", nil, append([]string{"Cookie", a.sess}, hdr...)...)
		if me.code != 200 || me.body["auth_age_s"].(float64) < 86400 {
			t.Fatalf("%s: /api/me %d auth_age_s %v", name, me.code, me.body["auth_age_s"])
		}
	}
	if r := a.e.poll(a.sess); r.code != 200 {
		t.Fatal("the first client is still inside the window", r.code, string(r.raw))
	}
	me := a.e.do("GET", "/api/me", nil, "Cookie", a.sess)
	if me.body["auth_age_s"].(float64) > 60 {
		t.Fatal("auth age of the confirming client", me.body["auth_age_s"])
	}
	// the window still ends for the confirming client
	a.clk.advance(time.Duration(a.e.s.Cfg.StepUpWindowS+1) * time.Second)
	if r := a.e.poll(a.sess); !stepUpRequired(r) {
		t.Fatal("after step_up_window_s the confirming client is asked again", r.code)
	}
	// a first sign-in is bound the same way
	e := setup(t)
	c := e.cookie(controller(), 0)
	if r := e.poll(c); r.code != 200 {
		t.Fatal(r.code, string(r.raw))
	}
	if r := e.poll(c, otherAddr...); !stepUpRequired(r) {
		t.Fatal("fresh sign-in, other address", r.code)
	}
}

// S-c: every step-up gives the session a new id. A copy of the cookie taken before it is signed out, the
// registry holds one session, and session_max_age still counts from the original sign-in.
func TestStepUpRotatesTheSessionID(t *testing.T) {
	a := signedIn(t)
	a.e.s.Cfg.SessionIdleTimeout = a.e.s.Cfg.SessionMaxAge
	before := a.sess
	a.clk.advance(time.Duration(a.e.s.Cfg.SessionMaxAge)*time.Second - time.Minute)
	if r := a.stepUp("&next=/", false); r.code != 303 {
		t.Fatal(r.code, string(r.raw))
	}
	if sidOf(a.e, before) == sidOf(a.e, a.sess) || sidOf(a.e, a.sess) == "" {
		t.Fatal("the session id did not change")
	}
	if r := a.e.do("GET", "/api/me", nil, "Cookie", before); r.code != 401 {
		t.Fatal("the cookie from before the step-up must stop working", r.code)
	}
	if r := a.e.poll(before); r.code != 401 || stepUpRequired(r) {
		t.Fatal("the old copy must be signed out, not offered a step-up", r.code, string(r.raw))
	}
	if r := a.e.poll(a.sess); r.code != 200 {
		t.Fatal("the new cookie works", r.code, string(r.raw))
	}
	if n := len(must(a.e.s.Poller.Admin.ListSessions())); n != 1 {
		t.Fatal("sessions registered:", n)
	}
	a.clk.advance(2 * time.Minute)
	if r := a.e.do("GET", "/api/me", nil, "Cookie", a.sess); r.code != 401 {
		t.Fatal("a step-up must not extend session_max_age", r.code)
	}
}

// S-c: a restart forgets every confirmation (they are in memory only). The session itself survives, control
// actions ask for a new confirmation.
func TestStepUpFailsClosedAfterRestart(t *testing.T) {
	e := setup(t)
	c := e.cookie(controller(), 0)
	if r := e.poll(c); r.code != 200 {
		t.Fatal(r.code, string(r.raw))
	}
	s2 := New(e.s.Cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, e.s.Poller, nil, "test") // the restarted panel
	srv := httptest.NewServer(s2.Handler())
	defer srv.Close()
	e2 := &env{t: t, s: s2, srv: srv, b: e.b}
	if r := e2.do("GET", "/api/me", nil, "Cookie", c); r.code != 200 {
		t.Fatal("the session survives a restart", r.code)
	}
	if r := e2.poll(c); !stepUpRequired(r) {
		t.Fatal("after a restart control must ask for a new confirmation", r.code, string(r.raw))
	}
}

// S-c, step_up_per_action: true. A confirmation authorises exactly the one kept action. Nothing is inherited:
// not by a second request from the same browser, not from a fresh sign-in, not from a step-up without an action.
func TestStepUpPerAction(t *testing.T) {
	a := signedIn(t)
	a.e.s.Cfg.StepUpPerAction = true
	a.e.s.Cfg.StepUpPromptLogin = true
	q, _ := a.idp.login(a.e, "/auth/login?step_up=1&next=/", a.sess)
	if q.Get("prompt") != "login" || q.Get("max_age") != "0" {
		t.Fatal("step_up_prompt_login: the step-up must ask Pocket ID for prompt=login and max_age=0", q)
	}
	if r := a.stepUp("&next=/", false); r.code != 303 { // a confirmation that names no action
		t.Fatal(r.code)
	}
	r := a.e.poll(a.sess)
	d, _ := r.body["detail"].(map[string]any)
	if !stepUpRequired(r) || d["per_action"] != true || d["pending"] != true {
		t.Fatal("a confirmation a second ago authorises nothing by itself", r.code, string(r.raw))
	}
	id := a.shutdown() // replaces the kept poll change
	if id == "" || a.powerCalls() != 0 {
		t.Fatal("every action is asked for")
	}
	if r := a.stepUp("&pending="+id+"&next=/", false); r.code != 303 || a.powerCalls() != 1 {
		t.Fatal("the confirmed action runs once", r.code, a.powerCalls())
	}
	me := a.e.do("GET", "/api/me", nil, "Cookie", a.sess)
	if pr, _ := me.body["pending_result"].(map[string]any); pr == nil || pr["ok"] != true {
		t.Fatal("result of the confirmed action", string(me.raw))
	}
	if r := a.e.poll(a.sess); !stepUpRequired(r) {
		t.Fatal("the next action, a moment later from the same browser, is asked again", r.code, string(r.raw))
	}
	if a.shutdown() == "" || a.powerCalls() != 1 {
		t.Fatal("the same action again is asked again")
	}
	// user and site changes (adminGuard) follow the same rule
	if r := a.e.do("POST", "/api/admin/sites", map[string]any{"name": "Attic"}, "Cookie", a.sess, "X-Requested-With", "fairy",
		"Content-Type", "application/json"); !stepUpRequired(r) {
		t.Fatal("site change", r.code, string(r.raw))
	}
	// a fresh sign-in authorises nothing either
	e := setup(t)
	e.s.Cfg.StepUpPerAction = true
	if r := e.poll(e.cookie(controller(), 0)); !stepUpRequired(r) {
		t.Fatal("fresh sign-in in per-action mode", r.code)
	}
	// API tokens are not sessions: no step-up, as before
	raw, _, _ := a.e.s.Tokens.Create("ctl", "system", "control", "cli:test", 0)
	if r := a.e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 5}, "Authorization", "Bearer "+raw); r.code != 200 {
		t.Fatal("control token", r.code, string(r.raw))
	}
}
