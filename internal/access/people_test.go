package access

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The accounts of these tests are made up. Nobody has these addresses.
//
//	sub-n  user name Juniper, owner@example.com (verified), name "Real Name": the admin
//	sub-s  user name PineTop, friend@example.com, name "Other Person"
//	sub-o  no user name (has not signed in since the panel keeps them), old@example.com, name "Old Timer"
//	sub-b  no user name and no address, name "Bare Name"
//	sub-z  nothing but its subject
func peopleData() Data {
	t, f := true, false
	return Data{Users: map[string]User{
		"sub-n": {Sub: "sub-n", Username: "Juniper", Email: "owner@example.com", EmailVerified: &t, Name: "Real Name", Role: RoleAdmin},
		"sub-s": {Sub: "sub-s", Username: "PineTop", Email: "friend@example.com", EmailVerified: &f, Name: "Other Person", Role: RoleViewer},
		"sub-o": {Sub: "sub-o", Email: "old@example.com", EmailVerified: &t, Name: "Old Timer", Role: RoleViewer},
		"sub-b": {Sub: "sub-b", Name: "Bare Name", Role: RoleViewer},
		"sub-z": {Sub: "sub-z", Role: RolePending},
	}}
}

// A stored text that names a person is handed out as the account's user name: the address an older version
// stored, with or without the mark of an address that is not verified, in any case; the name it stored for an
// account without an address; the user name a record has since build 17. An account without a user name is
// never named by its address or name: the words that say so stand there. What is no account's text stays.
func TestShownNamesAPersonByTheUsername(t *testing.T) {
	p := peopleData().People()
	for _, c := range []struct{ stored, want string }{
		{"owner@example.com", "Juniper"},
		{"OWNER@Example.com", "Juniper"},
		{"friend@example.com (unverified)", "PineTop"},
		{"friend@example.com", "PineTop"},
		{"Juniper", "Juniper"},
		{"juniper", "Juniper"},
		{"Real Name", "Juniper"},
		{"sub-n", "Juniper"},
		// an account without a user name: never its address, never its name
		{"old@example.com", HiddenAddress},
		{"old@example.com (unverified)", HiddenAddress},
		{"Old Timer", HiddenName},
		{"Bare Name", HiddenName},
		{"sub-o", "sub-o"}, // a subject is what such an account was named by before, and still is
		{"sub-z", "sub-z"},
		// an address of no known account, and anything else with an "@" in a field that names a person
		{"gone@example.com", HiddenAddress},
		{"gone@example.com (unverified)", HiddenAddress},
		{"someone@host", HiddenAddress},
		{"a@b", HiddenAddress},
		// not an account's text
		{"", ""},
		{"fairy", "fairy"},
		{"root (cli)", "root (cli)"},
		{"unknown", "unknown"},
		// an API token: its name, and its owner named like an account
		{"ci (token, owner sub-n)", "ci (token, owner Juniper)"},
		{"ci (token, owner sub-o)", "ci (token, owner " + HiddenAddress + ")"},
		{"ci (token, owner sub-z)", "ci (token, owner sub-z)"},
		{"ci (token, owner system)", "ci (token, owner system)"},
		{"ci (token, owner gone@example.com)", "ci (token, owner " + HiddenAddress + ")"},
		// a text with an address in it ("panel by <who>" of the Users page, what an agent repeats)
		{"panel by owner@example.com", "panel by Juniper"},
		{"panel by old@example.com (unverified)", "panel by " + HiddenAddress},
		{"cli by root", "cli by root"},
		{"friend@example.com unverified", "PineTop"}, // an agent keeps no brackets
		{"old@example.com unverified", HiddenAddress},
	} {
		if got := p.Shown(c.stored); got != c.want {
			t.Errorf("Shown(%q) = %q, want %q", c.stored, got, c.want)
		}
	}
	// nothing that Shown gives holds an address or a name of an account
	for _, u := range peopleData().Users {
		for _, stored := range []string{u.Email, u.Email + UnverifiedMark, u.Name, u.Sub, "panel by " + u.Email, "x (token, owner " + u.Sub + ")"} {
			got := p.Shown(stored)
			for _, v := range peopleData().Users {
				for _, private := range []string{v.Email, v.Name} {
					if private != "" && strings.Contains(strings.ToLower(got), strings.ToLower(private)) {
						t.Errorf("Shown(%q) = %q holds %q", stored, got, private)
					}
				}
			}
		}
	}
}

// Only one account may match. An address or a name that two known accounts have says nothing about which of
// them it was: the text is hidden, not given to one of them.
func TestShownOnlyAUniqueMatchCounts(t *testing.T) {
	d := peopleData()
	twin := d.Users["sub-s"]
	twin.Sub, twin.Username, twin.Email, twin.Name = "sub-t", "Twin", "owner@example.com", "Other Person"
	d.Users["sub-t"] = twin
	p := d.People()
	for _, c := range []struct{ stored, want string }{
		{"owner@example.com", HiddenAddress}, // sub-n and sub-t
		{"owner@example.com (unverified)", HiddenAddress},
		{"Other Person", HiddenName}, // sub-s and sub-t
		{"friend@example.com", "PineTop"},
		{"Real Name", "Juniper"},
		{"sub-t", "Twin"}, // by the subject there is nothing to mistake
		{"sub-n", "Juniper"},
		{"panel by owner@example.com", "panel by " + HiddenAddress},
	} {
		if got := p.Shown(c.stored); got != c.want {
			t.Errorf("Shown(%q) = %q, want %q", c.stored, got, c.want)
		}
	}
	if got := p.ShownIn("started by owner@example.com: 2 machines"); got != "started by "+HiddenAddress+": 2 machines" {
		t.Errorf("a sentence with an address two accounts have: %q", got)
	}
}

