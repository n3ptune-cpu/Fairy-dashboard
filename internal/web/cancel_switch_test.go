package web

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/audit"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/powerctl"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// Audit 5, round 3 (r3-p2-06): the kill switch, or the power switch of one host, flipped while a shutdown
// countdown runs. The machine went off, and nobody could call the countdown off from the panel: the route
// refused the cancel with 423 and powerctl with 409, like every other action. A cancel only ever stops a power
// action, so it passes both switches now; nothing else does.

// counting gives the laptop a shutdown countdown that its agent reports.
func counting(t *testing.T, e *env) {
	t.Helper()
	e.b.Mu.Lock()
	e.b.Metrics["laptop"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["power_control"] = jsonx.M{"enabled": true, "min_delay_s": 10.0,
			"pending": jsonx.M{"action": "shutdown", "at": float64(time.Now().Add(2 * time.Minute).Unix()), "delay_s": 120.0, "requested_by": "Alice"}}
		return m, nil
	}
	e.b.Mu.Unlock()
	e.s.Poller.PollOnce(context.Background())
}

// switchEnv is a panel whose laptop counts down, with what the tests below ask of it.
type switchEnv struct {
	*env
	c     string   // an admin's session
	admin []string // its headers for a write
}

func newSwitchEnv(t *testing.T) *switchEnv {
	e := setup(t)
	counting(t, e)
	e.b.Reply = func(_, path string) (int, jsonx.M, error) {
		if path == "/power" {
			return 200, jsonx.M{"action": "cancel", "pending": nil}, nil
		}
		return 0, nil, nil
	}
	c := e.cookie(controller(), 0)
	return &switchEnv{env: e, c: c, admin: []string{"Cookie", c, "X-Requested-With", "fairy"}}
}

// sent is how many power requests went to the laptop's agent.
func (e *switchEnv) sent() int {
	e.b.Mu.Lock()
	defer e.b.Mu.Unlock()
	n := 0
	for _, r := range e.b.Requests {
		if strings.HasPrefix(r, "laptop POST /power") {
			n++
		}
	}
	return n
}

func (e *switchEnv) post(action string, hdr ...string) resp {
	return e.do("POST", "/api/systems/laptop/power", map[string]any{"action": action, "delay_s": 60, "confirm": "LAPTOP"}, hdr...)
}

// available is what the Power menu offers.
func (e *switchEnv) available() string {
	var on []string
	caps := e.do("GET", "/api/systems/laptop/power", nil, "Cookie", e.c)
	for _, a := range caps.body["actions"].([]any) {
		if a := a.(map[string]any); a["available"] == true {
			on = append(on, a["action"].(string))
		}
	}
	return strings.Join(on, " ")
}

// cancels are the audit entries of the cancels.
func (e *switchEnv) cancels() []map[string]any {
	entries, _ := audit.Read(e.s.Cfg.AuditLog, "", "", "", nil, 500)
	var out []map[string]any
	for _, en := range entries {
		if en["action"] == "power.cancel" {
			out = append(out, en)
		}
	}
	return out
}

func TestTheCancelOfACountdownPassesTheKillSwitchAndNothingElseDoes(t *testing.T) {
	for _, how := range []string{"the kill switch", "the power switch of the host"} {
		e := newSwitchEnv(t)
		var refused int
		var word string
		if how == "the kill switch" {
			if err := e.s.Poller.Admin.SetControlDisabled(true, "root (cli)", "stop everything"); err != nil {
				t.Fatal(err)
			}
			refused, word = 423, "control is disabled by an administrator"
		} else {
			if err := e.s.Poller.Admin.SetPowerDisabled("laptop", true); err != nil {
				t.Fatal(err)
			}
			refused, word = 409, "power control for this host is disabled by an administrator"
		}
		if got := e.available(); got != "cancel" {
			t.Errorf("%s: the Power menu offers %q, want only the cancel", how, got)
		}
		// nothing else passes: every other power action is refused, and nothing is sent
		for _, action := range powerctl.Actions {
			if action == "cancel" {
				continue
			}
			// (the kill switch refuses at the route, before anything else: 423. The host's switch refuses in
			// powerctl, 409 with its reason, unless the action is not there for this host anyway: then that is
			// the reason. Either way: refused, and nothing sent.)
			r := e.post(action, e.admin...)
			if (how == "the kill switch" && r.code != 423) || r.code < 400 || e.sent() != 0 {
				t.Errorf("%s: %s: %d %s (requests sent: %d)", how, action, r.code, r.raw, e.sent())
			}
			if action == "shutdown" && (r.code != refused || !strings.Contains(r.raw2(), word)) {
				t.Errorf("%s: shutdown: %d %s", how, r.code, r.raw)
			}
		}
		// the cancel is still an admin's action with the CSRF header
		if r := e.post("cancel", "Cookie", e.cookie(viewer, 0), "X-Requested-With", "fairy"); r.code != 403 {
			t.Errorf("%s: a viewer's cancel: %d", how, r.code)
		}
		if r := e.post("cancel", "Cookie", e.c); r.code != 403 {
			t.Errorf("%s: a cancel without the CSRF header: %d", how, r.code)
		}
		if e.sent() != 0 || len(e.cancels()) != 0 {
			t.Fatalf("%s: a refused cancel was sent", how)
		}
		// and it goes through, also with a sign-in that is ten minutes old: a cancel is never asked to confirm
		// (approval_cancel_test.go)
		r := e.post("cancel", "Cookie", e.cookie(controller(), 10*time.Minute), "X-Requested-With", "fairy")
		if r.code != 200 || r.body["message"] != "The shutdown of laptop is cancelled." || e.sent() != 1 {
			t.Fatalf("%s: the page's Cancel for the countdown: %d %s (requests sent: %d)", how, r.code, r.raw, e.sent())
		}
		en := e.cancels()
		if len(en) != 1 || en[0]["outcome"] != "ok" || !strings.Contains(fmt.Sprint(en[0]["params"].(map[string]any)["control_off"]), word) {
			t.Errorf("%s: the audit entry of the cancel does not say that control was off: %v", how, en)
		}
	}
	// with control on, the audit entry has no such word
	e := newSwitchEnv(t)
	if r := e.post("cancel", e.admin...); r.code != 200 {
		t.Fatal(r.code, string(r.raw))
	}
	if en := e.cancels(); len(en) != 1 || en[0]["params"].(map[string]any)["control_off"] != nil {
		t.Errorf("control is on, and the audit entry says: %v", en)
	}
	// nothing pending: the kill switch does not turn the cancel into something else
	e = newSwitchEnv(t)
	e.b.Mu.Lock()
	e.b.Metrics["laptop"] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
	e.b.Mu.Unlock()
	e.s.Poller.PollOnce(context.Background())
	_ = e.s.Poller.Admin.SetControlDisabled(true, "root (cli)", "stop everything")
	if r := e.post("cancel", e.admin...); r.code != 409 || e.sent() != 0 || e.available() != "" {
		t.Errorf("a cancel with nothing pending: %d %s (the menu offers %q)", r.code, r.raw, e.available())
	}
	// the cancels have their own request budget, with the kill switch on as without it
	limited := false
	for i := 0; i < stopsPerMin+5 && !limited; i++ {
		limited = e.post("cancel", e.admin...).code == 429
	}
	if !limited {
		t.Errorf("%d cancels in a row, and none was refused by the request limit", stopsPerMin+5)
	}
	// the other control routes are refused by the kill switch as ever (routes_test.go has every route)
	if r := e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 5}, e.admin...); r.code != 423 {
		t.Errorf("polling rate with the kill switch on: %d", r.code)
	}
	if r := e.do("POST", "/api/admin/updates/laptop", map[string]any{}, e.admin...); r.code != 423 {
		t.Errorf("update agent with the kill switch on: %d", r.code)
	}
}

