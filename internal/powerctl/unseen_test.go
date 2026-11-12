package powerctl

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// Second review of fix7a (R7A-09): the cancel is offered only when the panel's own view shows a pending
// action. A wake / update / shut down run whose shutdown request got no answer knows of a request that may
// have arrived while the panel's view shows nothing (the polls since failed, or have not shown it yet): its
// cancel was refused inside the panel with "nothing pending", and nothing was sent. Request.PendingUnseen
// sends the cancel all the same. It is not part of the API: a request body cannot set it.
func TestACancelForAPendingActionThePanelDoesNotSee(t *testing.T) {
	p, b := testPoller(t)
	b.Reply = func(_, path string) (int, jsonx.M, error) {
		if path == "/power" {
			return 200, jsonx.M{"action": "cancel", "pending": nil}, nil // the agent's answer, with or without a countdown
		}
		return 0, nil, nil
	}
	systems := map[string]model.System{}
	for _, s := range p.Snapshot() {
		systems[s.ID] = s
	}
	laptop, ok := systems["laptop"]
	if !ok || laptop.PowerPending != nil {
		t.Fatalf("the fixture: %+v", laptop)
	}
	sent := func() int {
		b.Mu.Lock()
		defer b.Mu.Unlock()
		n := 0
		for _, r := range b.Requests {
			if strings.Contains(r, "POST /power") {
				n++
			}
		}
		return n
	}
	ctx := context.Background()
	valid := func(r Request) Request { // what the route and the run do first: it fills in the delay
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
		return r
	}
	// as ever: nothing pending in the panel's view, no cancel
	if _, err := Execute(ctx, p, laptop, valid(Request{Action: "cancel"}), "Ann"); err == nil || !strings.Contains(err.Error(), "nothing pending") || sent() != 0 {
		t.Fatalf("a cancel with nothing pending: %v (requests sent: %d)", err, sent())
	}
	// a body cannot ask for it
	var fromBody Request
	if err := json.Unmarshal([]byte(`{"action":"cancel","PendingUnseen":true,"pending_unseen":true}`), &fromBody); err != nil || fromBody.PendingUnseen {
		t.Fatalf("PendingUnseen was read from a request body: %+v %v", fromBody, err)
	}
	// the caller that knows of a request of its own: sent, also with the kill switch on, and to the agent only
	for _, killSwitch := range []bool{false, true} {
		if err := p.Admin.SetControlDisabled(killSwitch, "test", "stop everything"); err != nil {
			t.Fatal(err)
		}
		before := sent()
		res, err := Execute(ctx, p, laptop, valid(Request{Action: "cancel", PendingUnseen: true}), "Ann")
		if err != nil || res.Message != "cancelled" || res.Via != "agent" || sent() != before+1 {
			t.Fatalf("kill switch %v: a cancel for an action the panel does not see: %+v %v (requests sent: %d)", killSwitch, res, err, sent()-before)
		}
		// nothing else gets through that way
		for _, action := range []string{"shutdown", "reboot", "sleep", "wake"} {
			if killSwitch {
				if _, err := Execute(ctx, p, laptop, valid(Request{Action: action, PendingUnseen: true, Confirm: &laptop.Name}), "Ann"); err == nil || sent() != before+1 {
					t.Fatalf("kill switch on: %s with PendingUnseen: %v (requests sent: %d)", action, err, sent()-before-1)
				}
			}
		}
	}
	if err := p.Admin.SetControlDisabled(false, "test", ""); err != nil {
		t.Fatal(err)
	}
	// a system whose cancel does not go to an agent with power control is refused as before
	for id, s := range systems {
		if id == "laptop" || (s.Kind != "qemu" && s.Kind != "lxc" && s.Kind != "node" && s.Kind != "agent") {
			continue
		}
		before := sent()
		if _, err := Execute(ctx, p, s, valid(Request{Action: "cancel", PendingUnseen: true}), "Ann"); err == nil || sent() != before {
			t.Fatalf("%s (%s): a cancel with PendingUnseen: %v (requests sent: %d)", id, s.Kind, err, sent()-before)
		}
	}
}
