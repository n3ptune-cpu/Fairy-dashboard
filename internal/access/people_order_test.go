package access

import (
	"testing"
	"time"
)

// Of the accounts with one user name, an admin is shown by the plain name before a viewer, a viewer before an
// account that only waits; inside one of the three the account that has had the user name longest, then the
// smaller account id. An admin and a viewer counted as equal, so the time decided between them, and the time
// is "who signed in first" at the first start of this build and after build 16 wrote users.json (it drops the
// user name and since when): a viewer who had set the admin's user name at the provider and signed in first was
// "Juniper", and the admin "Juniper · id 3f6c1d", from then on.
func TestAnAdminComesBeforeAViewerAndAViewerBeforeOneThatWaits(t *testing.T) {
	u := func(sub, role string, since int) User {
		return User{Sub: sub, Username: "Juniper", Email: sub + "@example.org", Role: role, CreatedAt: *at(1), UsernameSince: at(since)}
	}
	for _, c := range []struct {
		name  string
		users []User
		plain string
	}{
		{"an admin after a viewer", []User{u("3f6c1d-admin", RoleAdmin, 9), u("7e7e7e-viewer", RoleViewer, 2)}, "3f6c1d-admin"},
		{"an admin after a viewer, the other way round in the ids", []User{u("9a9a9a-admin", RoleAdmin, 9), u("1b1b1b-viewer", RoleViewer, 2)}, "9a9a9a-admin"},
		{"an admin after one that waits", []User{u("3f6c1d-admin", RoleAdmin, 9), u("1b1b1b-waits", RolePending, 2)}, "3f6c1d-admin"},
		{"a viewer after one that waits", []User{u("7e7e7e-viewer", RoleViewer, 9), u("1b1b1b-waits", RolePending, 2)}, "7e7e7e-viewer"},
		{"all three, the admin last", []User{u("1b1b1b-waits", RolePending, 2), u("2c2c2c-viewer", RoleViewer, 3), u("3f6c1d-admin", RoleAdmin, 9)}, "3f6c1d-admin"},
		{"two admins: the one that has had it longer", []User{u("1b1b1b-admin", RoleAdmin, 5), u("3f6c1d-admin", RoleAdmin, 4)}, "3f6c1d-admin"},
		{"two viewers: the one that has had it longer", []User{u("1b1b1b-viewer", RoleViewer, 5), u("3f6c1d-viewer", RoleViewer, 4)}, "3f6c1d-viewer"},
		{"two that wait: the one that has had it longer", []User{u("1b1b1b-waits", RolePending, 5), u("3f6c1d-waits", RolePending, 4)}, "3f6c1d-waits"},
		{"two admins, the same time: the smaller id", []User{u("3f6c1d-admin", RoleAdmin, 4), u("1b1b1b-admin", RoleAdmin, 4)}, "1b1b1b-admin"},
		{"two viewers and an admin of the same time", []User{u("1b1b1b-viewer", RoleViewer, 4), u("2c2c2c-viewer", RoleViewer, 4), u("3f6c1d-admin", RoleAdmin, 4)}, "3f6c1d-admin"},
	} {
		d := Data{Users: map[string]User{}}
		for _, x := range c.users {
			d.Users[x.Sub] = x
		}
		p := d.People()
		for _, x := range c.users {
			got, _ := p.Of(x.Sub)
			want := "Juniper"
			if x.Sub != c.plain {
				want = "Juniper · id " + x.Sub[:6]
			}
			if got != want {
				t.Errorf("%s: %s (%s) is shown by %q, want %q", c.name, x.Sub, x.Role, got, want)
			}
		}
	}
	// The first start of this build, or the first one after build 16 wrote the file: nobody has a user name or a
	// time. The viewer's browser asks first, then the admin signs in.
	s := NewStore(t.TempDir())
	for _, id := range []Identity{{Sub: "3f6c1d00-admin", Email: "owner@example.com", EmailVerified: true}, {Sub: "7e7e7e7e-viewer", Email: "v@example.org", EmailVerified: true}} {
		role := RoleViewer
		if id.Sub == "3f6c1d00-admin" {
			role = RoleAdmin
		}
		if _, _, err := s.SeenIdentity(id, role, "first sign-in"); err != nil {
			t.Fatal(err)
		}
	}
	if done, err := s.FillUsername("7e7e7e7e-viewer", "juniper"); !done || err != nil {
		t.Fatal(done, err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, _, err := s.SeenIdentity(Identity{Sub: "3f6c1d00-admin", Email: "owner@example.com", EmailVerified: true, Username: "Juniper"}, RoleAdmin, ""); err != nil {
		t.Fatal(err)
	}
	d := s.Load()
	if a, v := d.Users["3f6c1d00-admin"], d.Users["7e7e7e7e-viewer"]; a.Role != RoleAdmin || v.Role != RoleViewer || a.UsernameSince == nil || v.UsernameSince == nil || !v.UsernameSince.Before(*a.UsernameSince) {
		t.Fatalf("the fixture: the viewer has had the user name longer than the admin: %+v %+v", a, v)
	}
	p := d.People()
	if a, _ := p.Of("3f6c1d00-admin"); a != "Juniper" {
		t.Errorf("the admin, who signed in after the viewer, is shown by %q", a)
	}
	if v, _ := p.Of("7e7e7e7e-viewer"); v != "juniper · id 7e7e7e" {
		t.Errorf("the viewer, who signed in first, is shown by %q", v)
	}
}
