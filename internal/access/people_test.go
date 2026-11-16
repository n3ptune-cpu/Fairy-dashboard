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

// A user name is what the account says. It is shown only when it is that account's alone: an account that
// calls itself by another account's address, name, user name or subject is not shown by it (and the other
// account's text is then not given to either). A user name that is the account's own address is the address.
func TestAUsernameMustBeTheAccountsOwn(t *testing.T) {
	for _, c := range []struct {
		name, username string
		wantOf         string // how the account sub-x (address x@example.com) is named
		stored, want   string // and what a stored text is shown as
	}{
		{"another account's address", "owner@example.com", HiddenAddress, "owner@example.com", HiddenAddress},
		{"another account's user name", "juniper", HiddenAddress, "Juniper", "Juniper"},
		{"another account's name", "Real Name", HiddenAddress, "Real Name", HiddenName},
		{"another account's subject", "sub-n", HiddenAddress, "sub-n", "sub-n"}, // the text stays: it is no address and no name
		{"its own address", "x@example.com", HiddenAddress, "x@example.com", HiddenAddress},
		{"its own address in other letters", "X@Example.COM", HiddenAddress, "x@example.com", HiddenAddress},
		{"a user name of its own", "Xavier", "Xavier", "x@example.com", "Xavier"},
		{"its own name", "Ex Person", "Ex Person", "Ex Person", "Ex Person"}, // a handle in both fields is a user name
		{"a user name with an @ of its own", "x@handle", "x@handle", "x@example.com", "x@handle"},
	} {
		d := peopleData()
		tr := true
		d.Users["sub-x"] = User{Sub: "sub-x", Username: c.username, Email: "x@example.com", EmailVerified: &tr, Name: "Ex Person"}
		p := d.People()
		if got, ok := p.Of("sub-x"); !ok || got != c.wantOf {
			t.Errorf("%s: the account is named %q, want %q", c.name, got, c.wantOf)
		}
		if got := p.Shown(c.stored); got != c.want {
			t.Errorf("%s: Shown(%q) = %q, want %q", c.name, c.stored, got, c.want)
		}
		// the admin's own records stay the admin's, whatever another account calls itself
		if c.username != "juniper" {
			if got, _ := p.Of("sub-n"); got != "Juniper" {
				t.Errorf("%s: the admin is named %q", c.name, got)
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
	// two accounts with the same user name: neither is shown by it
	d = peopleData()
	d.Users["sub-x"] = User{Sub: "sub-x", Username: "Juniper", Email: "x@example.com"}
	p := d.People()
	if a, _ := p.Of("sub-n"); a != HiddenAddress {
		t.Errorf("a user name two accounts have, the first: %q", a)
	}
	if b, _ := p.Of("sub-x"); b != HiddenAddress {
		t.Errorf("a user name two accounts have, the second: %q", b)
	}
	if got := p.Shown("Juniper"); got != "Juniper" { // a text that is only a user name names no address and no name
		t.Errorf("the text of that user name: %q", got)
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
	after.Username = ""
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
	if !strings.Contains(string(raw), `"username": "Juniper"`) {
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
		t.Errorf("the address that two accounts now have: %q", got) // (which of them an old record means cannot be told)
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
