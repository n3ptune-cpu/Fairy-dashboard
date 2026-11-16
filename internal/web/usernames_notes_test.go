package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
)

// A notification this version makes can hold a text a machine supplied: the message of an agent's failed
// update, the name of a socket. An address in such a text is not looked up among the accounts when the
// notification is handed out (it was, like every sentence: "agent update failed: owner@example.com" came back
// as "agent update failed: Juniper" to everybody who sees the machine, so whoever runs its agent could ask whose
// address that is). The people the panel itself wrote into the sentence are there by their user names already.
// An event of an older version names a person by the address, and still has the user name put in its place.
func TestANotificationOfNowIsNotLookedUpAmongTheAccounts(t *testing.T) {
	r := newNamesRig(t, true)
	e := r.env
	if _, _, err := e.s.Access.SeenIdentity(access.Identity{Sub: "sub-b", Name: "Bare Nameson", Username: "bare"}, access.RoleViewer, "first sign-in"); err != nil {
		t.Fatal(err)
	}
	for kind, text := range map[string]string{ // (a kind each: repeats of one kind are counted on the first)
		"update_failed":  "agent update failed: owner@example.com",
		"helper_stopped": "agent update failed: OWNER@EXAMPLE.COM (unverified), asked by Bare Nameson",
		"update_done":    "agent now runs friend@example.com (was 14)",
		"plug_switched":  "Lamp old@example.com (Desk bar) switched off by " + e.s.whoShown(&model.User{Sub: "sub-s", Auth: "session"}),
	} {
		e.s.Notes.Add(notifications.Event{Kind: kind, Type: "update", Severity: notifications.Warning, SystemID: "tower", Host: "TOWER", Text: text})
	}
	e.s.Notes.AddOld(notifications.Event{Kind: "host_options", Type: "host", Severity: notifications.Warning, SystemID: "tower", Host: "TOWER", Text: "run started by owner@example.com was cancelled by Bare Nameson"})
	readBob, _, err := e.s.Tokens.CreateForSites("bob-reader", "sub-n", "read", "cli:test", 0, []string{"Bob"})
	if err != nil {
		t.Fatal(err)
	}
	for what, hdr := range map[string][]string{"a viewer of one site": {"Cookie", r.ck["sub-l"]}, "a read token of one site": {"Authorization", "Bearer " + readBob}, "the admin": {"Cookie", r.ck["sub-n"]}} {
		x := e.do("GET", "/api/notifications?limit=200", nil, hdr...)
		_, stream := e.streamBody("/api/stream", hdr...)
		for where, body := range map[string]string{"the list": string(x.raw), "the stream": stream} {
			for _, want := range []string{`agent update failed: (address hidden)"`, `agent update failed: (address hidden), asked by (name hidden)"`, `agent now runs (address hidden) (was 14)"`,
				`Lamp (address hidden) (Desk bar) switched off by PineTop"`} {
				if where == "the list" && !strings.Contains(body, want) {
					t.Errorf("%s, %s: no notification says %s", what, where, want)
				}
			}
			for _, not := range []string{"failed: Juniper", "now runs PineTop", "failed: (address hidden), asked by bare", "Lamp (address hidden) (Desk bar) switched off by (", "by_name"} {
				if strings.Contains(body, not) {
					t.Errorf("%s, %s holds %q: a text a machine supplied was looked up among the accounts", what, where, not)
				}
			}
			if held := namesHeld(body); len(held) > 0 || strings.Contains(body, "Bare Nameson") {
				t.Errorf("%s, %s holds %q", what, where, held)
			}
		}
		if !strings.Contains(string(x.raw), `run started by Juniper was cancelled by bare"`) {
			t.Errorf("%s: the event of an older version: %s", what, clipText(string(x.raw), 600))
		}
	}
	// the store keeps the note with the event, and a version that does not know it reads the file all the same
	e.s.Notes.Flush()
	raw := string(must(os.ReadFile(filepath.Join(e.s.Cfg.DataDir, "notifications.json"))))
	if n := strings.Count(raw, `"by_name": true`) + strings.Count(raw, `"by_name":true`); n < 4 {
		t.Errorf("notifications.json marks %d events as written with user names:\n%s", n, clipText(raw, 600))
	}
}

