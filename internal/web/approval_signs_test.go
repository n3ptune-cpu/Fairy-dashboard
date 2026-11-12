package web

import (
	"fmt"
	"strings"
	"testing"
)

// Audit 5, round 3, r3-x1-03: the "Confirm it's you" dialog of a single action that a machine checks itself
// shows what the passkey signs, with its numbers, in one short line ("signs"). A batch has its line per machine
// (items) and a Turn on its own; an action no machine checks has none.
func TestTheSingleDialogSaysWhatThePasskeySigns(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop, a.tower)
	detail := func(r resp) map[string]any {
		t.Helper()
		if _, _, ok := approvalAsked(r); !ok {
			t.Fatalf("not the dialog for an approval: %d %s", r.code, r.raw)
		}
		return r.body["detail"].(map[string]any)
	}
	body := a.shutdownBody()
	body["delay_s"] = 86400
	for _, c := range []struct {
		path  string
		body  any
		signs string
	}{
		{"/api/systems/laptop/power", body, "shutdown in 86400 s"},
		{"/api/systems/laptop/power", map[string]any{"action": "reboot", "delay_s": 15, "confirm": body["confirm"]}, "reboot in 15 s"},
		{"/api/systems/tower/fan/duty", map[string]any{"zone": 1, "percent": 30}, "fan zone 1 to 30%"},
		{"/api/systems/tower/fan/mode", map[string]any{"mode": "full"}, "fan mode full"},
		{"/api/admin/updates/laptop", map[string]any{}, "update of its agent"},
	} {
		d := detail(a.post(c.path, c.body))
		if d["signs"] != c.signs || d["items"] != nil {
			t.Errorf("%s %v: signs %v items %v, want %q", c.path, c.body, d["signs"], d["items"], c.signs)
		}
		// it is what the machine was asked about: the canonical text of its last question holds the same numbers
		m := a.laptop
		if strings.Contains(c.path, "tower") {
			m = a.tower
		}
		asked := m.Asked()
		last := fmt.Sprint(asked[len(asked)-1].Body)
		for _, n := range []string{"86400", "30"} {
			if strings.Contains(c.signs, n) && !strings.Contains(last, n) {
				t.Errorf("%s: the line says %q, the machine was asked about %s", c.path, c.signs, last)
			}
		}
	}
	// "Update all" that comes to one machine is a single action too (a real batch has its line per machine
	// and no second one: internal/agentcontract, TestTheDialogOfASingleActionShowsWhatIsSigned)
	if d := detail(a.post("/api/admin/updates", map[string]any{})); d["signs"] != "update of its agent" || d["items"] != nil {
		t.Errorf("Update all for one machine: signs %v items %v", d["signs"], d["items"])
	}
	// the page: one line in the dialog, through esc()
	js := appJS(t)
	for _, want := range []string{
		"const signs = kept && appr && typeof detail.signs === \"string\" && detail.signs ? `<span class=\"note\">Your passkey signs</span><b id=\"su-signs\">${esc(detail.signs)}</b>` : \"\";",
		"<b>${esc(pending.label)}</b>${runs}${signs}${items}</div>",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
}

// A Turn on has its own lines and sentence, and an action that no machine checks has no such line.
func TestOnlyASingleCheckedActionHasTheLine(t *testing.T) {
	a := newApprovalEnv(t)
	r := a.turnOn("laptop")
	if _, _, ok := approvalAsked(r); !ok {
		t.Fatalf("Turn on: %d %s", r.code, r.raw)
	}
	if d := r.body["detail"].(map[string]any); d["signs"] != nil || d["items"] == nil {
		t.Errorf("Turn on: signs %v items %v", d["signs"], d["items"])
	}
	r = a.post("/api/systems/tower/fan/mode", map[string]any{"mode": "full"}) // tower has no check
	if d, _ := r.body["detail"].(map[string]any); r.code != 401 || d["approval"] != nil || d["signs"] != nil {
		t.Errorf("an action no machine checks: %d %s", r.code, r.raw)
	}
}
