package web

import (
	"encoding/json"
	"strings"
	"testing"
)

// Second review of fix7a (R7A-10): since the cancel of a countdown passes the kill switch (2a2c6af), an API
// token with control scope whose owner is an admin could cancel while the kill switch was on. A token skips
// "Confirm it's you", and before that change the kill switch stopped tokens entirely. With the kill switch on
// the cancel is for a signed-in admin's browser session only (with the confirmation, as before); a token gets
// the 423 it always got. With the kill switch off nothing changes for a token.
func TestWithTheKillSwitchOnACancelNeedsABrowserSession(t *testing.T) {
	token := func(raw string) []string { return []string{"Authorization", "Bearer " + raw} }
	// a panel whose laptop counts down, a control token and a read token (each step on a panel of its own: a
	// cancel that went through leaves nothing pending)
	fresh := func() (e *switchEnv, ctl, ro string) {
		e = newSwitchEnv(t)
		ctl, _, err := e.s.Tokens.Create("ops", "system", "control", "cli:test", 0)
		if err != nil {
			t.Fatal(err)
		}
		ro, _, _ = e.s.Tokens.Create("ha", "system", "read", "cli:test", 0)
		return e, ctl, ro
	}

	// the kill switch is off: a control token's cancel goes through, as ever
	e, ctl, ro := fresh()
	if r := e.post("cancel", token(ro)...); r.code != 403 || e.sent() != 0 {
		t.Fatalf("kill switch off, a read token's cancel: %d %s", r.code, r.raw)
	}
	if r := e.post("cancel", token(ctl)...); r.code != 200 || r.body["message"] != "cancelled" || e.sent() != 1 {
		t.Fatalf("kill switch off, a control token's cancel: %d %s (requests sent: %d)", r.code, r.raw, e.sent())
	}

	// the power switch of the host alone (the kill switch is off): nothing changes for a token either
	e, ctl, _ = fresh()
	if err := e.s.Poller.Admin.SetPowerDisabled("laptop", true); err != nil {
		t.Fatal(err)
	}
	if r := e.post("cancel", token(ctl)...); r.code != 200 || e.sent() != 1 {
		t.Fatalf("the host's power switch, a control token's cancel: %d %s (requests sent: %d)", r.code, r.raw, e.sent())
	}

	// the kill switch is on
	e, ctl, ro = fresh()
	if err := e.s.Poller.Admin.SetControlDisabled(true, "root (cli)", "stop everything"); err != nil {
		t.Fatal(err)
	}
	r := e.post("cancel", token(ctl)...)
	if r.code != 423 || !strings.Contains(r.raw2(), "control is disabled by an administrator (root (cli)): stop everything") || e.sent() != 0 {
		t.Errorf("kill switch on, a control token's cancel: %d %s (requests sent: %d); want 423 and nothing sent, as before the cancel passed the kill switch", r.code, r.raw, e.sent())
	}
	if r := e.post("cancel", token(ro)...); r.code != 403 || e.sent() != 0 {
		t.Errorf("kill switch on, a read token's cancel: %d %s", r.code, r.raw)
	}
	// every other action of the token: 423, as ever
	for _, action := range []string{"shutdown", "reboot", "sleep", "wake"} {
		if r := e.post(action, token(ctl)...); r.code != 423 || e.sent() != 0 {
			t.Errorf("kill switch on, a control token's %s: %d %s", action, r.code, r.raw)
		}
	}
	// a token cannot borrow a session's way through
	if r := e.post("cancel", append(token(ctl), e.admin...)...); r.code == 200 || e.sent() != 0 {
		t.Errorf("kill switch on, a token with a session cookie next to it: %d %s (requests sent: %d)", r.code, r.raw, e.sent())
	}
	if len(e.cancels()) != 0 {
		t.Errorf("a refused cancel has a power.cancel entry: %v", e.cancels())
	}
	// the signed-in admin's cancel still goes through, with the CSRF header and a fresh confirmation
	if r := e.post("cancel", e.admin...); r.code != 200 || r.body["message"] != "cancelled" || e.sent() != 1 {
		t.Errorf("kill switch on, the page's Cancel of a signed-in admin: %d %s (requests sent: %d)", r.code, r.raw, e.sent())
	}
}

// Whatever the body is, nothing but the agent's cancel leaves the panel while the kill switch is on (a second
// "action" key, a key in another case, another spelling). The reviewer's test of that; it held before.
func TestNothingDressedUpAsACancelPassesTheKillSwitch(t *testing.T) {
	for _, body := range []string{
		`{"action":"shutdown","ACTION":"cancel","delay_s":60,"confirm":"LAPTOP"}`,
		`{"action":"cancel","action":"shutdown","delay_s":60,"confirm":"LAPTOP"}`,
		`{"ACTION":"cancel","action":"reboot","delay_s":60}`,
		`{"action":"reboot","Action":"cancel","delay_s":10}`,
		`{"action":"Cancel","delay_s":60}`,
		`{"action":"cancel ","delay_s":60}`,
		`{"action":"cancel","delay_s":60,"only_via":"proxmox-api"}`,
		`{"action":"cancel","delay_s":60,"OnlyVia":"proxmox-api"}`,
		`{"action":"cancel","delay_s":60,"pending_unseen":true}`,
		`{"action":"cancel","delay_s":60,"PendingUnseen":true}`,
		`{"action":["cancel","shutdown"]}`,
	} {
		e := newSwitchEnv(t)
		if err := e.s.Poller.Admin.SetControlDisabled(true, "root (cli)", "stop everything"); err != nil {
			t.Fatal(err)
		}
		r := e.do("POST", "/api/systems/laptop/power", json.RawMessage(body), e.admin...)
		e.b.Mu.Lock()
		var sent []string
		for _, q := range e.b.Requests {
			if strings.Contains(q, "POST /power") {
				sent = append(sent, q)
			}
		}
		e.b.Mu.Unlock()
		for _, q := range sent {
			if !strings.Contains(q, "action:cancel") {
				t.Errorf("%s: with the kill switch on this left the panel: %s", body, q)
			}
		}
		if r.code == 200 && r.body["message"] != "cancelled" {
			t.Errorf("%s: 200 %s", body, r.raw)
		}
		if r.code == 200 {
			t.Errorf("%s: accepted (%s); every one of these bodies must be refused", body, r.raw)
		}
	}
}
