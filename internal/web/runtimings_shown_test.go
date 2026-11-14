package web

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// "Run timings": what is confirmed is what is saved, also when somebody else changes the timings between the
// moment a change is kept for "Confirm it's you" and the moment it is confirmed.

// secondSession is another browser session of an admin whose sign-in is an hour old (the fake identity
// provider knows one person; a second admin is the same case for everything asked here).
func (a *armed) secondSession() *armed {
	a.e.t.Helper()
	b := &armed{e: a.e, idp: a.idp, clk: a.clk}
	a.idp.claims["auth_time"] = a.clk.now().Add(-time.Hour).Unix()
	q, rt := a.idp.login(a.e, "/auth/login?next=/", "")
	b.sess = sessionCookie(a.e, a.e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt).hdr)
	if b.sess == "" || b.sess == a.sess {
		a.e.t.Fatal("no second session")
	}
	return b
}

// rtResult is what the panel's replay of the confirmed change answered.
func (a *armed) rtResult() map[string]any {
	a.e.t.Helper()
	res, _ := a.e.do("GET", "/api/me", nil, "Cookie", a.sess).body["pending_result"].(map[string]any)
	if res == nil {
		a.e.t.Fatal("the confirmed change has no result")
	}
	return res
}

func rtRefused(res map[string]any) bool {
	return res["ok"] == false && res["status"] == 409.0 && res["message"] == rtNotShown
}

// The lines of "Confirm it's you" are made when a change is kept. Another admin changes the store before it
// is confirmed. The confirmation then saves nothing unless what it would save is still exactly what it
// showed: a change of "counts as off after" or a shorter countdown is never made on a confirmation that had
// no line and no sentence about it; the audit log has the refusal with the lines that were confirmed, and
// for a saved change exactly the lines that were shown.
func TestRunTimingsAConfirmationSavesOnlyWhatItShowed(t *testing.T) {
	a := signedIn(t)
	e := a.e
	e.s.control, e.s.login = newRateLimiter(1000), newRateLimiter(1000) // this test confirms many changes in one minute
	b := a.secondSession()
	inForce := func(key string) int { return e.s.wuTimesNow().value(key) }
	ask := func(body map[string]any) (string, resp) { // A's last confirmation is over: the usual window is closed
		a.clk.advance(time.Duration(e.s.Cfg.StepUpWindowS+5) * time.Second)
		return a.rtAsk(body)
	}
	okEntries := func() (n int, lines []string) {
		for _, x := range e.auditEntries("", "wakeupdate.timings") {
			if x["outcome"] == "ok" {
				if n++; n == 1 {
					p, _ := x["params"].(map[string]any)
					lines = strs(p["lines"])
				}
			}
		}
		return n, lines
	}

	// 1. "counts as off after": B lowers it; A's request names the value in force next to a longer pause (the
	// usual confirmation, one line); B puts it back; A confirms the dialog from before
	b.rtConfirmed(map[string]any{"off_after_s": 300})
	how, r := a.rtAsk(map[string]any{"off_after_s": 300, "settle_s": 61})
	d, _ := r.body["detail"].(map[string]any)
	shown := strs(d["items"])
	if how != "window" || !slices.Equal(shown, []string{"Pause before the shutdown: 61 s = 1 min 1 s (was 60 s = 1 minute)"}) {
		t.Fatalf("the rig: A's request: %q %v", how, shown)
	}
	b.rtConfirmed(map[string]any{"reset": []string{"off_after_s"}})
	if inForce(config.KeyOffAfter) != 900 || e.rtValues() != "" {
		t.Fatalf("the rig: after B's reset: %q", e.rtValues())
	}
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatalf("A's confirmation: %d %s", cb.code, cb.raw)
	}
	if res := a.rtResult(); !rtRefused(res) || inForce(config.KeyOffAfter) != 900 || inForce(config.KeySettle) != 60 || e.rtValues() != "" {
		t.Fatalf("a confirmation that showed only the pause: %v; in force off_after_s %d, stored %q", res, inForce(config.KeyOffAfter), e.rtValues())
	}
	newest := e.auditEntries("", "wakeupdate.timings")[0]
	p, _ := newest["params"].(map[string]any)
	if n, _ := okEntries(); n != 2 || !strings.HasPrefix(fmt.Sprint(newest["outcome"]), "error 409: the run timings were changed after the confirmation was asked for") ||
		!slices.Equal(strs(p["confirmed"]), shown) || p["lines"] != nil || p["changes"] != nil {
		t.Fatalf("the audit log after the refusal: %d saved changes, newest %v", n, newest)
	}
	// the same request again says what it does now, and asks for its own confirmation
	how, r = a.rtAsk(map[string]any{"off_after_s": 300, "settle_s": 61})
	d, _ = r.body["detail"].(map[string]any)
	shown = strs(d["items"])
	if how != "each" || len(shown) != 3 || shown[0] != "Counts as off after: 300 s = 5 minutes (was 900 s = 15 minutes)" || shown[1] != rtConsequence(config.KeyOffAfter, 300) {
		t.Fatalf("asked again: %q %v", how, shown)
	}
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal(cb.code)
	}
	if n, lines := okEntries(); a.rtResult()["ok"] != true || e.rtValues() != "off_after_s=300 settle_s=61" || n != 3 || !slices.Equal(lines, shown) {
		t.Fatalf("its confirmation: stored %q, %d saved changes, audit lines %v, shown %v", e.rtValues(), n, lines, shown)
	}

	// 2. the countdown: the dialog said "longer" (60 s, was 30 s); with 3600 s stored in between the same
	// request would make it shorter, which nobody confirmed
	e.rtSet(map[string]int{config.KeyOffAfter: 0, config.KeySettle: 0, config.KeyShutdownDelay: 30})
	how, r = ask(map[string]any{"shutdown_delay_s": 60})
	d, _ = r.body["detail"].(map[string]any)
	if shown = strs(d["items"]); how != "window" || !slices.Equal(shown, []string{"Shutdown countdown: 60 s = 1 minute (was 30 s)"}) {
		t.Fatalf("the rig: %q %v", how, shown)
	}
	e.rtSet(map[string]int{config.KeyShutdownDelay: 3600})
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal(cb.code)
	}
	if res := a.rtResult(); !rtRefused(res) || e.rtValues() != "shutdown_delay_s=3600" {
		t.Fatalf("a countdown that would get shorter on a confirmation that said longer: %v, stored %q", res, e.rtValues())
	}

	// 3. changed and changed back: the lines are true again, and the change is saved as it was shown
	how, r = ask(map[string]any{"settle_s": 61})
	d, _ = r.body["detail"].(map[string]any)
	shown = strs(d["items"])
	e.rtSet(map[string]int{config.KeySettle: 70})
	e.rtSet(map[string]int{config.KeySettle: 0})
	if cb := a.confirm(pendingID(r), false); cb.code != 303 || how != "window" {
		t.Fatal(how, cb.code)
	}
	if n, lines := okEntries(); a.rtResult()["ok"] != true || inForce(config.KeySettle) != 61 || n != 4 || !slices.Equal(lines, shown) {
		t.Fatalf("changed and changed back: %v, audit lines %v, shown %v", a.rtResult(), lines, shown)
	}
	// 4. a change of another timing in between leaves the lines true: saved
	how, r = ask(map[string]any{"settle_s": 62})
	d, _ = r.body["detail"].(map[string]any)
	shown = strs(d["items"])
	e.rtSet(map[string]int{config.KeyRequestsPerMin: 10})
	if cb := a.confirm(pendingID(r), false); cb.code != 303 || how != "window" {
		t.Fatal(how, cb.code)
	}
	if n, lines := okEntries(); a.rtResult()["ok"] != true || inForce(config.KeySettle) != 62 || inForce(config.KeyRequestsPerMin) != 10 || n != 5 || !slices.Equal(lines, shown) {
		t.Fatalf("another timing changed in between: %v, audit lines %v", a.rtResult(), lines)
	}
	// 5. the "was" of a line is no longer true (the value is another one): refused, although the new value
	// would be the one that was shown
	_, r = ask(map[string]any{"settle_s": 63})
	e.rtSet(map[string]int{config.KeySettle: 100})
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal(cb.code)
	}
	if res := a.rtResult(); !rtRefused(res) || inForce(config.KeySettle) != 100 {
		t.Fatalf("a line whose \"was\" is no longer true: %v, in force %d", res, inForce(config.KeySettle))
	}
	// 6. somebody saved the very same value in between: nothing to change, and no entry as if it had been
	_, r = ask(map[string]any{"settle_s": 64})
	e.rtSet(map[string]int{config.KeySettle: 64})
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal(cb.code)
	}
	if n, _ := okEntries(); a.rtResult()["message"] != "nothing to change" || n != 5 {
		t.Fatalf("the same value saved in between: %v, %d saved changes", a.rtResult(), n)
	}
}

