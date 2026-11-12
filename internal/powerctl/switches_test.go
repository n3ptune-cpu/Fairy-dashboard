package powerctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Audit 5, round 3 (r3-p2-06): the kill switch and the power switch of a host take every power action away,
// and the cancel of a pending countdown was one of them: a countdown that ran when the switch was flipped
// could not be called off from the panel. The cancel passes both switches; nothing else does, for a host with
// an agent, a Proxmox guest and a node.
func TestOnlyTheCancelPassesTheSwitches(t *testing.T) {
	p, b := testPoller(t)
	b.Mu.Lock()
	b.Metrics["laptop"] = func() (jsonx.M, error) { // its agent reports a countdown
		m := testutil.AgentMetrics()
		m["power_control"] = jsonx.M{"enabled": true, "min_delay_s": 10.0,
			"pending": jsonx.M{"action": "reboot", "at": float64(time.Now().Add(2 * time.Minute).Unix()), "delay_s": 120.0, "requested_by": "Ann"}}
		return m, nil
	}
	b.Mu.Unlock()
	b.Reply = func(_, path string) (int, jsonx.M, error) {
		if path == "/power" {
			return 200, jsonx.M{"action": "cancel", "pending": nil}, nil
		}
		return 0, nil, nil
	}
	p.PollOnce(context.Background())
	systems := map[string]model.System{}
	for _, s := range p.Snapshot() {
		if _, have := systems[s.Kind]; (s.Kind == "agent" && s.ID == "laptop") || (s.Kind != "agent" && !have && s.Status == "online") {
			systems[s.Kind] = s
		}
	}
	laptop := systems["agent"]
	if laptop.ID != "laptop" || laptop.PowerPending == nil || systems["node"].ID == "" || systems["qemu"].ID == "" {
		t.Fatalf("the fixture: %+v", systems)
	}
	open := func(s model.System) string {
		var on []string
		for _, a := range Capabilities(p, s).Actions {
			if a.Available {
				on = append(on, a.Action)
			}
		}
		return strings.Join(on, " ")
	}
	if got := open(laptop); got != "shutdown reboot cancel" {
		t.Fatalf("with control on the laptop offers %q", got)
	}
	before := map[string]string{}
	for kind, s := range systems {
		before[kind] = open(s)
	}
	if before["qemu"] == "" { // (the node of the fixture has no power route of its own: it offers nothing either way)
		t.Fatalf("with control on: %v", before)
	}
	for _, how := range []string{"the kill switch", "the power switch of each system"} {
		if how == "the kill switch" {
			if err := p.Admin.SetControlDisabled(true, "test", "stop everything"); err != nil {
				t.Fatal(err)
			}
		} else {
			for _, s := range systems {
				if err := p.Admin.SetPowerDisabled(s.ID, true); err != nil {
					t.Fatal(err)
				}
			}
		}
		for kind, s := range systems {
			want := ""
			if kind == "agent" {
				want = "cancel"
			}
			if got := open(s); got != want {
				t.Errorf("%s: a %s that offered %q now offers %q, want %q", how, kind, before[kind], got, want)
			}
			for _, a := range Capabilities(p, s).Actions {
				if a.Action != "cancel" && (a.Reason == nil || (!a.Available && before[kind] != "" && strings.Contains(before[kind], a.Action) && !strings.Contains(*a.Reason, "disabled by an administrator"))) {
					t.Errorf("%s: %s of a %s is off without the switch as its reason: %v", how, a.Action, kind, a.Reason)
				}
			}
		}
		// every action but the cancel is refused before anything is sent
		sent := len(b.Requests)
		for _, action := range Actions {
			if action == "cancel" {
				continue
			}
			for _, s := range systems {
				req := Request{Action: action, Confirm: &s.Name}
				_ = req.Validate()
				var ae *apierr.Error
				if _, err := Execute(context.Background(), p, s, req, "Ann"); !errors.As(err, &ae) || len(b.Requests) != sent {
					t.Errorf("%s: %s of %s: %v (requests sent: %v)", how, action, s.ID, err, b.Requests[sent:])
				}
			}
		}
		// and the cancel goes out
		req := Request{Action: "cancel"}
		_ = req.Validate()
		res, err := Execute(context.Background(), p, laptop, req, "Ann")
		if err != nil || res.Message != "cancelled" || len(b.Requests) != sent+1 || !strings.HasPrefix(b.Requests[sent], "laptop POST /power map[action:cancel") {
			t.Fatalf("%s: the cancel: %+v %v (requests: %v)", how, res, err, b.Requests[sent:])
		}
		if err := p.Admin.SetControlDisabled(false, "test", ""); err != nil {
			t.Fatal(err)
		}
	}
	// the agent's update is no cancel: the kill switch refuses it (TestUpdateAgent)
}