// A user name is what the account says. Three are no name for anybody: one that is the account's own address,
// one that is written like an address and is another account's, and one that is another account's id. Such an
// account is named by the words for what it has instead. Everything else is shown, and what another account
// has as its name or its address takes it away from nobody: only user names compete with user names
// (TestTwoAccountsWithOneUsername).
func TestAUsernameMustBeTheAccountsOwn(t *testing.T) {
	for _, c := range []struct {
		name, username string
		wantOf, note   string // how the account sub-x (address x@example.com) is named, and why not by its user name
		stored, want   string // and what a stored text is shown as
	}{
		// the admin's old records stay the admin's: sub-x has an address of its own, and its own records have that
		{"another account's address", "owner@example.com", HiddenAddress, UsernameIsAddress, "owner@example.com", "Juniper"},
		{"another account's address in other letters", "Owner@Example.COM", HiddenAddress, UsernameIsAddress, "owner@example.com", "Juniper"},
		{"another account's subject", "sub-n", HiddenAddress, UsernameIsAccount, "sub-n", "Juniper"},
		{"its own address", "x@example.com", HiddenAddress, UsernameIsAddress, "x@example.com", HiddenAddress},
		{"its own address in other letters", "X@Example.COM", HiddenAddress, UsernameIsAddress, "x@example.com", HiddenAddress},
		{"a user name of its own", "Xavier", "Xavier", "", "x@example.com", "Xavier"},
		{"its own name", "Ex Person", "Ex Person", "", "Ex Person", "Ex Person"}, // a handle in both fields is a user name
		{"a user name with an @ of its own", "x@handle", "x@handle", "", "x@example.com", "x@handle"},
		// another account's NAME is a user name like any other: it is what sub-x is called, and a record that
		// keeps it is sub-x's (no record was ever written with the name of an account that has an address)
		{"another account's name", "Real Name", "Real Name", "", "Real Name", "Real Name"},
		{"the name of an account without an address", "Bare Name", "Bare Name", "", "Bare Name", HiddenName}, // (older versions named that one by it: whose record it is cannot be told)
	} {
		d := peopleData()
		tr := true
		d.Users["sub-x"] = User{Sub: "sub-x", Username: c.username, Email: "x@example.com", EmailVerified: &tr, Name: "Ex Person"}
		p := d.People()
		if got, ok := p.Of("sub-x"); !ok || got != c.wantOf || p.UsernameNote("sub-x") != c.note {
			t.Errorf("%s: the account is named %q (%q), want %q (%q)", c.name, got, p.UsernameNote("sub-x"), c.wantOf, c.note)
		}
		if got := p.Shown(c.stored); got != c.want {
			t.Errorf("%s: Shown(%q) = %q, want %q", c.name, c.stored, got, c.want)
		}
		// nobody else is named differently for it, whatever sub-x calls itself
		for sub, want := range map[string]string{"sub-n": "Juniper", "sub-s": "PineTop", "sub-o": HiddenAddress, "sub-b": HiddenName} {
			if got, _ := p.Of(sub); got != want {
				t.Errorf("%s: %s is named %q, want %q", c.name, sub, got, want)
			}
		}
	}
	// a user name with an "@" in it is no address: a text it was put into stays a text
	d := peopleData()
	d.Users["sub-x"] = User{Sub: "sub-x", Username: "x@handle", Email: "x@example.com"}
	if got := d.People().Shown("panel by x@example.com"); got != "panel by x@handle" {
		t.Errorf("a text with a user name that holds an @: %q", got)
	}
	if got := d.People().Shown("panel by x@example.com and y@host"); got != HiddenAddress {
		t.Errorf("... and with something else that holds one: %q", got)
	}
	// An account WITHOUT an address of its own that calls itself by another account's address is written into
	// records by that text (it has nothing else): an old record with the address can be either's, and is hidden.
	d = peopleData()
	d.Users["sub-q"] = User{Sub: "sub-q", Username: "owner@example.com"}
	if got := d.People().Shown("owner@example.com"); got != HiddenAddress {
		t.Errorf("an address one account has and another without an address calls itself by: %q", got)
	}
}

// What another account has as its NAME or its ADDRESS takes no user name away. An account that only waits for
// access gave itself the display name "viewer-all", or an address that is the admin's user name: the viewer
// and the admin are named as before, and so are their records. (Any account could blank another one's name
// this way, in every sentence of the panel.)
func TestOnlyUsernamesCompeteWithUsernames(t *testing.T) {
	d := peopleData()
	tr := true
	d.Users["sub-v"] = User{Sub: "sub-v", Username: "viewer-all", Email: "wide@example.com", EmailVerified: &tr, Name: "Wide Viewer", Role: RoleViewer}
	d.Users["sub-w1"] = User{Sub: "sub-w1", Username: "walt", Email: "walt@example.org", Name: "viewer-all", Role: RolePending}   // a display name
	d.Users["sub-w2"] = User{Sub: "sub-w2", Username: "wanda", Email: "Juniper", Name: "Waiting Wanda", Role: RolePending}        // an "address" without an @
	d.Users["sub-w3"] = User{Sub: "sub-w3", Username: "wilma", Email: "w3@example.org", Name: "PINETOP", Role: RolePending}       // in other letters
	d.Users["sub-w4"] = User{Sub: "sub-w4", Username: "wendy", Email: "w4@example.org", Name: "Juni\u200bper", Role: RolePending} // with a zero width space
	p := d.People()
	for sub, want := range map[string]string{"sub-v": "viewer-all", "sub-n": "Juniper", "sub-s": "PineTop", "sub-w1": "walt", "sub-w2": "wanda", "sub-w3": "wilma", "sub-w4": "wendy"} {
		if got, _ := p.Of(sub); got != want || p.UsernameNote(sub) != "" || p.SameUsername(sub) != nil {
			t.Errorf("%s is named %q (note %q, same as %v), want %q", sub, got, p.UsernameNote(sub), p.SameUsername(sub), want)
		}
	}
	// a record that keeps the user name is still that account's
	for stored, want := range map[string]string{"viewer-all": "viewer-all", "PineTop": "PineTop", "pinetop": "PineTop"} {
		if got := p.Shown(stored); got != want {
			t.Errorf("Shown(%q) = %q, want %q", stored, got, want)
		}
	}
	// (the one text that is an account's ADDRESS is that account's too: which of the two a record means cannot be told)
	if got := p.Shown("Juniper"); got != HiddenAddress {
		t.Errorf("a text that is one account's user name and another's address: %q", got)
	}
}

func at(day int) *time.Time {
	t := time.Date(2026, 11, day, 12, 0, 0, 0, time.UTC)
	return &t
}

