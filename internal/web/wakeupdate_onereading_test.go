package web

import (
	"slices"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
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

// The timings change in the instant after the replay read them, or after it compared them with what was
// confirmed. The run has the numbers that were read and compared: the start does not read them a second
// time, neither for the stamp it compares nor for the run.
func TestAStartDoesNotReadTheTimingsAgainAfterTheyWereCompared(t *testing.T) {
	for _, at := range []string{"read", "checked"} {
		a := signedIn(t)
		e := a.e
		f := newFarm(t, e)
		f.optIn("pc-a")
		f.offline()
		e.rtSet(map[string]int{config.KeyShutdownDelay: 45})
		seen := 0
		e.s.wuStartStep = func(step string) {
			if step == at {
				if seen++; seen == 2 { // the replay
					e.rtSet(map[string]int{config.KeyOffAfter: 300, config.KeyShutdownDelay: 10, config.KeySettle: 10, config.KeyRequestsPerMin: 30, config.KeyOnlineTimeout: 60})
				}
			}
		}
		r := e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a"}})
		if cb := a.confirm(pendingID(r), false); cb.code != 303 || seen != 2 {
			t.Fatal(at, cb.code, seen)
		}
		act, _ := e.s.WakeRuns.Snapshot()
		if act == nil || act.Settings.OffAfterS != 900 || act.Settings.ShutdownDelayS != 45 || act.Settings.SettleS != 60 || act.Settings.PerAgentMin != 8 || act.Settings.OnlineTimeoutS != 300 {
			res, _ := e.do("GET", "/api/me", nil, "Cookie", a.sess).body["pending_result"].(map[string]any)
			t.Fatalf("changed after %q: the run's settings are not the ones that were read and compared with the confirmation: %+v (answered %v)", at, act, res)
		}
		f.finish()
	}
}

// The same for a machine's OWN two timings: the start reads them with the panel's, makes its stamps and its
// lines from that reading, and hands them to the run. An Edit host change that lands after the reading (or
// after the comparison with what was confirmed) counts from the next run: the run has, for the machine, the
// numbers its confirmation showed.
func TestARunHasAMachinesOwnTimingsAsTheStartReadAndComparedThem(t *testing.T) {
	setOwn := func(e *env, id string, r adminstate.RunEdit) {
		t.Helper()
		if _, _, err := e.s.Poller.Admin.EditHostOptions("other admin", func(o *adminstate.HostOptions) error {
			o.SetHost(id, adminstate.HostEdit{Run: r})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		e.s.Poller.Bump()
	}
	val := func(p *int) int {
		if p == nil {
			return 0
		}
		return *p
	}
	for _, c := range []struct {
		what               string
		before, then       adminstate.RunEdit // pc-a's own at the start's reading; what another admin makes of it afterwards
		wantOff, wantDelay int                // pc-a's own in the run (0: the run's)
		line               string             // the confirmation's line about pc-a's own ("": none)
	}{
		{"none of its own, then the floors", adminstate.RunEdit{}, adminstate.RunEdit{OffAfterS: 300, ShutdownDelayS: 10}, 0, 0, ""},
		{"its own, then none", adminstate.RunEdit{OffAfterS: 1800, ShutdownDelayS: 300}, adminstate.RunEdit{}, 1800, 300,
			"PC-A, its own: counts as off after 30 minutes; shutdown countdown 5 minutes"},
		{"its own, then the floors", adminstate.RunEdit{OffAfterS: 1800}, adminstate.RunEdit{OffAfterS: 300, ShutdownDelayS: 10}, 1800, 0,
			"PC-A, its own: counts as off after 30 minutes; shutdown countdown 2 minutes"},
	} {
		for _, at := range []string{"read", "checked"} {
			a := signedIn(t)
			e := a.e
			f := newFarm(t, e)
			f.optIn("pc-a")
			f.offline()
			setOwn(e, "pc-a", c.before)
			seen := 0
			e.s.wuStartStep = func(step string) {
				if step == at {
					if seen++; seen == 2 { // the replay
						setOwn(e, "pc-a", c.then)
					}
				}
			}
			page := e.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess)
			r := e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a"}, "timings": shownBy(t, page, "pc-a")})
			d, _ := r.body["detail"].(map[string]any)
			items := strs(d["items"])
			if has := slices.ContainsFunc(items, func(l string) bool { return strings.HasPrefix(l, "PC-A, its own:") }); has != (c.line != "") || (c.line != "" && !slices.Contains(items, c.line)) {
				t.Fatalf("%s: the confirmation's lines: %q", c.what, items)
			}
			if cb := a.confirm(pendingID(r), false); cb.code != 303 || seen != 2 {
				t.Fatal(c.what, at, cb.code, seen)
			}
			act, _ := e.s.WakeRuns.Snapshot()
			if act == nil || len(act.Hosts) != 1 {
				res, _ := e.do("GET", "/api/me", nil, "Cookie", a.sess).body["pending_result"].(map[string]any)
				t.Fatalf("%s, changed after %q: no run: %v", c.what, at, res)
			}
			if h := act.Hosts[0]; val(h.OffAfterS) != c.wantOff || val(h.ShutdownDelayS) != c.wantDelay || act.Settings.OffAfterS != 900 || act.Settings.ShutdownDelayS != 120 {
				t.Errorf("%s, changed after %q: the run has pc-a's own %d s / %d s (the run's: %d / %d), want %d / %d as confirmed (%q)",
					c.what, at, val(h.OffAfterS), val(h.ShutdownDelayS), act.Settings.OffAfterS, act.Settings.ShutdownDelayS, c.wantOff, c.wantDelay, items)
			}
			f.finish()
			e.s.wuStartStep = nil
		}
	}
}
