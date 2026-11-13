package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
)

// A shutdown whose request got no answer of the agent (its 504: the helper was handed the request and did not
// answer; a gateway's 502 or 504; a timeout) may be counting down, and the page shows no countdown: the
// panel's view shows one only after a poll that carries it. powerctl answered a Cancel with "nothing pending"
// itself as long as none showed, so nothing reached the agent, and the agent's own answer of that minute
// ("... may still be scheduled: send the cancel again", Fairy-agent f439fc0) could not be followed from the
// panel (the gap the fixer of the approval half reported for the panel's core).
//
// For the time such a request can still arrive, a Cancel goes to the agent whatever the page shows, "Cancel
// pending" is on offer, and the answer says what the agent said: never "cancelled" for a countdown nobody saw.

const (
	agentLostShutdown = "the helper was sent the shutdown and did not answer in time: it may be scheduled all the same, now or in a moment. " +
		"Look at what the machine shows as pending; every cancel of the next minute is passed on to the helper"
	agentCancelAgain = "nothing was pending when the cancel reached the helper, but the shutdown that got no answer a moment ago may still be scheduled: " +
		"send the cancel again (for a minute after such a request the agent cannot say that nothing is pending)"
)

// lostPower scripts laptop's agent for /power: a shutdown gets what shutdown returns (nil: the machine answers
// as usual), a cancel what cancel returns (nil: the agent's "ok, nothing pending"). It counts the cancels that
// reached the agent.
type lostPower struct {
	mu       sync.Mutex
	shutdown func() (int, jsonx.M)
	cancel   func() (int, jsonx.M)
	cancels  int
}

func (l *lostPower) on(a *approvalEnv) {
	a.gb.Relay("laptop", func(path string, body any) (int, jsonx.M) {
		if path != "/power" {
			return 0, nil
		}
		raw, _ := json.Marshal(body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		l.mu.Lock()
		defer l.mu.Unlock()
		switch b["action"] {
		case "cancel":
			l.cancels++
			if l.cancel != nil {
				return l.cancel()
			}
			return 200, jsonx.M{"action": "cancel", "pending": nil} // the agent's answer to a cancel, also with nothing pending
		case "shutdown":
			if l.shutdown != nil {
				return l.shutdown()
			}
		}
		return 0, nil
	})
}

func (l *lostPower) sent() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cancels
}

// noteCount is how many notifications the panel has.
func noteCount(a *approvalEnv) int {
	return len(a.s.Notes.List("", func(notifications.Event) bool { return true }, notifications.Filter{Limit: 200}).Events)
}

// cancelOutcomes are the outcomes of the audit entries of laptop's cancels, sorted.
func cancelOutcomes(a *approvalEnv) (out []string, params []map[string]any) {
	for _, e := range a.auditEntries("", "power.cancel") {
		out = append(out, fmt.Sprint(e["outcome"]))
		p, _ := e["params"].(map[string]any)
		params = append(params, p)
	}
	return out, params
}

// cancelCap is the Power menu's "Cancel pending" for laptop, as the page gets it.
func cancelCap(t *testing.T, a *approvalEnv, cookie string) (available bool, pending any) {
	t.Helper()
	r := a.do("GET", "/api/systems/laptop/power", nil, "Cookie", cookie)
	if r.code != 200 {
		t.Fatalf("the power menu: %d %s", r.code, r.raw)
	}
	for _, x := range r.body["actions"].([]any) {
		if c := x.(map[string]any); c["action"] == "cancel" {
			return c["available"] == true, r.body["pending"]
		}
	}
	t.Fatal("no cancel in the power menu")
	return false, nil
}