// Two accounts with one user name: the same text in its folded form (other letters, a zero width space). One
// of them is shown by the plain name: an account with a role before one that only waits, whatever the times;
// among equals the one that has had the user name longest (the stored time; the account's creation for a user
// name from before that time was kept); then by subject. Every other one is shown by the name with a short
// form of its id, never by the plain name and never by nothing.
func TestTwoAccountsWithOneUsername(t *testing.T) {
	tr := true
	admin := User{Sub: "3f6c1d00-admin", Username: "Juniper", Email: "owner@example.com", EmailVerified: &tr, Name: "Real Name", Role: RoleAdmin, CreatedAt: *at(1)}
	for _, c := range []struct {
		name  string
		a, b  User   // b's subject is 9a1b2c00-other
		plain string // the subject that is shown by the plain name
	}{
		{"an account that waits, with the older user name", withSince(admin, at(10)),
			User{Username: "juniper", Email: "w@example.org", Role: RolePending, CreatedAt: *at(1), UsernameSince: at(2)}, "3f6c1d00-admin"},
		{"an account that waits, created earlier, both from before the time was kept", admin,
			User{Username: "JUNIPER", Email: "w@example.org", Role: RolePending, CreatedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}, "3f6c1d00-admin"},
		{"a viewer that took the name later", withSince(admin, at(3)),
			User{Username: "Juniper", Email: "v@example.org", Role: RoleViewer, CreatedAt: *at(1), UsernameSince: at(4)}, "3f6c1d00-admin"},
		{"a viewer that had the name first", withSince(admin, at(5)),
			User{Username: "Juniper", Email: "v@example.org", Role: RoleViewer, CreatedAt: *at(9), UsernameSince: at(4)}, "9a1b2c00-other"},
		{"a viewer without sites counts as an account with a role", withSince(admin, at(5)),
			User{Username: "Juniper", Email: "v@example.org", Role: RoleViewer, Sites: []string{}, CreatedAt: *at(1), UsernameSince: at(4)}, "9a1b2c00-other"},
		{"a user name from before the time was kept counts from the account's creation", withSince(admin, at(5)),
			User{Username: "Juniper", Email: "v@example.org", Role: RoleAdmin, CreatedAt: *at(4)}, "9a1b2c00-other"},
		{"... and loses against a stored time before it", withSince(admin, at(3)),
			User{Username: "Juniper", Email: "v@example.org", Role: RoleAdmin, CreatedAt: *at(4)}, "3f6c1d00-admin"},
		{"the same time: by subject", withSince(admin, at(5)),
			User{Username: "Juniper", Email: "v@example.org", Role: RoleAdmin, CreatedAt: *at(1), UsernameSince: at(5)}, "3f6c1d00-admin"},
		{"with a zero width space", withSince(admin, at(1)),
			User{Username: "Juni\u200bper", Email: "v@example.org", Role: RoleAdmin, CreatedAt: *at(1), UsernameSince: at(2)}, "3f6c1d00-admin"},
		{"with a soft hyphen and spaces", withSince(admin, at(1)),
			User{Username: " Juni\u00adper ", Email: "v@example.org", Role: RoleAdmin, CreatedAt: *at(1), UsernameSince: at(2)}, "3f6c1d00-admin"},
	} {
		c.b.Sub = "9a1b2c00-other"
		d := Data{Users: map[string]User{c.a.Sub: c.a, c.b.Sub: c.b, "sub-o": {Sub: "sub-o", Email: "old@example.com", Name: "Old Timer"}}}
		p := d.People()
		other := c.a.Sub
		if c.plain == other {
			other = c.b.Sub
		}
		plain, _ := p.Of(c.plain)
		tagged, _ := p.Of(other)
		if plain != "Juniper" && fold(plain) != "JUNIPER" {
			t.Errorf("%s: %s is shown by %q, want the plain user name", c.name, c.plain, plain)
		}
		if want := CleanUsername(d.Users[other].Username) + " · id " + other[:6]; tagged != want {
			t.Errorf("%s: %s is shown by %q, want %q", c.name, other, tagged, want)
		}
		if strings.ContainsAny(plain+tagged, "\u200b\u00ad") || plain != strings.TrimSpace(plain) {
			t.Errorf("%s: a shown form has a character without width, or a space at an end: %q %q", c.name, plain, tagged)
		}
		for _, sub := range []string{c.a.Sub, c.b.Sub} {
			if p.UsernameNote(sub) != UsernameShared || len(p.SameUsername(sub)) != 1 || p.SameUsername(sub)[0] == sub {
				t.Errorf("%s: %s: note %q, the same as %v", c.name, sub, p.UsernameNote(sub), p.SameUsername(sub))
			}
			if p.Username(sub) == "" || p.Record(sub, "x@example.org") != p.Username(sub) {
				t.Errorf("%s: %s: Username %q, Record %q", c.name, sub, p.Username(sub), p.Record(sub, "x@example.org"))
			}
		}
		// what a record keeps is shown as that account again: the plain name is the one account's, the name with
		// the id the other's, and neither text is hidden
		if got := p.Shown(plain); got != plain {
			t.Errorf("%s: Shown(%q) = %q", c.name, plain, got)
		}
		if got := p.Shown("juniper"); got != plain {
			t.Errorf("%s: Shown(juniper) = %q, want %q", c.name, got, plain)
		}
		if got := p.Shown(tagged); got != tagged {
			t.Errorf("%s: Shown(%q) = %q", c.name, tagged, got)
		}
		// an old record by address is each account's own
		if got := p.Shown(d.Users[other].Email); got != tagged {
			t.Errorf("%s: the old record of %s is shown as %q, want %q", c.name, other, got, tagged)
		}
		if got := p.ShownIn("switched off by " + d.Users[other].Email + " (unverified)"); got != "switched off by "+tagged {
			t.Errorf("%s: an old sentence about %s: %q", c.name, other, got)
		}
		if got := p.Named("juni"); len(got) != 2 {
			t.Errorf("%s: a search for the name finds %v", c.name, got)
		}
	}
}

func withSince(u User, t *time.Time) User {
	u.UsernameSince = t
	return u
}

