package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// Tests for lines of the panel half of runs that nothing held in place (independent review of 270e7ab: its
// mutants of these lines survived).

// What an answer to the ticket's request means for the run: an error of the machine's own (5xx) that is not
// one of its two "nothing was armed" is "no answer" (it may have got as far as arming), never a refusal.
func TestAnErrorOfTheMachineIsNotReadAsARefusalOfTheTicket(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   jsonx.M
		code   string
		text   string
	}{
		{"500", 500, jsonx.M{"error": "boom"}, wakerun.CodeNoAnswer, "http 500 from its agent"},
		{"500 with a code that is not one of the two", 500, jsonx.M{"code": "internal", "error": "x"}, wakerun.CodeNoAnswer, "http 500 from its agent"},
		{"503 run_unavailable", 503, jsonx.M{"code": api.CodeRunUnavailable, "error": "x"}, wakerun.CodeRunRefused, "it cannot read or write its run record"},
		{"409 run_refused", 409, jsonx.M{"code": api.CodeRunRefused, "reason": api.RunReasonLimits, "error": "x"}, wakerun.CodeRunRefused, "power_control.min_delay_s"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, run := started(t, "pc-a")
			r.gb.Relay("pc-a", func(path string, body any) (int, jsonx.M) {
				if path == api.PathRunArm {
					return c.status, c.body
				}
				return 0, nil
			})
			_, err := runOps{r.s}.Arm(context.Background(), "pc-a", run.ID)
			var oe *wakerun.OpError
			if !asOp(err, &oe) || oe.Code != c.code || !strings.Contains(oe.Text, c.text) {
				t.Fatalf("Arm: %#v", err)
			}
		})
	}
}

// What an answer to the confirmation's request means: "confirmed" only when the machine says, for this run,
// that it is confirmed; a 200 that says something else, and a request without an answer, are "no answer" (it
// may have arrived; it is not sent again; the machine's word decides).
func TestOnlyTheMachinesConfirmedCountsAsConfirmed(t *testing.T) {
	for _, c := range []struct {
		name string
		ans  func(run string) (int, jsonx.M)
		line string
	}{
		{"200 with another state", func(run string) (int, jsonx.M) {
			return 200, jsonx.M{"run": run, "state": api.RunArmed, "within_s": 1560.0}
		}, "PC-A: no answer (its agent answered, but not that this run is confirmed): it may have arrived, and it is not sent again"},
		{"no answer", func(string) (int, jsonx.M) { return 504, jsonx.M{} }, "PC-A: no answer ("},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, run := started(t, "pc-a")
			r.until(t, "asked", 300, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a != nil && a.Approval == "needed" })
			r.gb.Relay("pc-a", func(path string, body any) (int, jsonx.M) {
				if path == api.PathRunGo {
					return c.ans(run.ID)
				}
				return 0, nil
			})
			_, res := r.approved(t, "POST", "/api/admin/wake-update/"+run.ID+"/confirm", map[string]any{})
			items, _ := res["items"].([]any)
			if res["ok"] != false || len(items) != 1 || !strings.HasPrefix(items[0].(string), c.line) {
				t.Fatalf("the confirmation's result: %v", res)
			}
			if h := r.host("pc-a"); h.Confirm != wakerun.ConfirmUnanswered {
				t.Fatalf("pc-a: confirm %q %q", h.Confirm, h.Text)
			}
			if cur, _ := r.s.WakeRuns.Snapshot(); cur.Approval == "given" {
				t.Fatal("the run counts as confirmed")
			}
			r.tick(20)
			if n := len(r.sent("pc-a", "/update")) + len(r.sent("pc-a", "/power")); n != 0 {
				t.Fatalf("%d update or power requests went out to a machine that is not confirmed", n)
			}
		})
	}
}