// A sentence an older version stored names a person by the address. The address of a known account is replaced
// in it whatever it looks like: an address without a dot (an identity provider inside a network), a top-level
// name in its ASCII form. It was found by its shape only, so these went on to viewers, to read tokens and to
// the notification service.
func TestAKnownAddressOfAnUnusualShapeInAnOldSentence(t *testing.T) {
	r := newNamesRig(t, true)
	e := r.env
	for _, a := range []namesAccount{
		{"r4-corp", "corpuser", "ops@corp", true, "Corp Opsman", access.RoleAdmin, nil},
		{"r4-puny", "punyuser", "idn@example.xn--zckzah", true, "Puny Coder", access.RoleViewer, []string{"Bob"}},
		{"r4-none", "", "svc@intranet", true, "No Username", access.RoleViewer, []string{"Bob"}},
	} {
		e.cookie(a.user(true), 0)
		if _, _, err := e.s.Access.SetRole(a.sub, a.role, a.sites, "test", true); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []notifications.Event{
		{Kind: "plug_switched", Type: "power", Severity: notifications.Warning, SystemID: "tower", Host: "TOWER", Text: "Lamp (Desk bar) switched off by ops@corp"},
		{Kind: "plug_switched", Type: "power", Severity: notifications.Warning, SystemID: "tower", Host: "TOWER", Text: "Fan (Desk bar) switched on by idn@example.xn--zckzah"},
		{Kind: "host_options", Type: "host", Severity: notifications.Warning, SystemID: "tower", Host: "TOWER", Text: "links changed by OPS@CORP (unverified)."},
		{Kind: "host_options", Type: "host", Severity: notifications.Warning, SystemID: "tower", Host: "TOWER", Text: "run timings changed by svc@intranet"},
	} {
		e.s.Notes.AddOld(n) // an older version stored them
	}
	readBob, _, err := e.s.Tokens.CreateForSites("bob-reader", "sub-n", "read", "cli:test", 0, []string{"Bob"})
	if err != nil {
		t.Fatal(err)
	}
	private := []string{"ops@corp", "@corp", "xn--p1ai", "idn@", "svc@intranet", "@intranet"}
	for what, hdr := range map[string][]string{"a viewer of every site": {"Cookie", r.ck["sub-v"]}, "a viewer of one site": {"Cookie", r.ck["sub-l"]},
		"a read token of one site": {"Authorization", "Bearer " + readBob}, "the admin": {"Cookie", r.ck["sub-n"]}} {
		x := e.do("GET", "/api/notifications?limit=200", nil, hdr...)
		body := string(x.raw)
		for _, p := range private {
			if strings.Contains(strings.ToLower(body), p) {
				t.Errorf("%s: the notifications hold %q", what, p)
			}
		}
		for _, want := range []string{`switched off by corpuser"`, `switched on by punyuser"`, `links changed by corpuser."`, `run timings changed by (address hidden)"`} {
			if x.code != 200 || !strings.Contains(body, want) {
				t.Errorf("%s: the notifications (%d) do not hold %s", what, x.code, want)
			}
		}
		_, stream := e.streamBody("/api/stream", hdr...)
		for _, p := range private {
			if strings.Contains(strings.ToLower(stream), p) {
				t.Errorf("%s: the stream holds %q", what, p)
			}
		}
	}
	// ... and what leaves the panel
	var got []string
	e.s.sendOutside = func(title, message string) { got = append(got, title+" | "+message) }
	for _, text := range []string{"started by ops@corp: 1 machine", "links changed by idn@example.xn--zckzah", `{"email":"ops@corp"}`, "by svc@intranet."} {
		e.s.outside("Fairy: test", text)
	}
	all := strings.Join(got, "\n")
	for _, p := range private {
		if strings.Contains(strings.ToLower(all), p) {
			t.Errorf("a message to the notification service holds %q:\n%s", p, all)
		}
	}
	for _, want := range []string{"started by corpuser: 1 machine", "links changed by punyuser", `{"email":"corpuser"}`, "by (address hidden)."} {
		if !strings.Contains(all, want) {
			t.Errorf("no message holds %q:\n%s", want, all)
		}
	}
}
