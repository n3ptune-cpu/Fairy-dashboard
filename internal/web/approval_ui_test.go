package web

import (
	"regexp"
	"strings"
	"testing"
)

// The page's part of the machine's own check of approvals: the box on a machine's page with every field of
// the block, the flag for a broken machine on the Overview and the Updates page, the dialog's wording for an
// approval, and that every value from a machine goes through esc().
func TestUIApproval(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`function approvalBox(s) {`, `function approvalChip(s) {`, `approval broken`,
		`${approvalBox(s)}${managedBox(s)}`,                                                                       // on the machine's page, also while it is off
		`${helperChip(s)}${approvalChip(s)}${guestCount(s)}`,                                                      // the Overview's host list
		`r.approval === "broken"`,                                                                                 // the Updates page
		`fairy-agent helper-trust show`,                                                                           // where a broken machine is repaired
		`changed only on the machine, as administrator`,                                                           // locked
		`will check this itself before it does anything`, `Pocket ID will ask you to sign in to`, `data-approval`, // the dialog
		`esc(a.issuer || "–")`, `esc(a.client_id || "–")`, `esc(k.kid)`, `esc(shortKey(k.thumbprint))`, `esc(a.error)`, `esc(a.agent_error)`,
		`esc(a.users ?? "–")`, `esc(a.fan_window_s)`, `esc(a.boot_shutdown_s)`, `esc(apprWho)`, `esc(appr.client || "Fairy PC actions")`,
		// which machine it is (audit 5, r1-x1-01): one flag with its words on hover and click, the reasons behind
		// the info button, and for an admin one button
		`const AP_BINDING = table({`, `const AP_BINDING_MORE = table({`, `function approvalBinding(s, a) {`,
		"return infoBtn(`apflag-${s.id}`, name,", `{ cls: "hflag bad", icon: I.apKey, label:`,
		`data-act="ap-accept"`, `"Accept that its check is off" : "Accept this machine's new id"`,
		"infoBtn(`apbind-${s.id}`, b[0], infoList(more))", `m = admin ? apvMachine(s) : null`, // the record's details: admins only
		`if (apv.sig[s.id] !== apvSig(a) && Date.now() - apv.at > 3000) fetchApproval(true);`, // a block that changed after the approval page was fetched
		`${bind ? "" : '<div class="note">This machine does not check approvals itself.`,      // not under "Check gone"
		`a.last_known ? " · last known" : ""`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	// nothing of the block is put into the page without esc(): every ${a.…} in the approval code is wrapped
	box := js[strings.Index(js, "function approvalBox(s) {"):strings.Index(js, "// ---- Turn on (admins)")]
	for _, m := range regexp.MustCompile(`\$\{a\.[a-z_]+[^}]*\}`).FindAllString(box, -1) {
		if !strings.Contains(m, "?") && !strings.Contains(m, "esc(") { // a ?: that picks between fixed texts is fine
			t.Errorf("the approval box inserts a value of the machine without esc(): %s", m)
		}
	}
	// the dialog keeps the kept action's own description
	for _, want := range []string{`<b>${esc(pending.label)}</b>${runs}${items}`, `Fairy will run`} {
		if !strings.Contains(js, want) {
			t.Errorf("the dialog lost %q", want)
		}
	}
}

// /api/me says whether the panel has the approval client, and the Updates page's rows carry the machine's state.
func TestApprovalInMeAndUpdates(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop)
	me := a.do("GET", "/api/me", nil, "Cookie", a.admin)
	if me.body["approval_client"] != true {
		t.Fatalf("/api/me: %s", me.raw)
	}
	rows := a.do("GET", "/api/admin/updates", nil, "Cookie", a.admin).body["agents"].([]any)
	got := map[string]any{}
	for _, r := range rows {
		got[r.(map[string]any)["id"].(string)] = r.(map[string]any)["approval"]
	}
	if got["laptop"] != "required" || got["tower"] != "off" {
		t.Fatalf("the Updates page's rows: %v", got)
	}
	e := setup(t) // a panel without approval:, and agents without the block
	me = e.do("GET", "/api/me", nil, "Cookie", e.cookie(controller(), 0))
	if me.body["approval_client"] != false {
		t.Fatalf("/api/me without approval: %s", me.raw)
	}
	if x := e.do("GET", "/api/systems/laptop", nil, "Cookie", e.cookie(controller(), 0)); x.body["approval"] != nil {
		t.Fatalf("an agent without the block: %s", x.raw)
	}
}