func TestACancelAfterAPowerRequestWithoutAnAnswerReachesTheAgent(t *testing.T) {
	for _, check := range []bool{false, true} {
		t.Run(fmt.Sprintf("check on %v", check), func(t *testing.T) {
			a := newApprovalEnv(t)
			if check {
				a.require(t, a.laptop)
			}
			fresh := a.cookie(controller(), 0)
			hdr := []string{"Cookie", fresh, "X-Requested-With", "fairy", "Content-Type", "application/json"}
			cancel := func() resp {
				return a.do("POST", "/api/systems/laptop/power", map[string]any{"action": "cancel"}, hdr...)
			}
			l := &lostPower{}
			l.on(a)

			// as before: no countdown shows and no request of the panel is open: the panel answers itself
			if r := cancel(); r.code != 409 || r.body["detail"] != "nothing pending" || l.sent() != 0 {
				t.Fatalf("a cancel with nothing pending: %d %s (%d sent)", r.code, r.raw, l.sent())
			}
			if ok, _ := cancelCap(t, a, fresh); ok {
				t.Fatal("Cancel pending is on offer with nothing pending")
			}

			// the shutdown gets the agent's 504
			l.shutdown = func() (int, jsonx.M) { return 504, jsonx.M{"error": agentLostShutdown} }
			const lost = "no answer (http 504): it may have arrived"
			if check { // through the machine's approval
				id, _, ok := approvalAsked(a.do("POST", "/api/systems/laptop/power", a.shutdownBody(), hdr...))
				if !ok {
					t.Fatal("the dialog of the shutdown")
				}
				r := a.do("POST", stepUpPath, map[string]any{"pending_id": id, "next": "/"}, hdr...)
				back := a.iss.Approve(t, fmt.Sprint(r.body["url"]))
				if cb := a.do("GET", back.RequestURI(), nil, "Cookie", fresh); cb.code != 303 {
					t.Fatalf("callback: %d %s", cb.code, cb.raw)
				}
				pr, _ := a.do("GET", "/api/me", nil, "Cookie", fresh).body["pending_result"].(map[string]any)
				if pr == nil || pr["ok"] != false || !strings.HasPrefix(fmt.Sprint(pr["message"]), lost) {
					t.Fatalf("the shutdown's result: %v", pr)
				}
			} else if r := a.do("POST", "/api/systems/laptop/power", a.shutdownBody(), hdr...); r.code != 502 || r.body["detail"] != lost {
				t.Fatalf("the shutdown: %d %s", r.code, r.raw)
			}
			a.poll()
			if x := a.do("GET", "/api/systems/laptop", nil, "Cookie", fresh); x.body["power_pending"] != nil {
				t.Fatalf("the page shows a countdown: %s", x.raw)
			}
			// the Power menu offers the cancel, and shows no countdown that nobody saw
			if ok, pending := cancelCap(t, a, fresh); !ok || pending != nil {
				t.Fatalf("Cancel pending after a request without an answer: available %v, pending %v", ok, pending)
			}
			vc := a.cookie(viewer, 0)
			if _, _, err := a.s.Access.SetRole(viewer.Sub, "viewer", []string{}, "test", true); err != nil {
				t.Fatal(err)
			}
			// a viewer is offered nothing (one without this host's site does not see the host at all)
			if r := a.do("GET", "/api/systems/laptop/power", nil, "Cookie", vc); r.code == 200 {
				if ok, _ := cancelCap(t, a, vc); ok {
					t.Fatal("a viewer is offered Cancel pending")
				}
			} else if r.code != 404 {
				t.Fatalf("a viewer's power menu: %d %s", r.code, r.raw)
			}

			// 1. the agent's answer of that minute: its words, an error, never "cancelled"
			l.cancel = func() (int, jsonx.M) { return 409, jsonx.M{"error": agentCancelAgain} }
			r := cancel()
			if l.sent() != 1 || r.code < 400 || r.code > 499 || r.body["detail"] != agentCancelAgain || strings.Contains(string(r.raw), "cancelled") {
				t.Fatalf("the cancel in the agent's minute: %d sent, %d %s", l.sent(), r.code, r.raw)
			}
			if _, _, asked := approvalAsked(r); asked || keptID(a.env, fresh) != "" {
				t.Fatalf("the cancel asked for a confirmation: %s", r.raw)
			}
			// 2. sent again, as the agent says: it answers "ok, nothing pending". The panel saw no countdown and
			// says what the agent said, not "cancelled"; no notification of a cancel
			notes := noteCount(a)
			l.cancel = nil
			r = cancel()
			msg, _ := r.body["message"].(string)
			if l.sent() != 2 || r.code != 200 || !strings.HasPrefix(msg, "Cancel sent to laptop, and its agent answered that nothing is pending there now.") ||
				!strings.Contains(msg, "If the shutdown request that got no answer arrives after this") || strings.Contains(string(r.raw), "cancelled") {
				t.Fatalf("the cancel sent again: %d sent, %d %s", l.sent(), r.code, r.raw)
			}
			if n := noteCount(a); n != notes {
				t.Fatalf("a cancel of a countdown nobody saw made %d notifications", n-notes)
			}
			outs, params := cancelOutcomes(a)
			var nothing, again, unseen int
			for i, o := range outs {
				switch {
				case strings.HasPrefix(o, "error 409: nothing pending"):
					nothing++
				case strings.HasPrefix(o, "error 4") && strings.Contains(o, "may still be scheduled"):
					again++
				case o == "ok (no countdown was shown; the agent answered that nothing is pending)" && params[i]["pending"] == "not shown (a shutdown request got no answer)":
					unseen++
				}
			}
			if len(outs) != 3 || nothing != 1 || again != 1 || unseen != 1 {
				t.Fatalf("the audit log: %v %v", outs, params)
			}
			// 3. the request can still arrive: the cancel stays on offer and goes through again
			if ok, _ := cancelCap(t, a, fresh); !ok {
				t.Fatal("Cancel pending is gone while the request can still arrive")
			}
			if r := cancel(); r.code != 200 || l.sent() != 3 {
				t.Fatalf("a third cancel: %d %s", r.code, r.raw)
			}

			// 4. the kill switch: a signed-in admin's cancel passes it, as every cancel; a viewer and a request
			// without the CSRF header are refused, and nothing of theirs reaches the agent
			if err := a.s.Poller.Admin.SetControlDisabled(true, "test", "the kill switch"); err != nil {
				t.Fatal(err)
			}
			if r := cancel(); r.code != 200 || l.sent() != 4 {
				t.Fatalf("with the kill switch on: %d %s", r.code, r.raw)
			}
			off := 0
			_, params = cancelOutcomes(a)
			for _, p := range params {
				if strings.Contains(fmt.Sprint(p["control_off"]), "control is disabled by an administrator") {
					off++
				}
			}
			if off != 1 {
				t.Fatalf("the audit entry of the cancel with the kill switch on: %v", params)
			}
			// an API token's cancel does not pass the kill switch, here as everywhere (423)
			ctl, _, err := a.s.Tokens.Create("ops", "system", "control", "cli:test", 0)
			if err != nil {
				t.Fatal(err)
			}
			if r := a.do("POST", "/api/systems/laptop/power", map[string]any{"action": "cancel"}, "Authorization", "Bearer "+ctl, "Content-Type", "application/json"); r.code != 423 {
				t.Fatalf("a token's cancel with the kill switch on: %d %s", r.code, r.raw)
			}
			if r := a.do("POST", "/api/systems/laptop/power", map[string]any{"action": "cancel"}, "Cookie", vc, "X-Requested-With", "fairy", "Content-Type", "application/json"); r.code != 403 {
				t.Fatalf("a viewer's cancel: %d %s", r.code, r.raw)
			}
			if r := a.do("POST", "/api/systems/laptop/power", map[string]any{"action": "cancel"}, "Cookie", fresh, "Content-Type", "application/json"); r.code != 403 {
				t.Fatalf("without the CSRF header: %d %s", r.code, r.raw)
			}
			if l.sent() != 4 {
				t.Fatal("a refused cancel reached the agent")
			}
			if err := a.s.Poller.Admin.SetControlDisabled(false, "test", ""); err != nil {
				t.Fatal(err)
			}

			// 5. when the time in which the request can arrive is over (its delay of 10 s and 5 minutes), the
			// panel answers itself again
			at := time.Now().Add(10*time.Second + cancelUnseenFor + time.Second)
			a.s.codec.now = func() time.Time { return at }
			a.s.stopRL = newRateLimiter(100)
			fresh2 := a.cookie(controller(), 0)
			if ok, _ := cancelCap(t, a, fresh2); ok {
				t.Fatal("Cancel pending is still on offer after the time")
			}
			if r := a.do("POST", "/api/systems/laptop/power", map[string]any{"action": "cancel"}, "Cookie", fresh2, "X-Requested-With", "fairy", "Content-Type", "application/json"); r.code != 409 || r.body["detail"] != "nothing pending" || l.sent() != 4 {
				t.Fatalf("a cancel after the time: %d %s (%d sent)", r.code, r.raw, l.sent())
			}
		})
	}
}

