package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// The rules of a user name that more than one account wants, and of texts that come from outside the panel.
// The accounts are the ones of usernames_test.go, and made up like them.

func shownAs(e *env, ck string) string {
	return fmt.Sprint(e.do("GET", "/api/me", nil, "Cookie", ck).body["shown_as"])
}

func usersPage(t *testing.T, r *namesRig) map[string]map[string]any {
	t.Helper()
	u := r.env.do("GET", "/api/admin/users", nil, "Cookie", r.ck["sub-n"])
	if u.code != 200 {
		t.Fatal(u.code, string(u.raw))
	}
	got := map[string]map[string]any{}
	for _, x := range u.body["users"].([]any) {
		m := x.(map[string]any)
		got[m["sub"].(string)] = m
	}
	return got
}

// Any account could take another account's shown name away, also one that only waits for access: by signing in
// with the same user name in other letters, or with a display name that is the other's user name. The admin was
// then "(address hidden)" everywhere, a record the admin wrote kept the address again, and the Users page said
// "no user name yet" for the admin's row.
//
// Now only user names compete with user names, and of two accounts with one user name the one with a role (and
// among equals the one that had it first) keeps the plain name. The other is shown by the name with a part of
// its id, in every place, and the Users page's answer says for both which account has the same user name.
func TestAnAccountCannotTakeAnothersUsernameAway(t *testing.T) {
	r := newNamesRig(t, true)
	e := r.env
	if got := shownAs(e, r.ck["sub-n"]); got != "Juniper" {
		t.Fatalf("the admin before: %q", got)
	}
	// somebody who waits for access signs in with the admin's user name in small letters ...
	wanda := e.cookie(model.User{Sub: "9a1b2c3d-wanda", Name: model.P("Waiting Wanda"), PreferredUsername: model.P("juniper"), Email: model.P("wanda@example.org"), EmailVerified: true}, 0)
	// ... another one with a display name that is a viewer's user name, and one with the second admin's user
	// name and a zero width space in it
	walt := e.cookie(model.User{Sub: "sub-w3", Name: model.P("viewer-all"), PreferredUsername: model.P("walt3"), Email: model.P("walt3@example.org"), EmailVerified: true}, 0)
	zero := e.cookie(userFromClaims(map[string]any{"sub": "7e7e7e7e-zero", "name": "Zero Widthson", "preferred_username": "Pine\u200bTop", "email": "zw@example.net", "email_verified": true}, "groups"), 0)
	for sub, want := range map[string]string{"sub-n": "Juniper", "sub-v": "viewer-all", "sub-s": "PineTop"} {
		if got := shownAs(e, r.ck[sub]); got != want {
			t.Errorf("%s is shown as %q, want %q: another account took its name away", sub, got, want)
		}
	}
	if got := shownAs(e, wanda); got != "juniper · id 9a1b2c" {
		t.Errorf("the account that waits and calls itself juniper is shown as %q", got)
	}
	if got := shownAs(e, walt); got != "walt3" {
		t.Errorf("the account with the display name viewer-all is shown as %q", got)
	}
	if got := shownAs(e, zero); got != "PineTop · id 7e7e7e" {
		t.Errorf("the account that calls itself PineTop with a zero width space is shown as %q", got)
	}
	// the Users page says what is true of each row
	got := usersPage(t, r)
	for _, c := range []struct {
		sub, username, shown, note string
		named                      bool
		same                       []any
	}{
		{"sub-n", "Juniper", "Juniper", access.UsernameShared, true, []any{"9a1b2c3d-wanda"}},
		{"9a1b2c3d-wanda", "juniper", "juniper · id 9a1b2c", access.UsernameShared, true, []any{"sub-n"}},
		{"sub-s", "PineTop", "PineTop", access.UsernameShared, true, []any{"7e7e7e7e-zero"}},
		{"7e7e7e7e-zero", "PineTop", "PineTop · id 7e7e7e", access.UsernameShared, true, []any{"sub-s"}},
		{"sub-v", "viewer-all", "viewer-all", "", true, []any{}},
		{"sub-w3", "walt3", "walt3", "", true, []any{}},
		{"sub-o", "", "(address hidden)", "", false, []any{}},
	} {
		u := got[c.sub]
		username, _ := u["username"].(string)
		if u == nil || username != c.username || u["shown_as"] != c.shown || u["username_note"] != c.note || u["named"] != c.named || fmt.Sprint(u["same_username"]) != fmt.Sprint(c.same) {
			t.Errorf("the Users page, %s: username %v, shown_as %v, named %v, username_note %v, same_username %v", c.sub, u["username"], u["shown_as"], u["named"], u["username_note"], u["same_username"])
		}
	}
	// a record the admin writes while this lasts keeps the user name, not the address
	if p := e.do("PUT", "/api/admin/hosts/laptop/options", map[string]any{"name": "Den laptop 3"}, r.hdr("sub-n")...); p.code != 200 {
		t.Fatalf("Edit host: %d %s", p.code, p.raw)
	}
	if by := e.s.Poller.Admin.HostOptions().Hosts["laptop"].By; by != "Juniper" {
		t.Errorf("state.json after the admin changed a name while another account has the user name: By=%q", by)
	}
	// every place names the account that waits by the name with its id: its own audit entries (with its account
	// id, as always), the list of everyone's sessions, and the answer of a change on the Users page
	page := e.do("GET", "/api/audit?limit=200&q=9a1b2c3d-wanda", nil, "Cookie", r.ck["sub-n"])
	entries, _ := page.body["entries"].([]any)
	if len(entries) == 0 {
		t.Fatal("no audit entry of the account that waits")
	}
	for _, x := range entries {
		if en := x.(map[string]any); en["who"] != "juniper · id 9a1b2c" || en["sub"] != "9a1b2c3d-wanda" || en["username"] != "juniper · id 9a1b2c" {
			t.Errorf("an audit entry of the account that waits: who %v, sub %v, username %v", en["who"], en["sub"], en["username"])
		}
	}
	if n := len(e.do("GET", "/api/audit?limit=200&q=juniper", nil, "Cookie", r.ck["sub-n"]).body["entries"].([]any)); n <= len(entries) {
		t.Errorf("a search for the user name finds %d entries, the account that waits alone has %d", n, len(entries))
	}
	all := e.do("GET", "/api/admin/sessions", nil, "Cookie", r.ck["sub-n"])
	if !strings.Contains(string(all.raw), `{"sub":"9a1b2c3d-wanda","username":"juniper · id 9a1b2c","you":false}`) || !strings.Contains(string(all.raw), `{"sub":"sub-n","username":"Juniper","you":true}`) {
		t.Errorf("everyone's sessions: %s", clipText(string(all.raw), 1500))
	}
	if p := e.do("PUT", "/api/admin/users/9a1b2c3d-wanda", map[string]any{"role": "viewer", "sites": []string{"Bob"}}, r.hdr("sub-n")...); p.code != 200 ||
		p.body["message"] != "juniper · id 9a1b2c is now viewer of Bob" {
		t.Errorf("the answer of a change of the account that waits: %d %s", p.code, p.raw)
	}
	// with a role the account still does not get the plain name: the admin has had it longer
	if a, w := shownAs(e, r.ck["sub-n"]), shownAs(e, wanda); a != "Juniper" || w != "juniper · id 9a1b2c" {
		t.Errorf("after the account got a role: the admin %q, the other %q", a, w)
	}
	// nobody is told an address or a name for any of it
	for _, ck := range []string{r.ck["sub-l"], wanda, walt} {
		if held := namesHeld(string(e.do("GET", "/api/overview", nil, "Cookie", ck).raw)); len(held) > 0 {
			t.Errorf("the overview holds %q", held)
		}
	}
}
