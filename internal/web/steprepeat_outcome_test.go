package web

import (
	"context"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
)

// Audit 6, r4-p1-01. The page a repeated confirmation gets said "The action was already sent" whenever a kept
// action had been waiting, also when the panel refused that action and sent nothing: the kill switch or the
// host's power switch set between "Continue" and the way back, a wrong typed name, a cancel over its limit.
// "An action was waiting" is known before it runs; how it went is known after. The round trip now keeps the
// action's answer, and the repeat's page says that: sent, refused, or not known. The rule of the repeat is as
// it was: a page, no cookie, nothing run again.

// repeatPage is the page a reload of the callback gets, checked for what every repeat must be: 409, the title,
// one link back, no script, no cookie.
func repeatPage(t *testing.T, what string, r resp) string {
	t.Helper()
	body := string(r.raw)
	if r.code != 409 || !strings.Contains(body, "<h1>Already confirmed</h1>") || strings.Count(body, "<a ") != 1 || strings.Contains(body, "<script") {
		t.Fatalf("%s: %d %s", what, r.code, strings.TrimSpace(body))
	}
	noCookieSet(t, what, r.hdr)
	return body
}

func TestTheRepeatPageSaysWhatCameOfTheKeptAction(t *testing.T) {
	const (
		sent    = "The action was already sent. It is not sent again. Sign in again to go on."
		refused = "The action was not carried out: it was refused. It is not tried again. Sign in again to go on."
		unsure  = "The action ended with an error: Fairy does not know whether it went through. It is not sent again. Sign in again to go on."
	)
	for _, c := range []struct {
		name  string
		prep  func(t *testing.T, a *armed)
		want  string
		calls int // requests that reached the machine
	}{
		{"the action is sent (the control)", func(*testing.T, *armed) {}, sent, 1},
		{"the kill switch was set before the way back", func(t *testing.T, a *armed) {
			if err := a.e.s.Poller.Admin.SetControlDisabled(true, "root (cli)", "maintenance"); err != nil {
				t.Fatal(err)
			}
		}, refused, 0},
		{"the host's power switch was set before the way back", func(t *testing.T, a *armed) {
			if err := a.e.s.Poller.Admin.SetPowerDisabled("laptop", true); err != nil {
				t.Fatal(err)
			}
		}, refused, 0},
		{"the machine refuses the request", func(_ *testing.T, a *armed) {
			a.e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
				return 409, jsonx.M{"error": "a shutdown is already pending; cancel it first"}, nil
			}
		}, refused, 1},
		{"the request to the machine gets no answer", func(_ *testing.T, a *armed) {
			a.e.b.Reply = func(agent, path string) (int, jsonx.M, error) { return 0, nil, context.DeadlineExceeded }
		}, unsure, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := arm(t) // a shutdown of laptop is kept
			q, rt := a.idp.confirmStart(a.e, a.id, "/#/system/laptop", a.sess)
			c.prep(t, a)
			a.idp.claims["auth_time"] = a.clk.now().Unix()
			callback := "/auth/callback?code=c&state=" + q.Get("state")
			first := a.e.do("GET", callback, nil, "Cookie", rt) // its answer is lost
			if first.code != 303 || a.powerCalls() != c.calls {
				t.Fatalf("first callback: %d %s; requests to the machine: %d, want %d", first.code, first.raw, a.powerCalls(), c.calls)
			}
			a.idp.usedCode()
			for i := 0; i < 2; i++ {
				body := repeatPage(t, "the reload", a.e.do("GET", callback, nil, "Cookie", rt))
				if !strings.Contains(body, c.want) {
					t.Errorf("the reload's page lacks %q:\n%s", c.want, body[strings.Index(body, "<main"):])
				}
				if c.want != sent && strings.Contains(body, "already sent") {
					t.Errorf("the page says \"already sent\" for an action that was not: %s", body[strings.Index(body, "<main"):])
				}
			}
			if a.powerCalls() != c.calls {
				t.Errorf("a reload sent something: %d requests to the machine, were %d", a.powerCalls(), c.calls)
			}
			if r := a.e.do("GET", "/api/me", nil, "Cookie", rt); r.code != 401 { // the round trip's cookie is still no session
				t.Errorf("/api/me with the round trip's cookie: %d", r.code)
			}
		})
	}
}

// A wrong typed name, and a cancel of a countdown that its limit refuses: nothing is sent, and the reload does
// not say that it was. (For the cancel "already sent" was wrong to the unsafe side: the countdown goes on.)
func TestTheRepeatPageForARefusedTypedNameAndARefusedCancel(t *testing.T) {
	const refused = "The action was not carried out: it was refused. It is not tried again. Sign in again to go on."
	hdr := func(a *armed) []string {
		return []string{"Cookie", a.sess, "X-Requested-With", "fairy", "Content-Type", "application/json"}
	}
	for _, name := range []string{"a wrong typed name", "a cancel over its limit"} {
		a := signedIn(t)
		var r resp
		if name == "a wrong typed name" {
			r = a.e.do("POST", "/api/systems/laptop/power", map[string]any{"action": "shutdown", "delay_s": 10, "confirm": "not-the-name"}, hdr(a)...)
		} else {
			// The approval build: a cancel of a countdown is never kept for a confirmation (it asks for none:
			// conflict 9 of the rebase onto fix7r, approval_cancel_test.go), so a cancel cannot meet a repeat
			// page here, whatever its limit says. That is held first. The case itself, a kept request that its
			// limit refuses when it runs, is made with a restart over the control limit.
			if c := a.e.do("POST", "/api/systems/laptop/power", map[string]any{"action": "cancel"}, hdr(a)...); c.code == 401 || pendingID(c) != "" {
				t.Fatalf("%s: a cancel was kept for a confirmation: %d %s", name, c.code, c.raw)
			}
			r = a.e.do("POST", "/api/systems/laptop/power", map[string]any{"action": "reboot", "delay_s": 10}, hdr(a)...)
		}
		id := pendingID(r)
		if r.code != 401 || id == "" {
			t.Fatalf("%s: the request must be kept for a confirmation: %d %s", name, r.code, r.raw)
		}
		q, rt := a.idp.confirmStart(a.e, id, "/#/system/laptop", a.sess)
		if name == "a cancel over its limit" {
			a.e.s.control = newRateLimiter(1)
			a.e.s.control.allow("u1") // the minute's control requests are used up
		}
		a.idp.claims["auth_time"] = a.clk.now().Unix()
		callback := "/auth/callback?code=c&state=" + q.Get("state")
		if first := a.e.do("GET", callback, nil, "Cookie", rt); first.code != 303 || a.powerCalls() != 0 {
			t.Fatalf("%s: first callback: %d; requests to the machine: %d", name, first.code, a.powerCalls())
		}
		a.idp.usedCode()
		body := repeatPage(t, name, a.e.do("GET", callback, nil, "Cookie", rt))
		if !strings.Contains(body, refused) || strings.Contains(body, "already sent") {
			t.Errorf("%s: the reload's page: %s", name, body[strings.Index(body, "<main"):])
		}
	}
}
