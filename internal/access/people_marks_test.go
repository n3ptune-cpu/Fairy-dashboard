package access

import (
	"strings"
	"testing"
	"time"
)

// A user name can be written like the panel's own texts: with the mark the panel puts between a user name and
// an account id ("Juniper · id 3f6c1d", with the admin's id), like an API token of the admin ("ci (token, owner
// Juniper)"), or as one of the words the panel has for who did something when it was no account ("root", "(address
// hidden)"). Such a user name was shown plain. It is never shown plain now: the account is shown by it with the
// start of its own id behind it, whatever its role, and the Users page says why (UsernameMarked).
func TestAUsernameWithThePanelsMarksIsShownWithItsOwnID(t *testing.T) {
	tr := true
	admin := User{Sub: "3f6c1d00-admin", Username: "Juniper", Email: "owner@example.com", EmailVerified: &tr, Name: "Real Name", Role: RoleAdmin, CreatedAt: *at(5)}
	long := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC) // the other account has had its user name longer than the admin
	// what the panel shows for who did something, when it is no account
	const adminsToken = "ci (token, owner Juniper)"
	own := []string{"root", "root (cli)", "fairy", "Fairy", "unknown", "system", HiddenAddress, HiddenName, "This account", adminsToken, "cli by root", "panel by Juniper"}
	for _, name := range []string{"Juniper · id 3f6c1d", "juniper · id 3f6c1d00-admin", "Somebody · id 3f6c1d", "a·b", "·", "Juniper ·", adminsToken, "ci (token, owner 3f6c1d00-admin)",
		HiddenAddress, HiddenName, "(Address Hidden)", "address hidden", "Name Hidden", "root", "ROOT", "Root", "root (cli)", "fairy", "FAIRY", "unknown", "Unknown", "system", "This account", "this account",
		"the account", "This User", "x(", ")x", "(", "Juniper (admin)", "ro​ot", "root️", " root ", "address  hidden"} {
		for _, role := range []string{RolePending, RoleViewer, RoleAdmin} {
			w := User{Sub: "9a1b2c3d-wanda", Username: name, Email: "wanda@example.org", Role: role, CreatedAt: long}
			p := Data{Users: map[string]User{admin.Sub: admin, w.Sub: w}}.People()
			want := CleanUsername(name) + " · id 9a1b2c"
			if got, _ := p.Of(w.Sub); got != want || p.Username(w.Sub) != want || p.UsernameNote(w.Sub) != UsernameMarked || p.SameUsername(w.Sub) != nil {
				t.Errorf("an account (%s) with the user name %q is shown by %q (note %q, same as %v), want %q", role, name, got, p.UsernameNote(w.Sub), p.SameUsername(w.Sub), want)
			}
			if a, _ := p.Of(admin.Sub); a != "Juniper" || p.UsernameNote(admin.Sub) != "" {
				t.Errorf("next to the user name %q the admin is shown by %q (note %q)", name, a, p.UsernameNote(admin.Sub))
			}
			// no two who are not the same are shown by one text: the account, the admin, the admin's token, the
			// command line, the panel's words
			token := p.Shown("ci (token, owner " + admin.Sub + ")")
			if token != adminsToken {
				t.Errorf("next to the user name %q the admin's token reads %q", name, token)
			}
			shown, _ := p.Of(w.Sub)
			for _, other := range append([]string{"Juniper"}, own...) {
				if fold(shown) == fold(other) {
					t.Errorf("an account (%s) with the user name %q is shown by %q, which is the panel's text %q", role, name, shown, other)
				}
				if got := p.Shown(other); got != other {
					t.Errorf("next to the user name %q the panel's text %q reads %q", name, other, got)
				}
			}
			// what the panel's own words are when an agent repeats them is not that account
			for said, wantSaid := range map[string]string{"address hidden": HiddenAddress, "name hidden": HiddenName, "unknown": "unknown", "root": HiddenName, "Juniper": "Juniper"} {
				if got := p.FromOutside(said); got != wantSaid {
					t.Errorf("next to the user name %q (%s), an agent's %q reads %q, want %q", name, role, said, got, wantSaid)
				}
			}
			// a record of an account with a role keeps what it is shown by, and reads as that account's
			if role != RolePending {
				if got := p.Shown(p.Record(w.Sub, "wanda@example.org")); got != want {
					t.Errorf("a record of the account with the user name %q reads %q, want %q", name, got, want)
				}
			}
		}
	}
	// a user name that merely looks a little like one of them is a user name like any other
	for _, name := range []string{"Juniper2", "rooty", "groot", "id", "token", "Ju-ni.per_1", "a.b", "address", "hidden", "juniper id 3f6c1d", "owner Juniper", "o'neil", "x@handle", "[root]"} {
		w := User{Sub: "9a1b2c3d-wanda", Username: name, Email: "wanda@example.org", Role: RoleViewer, CreatedAt: long}
		p := Data{Users: map[string]User{admin.Sub: admin, w.Sub: w}}.People()
		if got, _ := p.Of(w.Sub); got != name || p.UsernameNote(w.Sub) != "" {
			t.Errorf("the user name %q is shown by %q (note %q)", name, got, p.UsernameNote(w.Sub))
		}
	}
	// two accounts with one such user name: each with its own id, and each row says which account has the same
	a := User{Sub: "7e7e7e7e-a", Username: "root", Role: RoleAdmin, CreatedAt: long}
	b := User{Sub: "9a1b2c3d-b", Username: "ROOT", Role: RolePending, CreatedAt: long}
	c := User{Sub: "7e7e7e7e-c", Username: "Root", Role: RoleViewer, CreatedAt: long}
	p := Data{Users: map[string]User{a.Sub: a, b.Sub: b, c.Sub: c}}.People()
	for sub, want := range map[string]string{a.Sub: "root · id 7e7e7e7e-a", b.Sub: "ROOT · id 9a1b2c", c.Sub: "Root · id 7e7e7e7e-c"} {
		if got, _ := p.Of(sub); got != want || p.UsernameNote(sub) != UsernameMarked || len(p.SameUsername(sub)) != 2 {
			t.Errorf("%s is shown by %q (note %q, same as %v), want %q", sub, got, p.UsernameNote(sub), p.SameUsername(sub), want)
		}
	}
	if got := p.Shown("root"); got != "root" {
		t.Errorf("the command line's root reads %q next to accounts that call themselves root", got)
	}
	// a user name of the form the panel writes, with the id of the account it would be the form of: still no two
	// accounts with one text
	first := User{Sub: "3f6c1d00-admin", Username: "Juniper", Role: RoleAdmin, CreatedAt: *at(1)}
	second := User{Sub: "9a1b2c3d-second", Username: "juniper", Role: RoleViewer, CreatedAt: *at(2)} // shown as "juniper · id 9a1b2c"
	third := User{Sub: "7e7e7e7e-third", Username: "Juniper · id 9a1b2c", Role: RoleViewer, CreatedAt: long}
	p = Data{Users: map[string]User{first.Sub: first, second.Sub: second, third.Sub: third}}.People()
	seen := map[string]string{}
	for _, sub := range []string{first.Sub, second.Sub, third.Sub} {
		n, _ := p.Of(sub)
		if other, twice := seen[fold(n)]; twice || n == "" || strings.Contains(n, "hidden") {
			t.Errorf("%s and %s are both shown by %q", sub, other, n)
		}
		seen[fold(n)] = sub
	}
	if got, _ := p.Of(third.Sub); got != "Juniper · id 9a1b2c · id 7e7e7e" {
		t.Errorf("the account that calls itself like the second one's shown form: %q", got)
	}
	if got, _ := p.Of(second.Sub); got != "juniper · id 9a1b2c" {
		t.Errorf("the second account: %q", got)
	}
}
