package web

import (
	"strings"
	"testing"
)

// "Switch on" for runs, for a machine that checks approvals itself (audit 6, r4-b1-04). The page asked "Fairy
// may then wake this machine, update its agent and switch it off again", "Confirm it's you" said "a run you
// start may then wake, update and switch off this machine", and the answer was "switched on, but no run can
// take it". One question now, and it says what is true: the page's own question is not asked for such a
// machine, and "Confirm it's you" says that no run can take it. Any other machine is asked as before.
func TestSwitchOnForRunsAsksOnceAndSaysTheTruth(t *testing.T) {
	a := signedIn(t)
	e := a.e
	f := newFarm(t, e)
	f.checksItself("pc-a", "farm-machine-id-of-pc-a-01")
	e.b.Mu.Lock()
	f.on["pc-a"], f.on["pc-b"] = true, true
	e.b.Mu.Unlock()
	f.tick()
	f.accept(t, "pc-a", "farm-machine-id-of-pc-a-01")
	put := func(id string) string {
		t.Helper()
		r := e.do("PUT", "/api/systems/"+id+"/wake-update", map[string]any{"enabled": true}, append([]string{"Cookie", a.sess}, wuWrite...)...)
		if !stepUpRequired(r) {
			t.Fatalf("%s: %d %s", id, r.code, r.raw)
		}
		return actionOf(r)
	}
	if got := put("pc-a"); !strings.HasSuffix(got, "(switch it on; no run can take it for now: it checks a passkey approval itself)") || strings.Contains(got, "may then wake") {
		t.Fatalf("Confirm it's you for the machine with the check: %q", got)
	}
	if got := put("pc-b"); !strings.HasSuffix(got, "(switch it on: a run you start may then wake, update and switch off this machine)") {
		t.Fatalf("Confirm it's you for a machine without the check: %q", got)
	}
	js := appJS(t)
	for _, want := range []string{
		"const inNoRun = !!(wu.data?.hosts || []).find((h) => h.id === t.dataset.id)?.approval;",
		"if (on && !inNoRun && !confirm(`Switch \"Update unpowered hosts\" on for ${name}?",
		"h.approval ? `No run can take it for now: ${h.why || \"it checks approvals itself\"}`",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
}
