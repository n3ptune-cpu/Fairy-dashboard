package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// "Confirm it's you" for a Turn on names everybody who may approve on a machine (audit 6, r4-b1-03). The list
// is locked into the machine afterwards. It stood in one line per machine, and a line of the dialog is cut at
// 240 bytes: with five ids in approval.users two people were named and the third was cut in its middle; with a
// long host name the second was. Each person has a line of their own now, with the user id in full, and what
// is shown is what the passkey signs.
func TestTurnOnConfirmationNamesEverybodyWhoMayApprove(t *testing.T) {
	a := newApprovalEnv(t)
	ids := []string{"user-e2e-1", "3f2b8c1e-7a4d-4c1b-9e2f-1a2b3c4d5e6f", "9d8c7b6a-5f4e-4d3c-8b2a-1f0e9d8c7b6a", "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d", "7e6d5c4b-3a2f-4e1d-9c8b-7a6f5e4d3c2b"}
	a.s.Cfg.Approval.Users = ids
	// tower gets a very long name and an owner of its own, so the two machines get different lists
	long := strings.Repeat("LONG-HOST-NAME-", 6) + "END"
	ag, _ := a.s.Poller.AgentByID("tower")
	ag.Name, ag.Owner = long, model.P("Straw")
	a.s.Poller.RemoveAgent("tower")
	if err := a.s.Poller.AddAgent(ag, nil); err != nil {
		t.Fatal(err)
	}
	a.s.Cfg.Approval.Owners = map[string]string{"Straw": "owner-of-the-tower-0001"}
	a.poll()

	r := a.turnOn("laptop", "tower")
	id, _, ok := approvalAsked(r)
	if !ok {
		t.Fatalf("the dialog: %d %s", r.code, r.raw)
	}
	var items []string
	for _, x := range r.body["detail"].(map[string]any)["items"].([]any) {
		items = append(items, x.(string))
	}
	all := strings.Join(items, "\n")
	if strings.Contains(all, "…") {
		t.Fatalf("a line of the dialog is cut:\n%s", all)
	}
	// what the passkey signs for each machine: the users of the proposal that is kept with the plan
	l := a.s.codec.live
	l.mu.Lock()
	trust := l.m[sidOf(a.env, a.admin)].pending.Approval.trust
	l.mu.Unlock()
	if len(trust["laptop"].Users) != 6 || len(trust["tower"].Users) != 7 {
		t.Fatalf("the proposals: laptop %v, tower %v", trust["laptop"].Users, trust["tower"].Users)
	}
	// the dialog: each machine, how many may approve on it, and every one of them with the whole id
	section := func(head, count string, subs []string) {
		t.Helper()
		at := -1
		for i, line := range items {
			if strings.HasPrefix(line, head) {
				at = i
			}
		}
		if at < 0 || at+1+len(subs) >= len(items) || items[at+1] != count {
			t.Fatalf("no %q followed by %q:\n%s", head, count, all)
		}
		for i, sub := range subs {
			if line := items[at+2+i]; !strings.HasPrefix(line, "· ") || !strings.Contains(line, "["+sub+", ") {
				t.Fatalf("line %d under %q is %q, want the person %s:\n%s", i, head, line, sub, all)
			}
		}
	}
	section("laptop (checked by its agent)", "6 people may approve on it:", trust["laptop"].Users)
	section(long+" (checked by its agent)", "7 people may approve on it:", trust["tower"].Users)
	if got := fmt.Sprint(trust["tower"].Users); !strings.Contains(got, "owner-of-the-tower-0001") || !strings.Contains(all, "[owner-of-the-tower-0001, owner (Straw)]") {
		t.Fatalf("tower's owner: %s\n%s", got, all)
	}
	if !strings.HasPrefix(items[len(items)-1], "After this each of these machines is locked") {
		t.Fatalf("the last line: %q", items[len(items)-1])
	}
	// and it is what the machines are sent
	if cb := a.approve(t, id); cb.code != 303 || a.result(t)["ok"] != true {
		t.Fatalf("Turn on: %d", cb.code)
	}
	for m, want := range map[string][]string{"laptop": trust["laptop"].Users, "tower": trust["tower"].Users} {
		mach := a.laptop
		if m == "tower" {
			mach = a.tower
		}
		sent := mach.Asked()[len(mach.Asked())-1]
		if fmt.Sprint(sent.Body["users"]) != fmt.Sprint(want) {
			t.Fatalf("%s was sent %v, the dialog showed %v", m, sent.Body["users"], want)
		}
	}
}
