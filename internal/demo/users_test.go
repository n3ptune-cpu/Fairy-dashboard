package demo

import (
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// The demo's three made-up accounts have user names, and the panel names each of them by it: the demo shows
// what a real panel shows, not "(address hidden)" in every line. The signed-in demo user is one of them.
//
// A record with the address of an account that has a role reads as that account's user name. The third account
// only waits for access: it is named by its user name where the panel names the account (the Users page), and
// no record is read as its address (it wrote none; the address is hidden like any other).
func TestDemoAccountsHaveUsernames(t *testing.T) {
	cfg, _, _, err := Build(t.TempDir(), "http://127.0.0.1:8090")
	if err != nil {
		t.Fatal(err)
	}
	if err := SeedUsers(cfg); err != nil {
		t.Fatal(err)
	}
	d := access.NewStore(cfg.DataDir).Load()
	p := d.People()
	for sub, want := range map[string]string{"demo-alice": "alice", "demo-bob": "bob", "demo-carol": "carol"} {
		if got, ok := p.Of(sub); !ok || got != want || d.Users[sub].Username != want {
			t.Errorf("%s is named %q (%v), stored user name %q; want %q", sub, got, ok, d.Users[sub].Username, want)
		}
		record := want
		if d.Users[sub].Role == access.RolePending {
			record = access.HiddenAddress
		}
		if mail := d.Users[sub].Email; p.Shown(mail) != record {
			t.Errorf("a record with the address %s is shown as %q, want %q", mail, p.Shown(mail), record)
		}
	}
	if got := d.Users["demo-carol"].Role; got != access.RolePending {
		t.Errorf("the demo's third account has the role %q: the demo shows one account that waits", got)
	}
	if User.Sub != "demo-alice" || model.Deref(User.PreferredUsername) != "alice" {
		t.Errorf("the signed-in demo user: %s %q", User.Sub, model.Deref(User.PreferredUsername))
	}
	// seeding again (every start of the demo) keeps them
	if err := SeedUsers(cfg); err != nil {
		t.Fatal(err)
	}
	if got := access.NewStore(cfg.DataDir).Load().Users["demo-bob"].Username; got != "bob" {
		t.Errorf("after a second start: %q", got)
	}
}
