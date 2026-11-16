package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
)

// An admin who signs another account's session out confirms a sentence that names the account. It named it by
// the user name alone: two accounts without a user name are both "(address hidden)", and a user name is what an
// account says about itself. The account id is in it now, in the server's words and in the page's label.
func TestSignOutOfAnotherAccountNamesItsID(t *testing.T) {
	r := newNamesRig(t, true)
	e := r.env
	sessionOf := func(sub string) string {
		list, err := e.s.Poller.Admin.Sessions()
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range list {
			if x.Sub == sub {
				return e.s.codec.handle(x.SID)
			}
		}
		t.Fatalf("no session of %s", sub)
		return ""
	}
	e.s.Cfg.StepUpWindowS = 180
	for sub, want := range map[string]string{
		"sub-s": "sign out a session (of PineTop [id sub-s]: ",
		"sub-o": "sign out a session (of (address hidden) [id sub-o]: ",
	} {
		x := e.do("POST", "/api/admin/sessions/revoke", map[string]any{"session": sessionOf(sub)}, r.hdr("sub-n")...)
		d, _ := x.body["detail"].(map[string]any)
		if action := fmt.Sprint(d["action"]); !stepUpRequired(x) || !strings.HasPrefix(action, want) {
			t.Errorf("the confirmation of a sign-out of %s: %d %q, want it to begin with %q", sub, x.code, action, want)
		}
		if held := namesHeld(string(x.raw)); len(held) > 0 {
			t.Errorf("the confirmation of a sign-out of %s holds %q", sub, held)
		}
	}
	// one's own other session needs no account in it
	name, mail := "Real Name", "owner@example.com"
	if err := e.s.Poller.Admin.AddSession("own-other-session-0123", adminstate.NewSession{Sub: "sub-n", Name: &name, Email: &mail, IP: "192.0.2.77", UA: uaFirefoxWin}); err != nil {
		t.Fatal(err)
	}
	x := e.do("POST", "/api/me/sessions/revoke", map[string]any{"session": e.s.codec.handle("own-other-session-0123")}, r.hdr("sub-n")...)
	d, _ := x.body["detail"].(map[string]any)
	if action := fmt.Sprint(d["action"]); !stepUpRequired(x) || strings.Contains(action, "[id ") || !strings.Contains(action, "192.0.2.77") {
		t.Errorf("the confirmation of a sign-out of one's own session: %d %q", x.code, action)
	}
}
