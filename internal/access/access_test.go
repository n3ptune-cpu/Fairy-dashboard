package access

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
)

func status(err error) int {
	var ae *apierr.Error
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

func TestGrantSeesAndCap(t *testing.T) {
	bob := Grant{Role: RoleViewer, Sites: []string{"Bob"}}
	if !AdminGrant().Sees("") || bob.Sees("") || bob.Sees("Alice") || !bob.Sees("Bob") {
		t.Fatal("Sees")
	}
	if (Grant{Role: RolePending}).Sees("Bob") || !(Grant{Role: RoleViewer, All: true}).Sees("") {
		t.Fatal("pending / all-sites")
	}
	both := Grant{Role: RoleViewer, Sites: []string{"Alice", "Bob"}}
	if c := both.Cap(bob); c.String() != "viewer:Bob" {
		t.Fatal(c)
	}
	if c := AdminGrant().Cap(bob); c.String() != "viewer:Bob" || c.Admin() {
		t.Fatal("a token of a viewer is never admin", c)
	}
	if c := (Grant{Role: RoleViewer, All: true}).Cap(bob); c.String() != "viewer:Bob" {
		t.Fatal(c)
	}
	if c := AdminGrant().Cap(Grant{Role: RolePending}); !c.Pending() {
		t.Fatal(c)
	}
	if c := bob.Cap(AdminGrant()); !c.Same(bob) {
		t.Fatal(c)
	}
	for in, want := range map[string]string{"admin": "admin", "pending": "pending", "viewer:Bob": "viewer:Bob",
		"viewer: Bob , Alice": "viewer:Alice,Bob"} {
		g, err := ParseView(in)
		if err != nil || g.String() != want {
			t.Errorf("%q -> %v %v", in, g, err)
		}
	}
	for _, bad := range []string{"", "viewer", "viewer:", "root", "admin:x", "viewer:<b>"} {
		if _, err := ParseView(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestStoreRolesSitesAndLastAdmin(t *testing.T) {
	s := NewStore(t.TempDir())
	// the identity provider verified both addresses: the tests below find the users by them
	if u, created, err := s.SeenIdentity(Identity{Sub: "sub-n", Email: "N@example.com", EmailVerified: true, Name: "Alice"}, RoleAdmin, "control_groups"); err != nil || !created || u.Role != RoleAdmin {
		t.Fatal(u, err)
	}
	if u, created, _ := s.Seen("sub-n", "", "Alice 2", RolePending, "first sign-in"); created || u.Role != RoleAdmin || u.Email != "N@example.com" || u.Name != "Alice 2" {
		t.Fatal("an existing user keeps their role", u)
	}
	_, _, _ = s.SeenIdentity(Identity{Sub: "sub-s", Email: "bob@example.com", EmailVerified: true, Name: "Bob"}, RolePending, "first sign-in")
	if _, _, err := s.SetRole("n@example.com", RoleViewer, nil, "test", false); status(err) != 409 {
		t.Fatal("last admin demoted", err)
	}
	if _, err := s.Remove("sub-n", false, "test"); status(err) != 409 {
		t.Fatal("last admin removed", err)
	}
	if _, _, err := s.SetRole("bob@example.com", RoleViewer, []string{"Bob"}, "test", false); status(err) != 422 {
		t.Fatal("unknown site accepted", err)
	}
	for _, n := range []string{"Bob", "Alice"} {
		if err := s.CreateSite(n); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateSite("Bob"); status(err) != 409 {
		t.Fatal("duplicate site")
	}
	if err := s.CreateSite(" bad"); status(err) != 422 {
		t.Fatal("invalid site name")
	}
	if _, after, err := s.SetRole("bob@example.com", RoleViewer, []string{"Bob"}, "panel by n", false); err != nil || after.Grant().String() != "viewer:Bob" {
		t.Fatal(after, err)
	}
	if _, err := s.Assign("bob-pc", "Bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Assign("power:desk", "Nope"); status(err) != 422 {
		t.Fatal("assign to unknown site")
	}
	if err := s.RenameSite("Bob", "Bob's place"); err != nil {
		t.Fatal(err)
	}
	d := s.Load()
	if d.Assign["bob-pc"] != "Bob's place" || d.Users["sub-s"].Sites[0] != "Bob's place" || d.HasSite("Bob") {
		t.Fatalf("rename: %+v", d)
	}
	if _, _, err := s.SetRole("sub-s", RoleAdmin, nil, "test", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SetRole("sub-n", RoleViewer, []string{"Alice"}, "test", false); err != nil {
		t.Fatal("two admins: demoting one is fine", err)
	}
	if u := s.Load().Users["sub-s"]; len(u.Sites) != 0 {
		t.Fatal("admins keep no site list")
	}
	if err := s.DeleteSite("Bob's place"); err != nil {
		t.Fatal(err)
	}
	if d := s.Load(); len(d.Assign) != 0 || d.HasSite("Bob's place") {
		t.Fatal("delete site", d)
	}
	if _, err := s.Remove("nobody", false, "test"); status(err) != 404 {
		t.Fatal("remove unknown user")
	}
	if _, err := s.Remove("sub-s", true, "test"); err != nil {
		t.Fatal("forced removal of the last admin (CLI --force)", err)
	}
}

// The CLI and the server write users.json at the same time: no change may be lost.
func TestStoreConcurrentWritesAreNotLost(t *testing.T) {
	dir := t.TempDir()
	a, b := NewStore(dir), NewStore(dir) // two processes' views of the same file
	if err := a.CreateSite("Alice"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			if _, _, err := a.Seen(fmt.Sprintf("u%d", i), "", "", RolePending, "first sign-in"); err != nil {
				t.Error(err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			if _, err := b.Assign(fmt.Sprintf("host-%d", i), "Alice"); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	d := NewStore(dir).Load()
	if len(d.Users) != 40 || len(d.Assign) != 40 {
		t.Fatalf("lost writes: %d users, %d assignments", len(d.Users), len(d.Assign))
	}
}
