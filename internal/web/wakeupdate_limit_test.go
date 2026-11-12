package web

import (
	"fmt"
	"strings"
	"testing"
)

// Found during the integration (part 1, "Noticed", 2): the panel takes six control requests a minute from one
// account. Switching six machines on for these runs and starting a run in the same minute got "too many
// control requests" for the start AFTER "Confirm it's you" had been confirmed, and the audit log had no line
// for the refused start.
//
// The limit is now looked at before a passkey is asked for, and a start that was confirmed is no longer
// refused by it. A refused start has its line in the audit log.
func TestWakeUpdateStartAndTheLimitOnControlRequests(t *testing.T) {
	a := signedIn(t)
	e := a.e
	f := newFarm(t, e)
	f.offline()
	if r := a.stepUp("&next=/", false); r.code != 303 { // inside the window: the switches run at once
		t.Fatal(r.code)
	}
	put := func(id string, on bool) resp {
		return e.do("PUT", "/api/systems/"+id+"/wake-update", map[string]any{"enabled": on}, append([]string{"Cookie", a.sess}, wuWrite...)...)
	}
	limit := e.s.Cfg.ControlRatePerMin
	if limit != 20 {
		t.Fatalf("the default is 20 control requests a minute (it was 6), the panel has %d", limit)
	}
	// all but one of the minute's control requests: the three machines are switched on, then pc-a off and on
	// again until one request is left (an even number of times, so it ends on)
	for i := 0; i < limit-1; i++ {
		id, on := "pc-a", i < 3 || i%2 == 0
		if i == 1 {
			id = "pc-b"
		} else if i == 2 {
			id = "laptop"
		}
		if r := put(id, on); r.code != 200 {
			t.Fatalf("switch %d (%s): %d %s", i+1, id, r.code, r.raw)
		}
	}
	body := map[string]any{"hosts": []string{"pc-a", "pc-b"}}

	// 1. asked while there is room: the dialog comes. Then the last switch uses the minute up, and the
	// confirmation comes back: the run starts all the same.
	r := e.wuStart(a.sess, body)
	if !stepUpRequired(r) || pendingID(r) == "" {
		t.Fatalf("a start inside the limit must be asked for its confirmation: %d %s", r.code, r.raw)
	}
	if x := put("laptop", false); x.code != 200 {
		t.Fatalf("the last control request of the minute: %d %s", x.code, x.raw)
	}
	if x := put("laptop", true); x.code != 429 {
		t.Fatalf("one more must be refused (the limit is still there): %d %s", x.code, x.raw)
	}
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal("confirmation:", cb.code, string(cb.raw))
	}
	run, _ := e.s.WakeRuns.Snapshot()
	if run == nil || len(run.Hosts) != 2 {
		pr := e.do("GET", "/api/me", nil, "Cookie", a.sess).body["pending_result"]
		t.Fatalf("the confirmed start did not run: %v", pr)
	}
	st := auditOf(e, "wakeupdate.start")
	if len(st) != 1 || !strings.HasPrefix(st[0]["outcome"].(string), "started: ") {
		t.Fatalf("audit of the start: %v", st)
	}

	// 2. asked when the minute is used up: refused at once, before anybody is asked for a passkey, nothing is
	// kept, and the audit log says so.
	if _, err := e.s.WakeRuns.CancelRun(run.ID, "test", false); err != nil {
		t.Fatal(err)
	}
	f.finish()
	r = e.wuStart(a.sess, body)
	if r.code != 429 || stepUpRequired(r) || fmt.Sprint(r.body["detail"]) != "too many control requests; slow down" {
		t.Fatalf("a start over the limit: %d %s", r.code, r.raw)
	}
	if keptID(e, a.sess) != "" {
		t.Fatal("the refused start was kept for a confirmation")
	}
	if now, _ := e.s.WakeRuns.Snapshot(); now != nil {
		t.Fatal("a run started")
	}
	st = auditOf(e, "wakeupdate.start")
	var refused map[string]any
	for _, en := range st {
		if strings.HasPrefix(en["outcome"].(string), "error 429: too many control requests") {
			refused = en
		}
	}
	if len(st) != 2 || refused == nil || refused["target"] != "run" || fmt.Sprint(refused["params"].(map[string]any)["hosts"]) != "[pc-a pc-b]" {
		t.Fatalf("the audit log has no line for the refused start: %v", st)
	}
}