// With the real poller, powerctl and run (the auditor's proof, TestR3P2TheKillSwitchLeavesACountdownNobody-
// CanCancel): the switch is flipped while a run's countdown runs on a machine.
func TestWakeUpdateASwitchFlippedDuringACountdown(t *testing.T) {
	for _, how := range []string{"the kill switch", "the power switch of the host, then the page's Cancel", "the power switch of the host, then Cancel run"} {
		e, f := lostFarm(t, "")
		r, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3600; i++ {
			f.tick()
			if a, _ := e.s.WakeRuns.Snapshot(); a != nil && hostOf(a, "pc-a").State == wakerun.StShutting {
				break
			}
		}
		for i := 0; i < 10; i++ {
			f.tick()
		}
		want := ""
		switch how {
		case "the kill switch": // "stop everything": the run stops, and calls its own countdown off
			if err := e.s.Poller.Admin.SetControlDisabled(true, "root (cli)", "stop everything"); err != nil {
				t.Fatal(err)
			}
			want = "the run stopped (control is disabled by an administrator): its shutdown countdown was called off. It came online after the wake and it is ON."
		case "the power switch of the host, then the page's Cancel": // the run goes on; an admin presses Cancel
			if err := e.s.Poller.Admin.SetPowerDisabled("pc-a", true); err != nil {
				t.Fatal(err)
			}
			f.tick()
			var sys model.System
			for _, x := range e.s.Poller.Snapshot() {
				if x.ID == "pc-a" {
					sys = x
				}
			}
			req := powerctl.Request{Action: "cancel"}
			_ = req.Validate()
			if _, err := powerctl.Execute(context.Background(), e.s.Poller, sys, req, "Alice"); err != nil {
				t.Errorf("%s: the page's Cancel for the countdown (pending: %v): %v", how, sys.PowerPending != nil, err)
			}
			want = "Its shutdown was cancelled, in the panel or on the machine, so it is ON."
		default:
			if err := e.s.Poller.Admin.SetPowerDisabled("pc-a", true); err != nil {
				t.Fatal(err)
			}
			f.tick()
			if _, err := e.s.WakeRuns.CancelRun(r.ID, "Alice", false); err != nil {
				t.Fatal(err)
			}
			want = "The run was cancelled by Alice: its shutdown countdown was called off. It came online after the wake and it is ON."
		}
		h := hostOf(f.finish(), "pc-a")
		for i := 0; i < 180; i++ {
			f.tick()
		}
		if !f.on["pc-a"] {
			t.Errorf("%s: the countdown the run started was not called off, and the machine went off: %s", how, h.Text)
		}
		if h.State != wakerun.StLeftOn || h.On != wakerun.OnYes || !strings.Contains(h.Text, want) {
			t.Errorf("%s: [%s] power %q: %s", how, h.State, h.On, h.Text)
		}
		if got := strings.Join(f.requests(), ", "); got != "nas /wol, pc-a /update, pc-a /power, pc-a /power" {
			t.Errorf("%s: requests: %s", how, got)
		}
		if how != "the power switch of the host, then the page's Cancel" { // the run's own cancel is in the audit log, with the word
			entries, _ := audit.Read(e.s.Cfg.AuditLog, "", "", "", nil, 500)
			said := false
			for _, en := range entries {
				p, _ := en["params"].(map[string]any)
				said = said || (en["action"] == "power.cancel" && en["outcome"] == "ok" && strings.Contains(fmt.Sprint(p["control_off"]), "disabled by an administrator"))
			}
			if !said {
				t.Errorf("%s: the audit entry of the run's cancel does not say that control was off", how)
			}
		}
	}
}
