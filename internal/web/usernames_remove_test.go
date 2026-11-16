package web

import (
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
)

// A removal is told to the notification service (ntfy, Discord) by how the panel names the account, decided
// before the account is gone: its user name, or the words for its address or name. It was looked up after the
// removal, when nothing could be found: an account without an address went out by its name, and one with a user
// name as "(address hidden)".
func TestARemovalIsToldOutsideByTheShownName(t *testing.T) {
	r := newNamesRig(t, true)
	e := r.env
	var got []string
	e.s.sendOutside = func(title, message string) { got = append(got, title+" | "+message) }
	for _, id := range []access.Identity{{Sub: "sub-b", Name: "Bare Nameson"}, {Sub: "sub-c", Email: "svc@localhost", Username: "svc-user"}, {Sub: "sub-d", Name: "Dana Nameson", Username: "dana"},
		{Sub: "9a1b2c3d-wanda", Name: "Waiting Wanda", Email: "wanda@example.org", Username: "juniper"}} {
		if _, _, err := e.s.Access.SeenIdentity(id, access.RolePending, "first sign-in"); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]string{"sub-b": "(name hidden)", "sub-c": "svc-user", "sub-d": "dana", "sub-l": "viewer-one", "sub-o": "(address hidden)", "9a1b2c3d-wanda": "juniper · id 9a1b2c"}
	for _, sub := range []string{"sub-b", "sub-c", "sub-d", "sub-l", "sub-o", "9a1b2c3d-wanda"} {
		got = nil
		p := e.do("DELETE", "/api/admin/users/"+sub, nil, r.hdr("sub-n")...)
		if p.code != 200 || len(got) != 1 {
			t.Fatalf("DELETE /api/admin/users/%s: %d %s; %d messages", sub, p.code, p.raw, len(got))
		}
		if !strings.HasPrefix(got[0], "Fairy: user.remove on "+want[sub]+" | Juniper via ui: ok ") {
			t.Errorf("the removal of %s is told outside as %q, want it to name the account %q", sub, got[0], want[sub])
		}
		for _, private := range []string{"Bare Nameson", "Dana Nameson", "svc@localhost", "narrow@example.com", "Narrow Viewer", "old@example.com", "Old Timer", "Waiting Wanda", "wanda@example.org"} {
			if strings.Contains(got[0], private) {
				t.Errorf("the removal of %s is told outside with %q: %s", sub, private, got[0])
			}
		}
	}
	// the audit entry keeps the account's own text and its id, as before
	en := auditOf(e, "user.remove")
	if len(en) != 6 || en[0]["target"] != "wanda@example.org" || en[5]["target"] != "Bare Nameson" || en[5]["params"].(map[string]any)["sub"] != "sub-b" {
		t.Errorf("the audit entries of the removals: %v", en)
	}
	// a change of a role is told the same way: by the account that was changed, also when its address is one
	// that two accounts have (looked up by the address it would be nobody's)
	for _, id := range []access.Identity{{Sub: "sub-e1", Email: "shared@example.org", Username: "eve1"}, {Sub: "sub-e2", Email: "shared@example.org", Username: "eve2"}} {
		if _, _, err := e.s.Access.SeenIdentity(id, access.RolePending, "first sign-in"); err != nil {
			t.Fatal(err)
		}
	}
	got = nil
	if p := e.do("PUT", "/api/admin/users/sub-e2", map[string]any{"role": "viewer", "sites": []string{"Bob"}}, r.hdr("sub-n")...); p.code != 200 || len(got) != 1 ||
		!strings.HasPrefix(got[0], "Fairy: user.set_role on eve2 | Juniper via ui: ok ") || strings.Contains(got[0], "shared@example.org") {
		t.Errorf("a change of the role of an account whose address two accounts have: %d %v", p.code, got)
	}
	// a removal that is refused names nobody it could not look up: the id that was asked for
	got = nil
	if p := e.do("DELETE", "/api/admin/users/nobody-with-this-id", nil, r.hdr("sub-n")...); p.code != 404 || len(got) != 1 || !strings.HasPrefix(got[0], "Fairy: user.remove on nobody-with-this-id | ") {
		t.Errorf("a removal of nobody: %d %v", p.code, got)
	}
}
