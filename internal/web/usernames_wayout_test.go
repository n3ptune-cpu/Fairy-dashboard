package web

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
)

// The accounts are the ones of usernames_test.go, and made up like them.

// What a machine supplies in a notification of this build (the message of a failed agent update, a socket's
// name, the outcome of an audited action) is not looked up among the accounts on the way to the notification
// service either: an address in it is hidden, never turned into a user name. "agent update failed:
// owner@example.com" went out as "agent update failed: Juniper", which told whoever runs that agent and reads
// the channel whose address it is. (The list and the stream had this already.) A sentence an older version
// stored is still read with the user name in the address's place.
func TestAMachinesTextIsNotLookedUpOnTheWayOut(t *testing.T) {
	r := newNamesRig(t, true)
	e := r.env
	var out []string
	e.s.sendOutside = func(title, message string) { out = append(out, title+" | "+message) }
	said := []string{"owner@example.com", "FRIEND@EXAMPLE.COM", "friend@example.com (unverified)", "wide@example.com unverified", "old@example.com", "nobody@example.org", "<narrow@example.com>"}
	for i, s := range said {
		// as the panel writes it, with what the agent's self_update.last.message says
		e.s.Notes.Add(notifications.Event{Kind: fmt.Sprint("update_failed_", i), Type: "update", Severity: notifications.Warning, SystemID: fmt.Sprint("machine-", i),
			Host: "TOWER", Text: "agent update failed: " + s})
	}
	if len(out) != len(said) {
		t.Fatalf("%d messages for %d events: %q", len(out), len(said), out)
	}
	for i, o := range out {
		want := "agent update failed: " + access.HiddenAddress
		if strings.HasPrefix(said[i], "<") {
			want = "agent update failed: <" + access.HiddenAddress + ">"
		}
		if o != "Fairy: TOWER: "+want+" | "+want+" (warning) at "+o[len(o)-20:] {
			t.Errorf("an agent said %q; the notification service is sent %q", said[i], o)
		}
	}
	// the outcome and the details of an audited control action
	out = nil
	c := &reqCtx{s: e.s, w: httptest.NewRecorder(), r: httptest.NewRequest("POST", "/api/systems/tower/update", nil)}
	c.audit(&model.User{Sub: "sub-n", Auth: "session", Email: model.P("owner@example.com"), EmailVerified: true}, "update.agent", "tower",
		map[string]any{"message": "friend@example.com is not allowed"}, "failed: the agent said owner@example.com")
	if len(out) != 1 || out[0] != `Fairy: update.agent on tower | Juniper via ui: failed: the agent said `+access.HiddenAddress+` {"message":"`+access.HiddenAddress+` is not allowed"}` {
		t.Errorf("an audited action with a machine's text: %q", out)
	}
	all := strings.Join(out, "\n")
	// a sentence made from what an older version stored: the address is the person, and is looked up
	out = nil
	e.s.Notes.AddOld(notifications.Event{Kind: "wakeupdate_interrupted", Type: "update", Severity: notifications.Warning, Host: "Fairy panel",
		Text: "the panel restarted during the run owner@example.com started at 10:00 UTC; it is not continued"})
	if len(out) != 1 || !strings.Contains(out[0], "during the run Juniper started at 10:00 UTC") {
		t.Errorf("a sentence of an older version: %q", out)
	}
	if held := namesHeld(all + strings.Join(out, "\n")); len(held) > 0 {
		t.Errorf("a message to the notification service holds %q", held)
	}
}
