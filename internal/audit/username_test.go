package audit

import (
	"path/filepath"
	"strings"
	"testing"
)

// An entry carries the user name the account had when it was written, next to the actor text and the subject,
// which stay what they were. The filter finds an entry by a part of that user name; with the subjects of the
// accounts that are named so today (ReadWho) it also finds their entries of an older version, which have the
// address and the subject and no user name. A user name is capped like the other fields.
func TestEntriesCarryTheUsernameAndTheFilterFindsIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	sub := func(s string) *string { return &s }
	for _, e := range []Entry{
		{Actor: "owner@example.com", Sub: sub("sub-n"), Action: "power.shutdown", Target: "tower", Outcome: "ok", Source: "ui"},                    // an older version's
		{Actor: "owner@example.com", Sub: sub("sub-n"), Username: "Juniper", Action: "power.reboot", Target: "tower", Outcome: "ok", Source: "ui"}, // this version's
		{Actor: "friend@example.com", Sub: sub("sub-s"), Username: "PineTop", Action: "host.add", Target: "shed", Outcome: "ok", Source: "ui"},
		{Actor: "root", Action: "control.disable", Outcome: "ok", Source: "cli"},
		{Actor: "x@example.com", Sub: sub("sub-x"), Username: strings.Repeat("u", 5000), Action: "auth.login", Outcome: "ok", Source: "ui"},
	} {
		if _, err := Write(path, e); err != nil {
			t.Fatal(err)
		}
	}
	actions := func(user string, subs []string) string {
		entries, _ := ReadWho(path, user, subs, "", "", nil, 100)
		var out []string
		for _, e := range entries {
			out = append(out, e["action"].(string))
		}
		return strings.Join(out, " ")
	}
	for _, c := range []struct {
		user string
		subs []string
		want string
	}{
		{"", nil, "auth.login control.disable host.add power.reboot power.shutdown"},
		{"juni", nil, "power.reboot"},                                 // by the user name an entry has
		{"JUNIPER", []string{"sub-n"}, "power.reboot power.shutdown"}, // ... and the older entry of the account named so today
		{"pine", []string{"sub-s"}, "host.add"},
		{"owner@", nil, "power.reboot power.shutdown"}, // by the address, as before
		{"sub-n", nil, "power.reboot power.shutdown"},
		{"nobody", []string{"sub-zz"}, ""},
		{"root", []string{"sub-n"}, "control.disable power.reboot power.shutdown"}, // (an entry without a subject is found by its text only)
	} {
		if got := actions(c.user, c.subs); got != c.want {
			t.Errorf("filter %q with %v finds %q, want %q", c.user, c.subs, got, c.want)
		}
	}
	if Matches(map[string]any{"actor": "a@example.com", "sub": "s1", "username": "Willow"}, "will", "", "") != true ||
		Matches(map[string]any{"actor": "a@example.com", "sub": "s1"}, "will", "", "") {
		t.Error("Matches by the user name")
	}
	// read back: actor and sub as written, the user name next to them, a long one capped
	entries, _ := Read(path, "", "", "", nil, 100)
	if e := entries[3]; e["actor"] != "owner@example.com" || e["sub"] != "sub-n" || e["username"] != "Juniper" {
		t.Errorf("an entry of this version: %v", e)
	}
	if e := entries[4]; e["actor"] != "owner@example.com" || e["username"] != nil {
		t.Errorf("an entry of an older version has no user name: %v", e)
	}
	if n, _ := entries[0]["username"].(string); len(n) == 0 || len(n) > maxField+4 || entries[0]["actor"] != "x@example.com" {
		t.Errorf("a long user name is kept with %d bytes (actor %v)", len(n), entries[0]["actor"])
	}
}
