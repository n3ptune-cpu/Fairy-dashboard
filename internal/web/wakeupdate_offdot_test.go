package web

import (
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// Seen in a real run (13 Nov): the run's report said "is off" for a machine while the machine's dot in the
// sidebar was still amber. The report comes from the run, which saw the machine go off after its own countdown;
// the dot comes from the machine's row, which is "stale" for offline_after (60 s) after the last answer,
// whatever is known about why the answers stopped. The run tells the poller now (Poller.MarkOff): the moment
// the report says "off", the row is "offline".
func TestTheRowIsOfflineWhenTheRunsReportSaysOff(t *testing.T) {
	a := signedIn(t)
	e := a.e
	f := newFarm(t, e)
	f.offline()
	f.optIn("pc-a")
	e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
		if path == "/power" { // the agent's own answer: the countdown it scheduled
			return 200, jsonx.M{"action": "shutdown", "pending": jsonx.M{"action": "shutdown", "at": float64(f.now.Add(120 * time.Second).Unix()), "delay_s": 120.0, "requested_by": "x"}}, nil
		}
		return 0, nil, nil
	}
	r := e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a"}})
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal(cb.code)
	}
	// while it is on in the run, its row is online; the first poll without an answer makes it "stale"
	sawOnline, sawStale := false, false
	var run *wakerun.Run
	for i := 0; i < 3600 && run == nil; i++ {
		f.tick()
		switch snapOf(e, "pc-a").Status {
		case "online":
			sawOnline = true
		case "stale":
			sawStale = sawOnline
		}
		if act, hist := e.s.WakeRuns.Snapshot(); act == nil && len(hist) > 0 {
			run = hist[0]
		}
	}
	if run == nil || !sawOnline || !sawStale {
		t.Fatalf("the fixture: the run ended %v, the row was online %v and then stale %v", run != nil, sawOnline, sawStale)
	}
	h := hostOf(run, "pc-a")
	if h.State != wakerun.StOff || h.On != wakerun.OnNo {
		t.Fatalf("pc-a: %s / %s: %s", h.State, h.On, h.Text)
	}
	row := snapOf(e, "pc-a")
	if row.LastSeen == nil {
		t.Fatal("the row has no last answer")
	}
	since := f.now.Sub(*row.LastSeen)
	if since >= time.Duration(e.s.Cfg.OfflineAfter)*time.Second {
		t.Fatalf("the fixture: the run ended %s after the machine's last answer, when the row is offline anyway (offline_after %v s)", since, e.s.Cfg.OfflineAfter)
	}
	if row.Status != "offline" {
		t.Fatalf("the report says %q (chip \"is off\") and the machine's row is %q, %s after its last answer", h.On, row.Status, since)
	}
	// the page gets the same: GET /api/systems/pc-a
	if sys := e.do("GET", "/api/systems/pc-a", nil, "Cookie", a.sess); sys.body["status"] != "offline" {
		t.Fatalf("the host's page: %v", sys.body["status"])
	}
	// and it stays so through the polls that follow
	for i := 0; i < 30; i++ {
		f.tick()
		if st := snapOf(e, "pc-a").Status; st != "offline" {
			t.Fatalf("%d s later the row is %q again", i+1, st)
		}
	}
}

// Only a report that says "off" moves the row. A machine that a run ends in any other way (here: it was on,
// it was only to be updated, and it stopped answering as the run began) keeps the row it has: "stale" while
// its last answer is younger than offline_after, because the panel does not know that it is off, and neither
// does the run. (The review of build 14 made every end of a machine mark its row, and no test noticed.)
func TestARunThatDoesNotSayOffLeavesTheRowAsItIs(t *testing.T) {
	a := signedIn(t)
	e := a.e
	f := newFarm(t, e)
	f.on["pc-a"], f.booted["pc-a"], f.ready["pc-a"] = true, f.now.Add(-time.Hour), f.now.Add(-time.Hour)
	f.offline()
	f.optIn("pc-a")
	if st := snapOf(e, "pc-a").Status; st != "online" {
		t.Fatalf("the fixture: pc-a is %s", st)
	}
	r := e.wuStart(a.sess, map[string]any{"update_only": []string{"pc-a"}})
	e.b.Mu.Lock()
	f.on["pc-a"] = false // it stops answering before the run's first step
	e.b.Mu.Unlock()
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal(cb.code)
	}
	var run *wakerun.Run
	for i := 0; i < 50 && run == nil; i++ {
		f.tick()
		if act, hist := e.s.WakeRuns.Snapshot(); act == nil && len(hist) > 0 {
			run = hist[0]
		}
	}
	if run == nil {
		t.Fatal("the fixture: the run is still going after 50 s, when the row is offline anyway")
	}
	h := hostOf(run, "pc-a")
	if h.On == wakerun.OnNo {
		t.Fatalf("the fixture: the run says off: [%s] %s", h.State, h.Text)
	}
	row := snapOf(e, "pc-a")
	if row.LastSeen == nil || f.now.Sub(*row.LastSeen) >= time.Duration(e.s.Cfg.OfflineAfter)*time.Second {
		t.Fatalf("the fixture: the row's last answer is %v", row.LastSeen)
	}
	if row.Status != "stale" {
		t.Fatalf("the run ended the machine as [%s/%s] %s, and its row is %q, %s after its last answer: want stale", h.State, h.On, h.Text, row.Status, f.now.Sub(*row.LastSeen))
	}
}