// The short form of the id is as long as it must be: two accounts that share a user name and the beginning of
// their ids are shown with the whole id. A record that kept a name with an id is still that account's when
// the account is shown by the plain name again (the other one was removed). And no account can be shown by a
// text another account is shown by: a user name written like another one with its id gets its own id too.
func TestUsernameWithIDAtTheEdges(t *testing.T) {
	d := Data{Users: map[string]User{
		"abcdef-1": {Sub: "abcdef-1", Username: "Juniper", Role: RoleAdmin, CreatedAt: *at(1)},
		"abcdef-2": {Sub: "abcdef-2", Username: "juniper", Role: RoleViewer, CreatedAt: *at(2)},
		"abcdef-3": {Sub: "abcdef-3", Username: "JUNIPER", Role: RolePending, CreatedAt: *at(3)},
		"zz":       {Sub: "zz", Username: "Juniper", Role: RolePending, CreatedAt: *at(4)},
	}}
	p := d.People()
	for sub, want := range map[string]string{"abcdef-1": "Juniper", "abcdef-2": "juniper · id abcdef-2", "abcdef-3": "JUNIPER · id abcdef-3", "zz": "Juniper · id zz"} {
		if got, _ := p.Of(sub); got != want {
			t.Errorf("%s is shown by %q, want %q", sub, got, want)
		}
	}
	if got := p.SameUsername("abcdef-2"); !reflect.DeepEqual(got, []string{"abcdef-1", "abcdef-3", "zz"}) {
		t.Errorf("the accounts with the same user name as abcdef-2: %v", got)
	}
	// the first one is removed: the viewer is the plain "juniper" now, and its record from before is still its
	delete(d.Users, "abcdef-1")
	p = d.People()
	if got, _ := p.Of("abcdef-2"); got != "juniper" {
		t.Fatalf("after the first account was removed: %q", got)
	}
	for stored, want := range map[string]string{"juniper · id abcdef-2": "juniper", "Juniper · id abcdef-2": "juniper", "JUNIPER · id abcdef-3": "JUNIPER · id abcdef-3", "Juniper · id abcdef": "Juniper · id abcdef", // (two ids begin so: the whole id, and a short one names neither)
		"Juniper · id nobody": "Juniper · id nobody", " · id zz": " · id zz"} {
		if got := p.Shown(stored); got != want {
			t.Errorf("Shown(%q) = %q, want %q", stored, got, want)
		}
	}
	// a user name that is written like another account's shown form
	d = Data{Users: map[string]User{
		"3f6c1d-a": {Sub: "3f6c1d-a", Username: "Juniper", Role: RoleAdmin, CreatedAt: *at(1)},
		"3f6c1d-b": {Sub: "3f6c1d-b", Username: "Juniper", Role: RolePending, CreatedAt: *at(2)},
		"9a1b2c-c": {Sub: "9a1b2c-c", Username: "juniper · id 3f6c1d-b", Role: RoleAdmin, CreatedAt: *at(1)},
	}}
	p = d.People()
	seen := map[string]string{}
	for sub := range d.Users {
		n, _ := p.Of(sub)
		if other, twice := seen[fold(n)]; twice || n == "" {
			t.Errorf("%s and %s are both shown by %q", sub, other, n)
		}
		seen[fold(n)] = sub
	}
	if got, _ := p.Of("3f6c1d-a"); got != "Juniper" {
		t.Errorf("the admin: %q", got)
	}
	if got, _ := p.Of("9a1b2c-c"); got != "juniper · id 3f6c1d-b · id 9a1b2c" {
		t.Errorf("the account that calls itself like another one's shown form: %q", got)
	}
}

// A user name is kept without the characters that have no width (the zero width space and joiners, the word
// joiner, the soft hyphen: Unicode category Cf), so that no name looks like another one and is not; and
// cleaning a clean name changes nothing.
func TestUsernameHasNoCharactersWithoutWidth(t *testing.T) {
	for in, want := range map[string]string{
		"Juniper\u200b":             "Juniper",
		"Ju\u200cni\u200dper\u2060": "Juniper",
		"Juni\u00adper":             "Juniper",
		"\ufeffJuniper":             "Juniper",
		" \u200b Juniper \u200b ":   "Juniper",
		"\u200b\u200d":              "",
		"Jüni per":                  "Jüni per",
		"Юнипер":                    "Юнипер",
		"bad\xffbyte":               "badbyte",
	} {
		got := CleanUsername(in)
		if got != want {
			t.Errorf("CleanUsername(%q) = %q, want %q", in, got, want)
		}
		if again := CleanUsername(got); again != got {
			t.Errorf("cleaned twice: %q, then %q", got, again)
		}
	}
	// ... and a text that has one of them is compared without it: a record that kept such a name is that account's
	d := peopleData()
	p := d.People()
	for _, stored := range []string{"Juni\u200bper", "\u2060juniper\u00ad", "OWNER\u200b@example.com"} {
		if got := p.Shown(stored); got != "Juniper" {
			t.Errorf("Shown(%q) = %q", stored, got)
		}
	}
	long := CleanUsername(strings.Repeat("ab\u200b ", 40))
	if len(long) > MaxUsername || CleanUsername(long) != long || strings.Contains(long, "\u200b") || long != strings.TrimSpace(long) {
		t.Errorf("a long name: %q", long)
	}
	// the store keeps it that way, from a sign-in and from a session's cookie
	s := NewStore(t.TempDir())
	u, _, err := s.SeenIdentity(Identity{Sub: "sub-z", Email: "zw@example.net", Username: "Juniper\u200b"}, RolePending, "first sign-in")
	if err != nil || u.Username != "Juniper" {
		t.Fatalf("a sign-in: %q %v", u.Username, err)
	}
	if _, _, err := s.SeenIdentity(Identity{Sub: "sub-y", Email: "y@example.net"}, RolePending, "first sign-in"); err != nil {
		t.Fatal(err)
	}
	if done, err := s.FillUsername("sub-y", "Pine\u200dTop\u00ad"); !done || err != nil || s.Load().Users["sub-y"].Username != "PineTop" {
		t.Fatalf("a session's cookie: %v %v %q", done, err, s.Load().Users["sub-y"].Username)
	}
}

