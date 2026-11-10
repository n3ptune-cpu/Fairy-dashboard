package web

import (
	"net/url"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// L4: a sign-in records whether the identity provider verified the e-mail address; the Users page gets it as
// email_verified next to the unchanged fields; and an address that is only claimed does not select an account
// in the role API either.
func TestSignInRecordsWhetherTheEmailIsVerified(t *testing.T) {
	e := setup(t)
	admin := controller()
	admin.Email, admin.EmailVerified = model.P("boss@example.com"), true
	ac := e.cookie(admin, 0)
	impostor := model.User{Sub: "u-impostor", Name: model.P("Boss"), Email: model.P("Boss@example.com"), Groups: []string{"viewers"}}
	e.signIn(impostor) // claims the admin's address; the provider did not verify it
	noMail := model.User{Sub: "u-nomail", Name: model.P("No Mail"), EmailVerified: true, Groups: []string{"viewers"}}
	e.signIn(noMail)

	d := e.s.Access.Load()
	if u := d.Users[admin.Sub]; !u.Verified() || u.Role != access.RoleAdmin {
		t.Fatal(u)
	}
	if u := d.Users["u-impostor"]; u.Verified() || u.EmailVerified == nil || *u.EmailVerified || u.Email != "Boss@example.com" {
		t.Fatal("an unverified address is kept, as unverified", u)
	}
	if u := d.Users["u-nomail"]; u.Verified() {
		t.Fatal("verified without an address", u)
	}

	page := e.do("GET", "/api/admin/users", nil, "Cookie", ac)
	if page.code != 200 {
		t.Fatal(page.code, string(page.raw))
	}
	got := map[string]map[string]any{}
	for _, x := range page.body["users"].([]any) {
		u := x.(map[string]any)
		got[u["sub"].(string)] = u
		for _, k := range []string{"sub", "email", "name", "role", "sites", "created_at", "last_seen", "via", "you", "email_verified"} {
			if _, ok := u[k]; !ok {
				t.Fatalf("users page: field %q missing in %v", k, u)
			}
		}
	}
	if got[admin.Sub]["email_verified"] != true || got["u-impostor"]["email_verified"] != false || got["u-nomail"]["email_verified"] != nil {
		t.Fatal(got)
	}

	csrf := []string{"Cookie", ac, "X-Requested-With", "fairy"}
	// both present: the address finds the admin's own (verified) account, never the impostor's. Demoting the
	// last admin is refused, which shows whose account was found.
	r := e.do("PUT", "/api/admin/users/"+url.PathEscape("Boss@example.com"), map[string]any{"role": "viewer", "sites": []string{}}, csrf...)
	if r.code != 409 || !strings.Contains(string(r.raw), "last admin") {
		t.Fatal(r.code, string(r.raw))
	}
	if d = e.s.Access.Load(); d.Users["u-impostor"].Role != access.RolePending || d.Users[admin.Sub].Role != access.RoleAdmin {
		t.Fatal("an account was changed", d.Users)
	}
	// unverified only: refused
	impostor.Email = model.P("someone-else@example.com")
	e.signIn(impostor)
	r = e.do("PUT", "/api/admin/users/"+url.PathEscape("someone-else@example.com"), map[string]any{"role": "admin", "sites": []string{}}, csrf...)
	if r.code != 409 || e.s.Access.Load().Users["u-impostor"].Role != access.RolePending {
		t.Fatal("a claimed address selected an account", r.code, string(r.raw))
	}
	// by subject, as the Users page does it: works
	r = e.do("PUT", "/api/admin/users/u-impostor", map[string]any{"role": "viewer", "sites": []string{}}, csrf...)
	if r.code != 200 || e.s.Access.Load().Users["u-impostor"].Role != access.RoleViewer {
		t.Fatal(r.code, string(r.raw))
	}
}
