package web

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// "Run timings" and a run, through the API with its confirmations: what an admin changed counts for the page
// and the next run, and a run that is going keeps the numbers it started with (runtimes_test.go has the
// tickets, the clamps and the start that is refused).

// The page and a run use what was changed in the panel: with off_after_s 360 and a countdown of 30 s, a
// machine that has been silent for seven minutes is "off", is woken, and is asked for a countdown of 30 s.
// Then the timings are changed while the run is going, through the API with its confirmations: the run keeps
// its numbers to its end, the page shows both, and the next run has the new ones.
func TestARunStartedBeforeAChangeKeepsItsNumbers(t *testing.T) {
	a := signedIn(t)
	e := a.e
	f := newFarm(t, e)
	f.optIn("pc-a", "pc-b")
	a.rtConfirmed(map[string]any{"off_after_s": 360})
	a.rtConfirmed(map[string]any{"shutdown_delay_s": 30})
	silent7(f)
	page := e.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess)
	if s := page.body["settings"].(map[string]any); page.body["off_after_s"] != 360.0 || s["off_after_s"] != 360.0 || s["shutdown_delay_s"] != 30.0 || s["settle_s"] != 60.0 {
		t.Fatalf("the page after the change: off_after_s %v, settings %v", page.body["off_after_s"], s)
	}
	if pa := wuHostRow(t, page, "pc-a"); pa["power"] != "off" || pa["choice"] != "wake" || pa["off_after_s"] != nil || pa["shutdown_delay_s"] != nil {
		t.Fatalf("pc-a, silent for seven minutes with six in force: %v", pa)
	}
	var delays []string
	e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
		if path == "/power" { // the agent's own answer: the countdown it was asked for (the backend's lock is held)
			last := e.b.Requests[len(e.b.Requests)-1]
			d := 0.0
			if m := farmDelay.FindStringSubmatch(last); m != nil {
				fmt.Sscan(m[1], &d)
				delays = append(delays, agent+" "+m[1])
			}
			return 200, jsonx.M{"action": "shutdown", "pending": jsonx.M{"action": "shutdown", "at": float64(f.now.Add(time.Duration(d) * time.Second).Unix()), "delay_s": d, "requested_by": "x"}}, nil
		}
		return 0, nil, nil
	}
	r := e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a"}})
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal(cb.code, string(r.raw))
	}
	act, _ := e.s.WakeRuns.Snapshot()
	if act == nil || act.Settings.OffAfterS != 360 || act.Settings.ShutdownDelayS != 30 || act.Settings.ShutdownDelay != 30*time.Second || act.Settings.SettleS != 60 ||
		act.Settings.OnlineTimeoutS != 300 || act.Settings.PerAgentMin != 8 {
		t.Fatalf("the run's settings at its start: %+v", act)
	}
	f.tick()
	// the change, while the run is going: every timing to the value that would make the run do more, sooner
	a.rtConfirmed(map[string]any{"off_after_s": 300})
	a.rtConfirmed(map[string]any{"shutdown_delay_s": 10})
	how, saved := a.rtAsk(map[string]any{"settle_s": 10, "online_timeout_s": 60, "agent_requests_per_min": 30, "run_approval_wait_s": 60})
	if how != "saved" || saved.body["message"] != "saved; the run that is going keeps its numbers" || saved.body["active"] != true {
		t.Fatalf("a change while a run is going: %q %s", how, saved.raw)
	}
	if entries := e.auditEntries("", "wakeupdate.timings"); len(entries) < 5 || entries[0]["params"].(map[string]any)["run_going"] != act.ID {
		t.Fatalf("the audit entry of a change during a run does not name the run: %v", entries[0])
	}
	// the page: the values in force, and the run with its own
	page = e.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess)
	if s := page.body["settings"].(map[string]any); s["off_after_s"] != 300.0 || s["shutdown_delay_s"] != 10.0 || s["settle_s"] != 10.0 || s["online_timeout_s"] != 60.0 || s["agent_requests_per_min"] != 30.0 {
		t.Fatalf("the page's settings after the change: %v", s)
	}
	if s := page.body["active"].(map[string]any)["settings"].(map[string]any); s["off_after_s"] != 360.0 || s["shutdown_delay_s"] != 30.0 || s["settle_s"] != 60.0 || s["online_timeout_s"] != 300.0 || s["agent_requests_per_min"] != 8.0 {
		t.Fatalf("the run's own settings on the page: %v", s)
	}
	settled := time.Time{}
	for i := 0; i < 3600; i++ {
		act, _ := e.s.WakeRuns.Snapshot()
		if act == nil {
			break
		}
		if act.Settings.ShutdownDelayS != 30 || act.Settings.OffAfterS != 360 || act.Settings.SettleS != 60 || act.Settings.OnlineTimeoutS != 300 || act.Settings.PerAgentMin != 8 {
			t.Fatalf("the run's settings changed while it was going: %+v", act.Settings)
		}
		if h := hostOf(act, "pc-a"); h.State == wakerun.StSettling && settled.IsZero() {
			settled = f.now
		}
		f.tick()
	}
	run := f.finish()
	if ha := hostOf(run, "pc-a"); ha.State != wakerun.StOff || !strings.Contains(ha.Text, "and switched off again.") {
		t.Fatalf("pc-a: %s: %s", ha.State, ha.Text)
	}
	if got := strings.Join(delays, ", "); got != "pc-a 30" {
		t.Fatalf("the countdown the run asked for: %q (its own 30 s, never the 10 s that came into force during it)", got)
	}
	e.b.Mu.Lock()
	all := strings.Join(e.b.Requests, "\n")
	e.b.Mu.Unlock()
	if !strings.Contains(all, "pc-a POST /power map[action:shutdown delay_s:30 requested_by:") || strings.Contains(all, "delay_s:10 ") {
		t.Fatalf("pc-a was not asked for the run's own countdown:\n%s", all)
	}
	if run.Settings.ShutdownDelayS != 30 || run.Settings.OffAfterS != 360 || run.Settings.SettleS != 60 {
		t.Fatalf("the report's settings: %+v", run.Settings)
	}
	// the next run: the numbers in force now
	delays = nil
	for i := 0; i < 301; i++ { // pc-a went off: five minutes of silence are enough now
		f.tick()
	}
	if pa := wuHostRow(t, e.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess), "pc-a"); pa["power"] != "off" || pa["choice"] != "wake" {
		t.Fatalf("pc-a, silent for five minutes with five in force: %v", pa)
	}
	r = e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a"}})
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal(cb.code, string(r.raw))
	}
	if act, _ := e.s.WakeRuns.Snapshot(); act == nil || act.Settings.OffAfterS != 300 || act.Settings.ShutdownDelayS != 10 || act.Settings.SettleS != 10 || act.Settings.OnlineTimeoutS != 60 || act.Settings.PerAgentMin != 30 {
		t.Fatalf("the next run's settings: %+v", act)
	}
	f.finish()
	if got := strings.Join(delays, ", "); got != "pc-a 10" {
		t.Fatalf("the countdown the next run asked for: %q", got)
	}
}

// The dialog's "time for the second passkey" is the one a run started now would use: the clamp that keeps an
// armed machine's wait inside an hour shortens it, and the dialog's data says the shortened time.
func TestRunTimingsDialogSaysTheConfirmWaitAfterTheClamp(t *testing.T) {
	e := setup(t)
	for _, c := range []struct{ online, wait, want int }{{300, 600, 600}, {300, 900, 900}, {1800, 3600, 1740}, {600, 3600, 2940}, {60, 3600, 3480}, {1800, 60, 60}} {
		e.rtSet(map[string]int{config.KeyOnlineTimeout: c.online, config.KeyRunApprovalWait: c.wait})
		if view := e.s.runTimingsView(e.s.Poller.Admin.RunTimings(), nil); view["confirm_wait_s"] != c.want {
			t.Errorf("online %d, wait %d: the dialog's confirm_wait_s %v, want %d", c.online, c.wait, view["confirm_wait_s"], c.want)
		}
	}
}
