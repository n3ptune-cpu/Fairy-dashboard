package agentcontract_test

import (
	"strings"
	"testing"

	agentapi "github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
)

// Audit 5, round 3, r3-x1-03: the auditor's TestR3X1_TheDialogOfASingleActionOmitsWhatIsSigned, turned round.
// The dialog of a single action said "power action on pc-a (action: shutdown)" and "fan speed of pc-a": not the
// delay, the zone or the percent that the machine binds its challenge to and the passkey signs. Now one line
// of the dialog says them, in the words the batch dialog uses, made from the same request the machine was
// asked about.
func TestTheDialogOfASingleActionShowsWhatIsSigned(t *testing.T) {
	pn := newApprovalPanel(t)
	pn.turnOn("pc-a")
	bound := func() string { // what the machine was asked to bind its last challenge to
		raw := pn.a.sent(agentapi.PathApprovalChallenge)
		return string(raw[len(raw)-1])
	}
	st, out := pn.call("POST", "/api/systems/pc-a/power", map[string]any{"action": "shutdown", "delay_s": 86400, "confirm": "PC-A"})
	d, _ := out["detail"].(map[string]any)
	if _, _, _, ok := asked(st, out); !ok {
		t.Fatalf("power: %d %v", st, out)
	}
	if d["signs"] != "shutdown in 86400 s" || !strings.Contains(bound(), `"delay_s":86400`) {
		t.Fatalf("the dialog of a shutdown in 24 hours: signs=%v, the machine was asked about %s", d["signs"], bound())
	}
	st, out = pn.call("POST", "/api/systems/pc-a/fan/duty", map[string]any{"zone": 1, "percent": 30})
	d, _ = out["detail"].(map[string]any)
	if _, _, _, ok := asked(st, out); !ok {
		t.Fatalf("fan duty: %d %v", st, out)
	}
	if d["signs"] != "fan zone 1 to 30%" || !strings.Contains(bound(), `"zone":1`) || !strings.Contains(bound(), `"percent":30`) {
		t.Fatalf("the dialog of a fan change: signs=%v, the machine was asked about %s", d["signs"], bound())
	}
	st, out = pn.call("POST", "/api/admin/updates/pc-a", map[string]any{})
	d, _ = out["detail"].(map[string]any)
	if _, _, _, ok := asked(st, out); !ok || d["signs"] != "update of its agent" {
		t.Fatalf("update: %d %v", st, out)
	}
	// a batch keeps its line per machine, and has no second one
	pn.turnOn("pc-b")
	st, out = pn.call("POST", "/api/admin/updates", map[string]any{})
	d, _ = out["detail"].(map[string]any)
	if _, _, items, ok := asked(st, out); !ok || d["signs"] != nil || !strings.Contains(items, "PC-A: update of its agent (checked on the machine)") ||
		!strings.Contains(items, "PC-B: update of its agent (checked on the machine)") {
		t.Fatalf("Update all for two machines: %d %v", st, out)
	}
}
