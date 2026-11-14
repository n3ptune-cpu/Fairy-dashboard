package web

import (
	"fmt"
	"strings"
	"testing"
)

// "Edit host", Wake-on-LAN: two guards that no test held by themselves (second review, N4: a change of each
// went unnoticed by every test).

// "Off" for a machine that has no Wake-on-LAN is nothing: no decision is stored for it (it would switch off
// a wol: that config.yaml gets later, with nothing on any page having said so), and nothing is in the audit
// log as a change.
func TestEditHostWolOffForAMachineWithoutWakeIsNothing(t *testing.T) {
	r := hedSetup(t)
	res := r.confirmed("pc-b", map[string]any{"wol": map[string]any{"off": true}})
	if res["ok"] != true || res["message"] != "nothing to change" {
		t.Fatalf("off for pc-b, which has no wol: %v", res)
	}
	if r.storedOptions() != nil {
		t.Errorf("stored: %v", r.storedOptions())
	}
	for _, e := range r.auditEntries("", "host.options") {
		if p, _ := e["params"].(map[string]any); e["target"] == "pc-b" && p["changes"] != nil {
			t.Errorf("audit: %v", e)
		}
	}
	// with a name next to it: the name is the one change
	res = r.confirmed("pc-b", map[string]any{"name": "Den", "wol": map[string]any{"off": true}})
	if res["ok"] != true || !strings.Contains(fmt.Sprint(res["message"]), "changes saved") || r.storedHost("pc-b")["wol"] != nil || r.storedHost("pc-b")["name"] != "Den" {
		t.Errorf("off with a name: %v, stored %v", res, r.storedHost("pc-b"))
	}
}

// The relay of an entry with a target address or a password is refused by the request's own check, in its own
// words ("cannot be changed here"), not only because the decision would not be used afterwards: the two
// guards are separate, and each is pinned (this one here, the other in the poller's and the reader's tests).
func TestEditHostWolRelayOfAFixedEntrySaysSo(t *testing.T) {
	r := hedSetup(t)
	for id, why := range map[string]string{"box-c": "target address", "box-d": "password"} {
		res := r.confirmed(id, map[string]any{"wol": map[string]any{"relay": "relay-b"}})
		msg := fmt.Sprint(res["message"])
		if res["status"] != 422.0 || !strings.HasPrefix(msg, "wol: the relay cannot be changed here: ") || !strings.Contains(msg, why) {
			t.Errorf("%s: %v", id, res)
		}
		// config.yaml's own relay, said again, is no change and no refusal
		if res := r.confirmed(id, map[string]any{"wol": map[string]any{"relay": "relay-a"}}); res["ok"] != true || res["message"] != "nothing to change" {
			t.Errorf("%s, its own relay: %v", id, res)
		}
	}
	if r.storedOptions() != nil {
		t.Errorf("stored: %v", r.storedOptions())
	}
}
