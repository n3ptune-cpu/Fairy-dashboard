package web

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// r1-b2-01 (audit 5): on a machine with the check on, Cancel of a countdown asked for a full sign-in at Pocket
// ID ("Confirm it's you"): the shutdown's own confirmation is the machine's approval, which opens no window in
// the panel. A ten-second shutdown could not be stopped, and the page then said "cancelled" although the
// machine had switched off.
//
// Cancel now needs an admin and the CSRF header, and nothing else, on machines with and without the check. It is
// the only one: shutdown, restart, sleep, fan changes and updates still ask. (The kill switch does not refuse a
// browser session's cancel either, and refuses an API token's: cancel_switch_test.go, cancel_token_test.go.)

// pendingAt makes laptop report a shutdown that is pending for the time at.
func (a *approvalEnv) pendingAt(at time.Time) {
	a.b.Mu.Lock()
	a.b.Metrics["laptop"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["agent_version"] = "41-aaaaaaa"
		m["features"].(jsonx.M)["self_update"] = true
		m["self_update"] = jsonx.M{"state": "idle", "since": 1.0}
		m["power_control"] = jsonx.M{"enabled": true, "min_delay_s": 10.0, "pending": jsonx.M{"action": "shutdown", "at": float64(at.Unix()), "delay_s": 10.0, "requested_by": "Alice"}}
		return m, nil
	}
	a.b.Mu.Unlock()
	a.poll()
}

func TestCancelNeedsNoConfirmationAndNoApproval(t *testing.T) {
	for name, withCheck := range map[string]bool{"a machine with the check": true, "a machine without the check": false} {
		t.Run(name, func(t *testing.T) {
			a := newApprovalEnv(t)
			if withCheck {
				a.require(t, a.laptop)
			}
			a.pendingAt(time.Now().Add(8 * time.Second)) // a ten-second shutdown, two seconds in
			if x := a.do("GET", "/api/systems/laptop", nil, "Cookie", a.admin); x.body["power_pending"] == nil {
				t.Fatalf("the page shows no countdown: %s", x.raw)
			}
			asked, tokens := len(a.laptop.Asked()), len(a.iss.Asked())
			// the admin's sign-in is an hour old: every other control action would ask
			r := a.post("/api/systems/laptop/power", map[string]any{"action": "cancel"})
			if r.code != 200 || !strings.Contains(fmt.Sprint(r.body["message"]), "The shutdown of laptop is cancelled.") {
				t.Fatalf("cancel with a sign-in that is an hour old: %d %s", r.code, r.raw)
			}
			if keptID(a.env, a.admin) != "" || len(a.iss.Asked()) != tokens {
				t.Fatal("cancel was kept for a confirmation, or somebody was sent to Pocket ID")
			}
			sent := a.laptop.Asked()[asked:]
			if len(sent) != 1 || sent[0].Path != "/power" || sent[0].Body["action"] != "cancel" || sent[0].Body["approval"] != nil {
				t.Fatalf("what the machine was sent: %+v", sent)
			}
			entries := a.auditEntries("", "power.cancel")
			if len(entries) != 1 || entries[0]["outcome"] != "ok" || entries[0]["params"].(map[string]any)["delay_s"] != nil || entries[0]["params"].(map[string]any)["pending"] != "shutdown" {
				t.Fatalf("audit: %v", entries)
			}
			// it is cancel only: everything else still asks, with the same session
			for what, c := range map[string]struct {
				path string
				body any
			}{
				"shutdown": {"/api/systems/laptop/power", map[string]any{"action": "shutdown", "delay_s": 10, "confirm": "laptop"}},
				"restart":  {"/api/systems/laptop/power", map[string]any{"action": "reboot", "delay_s": 10, "confirm": "laptop"}},
				"sleep":    {"/api/systems/laptop/power", map[string]any{"action": "sleep", "delay_s": 10, "confirm": "laptop"}},
				"update":   {"/api/admin/updates/laptop", map[string]any{}},
				"fan mode": {"/api/systems/tower/fan/mode", map[string]any{"mode": "full"}},
			} {
				n := len(a.laptop.Passed()) + len(a.tower.Passed())
				r := a.post(c.path, c.body)
				d, _ := r.body["detail"].(map[string]any)
				if _, _, viaMachine := approvalAsked(r); r.code != 401 || d["code"] != "step_up_required" || (withCheck && what != "fan mode" && what != "sleep" && !viaMachine) {
					if !(what == "sleep" && r.code != 200) { // (a machine that cannot sleep refuses before anybody is asked)
						t.Errorf("%s must still ask: %d %s", what, r.code, r.raw)
					}
				}
				if len(a.laptop.Passed())+len(a.tower.Passed()) != n {
					t.Errorf("%s was sent without a confirmation", what)
				}
			}
		})
	}
}

