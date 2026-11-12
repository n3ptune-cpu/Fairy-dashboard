package web

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Audit 5, round 3 (r3-p2-07), through the panel: a hand-written kill switch in state.json that is not the
// object Fairy writes ("control_disabled": true, or a text) left control ON: /api/me said control_disabled
// null and a control request went through. It is the kill switch, on.
func TestAHandWrittenKillSwitchIsOn(t *testing.T) {
	for _, value := range []string{`true`, `"disabled by Ann"`, `1`, `null`} {
		e := setup(t)
		c := e.cookie(controller(), 0)
		admin := []string{"Cookie", c, "X-Requested-With", "fairy"}
		if r := e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 5}, admin...); r.code != 200 {
			t.Fatal("setup:", r.code, string(r.raw))
		}
		if err := os.WriteFile(e.s.Poller.Admin.StatePath, []byte(`{"control_disabled": `+value+`}`), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond) // the file is looked at once a second
		me := e.do("GET", "/api/me", nil, "Cookie", c)
		if me.body["control_disabled"] == nil {
			t.Errorf("control_disabled %s: /api/me says that control is on: %s", value, me.raw)
		}
		r := e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 7}, admin...)
		if r.code != 423 || !strings.Contains(r.raw2(), "control is disabled by an administrator (fairy)") || !strings.Contains(r.raw2(), "fairy admin control enable") {
			t.Errorf("control_disabled %s: a control request: %d %s", value, r.code, r.raw)
		}
		caps := e.do("GET", "/api/systems/laptop/power", nil, "Cookie", c)
		for _, a := range caps.body["actions"].([]any) {
			if a.(map[string]any)["available"] == true {
				t.Errorf("control_disabled %s: a power action is offered: %v", value, a)
			}
		}
	}
}
