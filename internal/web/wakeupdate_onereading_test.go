package web

import (
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// A start of "Update unpowered hosts" reads the timings in force once. The lines of its confirmation, the
// numbers of a ticket, the stamp that is kept with it and the run's settings are all made from that reading.

// The timings change in the instant after the start read them and before it is kept for its confirmation.
// The confirmation shows the numbers that were read; the stamp is of the same numbers (not of a second
// reading), so the replay, which finds other timings in force, starts nothing.
func TestAKeptStartsStampIsOfTheReadingItsLinesWereMadeFrom(t *testing.T) {
	a := signedIn(t)
	e := a.e
	f := newFarm(t, e)
	f.optIn("pc-a")
	f.offline()
	e.s.wuStartStep = func(step string) {
		if step == "read" {
			e.s.wuStartStep = nil
			e.rtSet(map[string]int{config.KeyOffAfter: 300, config.KeyShutdownDelay: 10})
		}
	}
	r := e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a"}})
	if !stepUpRequired(r) {
		t.Fatalf("the start: %d %s", r.code, r.raw)
	}
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal(cb.code)
	}
	res, _ := e.do("GET", "/api/me", nil, "Cookie", a.sess).body["pending_result"].(map[string]any)
	if res["ok"] != false || res["status"] != 409.0 || res["message"] != wuTimingsChanged {
		t.Fatalf("a start kept with the numbers of one reading and replayed with others in force: %v", res)
	}
	if act, hist := e.s.WakeRuns.Snapshot(); act != nil || len(hist) != 0 {
		t.Fatal("a run was started")
	}
	// ... and for a machine that checks approvals itself the passkey's dialog had the countdown that was read
	rr := newRunRig(t, "pc-a")
	rr.s.wuStartStep = func(step string) {
		if step == "read" {
			rr.s.wuStartStep = nil
			rr.rtSet(map[string]int{config.KeyShutdownDelay: 45})
		}
	}
	asked := rr.req("POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}})
	id, _, ok := approvalAsked(asked)
	if !ok || !strings.Contains(string(asked.raw), "shut down in 120 s") || strings.Contains(string(asked.raw), "shut down in 45 s") {
		t.Fatalf("the passkey's dialog: %d %s", asked.code, asked.raw)
	}
	if back := rr.approve(t, id); back.code != 303 {
		t.Fatal(back.code)
	}
	if res := rr.result(t); res["ok"] != false || res["message"] != wuTimingsChanged {
		t.Fatalf("the start after the change: %v", res)
	}
	if held, _ := rr.s.tickets.held(); held != "" {
		t.Fatal("an approval is kept for a run that was not started")
	}
}

// The timings change in the instant after the replay compared them with what was confirmed. The run has the
// numbers that were compared: the start does not read them a second time.
func TestAStartDoesNotReadTheTimingsAgainAfterTheyWereCompared(t *testing.T) {
	a := signedIn(t)
	e := a.e
	f := newFarm(t, e)
	f.optIn("pc-a")
	f.offline()
	e.rtSet(map[string]int{config.KeyShutdownDelay: 45})
	checked := 0
	e.s.wuStartStep = func(step string) {
		if step == "checked" {
			if checked++; checked == 2 { // the replay
				e.rtSet(map[string]int{config.KeyOffAfter: 300, config.KeyShutdownDelay: 10, config.KeySettle: 10, config.KeyRequestsPerMin: 30, config.KeyOnlineTimeout: 60})
			}
		}
	}
	r := e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a"}})
	if cb := a.confirm(pendingID(r), false); cb.code != 303 || checked != 2 {
		t.Fatal(cb.code, checked)
	}
	act, _ := e.s.WakeRuns.Snapshot()
	if act == nil || act.Settings.OffAfterS != 900 || act.Settings.ShutdownDelayS != 45 || act.Settings.SettleS != 60 || act.Settings.PerAgentMin != 8 || act.Settings.OnlineTimeoutS != 300 {
		t.Fatalf("the run's settings are not the ones that were compared with the confirmation: %+v", act)
	}
	f.finish()
}
