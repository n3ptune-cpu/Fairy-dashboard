package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// The accounts are the ones of usernames_test.go, and made up like them.

// A user name written like a text of the panel's own (the mark with the admin's id, the text of the admin's
// token, "root") is shown with the start of its own account id in every place, and the Users page says why.
func TestAUsernameWithThePanelsMarkThroughThePanel(t *testing.T) {
	r := newNamesRig(t, true)
	e := r.env
	if _, _, err := e.s.Tokens.Create("ci", "sub-n", "control", "cli:test", 0); err != nil {
		t.Fatal(err)
	}
	x := e.cookie(model.User{Sub: "7e7e7e7e-x", Name: model.P("X"), PreferredUsername: model.P("Juniper · id sub-n"), Email: model.P("x@example.org"), EmailVerified: true}, 0)
	y := e.cookie(model.User{Sub: "9a1b2c3d-y", Name: model.P("Y"), PreferredUsername: model.P("ci (token, owner Juniper)"), Email: model.P("y@example.org"), EmailVerified: true}, 0)
	z := e.cookie(model.User{Sub: "5d5d5d5d-z", Name: model.P("Z"), PreferredUsername: model.P("Root"), Email: model.P("z@example.org"), EmailVerified: true}, 0)
	for sub, role := range map[string]string{"9a1b2c3d-y": "viewer", "5d5d5d5d-z": "admin"} { // (x only waits)
		if _, _, err := e.s.Access.SetRole(sub, role, []string{"Bob"}, "test", true); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]string{"7e7e7e7e-x": "Juniper · id sub-n · id 7e7e7e", "9a1b2c3d-y": "ci (token, owner Juniper) · id 9a1b2c", "5d5d5d5d-z": "Root · id 5d5d5d"}
	for ck, sub := range map[string]string{x: "7e7e7e7e-x", y: "9a1b2c3d-y", z: "5d5d5d5d-z"} {
		if got := shownAs(e, ck); got != want[sub] {
			t.Errorf("%s is shown as %q, want %q", sub, got, want[sub])
		}
	}
	if got := shownAs(e, r.ck["sub-n"]); got != "Juniper" {
		t.Errorf("the admin is shown as %q", got)
	}
	// the Users page: the user name as the account gave it, what it is shown by, and why
	page := usersPage(t, r)
	for sub, username := range map[string]string{"7e7e7e7e-x": "Juniper · id sub-n", "9a1b2c3d-y": "ci (token, owner Juniper)", "5d5d5d5d-z": "Root"} {
		u := page[sub]
		if u == nil || u["username"] != username || u["shown_as"] != want[sub] || u["named"] != true || u["username_note"] != access.UsernameMarked || fmt.Sprint(u["same_username"]) != "[]" {
			t.Errorf("the Users page, %s: username %v, shown_as %v, named %v, username_note %v, same_username %v", sub, u["username"], u["shown_as"], u["named"], u["username_note"], u["same_username"])
		}
	}
	if u := page["sub-n"]; u["shown_as"] != "Juniper" || u["username_note"] != "" {
		t.Errorf("the Users page, the admin: %v", u)
	}
	// a record the viewer writes keeps that text, and reads as the viewer's; the admin's token "ci" reads as
	// the admin's token, and the two are different texts
	who := e.s.people()
	rec := e.s.who(&model.User{Sub: "9a1b2c3d-y", Auth: "session"})
	token := who.Shown("ci (token, owner sub-n)")
	if rec != want["9a1b2c3d-y"] || who.Shown(rec) != rec || token != "ci (token, owner Juniper)" || token == rec {
		t.Errorf("a record of the viewer keeps %q and reads %q; the admin's token reads %q", rec, who.Shown(rec), token)
	}
	// everyone's sessions name each account by what it is shown by
	all := e.do("GET", "/api/admin/sessions", nil, "Cookie", r.ck["sub-n"])
	seen := map[string]bool{}
	for _, s := range all.body["sessions"].([]any) {
		us, _ := s.(map[string]any)["user"].(map[string]any)
		if w, ok := want[fmt.Sprint(us["sub"])]; ok {
			seen[fmt.Sprint(us["sub"])] = true
			if us["username"] != w {
				t.Errorf("everyone's sessions: %v, want the user name %q", us, w)
			}
		}
	}
	if len(seen) != 3 {
		t.Errorf("everyone's sessions have %d of the three accounts", len(seen))
	}
	// the page: a row whose user name is shown with the id for this reason says so in a tooltip
	js := appJS(t)
	if !strings.Contains(js, `panel_mark: "`) || !strings.Contains(js, "USERNAME_WITH_ID[u.username_note]") {
		t.Error("app.js has no words for a user name that is written like a text of the panel's own")
	}
}

