package web

import (
	"testing"
)

// After a Turn on the approval page never lists a machine it has just switched on as "off, ready" (audit 6,
// r4-b1-01). The id is on record from the moment the machine took the trust; the machine's own last answer
// still says "off" until the next poll, and the page was made from that answer: under "Last Turn on: tower
// switched on · locked" the table said "tower off ready", and the button counted it. The record counts now.
func TestAMachineJustSwitchedOnIsNeverReady(t *testing.T) {
	a := newApprovalEnv(t)
	page, _ := a.page(t)
	if page["ready"] != 2.0 || page["required"] != 0.0 {
		t.Fatalf("before: %v", page)
	}
	id, _, ok := approvalAsked(a.turnOn("tower"))
	if !ok {
		t.Fatal("the dialog of Turn on")
	}
	if cb := a.approve(t, id); cb.code != 303 || a.result(t)["ok"] != true {
		t.Fatalf("Turn on: %d", cb.code)
	}
	// no poll since: the machine's last answer to the panel is still the one from before
	if ap := a.s.Poller.ApprovalOf("tower"); ap == nil || ap.State != "off" {
		t.Fatalf("the test wants the panel's last answer of tower to be the old one: %+v", ap)
	}
	page, ms := a.page(t)
	if m := ms["tower"]; m["state"] != "required" || m["ready"] == true || m["binding"] != nil || m["pinned"] != "turn-on" {
		t.Fatalf("tower right after its Turn on: %v", m)
	}
	if page["ready"] != 1.0 || page["required"] != 1.0 {
		t.Fatalf("the counts right after the Turn on: ready %v, required %v", page["ready"], page["required"])
	}
	last := page["last"].(map[string]any)["results"].([]any)[0].(map[string]any)
	if last["id"] != "tower" || last["outcome"] != "accepted" {
		t.Fatalf("Last Turn on: %v", last)
	}
	// a second Turn on for it is refused as "already on", without asking the machine
	asked := len(a.tower.Asked())
	if r := a.turnOn("tower"); r.code != 409 || len(a.tower.Asked()) != asked {
		t.Fatalf("Turn on again: %d %s", r.code, r.raw)
	}
	// after the poll it is the machine's own answer
	a.poll()
	if _, ms := a.page(t); ms["tower"]["state"] != "required" || ms["tower"]["ready"] == true {
		t.Fatalf("after the poll: %v", ms["tower"])
	}
}
