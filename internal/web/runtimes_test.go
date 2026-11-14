package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// The timings of "Update unpowered hosts" in force (runtimes.go): what was changed in the panel (the store,
// adminstate.RunTimings) counts for the numbers a passkey signs and for the run, with the same clamps as a
// timing of config.yaml; a run keeps the numbers it started with; and a start is refused when the timings are
// no longer the ones it was confirmed with.

// rtSet stores decisions as the panel's store has them after a confirmed change (the API's own rules for who
// may and what is confirmed are in runtimings_test.go).
func (e *env) rtSet(kv map[string]int) {
	e.t.Helper()
	if _, _, err := e.s.Poller.Admin.EditRunTimings("test admin", func(o *adminstate.RunTimings) error {
		for k, v := range kv {
			if v == 0 {
				o.Drop(k)
			} else {
				o.Set(k, v)
			}
		}
		return nil
	}); err != nil {
		e.t.Fatal(err)
	}
}

// A start is confirmed for the timings its dialog showed. The start is asked for (the panel keeps it), a
// timing is changed, the admin confirms: nothing is started, the answer says why, the audit log has it. The
// same start asked for again runs with the new timings.
func TestAStartIsRefusedWhenTheTimingsChangedSinceItWasAskedFor(t *testing.T) {
	for name, change := range map[string]map[string]int{
		"off_after_s lowered":       {config.KeyOffAfter: 300},
		"the countdown shortened":   {config.KeyShutdownDelay: 10},
		"the pause":                 {config.KeySettle: 10},
		"the wait for the machine":  {config.KeyOnlineTimeout: 1800},
		"the pacing":                {config.KeyRequestsPerMin: 30},
		"the second passkey's time": {config.KeyRunApprovalWait: 60},
	} {
		a := signedIn(t)
		e := a.e
		f := newFarm(t, e)
		f.optIn("pc-a")
		f.offline()
		r := e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a"}})
		if !stepUpRequired(r) {
			t.Fatalf("%s: the start: %d %s", name, r.code, r.raw)
		}
		e.rtSet(change)
		before := len(f.requests())
		if cb := a.confirm(pendingID(r), false); cb.code != 303 {
			t.Fatalf("%s: the confirmation: %d", name, cb.code)
		}
		res, _ := e.do("GET", "/api/me", nil, "Cookie", a.sess).body["pending_result"].(map[string]any)
		if res["ok"] != false || res["status"] != 409.0 || res["message"] != wuTimingsChanged {
			t.Fatalf("%s: the start after the change: %v", name, res)
		}
		if act, hist := e.s.WakeRuns.Snapshot(); act != nil || len(hist) != 0 || len(f.requests()) != before {
			t.Fatalf("%s: a run was started, or something was sent: %v %v", name, act, f.requests()[before:])
		}
		if st := e.auditEntries("", "wakeupdate.start"); len(st) != 1 || !strings.Contains(fmt.Sprint(st[0]["outcome"]), "the run timings were changed after the start was asked for; nothing was started") {
			t.Fatalf("%s: the audit entry: %v", name, st)
		}
		// asked for again: the dialog and the run have the timings in force now
		r = e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a"}})
		if cb := a.confirm(pendingID(r), false); cb.code != 303 {
			t.Fatalf("%s: the second confirmation: %d", name, cb.code)
		}
		act, _ := e.s.WakeRuns.Snapshot()
		if act == nil {
			t.Fatalf("%s: the start that was asked for again did not start", name)
		}
		got := wuTimes{set: act.Settings, confirmWait: e.s.wuTimesNow().confirmWait}
		for k, v := range change {
			if got.value(k) != v {
				t.Errorf("%s: the run's %s is %d, in force is %d", name, k, got.value(k), v)
			}
		}
	}
	// a start that is confirmed with nothing changed in between starts, also when timings were changed before it
	a := signedIn(t)
	e := a.e
	f := newFarm(t, e)
	f.optIn("pc-a")
	f.offline()
	e.rtSet(map[string]int{config.KeyOffAfter: 600, config.KeyShutdownDelay: 45})
	r := e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a"}})
	e.rtSet(map[string]int{config.KeyOffAfter: 600}) // a write that changes nothing
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal(cb.code)
	}
	if act, _ := e.s.WakeRuns.Snapshot(); act == nil || act.Settings.OffAfterS != 600 || act.Settings.ShutdownDelayS != 45 {
		t.Fatalf("a start with nothing changed in between: %+v", act)
	}
}

