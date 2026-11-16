package web

import (
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// The list of what the Audit page draws as dots (`private`: the address and the name of every account) is for
// the page, so for an admin in a browser. An API token has no dots to draw: a control token reads the entries
// as it always did and gets an empty list. It was handed the address and the name of every account, also of an
// account that only waits for access and has no entry in the log, while the Users page is closed to tokens.
func TestAuditPrivateListIsForAnAdminInABrowser(t *testing.T) {
	r := newNamesRig(t, true)
	e := r.env
	control, _, err := e.s.Tokens.Create("ci", "sub-n", "control", "cli:test", 0)
	if err != nil {
		t.Fatal(err)
	}
	// an account that waits for access and never did anything
	e.cookie(model.User{Sub: "sub-w", Name: model.P("Waiting Wanda"), PreferredUsername: model.P("wanda"), Email: model.P("wanda@example.org"), EmailVerified: true}, 0)
	x := e.do("GET", "/api/audit?limit=1&action=power.shutdown", nil, "Authorization", "Bearer "+control)
	if x.code != 200 || len(x.body["entries"].([]any)) != 1 {
		t.Fatalf("a control token reads the audit log: %d %s", x.code, x.raw)
	}
	if private, ok := x.body["private"].([]any); !ok || len(private) != 0 {
		t.Errorf("a control token is handed `private`: %v", x.body["private"])
	}
	for _, p := range []string{"Waiting Wanda", "wanda@example.org", "Real Name", "Other Person", "friend@example.com", "old@example.com"} {
		if strings.Contains(string(x.raw), p) {
			t.Errorf("a control token's answer of the audit log holds %q (an account that has no part in the entry)", p)
		}
	}
	// the admin in a browser gets the list, with the account that only waits
	y := e.do("GET", "/api/audit?limit=1&action=power.shutdown", nil, "Cookie", r.ck["sub-n"])
	private, _ := y.body["private"].([]any)
	has := map[string]bool{}
	for _, p := range private {
		has[p.(string)] = true
	}
	if y.code != 200 || !has["Waiting Wanda"] || !has["wanda@example.org"] || !has["Real Name"] || !has["owner@example.com"] {
		t.Errorf("the admin's list of what the page draws as dots: %d %v", y.code, private)
	}
}

// Every other answer that carries addresses or names, asked with every kind of API token: the Users page and
// both lists of sessions are closed to a token whatever its scope, the audit log to a read token, and what a
// token is told about itself has its own name and its owner's id, no address and no name of a person.
func TestNoTokenIsHandedAddressesOrNames(t *testing.T) {
	r := newNamesRig(t, true)
	e := r.env
	tokens := map[string]string{}
	var err error
	if tokens["a control token"], _, err = e.s.Tokens.Create("ci", "sub-n", "control", "cli:test", 0); err != nil {
		t.Fatal(err)
	}
	if tokens["a read token"], _, err = e.s.Tokens.Create("reader", "sub-n", "read", "cli:test", 0); err != nil {
		t.Fatal(err)
	}
	if tokens["a read token of one site"], _, err = e.s.Tokens.CreateForSites("bob-reader", "sub-n", "read", "cli:test", 0, []string{"Bob"}); err != nil {
		t.Fatal(err)
	}
	if tokens["a control token of the system"], _, err = e.s.Tokens.Create("deploy", "system", "control", "cli:test", 0); err != nil {
		t.Fatal(err)
	}
	for what, tok := range tokens {
		hdr := []string{"Authorization", "Bearer " + tok}
		for _, path := range []string{"/api/admin/users", "/api/admin/sessions", "/api/me/sessions"} {
			if x := e.do("GET", path, nil, hdr...); x.code != 403 || len(namesHeld(string(x.raw))) > 0 {
				t.Errorf("%s: GET %s: %d %s", what, path, x.code, clipText(string(x.raw), 300))
			}
		}
		audit := e.do("GET", "/api/audit?limit=200", nil, hdr...)
		switch {
		case strings.Contains(what, "control"):
			if private, ok := audit.body["private"].([]any); audit.code != 200 || !ok || len(private) != 0 {
				t.Errorf("%s: the audit log: %d, private %v", what, audit.code, audit.body["private"])
			}
		case audit.code != 403 || len(namesHeld(string(audit.raw))) > 0:
			t.Errorf("%s: the audit log: %d %s", what, audit.code, clipText(string(audit.raw), 300))
		}
		me := e.do("GET", "/api/me", nil, hdr...)
		if me.code != 200 || me.body["email"] != nil || me.body["preferred_username"] != nil || len(namesHeld(string(me.raw))) > 0 {
			t.Errorf("%s: /api/me: %d %s", what, me.code, clipText(string(me.raw), 400))
		}
		if name, _ := me.body["name"].(string); name != me.body["token_name"] {
			t.Errorf("%s: /api/me names the token %q, token_name %v", what, name, me.body["token_name"])
		}
	}
}
