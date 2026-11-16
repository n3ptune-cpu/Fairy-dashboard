package access

import (
	"testing"
)

// The accounts of these tests are made up. Nobody has these addresses.

// waitingData is peopleData with one more account that has a role and no address: sub-b2, user name "bare",
// name "Second Bare". (sub-b, "Bare Name", has no user name.)
func waitingData() Data {
	d := peopleData()
	d.Users["sub-b2"] = User{Sub: "sub-b2", Username: "bare", Name: "Second Bare", Role: RoleViewer}
	return d
}

// An account that only waits for access takes part in no reading of a stored text: not by its name, not by
// its address, and not by a user name the panel does not show. It wrote no record (it cannot act), and what it
// calls itself is its own choice. So whatever such an account says, every record and every sentence reads as
// it does without the account; in particular a record by an admin's user name stays the admin's, and a word of
// a sentence stays a word.
func TestAWaitingAccountTakesNoPartInStoredText(t *testing.T) {
	records := []string{"Juniper", "juniper", "owner@example.com", "OWNER@example.com (unverified)", "Real Name", "PineTop", "friend@example.com",
		"Bare Name", "Second Bare", "bare", "old@example.com", "Old Timer", "sub-n", "ci (token, owner sub-n)", "panel by owner@example.com", "panel by Second Bare", "off", "root"}
	sentences := []string{"Lamp (Desk bar) switched off by Juniper", "started by owner@example.com: 3 machines", "shutdown requested by Second Bare (in 120 s)",
		"its machine id was accepted by Bare Name", "run started by Juniper was cancelled by PineTop", "agent update failed: owner@example.com", "switched off", "Keep on by off: the run calls its switch-off off"}
	base := waitingData().People()
	if got := base.Shown("Second Bare"); got != "bare" {
		t.Fatalf("the fixture: a record by the name of an account without an address reads %q", got)
	}
	for _, w := range []User{
		{Name: "Juniper"},                                // no address, the admin's user name as its name
		{Name: "juniper"},                                //
		{Name: "Juniper", Username: "wanda"},             // ... with a user name of its own
		{Name: "off"},                                    // a word of the panel's sentences
		{Name: "switched"},                               //
		{Name: "Second Bare"},                            // the name of an account that has a role and no address
		{Name: "Bare Name", Username: "bn"},              //
		{Name: "PineTop"},                                // another account's user name
		{Name: "Real Name", Email: "w@example.org"},      // next to an address: another account's name
		{Email: "owner@example.com", Username: "wanda"},  // the admin's address
		{Email: "OWNER@EXAMPLE.COM"},                     //
		{Email: "Juniper", Username: "wanda"},            // an "address" that is the admin's user name
		{Email: "friend@example.com", Name: "Old Timer"}, //
		{Username: "owner@example.com"},                  // a user name that is not shown, of an account without an address
		{Username: "sub-n"},                              //
		{Username: "Second Bare"},                        //
		{Username: "off", Name: "by"},                    //
	} {
		w.Sub, w.Role = "9a1b2c3d-wanda", RolePending
		d := waitingData()
		d.Users[w.Sub] = w
		p := d.People()
		for _, sub := range []string{"sub-n", "sub-s", "sub-o", "sub-b", "sub-b2"} {
			want, _ := base.Of(sub)
			if got, _ := p.Of(sub); got != want {
				t.Errorf("a waiting account %+v: %s is shown by %q, without it by %q", w, sub, got, want)
			}
		}
		for _, r := range records {
			if got, want := p.Shown(r), base.Shown(r); got != want {
				t.Errorf("a waiting account {name %q, address %q, user name %q}: the record %q reads %q, without the account %q", w.Name, w.Email, w.Username, r, got, want)
			}
		}
		for _, s := range sentences {
			if got, want := p.ShownIn(s), base.ShownIn(s); got != want {
				t.Errorf("a waiting account {name %q, address %q, user name %q}: the sentence %q reads %q, without the account %q", w.Name, w.Email, w.Username, s, got, want)
			}
			if got, want := p.HiddenIn(s), base.HiddenIn(s); got != want {
				t.Errorf("a waiting account {name %q, address %q, user name %q}: the sentence of now %q reads %q, without the account %q", w.Name, w.Email, w.Username, s, got, want)
			}
		}
	}
	// What a waiting account calls itself is not looked up either: its name stays a text like any other, and
	// its address is hidden as an address (it is never turned into the account's user name).
	d := waitingData()
	d.Users["9a1b2c3d-wanda"] = User{Sub: "9a1b2c3d-wanda", Username: "wanda", Name: "Waiting Wanda", Email: "ops@corp", Role: RolePending}
	d.Users["9a1b2c3d-wilma"] = User{Sub: "9a1b2c3d-wilma", Name: "Waiting Wilma", Role: RolePending}
	p := d.People()
	for stored, want := range map[string]string{"Waiting Wanda": "Waiting Wanda", "Waiting Wilma": "Waiting Wilma", "ops@corp": HiddenAddress, "wanda": "wanda", "9a1b2c3d-wanda": "wanda"} {
		if got := p.Shown(stored); got != want {
			t.Errorf("Shown(%q) = %q, want %q", stored, got, want)
		}
	}
	for text, want := range map[string]string{"accepted by Waiting Wilma": "accepted by Waiting Wilma", "accepted by ops@corp.": "accepted by " + HiddenAddress + ".",
		"asked by OPS@corp (unverified)": "asked by " + HiddenAddress} {
		if got := p.ShownIn(text); got != want {
			t.Errorf("ShownIn(%q) = %q, want %q", text, got, want)
		}
		if got := p.HiddenIn(text); got != want {
			t.Errorf("HiddenIn(%q) = %q, want %q", text, got, want)
		}
	}
	// ... and once the account has a role, the old rule holds for it again (an account can have written records
	// under an older version, by its name, while it had a role)
	w := d.Users["9a1b2c3d-wilma"]
	w.Role = RoleViewer
	d.Users[w.Sub] = w
	if got := d.People().Shown("Waiting Wilma"); got != HiddenName {
		t.Errorf("the same account with a role: its name in a record reads %q", got)
	}
}