// The numbers a run's passkeys sign come from the timings in force, through the same clamps as config.yaml's:
// for every combination, a panel whose timings were changed in the panel signs exactly what a panel signs
// whose config.yaml has those values; wait_s never exceeds the hour a machine takes (the time to confirm
// gives way), within_s never the half hour.
func TestChangedTimingsGoThroughTheSameClampsAsConfigYAML(t *testing.T) {
	changed, plain := setup(t), setup(t)
	for _, c := range []struct {
		online, settle, wait int
		want                 wakerun.Ticket
	}{
		{300, 60, 600, wakerun.Ticket{ValidS: 420, WaitS: 960, NoticeS: 120, WithinS: 1560, ConfirmWaitS: 600}},
		{300, 60, 900, wakerun.Ticket{ValidS: 420, WaitS: 1260, NoticeS: 120, WithinS: 1560, ConfirmWaitS: 900}},
		{60, 10, 60, wakerun.Ticket{ValidS: 420, WaitS: 180, NoticeS: 120, WithinS: 1510, ConfirmWaitS: 60}},
		{1800, 600, 3600, wakerun.Ticket{ValidS: 420, WaitS: 3600, NoticeS: 120, WithinS: 1800, ConfirmWaitS: 1740}},
		{1800, 300, 1740, wakerun.Ticket{ValidS: 420, WaitS: 3600, NoticeS: 120, WithinS: 1800, ConfirmWaitS: 1740}},
		{1800, 299, 1739, wakerun.Ticket{ValidS: 420, WaitS: 3599, NoticeS: 120, WithinS: 1799, ConfirmWaitS: 1739}},
		{600, 60, 3600, wakerun.Ticket{ValidS: 420, WaitS: 3600, NoticeS: 120, WithinS: 1560, ConfirmWaitS: 2940}},
		{60, 600, 3600, wakerun.Ticket{ValidS: 420, WaitS: 3600, NoticeS: 120, WithinS: 1800, ConfirmWaitS: 3480}},
	} {
		changed.rtSet(map[string]int{config.KeyOnlineTimeout: c.online, config.KeySettle: c.settle, config.KeyRunApprovalWait: c.wait})
		// the panel whose config.yaml says the same
		plain.s.WakeRuns.Settings = wakerun.NewSettings(c.online, c.settle, 120, 8)
		plain.s.Cfg.Approval.RunApprovalWaitS = c.wait
		got, same := changed.s.runTimings(), plain.s.runTimings()
		if got != c.want || got != same {
			t.Errorf("online %d, settle %d, wait %d: changed in the panel %+v, from config.yaml %+v, want %+v", c.online, c.settle, c.wait, got, same, c.want)
		}
		if got.WaitS > api.RunWaitMaxS || got.WaitS < api.RunWaitMinS || got.WithinS > api.RunWithinMaxS || got.WithinS < api.RunWithinMinS || got.ValidS != 420 || got.NoticeS != 120 {
			t.Errorf("online %d, settle %d, wait %d: outside what a machine takes: %+v", c.online, c.settle, c.wait, got)
		}
	}
	// every combination of the ends of the three ranges stays inside the machines' limits
	on, _ := config.RunTimingOf(config.KeyOnlineTimeout)
	se, _ := config.RunTimingOf(config.KeySettle)
	wa, _ := config.RunTimingOf(config.KeyRunApprovalWait)
	for _, o := range []int{on.Min, on.Max} {
		for _, s := range []int{se.Min, se.Max} {
			for _, w := range []int{wa.Min, wa.Max} {
				changed.rtSet(map[string]int{config.KeyOnlineTimeout: o, config.KeySettle: s, config.KeyRunApprovalWait: w})
				tk := changed.s.runTimings()
				run := "0000000000000000"
				if _, err := (api.ApprovalRequest{Verb: api.ApproveRunArm, Run: &run, ValidS: &tk.ValidS, WaitS: &tk.WaitS, NoticeS: &tk.NoticeS}).Canonical(); err != nil {
					t.Errorf("online %d, settle %d, wait %d: a machine refuses the ticket: %v", o, s, w, err)
				}
				if tk.ConfirmWaitS < 60 || tk.ConfirmWaitS > w || tk.WaitS < o+tk.ConfirmWaitS+60 && tk.WaitS != api.RunWaitMaxS {
					t.Errorf("online %d, settle %d, wait %d: %+v", o, s, w, tk)
				}
			}
		}
	}
}

