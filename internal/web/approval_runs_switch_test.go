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
	// (review of passkey-k, RK-04: this dialog is the ONE question such a machine is asked about, so it also says
	// what the switch allows once the machine is one a run can take. It still does not say that a run may wake it now.)
	if got := put("pc-a"); !strings.HasSuffix(got, "(switch it on; no run can take it for now: it checks a passkey approval itself. "+
		"After that, a run you start may wake, update and switch it off)") || strings.Contains(got, "may then wake") {
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

// RK-04 (review of passkey-k). Inside the confirmation window "Switch on" for a machine that checks approvals
// itself asked nothing at all: the page leaves its own question out for such a machine (it was the untrue one),
// "Confirm it's you" is not due, and the switch was recorded. It stays on, and counts for runs as soon as the
// check is accepted as off, without the admin ever having been told what it allows. Now the server records
// nothing for such a machine before the question was put: it answers "ask_first" with the sentence, the page
// asks it and sends the request again with "asked". One question, inside the window and outside it.
func TestSwitchOnForRunsInsideTheWindowIsAskedOnce(t *testing.T) {
	a := signedIn(t)
	e := a.e
	f := newFarm(t, e)
	f.checksItself("pc-a", "farm-machine-id-of-pc-a-01")
	e.b.Mu.Lock()
	f.on["pc-a"], f.on["pc-b"] = true, true
	e.b.Mu.Unlock()
	f.tick()
	f.accept(t, "pc-a", "farm-machine-id-of-pc-a-01")
	put := func(id string, body map[string]any) resp {
		return e.do("PUT", "/api/systems/"+id+"/wake-update", body, append([]string{"Cookie", a.sess}, wuWrite...)...)
	}
	optedIn := func(id string) any {
		return wuHostRow(t, e.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess), id)["opted_in"]
	}
	// open the confirmation window with another machine's switch
	r := put("pc-b", map[string]any{"enabled": true})
	if cb := a.confirm(pendingID(r), false); !stepUpRequired(r) || cb.code != 303 || optedIn("pc-b") != true {
		t.Fatalf("the confirmation for pc-b: %d %s", r.code, r.raw)
	}
	// inside the window: nothing is recorded before the question was put
	r = put("pc-a", map[string]any{"enabled": true})
	d, _ := r.body["detail"].(map[string]any)
	const question = "Switch \"Update unpowered hosts\" on for PC-A? No run can take it for now: it checks a passkey approval itself. " +
		"After that, a run you start may wake, update and switch it off."
	if r.code != 409 || d["code"] != "ask_first" || d["message"] != question {
		t.Fatalf("inside the window, not asked: %d %s", r.code, r.raw)
	}
	if optedIn("pc-a") != false {
		t.Fatal("the switch was recorded without a question")
	}
	for _, en := range e.auditEntries("", "wakeupdate.switch") {
		if en["target"] == "pc-a" {
			t.Fatalf("the audit log has a switch for pc-a that did not happen: %v", en)
		}
	}
	// the page asks with that sentence and sends the request again
	r = put("pc-a", map[string]any{"enabled": true, "asked": true})
	if msg, _ := r.body["message"].(string); r.code != 200 || msg != "PC-A is switched on, but no run can take it: it checks a passkey approval itself" || optedIn("pc-a") != true {
		t.Fatalf("asked: %d %s", r.code, r.raw)
	}
	// switching off, and a machine without the check, are as they were: no "ask_first"
	if r := put("pc-a", map[string]any{"enabled": false}); r.code != 200 || optedIn("pc-a") != false {
		t.Fatalf("switch off: %d %s", r.code, r.raw)
	}
	if r := put("pc-b", map[string]any{"enabled": true}); r.code != 200 {
		t.Fatalf("a machine without the check, inside the window: %d %s", r.code, r.raw)
	}
	// the page: the question is asked with the server's sentence, and only then is the request sent again
	js := appJS(t)
	for _, want := range []string{
		`if (x.status !== 409 || x.detail?.code !== "ask_first") throw x;`,
		`const asked = { ...body, asked: true };`,
		`if (confirm(x.detail.message)) r = await controlCall({ kind: "request", label, method: "PUT", path, body: asked }, () => api(path, { method: "PUT", body: JSON.stringify(asked) }));`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
}

// Outside the window the one question is "Confirm it's you" (its dialog has the whole sentence): the kept
// request is recorded when the confirmation comes back, without a second question.
func TestSwitchOnForRunsOutsideTheWindowIsAskedOnceByTheConfirmation(t *testing.T) {
	a := signedIn(t)
	e := a.e
	f := newFarm(t, e)
	f.checksItself("pc-a", "farm-machine-id-of-pc-a-01")
	e.b.Mu.Lock()
	f.on["pc-a"] = true
	e.b.Mu.Unlock()
	f.tick()
	f.accept(t, "pc-a", "farm-machine-id-of-pc-a-01")
	r := e.do("PUT", "/api/systems/pc-a/wake-update", map[string]any{"enabled": true}, append([]string{"Cookie", a.sess}, wuWrite...)...)
	if !stepUpRequired(r) {
		t.Fatalf("outside the window: %d %s", r.code, r.raw)
	}
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatalf("the confirmation: %d %s", cb.code, cb.raw)
	}
	if h := wuHostRow(t, e.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess), "pc-a"); h["opted_in"] != true {
		t.Fatalf("pc-a after the confirmation: %v", h)
	}
}