// Since when an account has its user name is kept: set with the first user name and with another one, not
// changed by a sign-in that sends the same name (also in other letters) or none.
func TestStoreKeepsSinceWhenTheUsername(t *testing.T) {
	s := NewStore(t.TempDir())
	id := Identity{Sub: "sub-n", Email: "owner@example.com", EmailVerified: true}
	u, _, err := s.SeenIdentity(id, RoleAdmin, "first sign-in")
	if err != nil || u.UsernameSince != nil {
		t.Fatalf("an account without a user name: %+v %v", u, err)
	}
	id.Username = "Juniper"
	u, _, _ = s.SeenIdentity(id, RoleAdmin, "")
	first := u.UsernameSince
	if first == nil || first.Before(u.CreatedAt) || time.Since(*first) > time.Minute {
		t.Fatalf("the first user name: since %v", first)
	}
	time.Sleep(2 * time.Millisecond)
	for _, name := range []string{"Juniper", "JUNIPER", "juni\u200bper", ""} {
		id.Username = name
		if u, _, _ = s.SeenIdentity(id, RoleAdmin, ""); u.UsernameSince == nil || !u.UsernameSince.Equal(*first) {
			t.Fatalf("a sign-in with the user name %q moved the time: %v, was %v", name, u.UsernameSince, first)
		}
	}
	if u, _, _ = s.Seen("sub-n", "", "", RolePending, ""); u.UsernameSince == nil || !u.UsernameSince.Equal(*first) {
		t.Fatalf("a request moved the time: %v", u.UsernameSince)
	}
	id.Username = "Aspen"
	if u, _, _ = s.SeenIdentity(id, RoleAdmin, ""); u.UsernameSince == nil || !u.UsernameSince.After(*first) {
		t.Fatalf("another user name: since %v, the one before since %v", u.UsernameSince, first)
	}
	raw, _ := os.ReadFile(s.Path)
	if !strings.Contains(string(raw), `"username_since": "`) {
		t.Fatalf("the file has no username_since:\n%s", raw)
	}
	// a session's cookie that fills a user name in: from now
	if _, _, err := s.SeenIdentity(Identity{Sub: "sub-o", Email: "old@example.com"}, RolePending, "first sign-in"); err != nil {
		t.Fatal(err)
	}
	if done, _ := s.FillUsername("sub-o", "Oldie"); !done || s.Load().Users["sub-o"].UsernameSince == nil {
		t.Fatalf("FillUsername: %+v", s.Load().Users["sub-o"])
	}
}

// In a sentence the address of a KNOWN account is replaced wherever it stands as a text of its own, whatever it
// looks like: an address without a dot (a provider inside a network), a top-level name in its ASCII form, in
// any case of its letters. (It was found by its shape only, so such an address in a sentence an older version
// stored went on to viewers, read tokens and the notification service.) What merely contains it is another
// address.
func TestShownInAKnownAddressOfAnyShape(t *testing.T) {
	d := peopleData()
	tr := true
	d.Users["r4-corp"] = User{Sub: "r4-corp", Username: "corpuser", Email: "ops@corp", EmailVerified: &tr, Name: "Corp Opsman", Role: RoleAdmin}
	d.Users["r4-puny"] = User{Sub: "r4-puny", Username: "punyuser", Email: "idn@beispiel.xn--p1ai", EmailVerified: &tr, Name: "Puny Coder", Role: RoleViewer}
	d.Users["r4-none"] = User{Sub: "r4-none", Email: "svc@localhost", Name: "No Username"}
	d.Users["r4-bang"] = User{Sub: "r4-bang", Username: "banguser", Email: "a!b#c@corp", Name: "Bang"}
	d.Users["r4-pam"] = User{Sub: "r4-pam", Username: "pamuser", Email: "admin@pam", Name: "Pam"}
	p := d.People()
	for _, c := range []struct{ text, want string }{
		{"Lamp (Desk bar) switched off by ops@corp", "Lamp (Desk bar) switched off by corpuser"},
		{"switched off by OPS@Corp.", "switched off by corpuser."},
		{"switched off by ops@corp (unverified), then on", "switched off by corpuser, then on"},
		{"Fan switched on by idn@beispiel.xn--p1ai", "Fan switched on by punyuser"},
		{"started by ops@corp: 1 machine", "started by corpuser: 1 machine"},
		{"<ops@corp> and \"ops@corp\", see mailto:ops@corp", "<corpuser> and \"corpuser\", see mailto:corpuser"},
		{"by svc@localhost-", "by " + HiddenAddress + "-"},
		{"by a!b#c@corp", "by banguser"},
		{`{"email":"ops@corp","name":"x"}`, `{"email":"corpuser","name":"x"}`},
		{"ops@corp idn@beispiel.xn--p1ai owner@example.com", "corpuser punyuser Juniper"},
		// not the account's address: a longer one that only holds it
		{"devops@corp", "devops@corp"},
		{"x.ops@corp", "x.ops@corp"},
		{"ops@corp.example", HiddenAddress},
		{"ops@corp-two", "ops@corp-two"},
		{"ops@corporate", "ops@corporate"},
		{"ops@corp@corp", "ops@corp@corp"},
		{"ops@corp_x", "ops@corp_x"},
		// an error text with a Proxmox user stays, unless an account has that very address
		{"authentication failure for root@pam", "authentication failure for root@pam"},
		{"authentication failure for admin@pam", "authentication failure for pamuser"},
		// the user name that is put in is not read again
		{"by ops@corp and stranger@example.org", "by corpuser and " + HiddenAddress},
	} {
		if got := p.ShownIn(c.text); got != c.want {
			t.Errorf("ShownIn(%q)\n  got  %q\n  want %q", c.text, got, c.want)
		}
	}
	// a user name that is itself written like an address is put in once and stays
	d.Users["r4-at"] = User{Sub: "r4-at", Username: "me@handle.example", Email: "at@corp", Name: "At"}
	if got := d.People().ShownIn("by at@corp, at@corp"); got != "by me@handle.example, me@handle.example" {
		t.Errorf("a user name that looks like an address: %q", got)
	}
}

