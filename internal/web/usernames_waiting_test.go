package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
)

// The accounts are the ones of usernames_test.go, and made up like them.

// An account that only waits for access and has no e-mail address (its provider sends none) signs in with a
// display name that is the admin's user name, or a word of the panel's sentences. Every record the admin wrote
// then read "(name hidden)" for everybody, and "Lamp (Desk bar) switched off by Juniper" read "switched (name
// hidden) by (name hidden)", in the list and on the way to the notification service.
//
// An account that waits takes part in no reading of a stored text: what everybody reads is the same before and
// after such an account signed in.
func TestAWaitingAccountWithoutAnAddressChangesNoRecord(t *testing.T) {
	r := newNamesRig(t, true)
	e := r.env
	var out []string
	e.s.sendOutside = func(title, message string) { out = append(out, title+" | "+message) }
	if p := e.do("PUT", "/api/admin/hosts/laptop/options", map[string]any{"name": "Den laptop 5"}, r.hdr("sub-n")...); p.code != 200 {
		t.Fatal(p.code, string(p.raw))
	}
	n := 0
	note := func() { // a notification as this version writes it, and one an older version stored
		n++
		e.s.Notes.Add(notifications.Event{Kind: fmt.Sprint("waiting_now_", n), Type: "power", Severity: notifications.Warning, SystemID: "tower", Host: "TOWER",
			Text: "Kettle (Den bar) switched off by " + e.s.whoShown(&model.User{Sub: "sub-n", Auth: "session"})})
		e.s.Notes.AddOld(notifications.Event{Kind: fmt.Sprint("waiting_old_", n), Type: "power", Severity: notifications.Warning, SystemID: "laptop", Host: "LAPTOP",
			Text: "Kettle (Den bar) switched off by owner@example.com"})
	}
	read := func(what string) {
		t.Helper()
		if got := shownAs(e, r.ck["sub-n"]); got != "Juniper" {
			t.Errorf("%s: the admin is shown as %q", what, got)
		}
		h, _ := e.do("GET", "/api/admin/hosts/laptop/options", nil, "Cookie", r.ck["sub-s"]).body["host"].(map[string]any)
		name, _ := h["name"].(map[string]any)
		if name["by"] != "Juniper" {
			t.Errorf("%s: who changed the name in Edit host reads %v", what, name["by"])
		}
		for who, ck := range map[string]string{"a viewer": r.ck["sub-v"], "the admin": r.ck["sub-n"]} {
			x := e.do("GET", "/api/notifications?limit=200", nil, "Cookie", ck)
			seen := 0
			for _, ev := range x.body["events"].([]any) {
				if s := fmt.Sprint(ev.(map[string]any)["text"]); strings.HasPrefix(s, "Kettle (Den bar)") {
					seen++
					if s != "Kettle (Den bar) switched off by Juniper" {
						t.Errorf("%s: %s reads the notification as %q", what, who, s)
					}
				}
			}
			if seen != 2*n {
				t.Errorf("%s: %s reads %d of the %d notifications", what, who, seen, 2*n)
			}
		}
		pushed := 0
		for _, o := range out {
			if strings.Contains(o, "Kettle (Den bar)") {
				pushed++
				if !strings.Contains(o, "Kettle (Den bar) switched off by Juniper (warning)") || strings.Contains(o, access.HiddenName) {
					t.Errorf("%s: the notification service is sent %q", what, o)
				}
			}
		}
		if pushed != 2*n {
			t.Errorf("%s: %d of the %d notifications were sent on", what, pushed, 2*n)
		}
	}
	note()
	read("before")
	// no address, the display name is the admin's user name
	wanda := e.cookie(model.User{Sub: "9a1b2c3d-wanda", Name: model.P("Juniper"), PreferredUsername: model.P("wanda")}, 0)
	if me := e.do("GET", "/api/me", nil, "Cookie", wanda); me.code != 200 || me.body["shown_as"] != "wanda" {
		t.Fatalf("the account that waits: %d %s", me.code, me.raw)
	}
	if u := e.s.Access.Load().Users["9a1b2c3d-wanda"]; u.Role != access.RolePending || u.Email != "" || u.Name != "Juniper" {
		t.Fatalf("the fixture: users.json has %+v", u)
	}
	note()
	read("after an account that waits, without an address, signed in with the display name Juniper")
	// ... and others with a word of the sentence, and with nothing but that name
	e.cookie(model.User{Sub: "7e7e7e7e-off", Name: model.P("off")}, 0)
	e.cookie(model.User{Sub: "7e7e7e7e-switched", Name: model.P("switched"), PreferredUsername: model.P("Lamp")}, 0)
	e.cookie(model.User{Sub: "7e7e7e7e-jun", Name: model.P("juniper")}, 0)
	note()
	read("after more accounts that wait signed in with the display names off, switched and juniper")
}