// A text that is what an account is shown by is that account's, and nothing else's: a record keeps exactly
// that text since build 17. Another account that has it as its name (and no address), or as its address, does
// not make it "two accounts": the admin's records were hidden by that.
func TestATextAnAccountIsShownByIsThatAccounts(t *testing.T) {
	for _, other := range []User{
		{Name: "Juniper", Role: RoleViewer},                        // no address: an older version named it by this name
		{Name: "JUNIPER", Username: "june", Role: RoleViewer},      //
		{Email: "Juniper", Username: "jay", Role: RoleViewer},      // an "address" without an @
		{Username: "sub-n", Role: RoleViewer},                      // (not shown: another account's id)
		{Name: "juniper", Username: "owner@x", Role: RoleViewer},   //
		{Name: "Juniper", Email: "j@example.org", Role: RoleAdmin}, // a name next to an address never counted
	} {
		other.Sub = "7e7e7e7e-other"
		d := peopleData()
		d.Users[other.Sub] = other
		p := d.People()
		for _, stored := range []string{"Juniper", "juniper"} {
			if got := p.Shown(stored); got != "Juniper" {
				t.Errorf("next to %+v: Shown(%q) = %q, want the admin's user name", other, stored, got)
			}
		}
		for _, s := range []string{"Lamp (Desk bar) switched off by Juniper", "run started by Juniper was cancelled by Juniper", "confirmed by Juniper: its agent lets one update through"} {
			if got := p.ShownIn(s); got != s {
				t.Errorf("next to %+v: ShownIn(%q) = %q", other, s, got)
			}
			if got := p.HiddenIn(s); got != s {
				t.Errorf("next to %+v: HiddenIn(%q) = %q", other, s, got)
			}
		}
	}
}