// What an agent answers as "requested by" is whatever whoever runs the agent makes it say. It is never looked
// up among the addresses and names of the accounts (that told them which user name belongs to an address or to
// a name): a text that is what one account is shown by, as an agent repeats it, is shown; anything else with
// an "@" is HiddenAddress, anything else HiddenName.
func TestFromOutsideIsNeverLookedUpAmongAddressesAndNames(t *testing.T) {
	d := peopleData()
	tr := true
	d.Users["9a1b2c-w"] = User{Sub: "9a1b2c-w", Username: "juniper", Email: "w@example.org", Role: RolePending}
	d.Users["sub-u"] = User{Sub: "sub-u", Username: "Jüni Pér", Email: "u@example.org", EmailVerified: &tr, Role: RoleViewer}
	d.Users["sub-k"] = User{Sub: "sub-k", Username: "Юнипер", Email: "k@example.org", Role: RoleViewer}
	p := d.People()
	for _, c := range []struct{ said, want string }{
		// what the panel told an agent, as the agent repeats it
		{"Juniper", "Juniper"},
		{"juniper", "Juniper"},
		{"PineTop", "PineTop"},
		{"juniper  id 9a1b2c", "juniper · id 9a1b2c"}, // an agent drops the "·"
		{"juniper · id 9a1b2c", "juniper · id 9a1b2c"},
		{"Jni Pr", "Jüni Pér"}, // ... and every letter that is not A to Z
		{"address hidden", HiddenAddress},
		{"(address hidden)", HiddenAddress},
		{"name hidden", HiddenName},
		{"unknown", "unknown"}, // the agent's own word for nobody
		{"", ""},
		// addresses and names of accounts: never the user name
		{"owner@example.com", HiddenAddress},
		{"OWNER@EXAMPLE.COM", HiddenAddress},
		{"friend@example.com unverified", HiddenAddress},
		{"old@example.com", HiddenAddress},
		{"nobody@example.org", HiddenAddress},
		{"root@pam", HiddenAddress},
		{"Real Name", HiddenName},
		{"real name", HiddenName},
		{"Old Timer", HiddenName},
		{"Bare Name", HiddenName},
		{"sub-n", HiddenName},
		// ... and whatever else it says
		{"Some Stranger", HiddenName},
		{"root", HiddenName},
		{"ci token owner Juniper", HiddenName},
		{"Juniper and more", HiddenName},
	} {
		if got := p.FromOutside(c.said); got != c.want {
			t.Errorf("FromOutside(%q) = %q, want %q", c.said, got, c.want)
		}
	}
	// two accounts an agent's answer cannot tell apart: neither
	d.Users["sub-j"] = User{Sub: "sub-j", Username: "Jni Pr", Email: "j@example.org", Role: RoleViewer}
	if got := d.People().FromOutside("Jni Pr"); got != HiddenName {
		t.Errorf("a text that two shown names turn into at an agent: %q", got)
	}
	// an account that calls itself "unknown" is not what the agent's word for nobody means
	d.Users["sub-q"] = User{Sub: "sub-q", Username: "unknown", Email: "q@example.org"}
	if got := d.People().FromOutside("unknown"); got != "unknown" {
		t.Errorf("unknown: %q", got)
	}
	// nothing FromOutside gives holds an address or a name
	for _, u := range d.Users {
		for _, said := range []string{u.Email, u.Name, u.Sub, strings.ToUpper(u.Email), u.Email + " unverified"} {
			got := d.People().FromOutside(said)
			for _, v := range d.Users {
				for _, private := range []string{v.Email, v.Name} {
					if private != "" && strings.Contains(strings.ToLower(got), strings.ToLower(private)) && fold(private) != fold(v.Username) {
						t.Errorf("FromOutside(%q) = %q holds %q", said, got, private)
					}
				}
			}
		}
	}
}

// Of names an account by its subject: the user name, or the words for what it has instead. Record is what a
// record keeps: the user name, or the text that was kept before while there is none.
func TestOfAndRecord(t *testing.T) {
	p := peopleData().People()
	for sub, want := range map[string]string{"sub-n": "Juniper", "sub-s": "PineTop", "sub-o": HiddenAddress, "sub-b": HiddenName, "sub-z": "sub-z"} {
		if got, ok := p.Of(sub); !ok || got != want {
			t.Errorf("Of(%s) = %q, %v; want %q", sub, got, ok, want)
		}
	}
	if got, ok := p.Of("sub-unknown"); ok || got != "" {
		t.Errorf("an account that is not known: %q, %v", got, ok)
	}
	if got := p.Record("sub-n", "owner@example.com"); got != "Juniper" {
		t.Errorf("a record of an account with a user name keeps %q", got)
	}
	if got := p.Record("sub-o", "old@example.com"); got != "old@example.com" {
		t.Errorf("a record of an account without one keeps %q", got)
	}
	if got := p.Record("sub-unknown", "x"); got != "x" {
		t.Errorf("a record of an unknown account keeps %q", got)
	}
	if got := p.Username("sub-n") + "|" + p.Username("sub-o") + "|" + p.Username("nobody"); got != "Juniper||" {
		t.Errorf("Username: %q", got)
	}
}

// A sentence an older version made keeps its words; only the person in it changes. What merely looks a little
// like an address (a Proxmox user, a user at an IP address) is not one.
func TestShownInASentence(t *testing.T) {
	p := peopleData().People()
	for _, c := range []struct{ text, want string }{
		{"started by owner@example.com: 3 machines", "started by Juniper: 3 machines"},
		{"removed from the panel by friend@example.com (unverified) (the agent keeps running)", "removed from the panel by PineTop (the agent keeps running)"},
		{"run started by owner@example.com was cancelled by friend@example.com", "run started by Juniper was cancelled by PineTop"},
		{"switched off by old@example.com.", "switched off by " + HiddenAddress + "."},
		{"elevated helper started by gone@example.org", "elevated helper started by " + HiddenAddress},
		{"its machine id was accepted by Bare Name", "its machine id was accepted by " + HiddenName},
		{"Bare Names are not Bare Name's", "Bare Names are not " + HiddenName + "'s"}, // a word of its own only
		{"Real Name did it", "Real Name did it"},                                      // an account with an address was never named by its name in a sentence
		{"authentication failure for root@pam", "authentication failure for root@pam"},
		{"ssh fairy@192.0.2.7 failed", "ssh fairy@192.0.2.7 failed"},
		{"token fairy@pve!panel refused", "token fairy@pve!panel refused"},
		{"agent now runs 14 (was 13)", "agent now runs 14 (was 13)"},
		{"@", "@"}, {"a@", "a@"}, {"@b.example", "@b.example"}, {"", ""},
		{"owner@example.com", "Juniper"},
		{"(owner@example.com)", "(Juniper)"},
		{"by <owner@example.com>, then OWNER@EXAMPLE.COM", "by <Juniper>, then Juniper"},
		{"two@example.com three@example.com", HiddenAddress + " " + HiddenAddress},
	} {
		if got := p.ShownIn(c.text); got != c.want {
			t.Errorf("ShownIn(%q)\n  got  %q\n  want %q", c.text, got, c.want)
		}
	}
}

// What the Users page and the Audit log draw as dots, and what a search by the shown name finds.
func TestPrivateAndNamed(t *testing.T) {
	p := peopleData().People()
	want := []string{"friend@example.com", "owner@example.com", "old@example.com", "Other Person", "Bare Name", "Old Timer", "Real Name"}
	if got := p.Private(); !reflect.DeepEqual(got, want) {
		t.Errorf("Private:\n got  %q\n want %q", got, want)
	}
	for q, want := range map[string][]string{"juni": {"sub-n"}, "P": {"sub-n", "sub-s"}, "pinetop": {"sub-s"}, "owner@": nil, "real": nil, "": nil} {
		if got := p.Named(q); !reflect.DeepEqual(got, want) {
			t.Errorf("Named(%q) = %q, want %q", q, got, want)
		}
	}
}

