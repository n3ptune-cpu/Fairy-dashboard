package access

import (
	"strings"
	"testing"
)

// The accounts of these tests are made up. Nobody has these addresses.

// setBack is what users.json says of an account that had a role and was set back to "No access (waiting)":
// who changed the role last (Store.SetRole writes it), or, when the role in the file was changed by hand, how
// the account became an admin at its first sign-in. neverARole is what it says of an account that signed in
// and was never given one (the last two are written by a request of a session whose account is gone, and by
// the demo).
var (
	setBack    = []string{"panel by owner@example.com", "panel by Juniper", "cli by root", "control_groups", "bootstrap_admins"}
	neverARole = []string{"first sign-in", "first sign-in (removed before: no bootstrap)", "", "demo"}
)

var (
	setBackRecords = []string{"Dana Demotedson", "dana demotedson", "dd", "DD", "dana@example.org", "dana@example.org (unverified)",
		"panel by Dana Demotedson", "panel by dana@example.org", "Juniper", "owner@example.com", "Bare Name", "sub-n", "friend@example.com", "off", "root"}
	setBackSentences = []string{"shutdown requested by Dana Demotedson (the machine shuts down in 60 s)", "started by Dana Demotedson: 2 machines",
		"the panel restarted during the run Dana Demotedson started at 10:04 UTC; it is not continued", "run started by Dana Demotedson was cancelled by Dana Demotedson",
		"No machine accepted this confirmation (by Dana Demotedson). The run is not confirmed and still asks", "started by dana@example.org: 3 machines",
		"switched off by dd", "Lamp (Desk bar) switched off by Juniper", "run started by Juniper was cancelled by Bare Name", "started by owner@example.com: 3 machines",
		"agent update failed: owner@example.com", "switched off", "Keep on by off: the run calls its switch-off off", "accepted by sub-n"}
)

// reads is everything the panel reads out of stored text and out of an agent's answer with these accounts, and
// how it names each of them: one line per question.
func reads(d Data) []string {
	p := d.People()
	var out []string
	for _, sub := range []string{"sub-n", "sub-s", "sub-o", "sub-b", "sub-z"} {
		n, _ := p.Of(sub)
		out = append(out, "Of "+sub+" = "+n)
	}
	for _, r := range setBackRecords {
		out = append(out, "Shown "+r+" = "+p.Shown(r), "FromOutside "+r+" = "+p.FromOutside(r))
	}
	for _, s := range setBackSentences {
		out = append(out, "ShownIn "+s+" = "+p.ShownIn(s), "HiddenIn "+s+" = "+p.HiddenIn(s))
	}
	return out
}