// Who may cancel: an admin. A viewer is refused, and so is a request without the CSRF header. An admin's API
// token with control scope may, as before (a token is never asked to confirm).
func TestCancelIsForAdmins(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop)
	a.pendingAt(time.Now().Add(time.Minute))
	v := viewer
	v.Sub = "viewer-9"
	body := map[string]any{"action": "cancel"}
	n := len(a.laptop.Asked())
	if r := a.do("POST", "/api/systems/laptop/power", body, "Cookie", a.cookie(v, 0), "X-Requested-With", "fairy", "Content-Type", "application/json"); r.code != 403 {
		t.Fatalf("a viewer's cancel: %d %s", r.code, r.raw)
	}
	if r := a.do("POST", "/api/systems/laptop/power", body, "Cookie", a.admin, "Content-Type", "application/json"); r.code != 403 {
		t.Fatalf("without the CSRF header: %d %s", r.code, r.raw)
	}
	if r := a.do("POST", "/api/systems/laptop/power", body, "Cookie", a.admin, "X-Requested-With", "fairy", "Origin", "https://evil.example", "Content-Type", "application/json"); r.code != 403 {
		t.Fatalf("from another origin: %d %s", r.code, r.raw)
	}
	if r := a.do("POST", "/api/systems/laptop/power", body, "Content-Type", "application/json"); r.code != 401 {
		t.Fatalf("without a session: %d", r.code)
	}
	if len(a.laptop.Asked()) != n {
		t.Fatal("a refused cancel reached the machine")
	}
	if err := a.s.Poller.Admin.SetControlDisabled(true, "test", "the kill switch"); err != nil {
		t.Fatal(err)
	}
	// the kill switch does not refuse the cancel of a signed-in admin (a countdown that runs when the switch is
	// flipped can still be called off), and the audit entry says that control was off
	if r := a.post("/api/systems/laptop/power", body); r.code != 200 {
		t.Fatalf("with the kill switch on, an admin's cancel from the page: %d %s", r.code, r.raw)
	}
	if e := a.auditEntries("", "power.cancel"); len(e) != 1 || e[0]["outcome"] != "ok" || !strings.Contains(fmt.Sprint(e[0]["params"].(map[string]any)["control_off"]), "control is disabled by an administrator") {
		t.Fatalf("audit of the cancel with the kill switch on: %v", e)
	}
}

// The page says what is true. The agent answers a cancel with 200 also when the countdown has run out (it
// keeps it for another minute). When the time was up before the cancel arrived, the answer is an error that
// says so; right at the end it says to look.
func TestCancelSaysWhetherItCameInTime(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop)
	a.pendingAt(time.Now().Add(-6 * time.Second)) // the shutdown started six seconds ago
	r := a.post("/api/systems/laptop/power", map[string]any{"action": "cancel"})
	if msg := fmt.Sprint(r.body["detail"]); r.code != 409 || !strings.Contains(msg, "Too late: the shutdown of laptop had started") || !strings.Contains(msg, "Nothing was cancelled.") {
		t.Fatalf("a cancel after the countdown ran out: %d %s", r.code, r.raw)
	}
	if e := a.auditEntries("", "power.cancel"); len(e) != 1 || !strings.Contains(fmt.Sprint(e[0]["outcome"]), "too late") {
		t.Fatalf("audit: %v", e)
	}
	a = newApprovalEnv(t)
	a.require(t, a.laptop)
	a.pendingAt(time.Now())
	r = a.post("/api/systems/laptop/power", map[string]any{"action": "cancel"})
	if r.code != 200 || !strings.Contains(fmt.Sprint(r.body["message"]), "Cancel sent as the countdown of the shutdown of laptop ran out: look whether it is still on.") {
		t.Fatalf("a cancel at the end of the countdown: %d %s", r.code, r.raw)
	}
}

// A machine whose binding is an alarm gets no approval and no request, but a countdown on it can still be
// stopped.
func TestCancelWorksOnAMachineWithAnAlarm(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop)
	a.setTrustID(t, a.laptop, otherID)
	a.pendingAt(time.Now().Add(time.Minute))
	if a.s.Poller.BindingOf("laptop").OK() {
		t.Fatal("the test needs a flagged machine")
	}
	if r := a.post("/api/systems/laptop/power", map[string]any{"action": "cancel"}); r.code != 200 {
		t.Fatalf("%d %s", r.code, r.raw)
	}
}