// The user name is kept at a sign-in, cleaned and cut. A sign-in that sends none keeps the stored one (as with
// the name). A session that began before the panel kept user names gives its account the user name of its
// cookie only while the account has none.
func TestStoreKeepsTheUsername(t *testing.T) {
	s := NewStore(t.TempDir())
	id := Identity{Sub: "sub-n", Email: "owner@example.com", EmailVerified: true, Name: "Real Name", Username: "Juniper"}
	u, created, err := s.SeenIdentity(id, RoleAdmin, "first sign-in")
	if err != nil || !created || u.Username != "Juniper" {
		t.Fatalf("first sign-in: %+v %v %v", u, created, err)
	}
	if got := s.Load().Users["sub-n"].Username; got != "Juniper" {
		t.Fatalf("stored: %q", got)
	}
	id.Username = ""
	if u, _, _ = s.SeenIdentity(id, RoleAdmin, ""); u.Username != "Juniper" {
		t.Fatalf("a sign-in without a user name took it away: %q", u.Username)
	}
	if u, _, _ = s.Seen("sub-n", "", "", RolePending, ""); u.Username != "Juniper" {
		t.Fatalf("a request took it away: %q", u.Username)
	}
	id.Username = "Juni\x1b[31mper‮\n" + strings.Repeat("x", 200)
	u, _, _ = s.SeenIdentity(id, RoleAdmin, "")
	if want := "Juniper " + strings.Repeat("x", MaxUsername-len("Juniper ")); u.Username != want {
		t.Fatalf("a user name with an escape sequence, an override, a line break and 200 letters is kept as %q, want %q", u.Username, want)
	}
	id.Username = "\x00‮ \n"
	if u, _, _ = s.SeenIdentity(id, RoleAdmin, ""); !strings.HasPrefix(u.Username, "Juniper x") {
		t.Fatalf("a user name of control characters alone replaced the stored one: %q", u.Username)
	}
	// FillUsername: only for an account that has none, and only one that is known
	if done, err := s.FillUsername("sub-n", "Other"); done || err != nil || !strings.HasPrefix(s.Load().Users["sub-n"].Username, "Juniper x") {
		t.Fatalf("a cookie replaced a stored user name: %v %v", done, err)
	}
	if _, _, err := s.SeenIdentity(Identity{Sub: "sub-o", Email: "old@example.com", EmailVerified: true, Name: "Old Timer"}, RolePending, "first sign-in"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ sub, name string }{{"sub-o", ""}, {"sub-o", "\x07"}, {"", "x"}, {"sub-unknown", "x"}} {
		if done, err := s.FillUsername(c.sub, c.name); done || err != nil {
			t.Fatalf("FillUsername(%q, %q) wrote: %v %v", c.sub, c.name, done, err)
		}
	}
	if _, ok := s.Load().Users["sub-unknown"]; ok {
		t.Fatal("FillUsername made an account")
	}
	before := s.Load().Users["sub-o"]
	if done, err := s.FillUsername("sub-o", " Old\tie "); !done || err != nil {
		t.Fatalf("FillUsername for an account without one: %v %v", done, err)
	}
	after := s.Load().Users["sub-o"]
	if after.Username != "Old ie" {
		t.Fatalf("filled in as %q", after.Username)
	}
	if after.UsernameSince == nil {
		t.Fatal("FillUsername did not keep since when")
	}
	after.Username, after.UsernameSince = "", nil
	if !reflect.DeepEqual(before, after) { // nothing else of the account moves: not the role, not the address, not last seen
		t.Fatalf("FillUsername changed more than the user name:\n%+v\n%+v", before, after)
	}
	if done, _ := s.FillUsername("sub-o", "Second"); done || s.Load().Users["sub-o"].Username != "Old ie" {
		t.Fatal("a second cookie replaced the user name of the first")
	}
}

// A users.json of a version without the field is read as before: nobody has a user name, and nobody is named
// by an address for it.
func TestOldUsersFileHasNoUsernames(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "users.json"), []byte(oldUsersJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	d := NewStore(dir).Load()
	if len(d.Users) != 2 || d.Users["sub-n"].Username != "" || d.Users["sub-n"].Role != RoleAdmin {
		t.Fatalf("%+v", d.Users)
	}
	p := d.People()
	if got := p.Shown("alice@example.com"); got != HiddenAddress {
		t.Errorf("the admin of an old file is shown as %q", got)
	}
	if got := p.Shown("panel by alice@example.com"); got != "panel by "+HiddenAddress {
		t.Errorf("via: %q", got)
	}
}

// build16User and build16Data are the types of users.json as build 16 has them (internal/access/store.go at
// that release): no user name.
type build16User struct {
	Sub           string     `json:"sub"`
	Email         string     `json:"email"`
	EmailVerified *bool      `json:"email_verified"`
	Name          string     `json:"name"`
	Role          string     `json:"role"`
	Sites         []string   `json:"sites"`
	CreatedAt     time.Time  `json:"created_at"`
	LastSeen      *time.Time `json:"last_seen"`
	Via           string     `json:"via"`
}

type build16Data struct {
	Users   map[string]build16User `json:"users"`
	Sites   []string               `json:"sites"`
	Assign  map[string]string      `json:"assign"`
	Removed map[string]Removal     `json:"removed"`
}