// The name of an account without an address is replaced in a sentence only where an older version wrote who
// did something: after "by" ("switched off by X", "started by X: ...", "requested by X (...)", "cancelled by
// X") and after "the run" ("the panel restarted during the run X started at ...", "for the run X started").
// Everywhere else the words of a sentence stay: a name that is an ordinary word ("off") took that word out of
// every sentence.
func TestANameInASentenceOnlyWhereItNamesWhoDidIt(t *testing.T) {
	d := peopleData()
	d.Users["sub-off"] = User{Sub: "sub-off", Username: "ollie", Name: "off", Role: RoleViewer}
	d.Users["sub-run"] = User{Sub: "sub-run", Name: "the run", Role: RoleViewer}             // no user name: hidden
	d.Users["sub-up"] = User{Sub: "sub-up", Username: "upton", Name: "UPS", Role: RoleAdmin} //
	p := d.People()
	for _, c := range []struct{ text, old, now string }{
		{"Lamp (Desk bar) switched off by Juniper", "", ""},
		{"switched off", "", ""},
		{"off", "", ""},
		{"standby off", "", ""}, // "by" as the end of another word is no "by"
		{"goodby off", "", ""},
		{"UPS on battery: 12 min left", "", ""},
		{"the run is over; the run was cancelled", "", ""},
		{"shutdown requested by off (in 120 s)", "shutdown requested by ollie (in 120 s)", "shutdown requested by " + HiddenName + " (in 120 s)"},
		{"switched off by off", "switched off by ollie", "switched off by " + HiddenName},
		{"off by off: off", "off by ollie: off", "off by " + HiddenName + ": off"},
		{"run started by off was cancelled by off", "run started by ollie was cancelled by ollie", "run started by " + HiddenName + " was cancelled by " + HiddenName},
		{"run started by off was cancelled by UPS", "run started by ollie was cancelled by upton", "run started by " + HiddenName + " was cancelled by " + HiddenName},
		{"Keep on by off: the run calls its switch-off off", "Keep on by ollie: the run calls its switch-off off", "Keep on by " + HiddenName + ": the run calls its switch-off off"},
		{"Started By off.", "Started By ollie.", "Started By " + HiddenName + "."},
		{"the panel restarted during the run off started at 10:04 UTC; it is not continued", "the panel restarted during the run ollie started at 10:04 UTC; it is not continued",
			"the panel restarted during the run " + HiddenName + " started at 10:04 UTC; it is not continued"},
		{"Fairy woke TOWER for the run UPS started, and it waits for your passkey", "Fairy woke TOWER for the run upton started, and it waits for your passkey",
			"Fairy woke TOWER for the run " + HiddenName + " started, and it waits for your passkey"},
		{"accepted by the run", "accepted by " + HiddenName, "accepted by " + HiddenName},
		{"accepted by offline", "", ""}, // a word of its own only
		{"its machine id was accepted by Bare Name", "its machine id was accepted by " + HiddenName, "its machine id was accepted by " + HiddenName},
		{"Bare Name accepted it", "", ""}, // no sentence of an older version begins with who did it
		{"by  off", "", ""},               // one space, as the panel wrote it
	} {
		if c.old == "" {
			c.old, c.now = c.text, c.text
		}
		if got := p.ShownIn(c.text); got != c.old {
			t.Errorf("ShownIn(%q)\n  got  %q\n  want %q", c.text, got, c.old)
		}
		if got := p.HiddenIn(c.text); got != c.now {
			t.Errorf("HiddenIn(%q)\n  got  %q\n  want %q", c.text, got, c.now)
		}
	}
	// a record (a field that holds who did it and nothing else) is still read as a whole
	for stored, want := range map[string]string{"off": "ollie", "UPS": "upton", "the run": HiddenName, "panel by off": "panel by ollie"} {
		if got := p.Shown(stored); got != want {
			t.Errorf("Shown(%q) = %q, want %q", stored, got, want)
		}
	}
}