// A run with a machine that checks approvals itself: the start dialog names the ticket's numbers from the
// timings in force, the passkey signs those, the run keeps them when the timings are changed while it waits
// for its second passkey, and the machine is armed with the wait that was signed.
func TestATicketIsSignedWithTheTimingsInForceAndKeepsThem(t *testing.T) {
	r := newRunRig(t, "pc-a")
	r.rtSet(map[string]int{config.KeyRunApprovalWait: 900, config.KeyOnlineTimeout: 600, config.KeyShutdownDelay: 45})
	asked, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}})
	d := asked.body["detail"].(map[string]any)
	if items := fmt.Sprint(d["items"]); !strings.Contains(items, "PC-A: wake; signs its ticket: once up it switches itself off after 1560 s (notice 120 s) unless a second passkey confirms: update to "+farmNew+", shut down in 45 s.") {
		t.Errorf("the start dialog does not name the numbers in force:\n%s", items)
	}
	if msg := fmt.Sprint(d["message"]); !strings.Contains(msg, "up to 15 minutes to give it") {
		t.Errorf("the start dialog's sentence: %s", msg)
	}
	if res["ok"] != true {
		t.Fatalf("the start: %v", res)
	}
	signed := wakerun.Ticket{ValidS: 420, WaitS: 1560, NoticeS: 120, WithinS: 1560, ConfirmWaitS: 900}
	run, _ := r.s.WakeRuns.Snapshot()
	if run == nil || run.Ticket == nil || *run.Ticket != signed || run.Settings.OnlineTimeoutS != 600 || run.Settings.ShutdownDelayS != 45 {
		t.Fatalf("the run and the numbers that were signed: %+v", run)
	}
	// the change while the run is going: everything shorter, and the wait for a machine longer
	r.rtSet(map[string]int{config.KeyRunApprovalWait: 60, config.KeyOnlineTimeout: 1800, config.KeyShutdownDelay: 10, config.KeySettle: 600})
	if now := r.s.runTimings(); now == signed || now.ConfirmWaitS != 60 {
		t.Fatalf("the rig: the numbers in force changed: %+v", now)
	}
	r.until(t, "pc-a armed", 200, func() bool { return r.host("pc-a").Arm == wakerun.ArmArmed })
	if st := r.m["pc-a"].Gate.StatusNow().Run; st == nil || st.ID != run.ID || st.State != api.RunArmed {
		t.Fatalf("the machine's own word: %+v", st)
	}
	r.until(t, "the run asking for its confirmation", 60, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a != nil && a.Approval == "needed" })
	w := r.req("GET", "/api/admin/wake-update/waiting", nil)
	if left, _ := w.body["left_s"].(float64); left < 880 || left > 900 {
		t.Fatalf("the time left for the second passkey: %v (the run's own 900 s, not the 60 s in force now): %s", w.body["left_s"], w.raw)
	}
	if a, _ := r.s.WakeRuns.Snapshot(); a == nil || *a.Ticket != signed || a.Settings.OnlineTimeoutS != 600 || a.Settings.ShutdownDelayS != 45 || a.Settings.SettleS != 60 {
		t.Fatalf("the run's numbers after the change: %+v", a)
	}
	// the second passkey names the run's own countdown and within_s
	asked, res = r.approved(t, "POST", "/api/admin/wake-update/"+run.ID+"/confirm", map[string]any{})
	if items := fmt.Sprint(asked.body["detail"].(map[string]any)["items"]); !strings.Contains(items, "PC-A: update to "+farmNew+", then shut down in 45 s (good for 26 minutes)") {
		t.Errorf("the confirmation's dialog: %s", items)
	}
	if res["ok"] != true {
		t.Fatalf("the confirmation: %v", res)
	}
	r.until(t, "the run over", 1200, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a == nil })
	if pow := r.sent("pc-a", "/power"); len(pow) != 1 || !strings.Contains(pow[0], "delay_s:45") {
		t.Fatalf("pc-a's shutdown has the countdown that was confirmed: %v", pow)
	}
	if h := r.host("pc-a"); h.State != wakerun.StOff || h.Update != wakerun.UpdOK {
		t.Fatalf("pc-a: %s %s %q", h.State, h.Update, h.Text)
	}
}

// ... and a ticket is never used for other timings than the ones it was signed with: the timings are
// changed between the start dialog and the passkey, the passkey comes back, and nothing is started, nothing
// is kept and nothing reaches the machine.
func TestNoTicketIsUsedWithTimingsItWasNotSignedFor(t *testing.T) {
	for name, change := range map[string]map[string]int{
		"the second passkey's time": {config.KeyRunApprovalWait: 900},
		"the wait for a machine":    {config.KeyOnlineTimeout: 301},
		"the pause":                 {config.KeySettle: 61},
		"off_after_s":               {config.KeyOffAfter: 300},
		"the countdown":             {config.KeyShutdownDelay: 119},
	} {
		r := newRunRig(t, "pc-a")
		asked := r.req("POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}})
		id, _, ok := approvalAsked(asked)
		if !ok {
			t.Fatalf("%s: the start: %d %s", name, asked.code, asked.raw)
		}
		r.rtSet(change)
		if back := r.approve(t, id); back.code != 303 {
			t.Fatalf("%s: the way back: %d %s", name, back.code, back.raw)
		}
		res := r.result(t)
		if res["ok"] != false || res["message"] != wuTimingsChanged {
			t.Fatalf("%s: the start after the change: %v", name, res)
		}
		if a, hist := r.s.WakeRuns.Snapshot(); a != nil || len(hist) != 0 {
			t.Fatalf("%s: a run was started", name)
		}
		if held, _ := r.s.tickets.held(); held != "" {
			t.Fatalf("%s: an approval is kept for a run that was not started", name)
		}
		if n := len(r.m["pc-a"].Passed()); n != 0 || len(r.sent("nas", "/wol")) != 0 {
			t.Fatalf("%s: %d requests got through to the machine, %d wakes went out", name, n, len(r.sent("nas", "/wol")))
		}
	}
}
