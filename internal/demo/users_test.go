package demo

import (
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// The demo's three made-up accounts have user names, and the panel names each of them by it: the demo shows
// what a real panel shows, not "(address hidden)" in every line. The signed-in demo user is one of them.
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
		if mail := d.Users[sub].Email; p.Shown(mail) != want {
			t.Errorf("a record with the address %s is shown as %q", mail, p.Shown(mail))
		}
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