// The audit entry and the answer have what was written, not what was planned before the store was locked:
// a change saved inside the usual window while another admin's change lands between its plan and its write.
func TestRunTimingsTheAuditEntryHoldsWhatWasWritten(t *testing.T) {
	a := signedIn(t)
	e := a.e
	if cb := a.stepUp("", false); cb.code != 303 { // confirmed just now: inside the window
		t.Fatal("the step-up:", cb.code)
	}
	e.s.rtBeforeWrite = func() {
		e.s.rtBeforeWrite = nil
		e.rtSet(map[string]int{config.KeySettle: 70})
	}
	how, r := a.rtAsk(map[string]any{"settle_s": 90})
	want := []string{"Pause before the shutdown: 90 s = 1 min 30 s (was 70 s = 1 min 10 s)"}
	if how != "saved" || !slices.Equal(strs(r.body["changed"]), want) || e.rtValues() != "settle_s=90" {
		t.Fatalf("the answer: %q %s", how, r.raw)
	}
	entries := e.auditEntries("", "wakeupdate.timings")
	p, _ := entries[0]["params"].(map[string]any)
	changes, _ := p["changes"].([]any)
	if len(entries) != 1 || !slices.Equal(strs(p["lines"]), want) || len(changes) != 1 || changes[0].(map[string]any)["before"] != 70.0 ||
		changes[0].(map[string]any)["before_from"] != "the panel" || changes[0].(map[string]any)["after"] != 90.0 {
		t.Fatalf("the audit entry: %v", entries)
	}
}
