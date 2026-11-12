package web

import (
	"regexp"
	"strings"
	"testing"
)

// The page's part of the machine's own check of approvals: the box of a machine with every field of the block
// (opened from the button in the host's header: approval_dialog_test.go), the flag for a broken machine on the
// Overview and the Updates page, the dialog's wording for an approval, and that every value from a machine goes
// through esc().
func TestUIApproval(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`function approvalBox(s) {`, `function approvalChip(s) {`, `approval broken`,
		`<div class="actions">${approvalBtn(s)}${liveLabel(s)}`,                                                   // the button in the header of the machine's page, also while it is off
		`${helperChip(s)}${approvalChip(s)}${guestCount(s)}`,                                                      // the Overview's host list
		`r.approval === "broken"`,                                                                                 // the Updates page
		`fairy-agent helper-trust show`,                                                                           // where a broken machine is repaired
		`on the machine, as administrator, <code>fairy-agent helper-trust</code> (show, set, clear)`,              // (r4-b1-08: said once; the lead has "Locked")                                                           // locked
		`will check this itself before it does anything`, `Pocket ID will ask for your passkey.`, `data-approval`, // the dialog
		`apprMany ? "will each check this themselves before they do anything" :`,
		`esc(a.issuer || "–")`, `esc(a.client_id || "–")`, `esc(k.kid)`, `esc(shortKey(k.thumbprint))`, `esc(a.error)`, `esc(a.agent_error)`,
		`esc(a.users ?? "–")`, `esc(a.fan_window_s)`, `esc(a.boot_shutdown_s)`, `esc(apprWho)`, `esc(appr.client_id || "–")`,
		// which machine it is (audit 5, r1-x1-01): one flag with its words on hover and click, the reasons behind
		// the info button, and for an admin one button
		`const AP_BINDING = table({`, `const AP_BINDING_MORE = table({`, `function approvalBinding(s, a) {`,
		"return infoBtn(`apflag-${s.id}`, name,", `{ cls: "hflag bad", icon: I.apKey, label:`,
		`data-act="ap-accept"`, `id: ["Accept this machine's new id", (name) =>`, `keep: ["This host keeps its machine id", (name) =>`, `off: ["Accept that its check is off", (name) =>`,
		// one id is accepted for one host only, and exactly the id the page shows (audit 5, r2-b2-02, r2-x1-01)
		// (audit 6, r4-b1-07: "check gone" has its button from the live data; it was `m?.can_accept && !held` alone)
		`const what = a.binding === "vanished" && m ? "off" : m?.can_accept ? m.accept : "";`, `const btn = what && !held ?`,
		`data-shown="${esc(what === "off" ? "" : m.shown_id || "")}"`, `const body = off ? { off: true } : { machine_id: t.dataset.shown || "" };`,
		`id="ap-todo">Give this machine a new id on the machine itself (<code>fairy-agent helper-trust set --new-machine-id</code>), then accept the new id here.`,
		`if (held) return ` + "`${held} keeps this machine id. No approvals for this host until it shows an id of its own.`;",
		"return [`<div class=\"warnbox ap-bind\" id=\"ap-bind\"><b>${esc(b[0])}.</b> ${esc(apBindingText(a))}${todo}${btn}</div>`, more];", // the reasons: the dialog's "More"
		`m = admin ? apvMachine(s) : null`,                                            // the record's details: admins only
		`if (apvStale(s, a) && Date.now() - apv.at > APV_AGAIN) fetchApproval(true);`, // a block that changed after the approval page was fetched (audit 6, r4-b1-01 and -07: once a second, it was every 3 s)
		`const apvStale = (s, a) => !!apv.data && (apv.sig[s.id] ?? "") !== apvSig(a);`, "const APV_AGAIN = 1000;",
		// the Updates page's list follows the live data too (r4-b1-01)
		`if (d && Date.now() - apv.at > APV_AGAIN && state.systems.some((s) => (apv.sig[s.id] ?? "") !== apvSig(approvalOf(s)))) fetchApproval(true);`,
		`${bind ? "" : '<p id="ap-lead">This machine does not check approvals itself.`, // not under "Check gone"
		`...(bind ? [] : ["The panel confirms these requests itself (Confirm it's you) and sends them with the panel's control token."])`,
		`a.last_known ? " · last known" : ""`,
		// the agent's changes of 2026-11-12: an old running helper is restarted, not started; how the trust was set
		`const AP_HELPER_OLD = "the running fairy-agent-helper is older than this agent";`, `<b>Restart the helper</b>: Stop helper, then Start helper`,
		`row("Trust set", `, `>other trust</span>`,
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
	for _, want := range []string{`<b>${esc(pending.label)}</b>${runs}${signs}${items}`, `Fairy will run`} {
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

// "Check gone" warns like "Machine id changed" (second review of passkey-g, G-09): behind the info button, when
// nobody took the check off, do not accept. "Accept that its check is off" on one host, then an accept on a
// host that shows that machine's id, is the way to hand a machine's id to another host with two accepts.
func TestUICheckGoneWarnsBeforeAccepting(t *testing.T) {
	js := appJS(t)
	const warn = `"If nobody did, what answers at this host's address may be another machine: do not accept, look at the host."`
	more := js[strings.Index(js, "const AP_BINDING_MORE = table({"):]
	more = more[:strings.Index(more, "});")]
	for _, kind := range []string{"changed", "vanished"} {
		i := strings.Index(more, "\n  "+kind+": [")
		if i < 0 {
			t.Fatalf("no reasons for %q", kind)
		}
		entry := more[i+1:]
		if j := strings.Index(entry, "],\n"); j >= 0 {
			entry = entry[:j]
		}
		if !strings.Contains(entry, warn) {
			t.Errorf("%s: the info button lacks the warning: %s", kind, entry)
		}
	}
	// the warning comes before what to do
	if !strings.Contains(more, warn+",\n    \"Turn the check on again, or accept that it is off.\"],") {
		t.Error("\"Check gone\": the warning must stand before \"Turn the check on again, or accept that it is off.\"")
	}
}
