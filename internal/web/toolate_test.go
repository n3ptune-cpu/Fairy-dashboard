package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
)

// The agent answers a cancel that comes after the action was carried out with 409 "too late to cancel: ..."
// (Fairy-agent, r4-a1-03; it was 200 "cancelled" for a machine that was going off). The panel reads it as what
// it is, the agent's "no" in its words: the page shows them as an error, the audit entry is an error, and
// nothing says "cancelled". (The same for the agent's 409 while the action is being carried out.)
func TestACancelThatIsTooLateIsNotCancelled(t *testing.T) {
	for _, said := range []string{"too late to cancel: systemctl poweroff was already run", "too late to cancel: the shutdown is being carried out"} {
		e := newSwitchEnv(t)
		e.b.Reply = func(_, path string) (int, jsonx.M, error) {
			if path == "/power" {
				return 409, jsonx.M{"error": said}, nil
			}
			return 0, nil, nil
		}
		r := e.post("cancel", e.admin...)
		if r.code != 400 || r.body["detail"] != said || r.body["ok"] != nil {
			t.Errorf("the cancel: %d %s, want 400 and the agent's words", r.code, r.raw)
		}
		outcome := ""
		for _, a := range e.auditEntries("", "power.cancel") {
			outcome = fmt.Sprint(a["outcome"])
		}
		if outcome != "error 400: "+said {
			t.Errorf("the audit entry says %q", outcome)
		}
		for _, n := range e.do("GET", "/api/notifications", nil, "Cookie", e.c).body["events"].([]any) {
			if text := fmt.Sprint(n.(map[string]any)["text"]); strings.Contains(text, "cancel") {
				t.Errorf("a notification speaks of a cancel: %q", text)
			}
		}
	}
}