// The Users page's row for such an account: the heading is what the panel shows the account by, with a tooltip
// that says why the id is there. A row of an account whose user name is its own plain text has no tooltip.
func TestUIUsersARowWithThePanelsMark(t *testing.T) {
	js := appJS(t)
	from := strings.Index(js, "const ROLE_LABEL = table(")
	to := strings.Index(js, "async function adminCall(")
	if from < 0 || to < from {
		t.Fatal("app.js has no Users page any more")
	}
	runPageScript(t, "users-marks", namesTools+jsLines(t, js, "dict", "table", "esc", "meShown", "demoViewLabel")+veilJS(t, js)+`
const admin = { data: null, error: null }, browserOf = () => "", fmtSpan = () => "";
`+js[from:to]+`
state.me = { can_manage_users: true, role: "admin" };
const user = (o) => ({ role: "viewer", sites: [], last_seen: "2026-11-16T10:00:00Z", via: "first sign-in", you: false, email_verified: true, same_username: [], ...o });
admin.data = { roles: ["admin", "viewer", "pending"], sites: [], targets: [], users: [
  user({ sub: "sub-n", username: "Juniper", shown_as: "Juniper", named: true, username_note: "", email: "owner@example.com", name: "Real Name", role: "admin", you: true }),
  user({ sub: "9a1b2c3d-y", username: "ci (token, owner Juniper)", shown_as: "ci (token, owner Juniper) · id 9a1b2c", named: true, username_note: "panel_mark", email: "y@example.org", name: "Y" }),
  user({ sub: "7e7e7e7e-x", username: "juniper", shown_as: "juniper · id 7e7e7e", named: true, same_username: ["sub-n"], username_note: "same_username", email: "x@example.org", name: "X", role: "pending" }),
] };
const html = pageUsers();
const rows = html.split('<tr class="row" data-key="u-').slice(1, 4).map((r) => r.split("</td>")[0]);
if (!rows[0].includes("<b>Juniper</b>")) fail("the admin's row: " + rows[0]);
if (!rows[1].includes('<b title="This account&#39;s user name is written like a text of the panel&#39;s own') && !rows[1].includes("<b title=\"This account's user name is written like a text of the panel's own")) fail("the row of a user name with the panel's mark: " + rows[1]);
if (!rows[1].includes(">ci (token, owner Juniper) · id 9a1b2c</b>") || rows[1].includes("user name hidden") || rows[1].includes("same user name")) fail("the row of a user name with the panel's mark: " + rows[1]);
if (words(rows[1].slice(rows[1].indexOf(">") + 1)) !== "ci (token, owner Juniper) · id 9a1b2c name •••••• e-mail •••••• id 9a1b2c3d-y") fail("what the row reads: " + words(rows[1].slice(rows[1].indexOf(">") + 1)));
if (!rows[2].includes("<b>juniper · id 7e7e7e</b>") || !rows[2].includes("same user name as id sub-n")) fail("the row of a second account with a user name: " + rows[2]);
if (!process.exitCode) console.log("ok");
`)
}