// Going back to build 16: the users.json this version writes is read by that version's types with everything
// they know in place (roles, sites, addresses, the removed accounts), and the file that version writes back
// (without the user names) is read by this one again: the accounts are as they were, without a user name until
// their next sign-in.
func TestUsersFileIsReadByTheVersionBefore(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.CreateSite("Home"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []Identity{
		{Sub: "sub-n", Email: "owner@example.com", EmailVerified: true, Name: "Real Name", Username: "Juniper"},
		{Sub: "sub-s", Email: "friend@example.com", Name: "Other Person", Username: "PineTop"},
		{Sub: "sub-g", Email: "gone@example.com", EmailVerified: true, Name: "Gone", Username: "gone"},
	} {
		if _, _, err := s.SeenIdentity(id, RoleAdmin, "first sign-in"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.SetRole("sub-s", RoleViewer, []string{"Home"}, "panel by Juniper", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remove("sub-g", false, "panel by Juniper"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Assign("desk-pc", "Home"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"username": "Juniper"`) || !strings.Contains(string(raw), `"username_since": "`) {
		t.Fatalf("the file has no user name:\n%s", raw)
	}
	var old build16Data
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&old); err != nil { // as that version reads it: fields it does not know are passed over
		t.Fatal("build 16 cannot read the file: ", err)
	}
	now := s.Load()
	for sub, u := range now.Users {
		o, ok := old.Users[sub]
		if !ok || o.Email != u.Email || o.Name != u.Name || o.Role != u.Role || !reflect.DeepEqual(o.Sites, u.Sites) || o.Via != u.Via ||
			!reflect.DeepEqual(o.EmailVerified, u.EmailVerified) || !o.CreatedAt.Equal(u.CreatedAt) {
			t.Errorf("%s as build 16 reads it: %+v, now %+v", sub, o, u)
		}
	}
	if len(old.Users) != 2 || old.Users["sub-n"].Role != RoleAdmin || old.Users["sub-s"].Role != RoleViewer ||
		!reflect.DeepEqual(old.Sites, []string{"Home"}) || old.Assign["desk-pc"] != "Home" || old.Removed["sub-g"].Label != "gone@example.com" {
		t.Fatalf("build 16 reads: %+v", old)
	}
	// ... and what it writes back is read here
	back, _ := json.MarshalIndent(old, "", " ")
	if strings.Contains(string(back), "username") || strings.Contains(string(back), "Juniper\",\n") {
		t.Fatalf("the mirror of build 16 knows the field:\n%s", back)
	}
	if err := os.WriteFile(s.Path, back, 0o600); err != nil {
		t.Fatal(err)
	}
	again := NewStore(dir).Load()
	if len(again.Users) != 2 || again.Users["sub-n"].Role != RoleAdmin || again.Users["sub-n"].Username != "" ||
		again.Users["sub-s"].Role != RoleViewer || !reflect.DeepEqual(again.Users["sub-s"].Sites, []string{"Home"}) || again.Removed["sub-g"].By != "panel by Juniper" {
		t.Fatalf("after build 16 wrote it: %+v", again)
	}
	if _, _, err := NewStore(dir).SeenIdentity(Identity{Sub: "sub-n", Email: "owner@example.com", EmailVerified: true, Username: "Juniper"}, RolePending, ""); err != nil {
		t.Fatal(err)
	}
	if u := NewStore(dir).Load().Users["sub-n"]; u.Username != "Juniper" || u.Role != RoleAdmin {
		t.Fatalf("the next sign-in: %+v", u)
	}
}

// Cases at the edges of the rules above, each of which a wrong line in people.go got past the other tests.
func TestShownAtTheEdges(t *testing.T) {
	f := false
	d := peopleData()
	d.Users["sub-w"] = User{Sub: "sub-w", Username: "willow", Name: "Wide Name"}                           // named by its name before, has a user name now
	d.Users["sub-m"] = User{Sub: "sub-m", Username: "Maple", Name: "MAPLE", Email: "m@example.com"}        // its name is its user name
	d.Users["sub-c"] = User{Sub: "sub-c", Username: "svc-user", Email: "svc@localhost", EmailVerified: &f} // an address that does not look like one
	d.Users["sub-1"] = User{Sub: "sub-1", Username: "one", Email: "shared mailbox"}                        // two accounts whose "address" has no @
	d.Users["sub-2"] = User{Sub: "sub-2", Username: "two", Email: "shared mailbox"}                        //
	p := d.People()
	for _, c := range []struct{ stored, want string }{
		// a sentence with the name of an account that had no address: the user name it has now
		{"its machine id was accepted by Wide Name", "its machine id was accepted by willow"},
		// an API token whose name holds an address
		{"owner@example.com laptop (token, owner sub-n)", "Juniper laptop (token, owner Juniper)"},
		{"gone@example.org key (token, owner sub-o)", HiddenAddress + " key (token, owner " + HiddenAddress + ")"},
		// the mark of an address that is not verified comes off before the account is looked up
		{"svc@localhost (unverified)", "svc-user"},
		{"svc@localhost", "svc-user"},
		// what two accounts give as their address is hidden, also when it has no @
		{"shared mailbox", HiddenAddress},
		// what only looks a little like an address stays in a sentence
		{"ssh to 10.0.0.25 as fairy@10.0.0.25 failed: no route", HiddenAddress}, // (a field that names a person: hidden as a whole)
	} {
		if got := p.Shown(c.stored); got != c.want {
			t.Errorf("Shown(%q) = %q, want %q", c.stored, got, c.want)
		}
	}
	for _, c := range []struct{ text, want string }{
		{"ssh fairy@10.0.0.25 failed", "ssh fairy@10.0.0.25 failed"}, // digits after the last dot: an IP address
		{"mount of backup@nas.local.7z failed", "mount of backup@nas.local.7z failed"},
		{"sent to a@b.c", "sent to a@b.c"}, // one letter after the dot is no domain
		{"sent to a@b.cd", "sent to " + HiddenAddress},
	} {
		if got := p.ShownIn(c.text); got != c.want {
			t.Errorf("ShownIn(%q) = %q, want %q", c.text, got, c.want)
		}
	}
	// an account that calls itself by another account's address and has no address or name of its own
	dq := peopleData()
	dq.Users["sub-q"] = User{Sub: "sub-q", Username: "owner@example.com"}
	q := dq.People()
	if got, ok := q.Of("sub-q"); !ok || got != HiddenName {
		t.Errorf("Of(sub-q) = %q, %v: an account must not be named by a user name that is another account's address", got, ok)
	}
	if got, _ := q.Of("sub-n"); got != "Juniper" {
		t.Errorf("the account whose address it is: %q", got) // (its own user name is its alone: it is still shown)
	}
	if got := q.Shown("owner@example.com"); got != HiddenAddress {
		t.Errorf("the address one account has and another, without an address, is written into records by: %q", got) // (which of them a record means cannot be told)
	}
	// the dots of the Users page and the Audit log are for addresses and names, not for a user name
	for _, v := range p.Private() {
		if strings.EqualFold(v, "Maple") || v == "willow" || v == "Juniper" {
			t.Errorf("Private holds the user name %q", v)
		}
	}
	if !contains(p.Private(), "Wide Name") || !contains(p.Private(), "m@example.com") || !contains(p.Private(), "shared mailbox") {
		t.Errorf("Private: %q", p.Private())
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