// What does not open the door: the agent's own "no" to the shutdown (nothing can arrive after it), a request
// that never left the panel, and a wake without an answer (there is nothing to cancel). And a countdown that
// shows after the lost request is cancelled the ordinary way, with the ordinary words; after that the door is
// shut again.
func TestOnlyAPowerRequestThatMayHaveArrivedLetsACancelThrough(t *testing.T) {
	a := newApprovalEnv(t)
	fresh := a.cookie(controller(), 0)
	hdr := []string{"Cookie", fresh, "X-Requested-With", "fairy", "Content-Type", "application/json"}
	cancel := func() resp {
		return a.do("POST", "/api/systems/laptop/power", map[string]any{"action": "cancel"}, hdr...)
	}
	l := &lostPower{}
	l.on(a)
	// the agent's own refusal
	l.shutdown = func() (int, jsonx.M) { return 409, jsonx.M{"error": "a reboot is already pending; cancel it first"} }
	if r := a.do("POST", "/api/systems/laptop/power", a.shutdownBody(), hdr...); r.code < 400 || r.code > 499 {
		t.Fatalf("a refused shutdown: %d %s", r.code, r.raw)
	}
	if ok, _ := cancelCap(t, a, fresh); ok {
		t.Fatal("Cancel pending after the agent's own no")
	}
	if r := cancel(); r.code != 409 || r.body["detail"] != "nothing pending" || l.sent() != 0 {
		t.Fatalf("a cancel after the agent's own no: %d %s (%d sent)", r.code, r.raw, l.sent())
	}
	// a gateway's 502: it may have arrived
	l.shutdown = func() (int, jsonx.M) { return 502, nil }
	if r := a.do("POST", "/api/systems/laptop/power", a.shutdownBody(), hdr...); r.code != 502 || r.body["detail"] != "no answer (http 502): it may have arrived" {
		t.Fatalf("a shutdown behind a gateway: %d %s", r.code, r.raw)
	}
	if ok, _ := cancelCap(t, a, fresh); !ok {
		t.Fatal("no Cancel pending after a shutdown without an answer")
	}
	// the countdown shows after all: the ordinary cancel, the ordinary words, and the door shuts
	a.pendingAt(time.Now().Add(40 * time.Second))
	if r := cancel(); r.code != 200 || r.body["message"] != "The shutdown of laptop is cancelled." || l.sent() != 1 {
		t.Fatalf("the cancel of the countdown that showed: %d %s", r.code, r.raw)
	}
	a.b.Mu.Lock()
	delete(a.b.Metrics, "laptop")
	a.b.Mu.Unlock()
	a.poll()
	if x := a.do("GET", "/api/systems/laptop", nil, "Cookie", fresh); x.body["power_pending"] != nil {
		t.Fatalf("the page still shows the countdown: %s", x.raw)
	}
	if ok, _ := cancelCap(t, a, fresh); ok {
		t.Fatal("Cancel pending stays on offer after the countdown was cancelled")
	}
	if r := cancel(); r.code != 409 || r.body["detail"] != "nothing pending" || l.sent() != 1 {
		t.Fatalf("a cancel after the cancel: %d %s (%d sent)", r.code, r.raw, l.sent())
	}
}