// An account that had a role and was set back to "No access (waiting)" is not an account that only waits: it
// may have written records while it had its role, and an older version wrote its name into them when it had no
// e-mail address. Left out of the reading of stored text like an account that never had a role, its name was
// readable again in every such sentence, for every viewer.
//
// So every stored text reads with such an account exactly as it does while the account has a role: its name
// is hidden where a sentence says who did it, a record by its name or its address is its user name (the words
// that stand for them while it has none), and a record by its user name is its own. An account that signed in
// and was never given a role still takes part in nothing: everything reads as it does without the account.
func TestAnAccountSetBackToWaitingIsReadLikeOneWithARole(t *testing.T) {
	for _, w := range []User{
		{Name: "Dana Demotedson"},                                            // no address, no user name yet (as build 16 left it)
		{Name: "Dana Demotedson", Username: "dd"},                            // ... and after its next sign-in
		{Name: "Dana Demotedson", Username: "sub-n"},                         // a user name the panel does not show
		{Name: "Dana Demotedson", Username: "owner@example.com"},             //
		{Name: "Dana Demotedson", Email: "dana@example.org"},                 // with an address
		{Name: "Dana Demotedson", Email: "dana@example.org", Username: "dd"}, //
		{Name: "off"},                 // what it calls itself now can be anything
		{Name: "off", Username: "dd"}, //
		{Email: "owner@example.com", Name: "Dana Demotedson", Username: "PineTop"}, // another account's address, another's user name
	} {
		w.Sub = "sub-dana"
		without := reads(peopleData())
		w.Role, w.Via = RoleViewer, "panel by owner@example.com"
		d := peopleData()
		d.Users[w.Sub] = w
		withRole := reads(d)
		if strings.Join(withRole, "\n") == strings.Join(without, "\n") {
			t.Fatalf("the fixture: %+v with a role changes no reading", w)
		}
		for _, via := range setBack {
			w.Role, w.Via = RolePending, via
			d.Users[w.Sub] = w
			for i, got := range reads(d) {
				// (the one thing the role itself decides: of two accounts with one user name, the one that
				// may do more is shown by the plain name; that is about the accounts of now, not about a record)
				if got != withRole[i] && w.Username != "PineTop" {
					t.Errorf("set back to waiting (via %q), %+v:\n  got  %s\n  with a role  %s", via, w, got, withRole[i])
				}
			}
			if w.Name != "Dana Demotedson" || w.Email != "" { // (an older version wrote the name of an account without an address only)
				continue
			}
			p := d.People()
			for _, s := range setBackSentences[:5] {
				if strings.Contains(p.ShownIn(s), "Dana") {
					t.Errorf("set back to waiting (via %q), %+v: the sentence reads %q", via, w, p.ShownIn(s))
				}
				if strings.Contains(p.HiddenIn(s), "Dana") {
					t.Errorf("set back to waiting (via %q), %+v: the sentence of now reads %q", via, w, p.HiddenIn(s))
				}
			}
			if got := p.Shown("Dana Demotedson"); got != HiddenName && got != "dd" {
				t.Errorf("set back to waiting (via %q), %+v: a record by its name reads %q", via, w, got)
			}
		}
		for _, via := range neverARole {
			w.Role, w.Via = RolePending, via
			d.Users[w.Sub] = w
			for i, got := range reads(d) {
				if got != without[i] {
					t.Errorf("never given a role (via %q), %+v:\n  got  %s\n  without the account  %s", via, w, got, without[i])
				}
			}
		}
	}
	// the plain cases, spelled out
	d := peopleData()
	d.Users["sub-dana"] = User{Sub: "sub-dana", Name: "Dana Demotedson", Role: RolePending, Via: "panel by owner@example.com"}
	p := d.People()
	for text, want := range map[string]string{
		"started by Dana Demotedson: 2 machines":                           "started by " + HiddenName + ": 2 machines",
		"run started by Dana Demotedson was cancelled by Dana Demotedson":  "run started by " + HiddenName + " was cancelled by " + HiddenName,
		"the panel restarted during the run Dana Demotedson started at 10": "the panel restarted during the run " + HiddenName + " started at 10",
	} {
		if got := p.ShownIn(text); got != want {
			t.Errorf("ShownIn(%q) = %q, want %q", text, got, want)
		}
	}
	if got := p.Shown("Dana Demotedson"); got != HiddenName {
		t.Errorf("a record by the name of the account that was set back reads %q", got)
	}
	u := d.Users["sub-dana"]
	u.Username = "dd"
	d.Users["sub-dana"] = u
	p = d.People()
	if got := p.ShownIn("started by Dana Demotedson: 2 machines"); got != "started by dd: 2 machines" {
		t.Errorf("with a user name: %q", got)
	}
	if got := p.Shown("Dana Demotedson"); got != "dd" {
		t.Errorf("with a user name, a record by its name reads %q", got)
	}
	u.Via = "first sign-in" // the same account, had it never been given a role
	d.Users["sub-dana"] = u
	p = d.People()
	if got := p.ShownIn("started by Dana Demotedson: 2 machines"); got != "started by Dana Demotedson: 2 machines" {
		t.Errorf("an account that never had a role: the sentence reads %q (its name is a text like any other)", got)
	}
}

// What the store writes for the two kinds of account: an account an admin sets back to waiting, on the Users
// page or on the command line, keeps who did it, and is not an account that only waits; one that signs in and
// is given nothing is, also after an admin has "set" it to waiting once more.
func TestTheStoreTellsAnAccountSetBackFromOneThatOnlyWaits(t *testing.T) {
	st := NewStore(t.TempDir())
	if _, _, err := st.Seen("sub-a", "owner@example.com", "Real Name", RoleAdmin, "control_groups"); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"sub-panel", "sub-cli", "sub-viewer", "sub-waits"} {
		if _, _, err := st.SeenIdentity(Identity{Sub: sub, Name: "Name of " + sub}, RolePending, "first sign-in"); err != nil {
			t.Fatal(err)
		}
	}
	set := func(sub, role, by string) {
		t.Helper()
		if _, _, err := st.SetRole(sub, role, nil, by, false); err != nil {
			t.Fatal(err)
		}
	}
	set("sub-panel", RoleAdmin, "panel by Juniper")
	set("sub-panel", RolePending, "panel by Juniper")
	set("sub-cli", RoleAdmin, "panel by Juniper")
	set("sub-cli", RolePending, "cli by root")
	set("sub-viewer", RoleViewer, "cli by root")
	set("sub-viewer", RolePending, "panel by Juniper")
	set("sub-waits", RolePending, "panel by Juniper") // no change of the role: nothing is written
	d := st.Load()
	for sub, want := range map[string]string{"sub-panel": "panel by Juniper", "sub-cli": "cli by root", "sub-viewer": "panel by Juniper", "sub-waits": "first sign-in"} {
		if u := d.Users[sub]; u.Role != RolePending || u.Via != want {
			t.Errorf("%s: role %q, via %q; want it to wait, via %q", sub, u.Role, u.Via, want)
		}
	}
	p := d.People()
	for sub, want := range map[string]string{"sub-panel": HiddenName, "sub-cli": HiddenName, "sub-viewer": HiddenName, "sub-waits": "Name of sub-waits"} {
		if got := p.Shown("Name of " + sub); got != want {
			t.Errorf("a record by the name of %s reads %q, want %q", sub, got, want)
		}
	}
}