// The numbers a run's passkeys sign stay inside what a machine takes, whatever the configuration says:
// within_s at most 30 minutes; wait_s at most an hour, and then the time to confirm gives way.
func TestTheRunsNumbersStayInsideTheMachinesLimits(t *testing.T) {
	r := newRunRig(t, "pc-a")
	base := r.s.runTimings()
	if base.ValidS != 420 || base.WaitS != 300+600+60 || base.NoticeS != 120 || base.WithinS != 1500+60 || base.ConfirmWaitS != 600 {
		t.Fatalf("the defaults: %+v", base)
	}
	r.s.WakeRuns.Settings.Settle = 20 * time.Minute
	if tm := r.s.runTimings(); tm.WithinS != api.RunWithinMaxS {
		t.Fatalf("a long settle time: within_s %d", tm.WithinS)
	}
	r.s.WakeRuns.Settings.Settle = time.Minute
	r.s.WakeRuns.Settings.OnlineTimeout = 30 * time.Minute
	r.s.Cfg.Approval.RunApprovalWaitS = 3000
	if tm := r.s.runTimings(); tm.WaitS != api.RunWaitMaxS || tm.ConfirmWaitS != 3600-1800-60 {
		t.Fatalf("a long online timeout and a long wait: wait_s %d, confirm_wait_s %d", tm.WaitS, tm.ConfirmWaitS)
	}
	r.s.WakeRuns.Settings.OnlineTimeout = 59 * time.Minute
	if tm := r.s.runTimings(); tm.WaitS != api.RunWaitMaxS || tm.ConfirmWaitS != 60 {
		t.Fatalf("an online timeout of almost an hour: wait_s %d, confirm_wait_s %d", tm.WaitS, tm.ConfirmWaitS)
	}
}

// A machine with the check whose shutdown countdown is longer than a confirmation can name (10 minutes) is
// refused when the run is asked for, before anybody is asked for a passkey.
func TestALongShutdownCountdownIsRefusedBeforeThePasskey(t *testing.T) {
	r := newRunRig(t, "pc-a")
	r.s.WakeRuns.Settings.ShutdownDelay = 660 * time.Second
	x := r.req("POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}})
	if _, _, asked := approvalAsked(x); asked || x.code != 400 || !strings.Contains(string(x.raw), "takes a shutdown countdown of at most 10 minutes; its countdown is 11 minutes") {
		t.Fatalf("the start: %d %s", x.code, x.raw)
	}
	if a, _ := r.s.WakeRuns.Snapshot(); a != nil {
		t.Fatal("a run was started")
	}
}

// The kept token does not outlive its run: a run that ends before the ticket's time is over (its machine never
// came up) drops it, and its bytes are wiped.
func TestTheKeptTicketIsDroppedWhenTheRunEnds(t *testing.T) {
	r := newRunRig(t, "pc-a")
	r.f.wakes = false // the wake packets do nothing: pc-a never comes up, and the run gives up after online_timeout_s
	if _, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}}); res["ok"] != true {
		t.Fatal(res)
	}
	r.s.tickets.mu.Lock()
	bytes := r.s.tickets.cur.token
	r.s.tickets.mu.Unlock()
	run, _ := r.s.WakeRuns.Snapshot()
	r.until(t, "the run over", 400, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a == nil })
	if since := r.f.now.Sub(run.StartedAt); since >= 420*time.Second {
		t.Fatalf("the setup: the run ended %s after its start, when the ticket's time was over anyway", since)
	}
	if held, open := r.s.tickets.held(); held != "" || open != 0 {
		t.Fatalf("after the run the keeper still holds %q %d", held, open)
	}
	for _, b := range bytes {
		if b != 0 {
			t.Fatal("the token's bytes are still in memory after the run")
		}
	}
}

// What a machine says about runs stays on record while it shows the check in another state (its helper is not
// answering: "unknown" says nothing about what its checker can do): it is still a machine a run can take.
func TestTheRecordOfRunsIsKeptWhileTheCheckIsNotReadable(t *testing.T) {
	r := newRunRig(t, "pc-a")
	r.b.Mu.Lock()
	r.f.on["pc-a"], r.f.booted["pc-a"] = true, r.f.now.Add(-time.Hour)
	r.b.Mu.Unlock()
	r.boot["pc-a"] = r.f.booted["pc-a"]
	r.tick(2)
	if rec := r.s.Poller.BindingOf("pc-a").Record; rec.Runs != api.ApprovalRuns {
		t.Fatalf("the setup: runs on record %d", rec.Runs)
	}
	block := r.m["pc-a"].Block()
	block["state"] = api.ApprovalUnknown
	delete(block, "runs")
	r.gb.Stale("pc-a", block)
	r.tick(3)
	if rec := r.s.Poller.BindingOf("pc-a").Record; rec.State != api.ApprovalUnknown || rec.Runs != api.ApprovalRuns {
		t.Fatalf("after an answer in the state %q: state %q, runs %d on record", api.ApprovalUnknown, rec.State, rec.Runs)
	}
}
