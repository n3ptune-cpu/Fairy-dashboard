package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The machine's own check of approvals is no box on the host's page any more. A button in the host's header
// ("Approval", left of "live") opens it as a dialog; its colour is the state, and so are its tooltip and its name
// for a screen reader. The dialog is drawn from the live data like the page, and never touches another dialog
// that took its place ("Turn on", "Confirm it's you").
func TestUIApprovalButtonAndDialog(t *testing.T) {
	js, css := appJS(t), appCSS(t)
	for _, want := range []string{
		`<div class="actions">${approvalBtn(s)}${liveLabel(s)}`, // the first control of the header's row, left of "live"
		"return `<button class=\"btn ap-btn ${v.tone}\" type=\"button\" data-act=\"ap-dlg\" aria-haspopup=\"dialog\" title=\"${esc(`${AP_LABEL}: ${v.word}`)}\" aria-label=\"${esc(`${AP_LABEL}: ${v.word}`)}\">${I.shield}${esc(AP_LABEL)}</button>`;",
		`tone: bind || a.state === "broken" ? "bad" : cls === "ok" && !amber ? "ok" : "warn",`,
		`role="dialog" aria-modal="true" aria-labelledby="apd-t" id="ap-dlg" data-sys="${esc(id)}"`,
		`<button class="btn" type="button" id="apd-more" aria-expanded="false" aria-controls="ap-more">More</button><button class="btn" type="button" id="apd-x">Close</button>`,
		"<h3><span id=\"apd-t\">${esc(AP_LABEL)}</span> ${v.chip}</h3>${v.html}",
		`<div class="ap-more" id="ap-more"${apDlg.more ? "" : " hidden"}>`,
		// live: every render() draws the open dialog anew, and only the element with this dialog's id
		`renderNav(); renderAlerts(); approvalDialogSync();`,
		"const box = $(\"#ap-dlg\");\n  if (!box || $(\"#modal\").hidden) return;",
		`morph($("#ap-dlg-body"), html);`,
		`if (!s || !approvalOf(s) || stepUpOpen) return;`, // never over "Confirm it's you"
		`setTimeout(() => $("#apd-x")?.focus(), 0);`,      // the focus starts on Close, not on the first control (which can be an accept)
		// Turn on, pressed in the dialog: Cancel leads back to it
		`openTurnOn(t.dataset.id || "", t.closest("#ap-dlg")?.dataset.sys || "");`, `$("#ap-x").onclick = back ? () => openApproval(back) : closeModal;`,
		// next to Turn on: "Ready", or the reason in a few words; the whole sentence is behind "More"
		`<span class="note" id="ap-on-note">${m.ready ? "Ready" : esc(` + "`Not ready: ${apShortWhy(m.why) || \"no reason given\"}`" + `)}</span>`,
		`[m.ready ? "After it is switched on, it is changed only on the machine itself." : ` + "`It cannot be switched on right now: ${esc(m.why || \"the panel gives no reason\")}.`]",
		// what stood in the box and is behind "More" now, word for word
		`verifies a Pocket ID approval (passkey) made for exactly that action. The panel cannot skip it.`,
		`"Locked: all of this is changed only on the machine, as administrator, with <code>fairy-agent helper-trust</code> (show, set, clear). The panel cannot change or remove it."`,
		`"If nobody did, what answers at this host's address may be another machine: do not accept, look at the host."`,
		"compare the id with the machine's own: on the machine <code>fairy-agent helper-status</code>, line <code>machine id:</code>.",
		`It checked approvals itself when Fairy last heard from it: that is why Fairy sends it nothing on its own confirmation and it is in no run.`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	// the name is in one place
	if n := strings.Count(js, `const AP_LABEL = "Approval";`); n != 1 {
		t.Errorf("want one AP_LABEL, found %d", n)
	}
	for _, gone := range []string{
		`${approvalBox(s)}`, `data-key="approval"`, `<h3>Approval `, // no box on the page
		">Approval</button>", ">Approval settings<", `aria-label="Approval`, // the name is never written out next to the constant
		"infoBtn(`apbind-", // the reasons of an alarm are behind "More": one way, not two
	} {
		if strings.Contains(js, gone) {
			t.Errorf("app.js still has %q", gone)
		}
	}
	// the info button of the row "Client ID" stays
	if !strings.Contains(js, "${row(`Client ID${clientIdInfo(`apbox-client-${s.id}`)}`, `<code>${esc(a.client_id || \"–\")}</code>`)}") {
		t.Error("the row Client ID lost its info button")
	}
	// the colours are the stylesheet's own: the chips' green and amber, the red of .errcard
	for _, want := range []string{
		".btn.ap-btn.ok { color: var(--ok); border-color: rgba(61, 220, 151, .25); }",
		".btn.ap-btn.warn { color: var(--warn); border-color: rgba(245, 184, 75, .25); }",
		".btn.ap-btn.bad { color: var(--bad); border-color: rgba(255, 93, 108, .6); }",
		".btn.ap-btn svg", "{ width: 16px; height: 16px; }", // 16 px like every icon in a button: the button is 34 px high
	} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css lacks %q", want)
		}
	}
	at := strings.Index(css, "/* The machine's own check, opened from the host's header.")
	if at < 0 {
		t.Fatal("app.css has no block for the header's button")
	}
	for _, c := range regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|rgba?\([^)]*\)|var\(--[a-z0-9-]+\)`).FindAllString(css[at:], -1) {
		if !strings.Contains(css[:at], c) {
			t.Errorf("the block of the header's button brings a colour the stylesheet did not have: %s", c)
		}
	}
}

// The states the check can have, drawn by the page's own functions (run in node), for an admin and for a viewer
// who is none: the colour of the header's button and its words, what the dialog shows, what is behind "More",
// and that a viewer's dialog holds nothing of the admin's part (the machine id on record, the id a machine
// shows, the buttons, why it cannot be switched on, "old key", "other trust").
func TestUIApprovalDialogInEveryState(t *testing.T) {
	js := appJS(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the page's own functions are not run here")
	}
	var parts []string
	for _, re := range []string{`(?m)^const dict = .*$`, `(?m)^const table = .*$`, `(?m)^const esc = .*$`, `(?m)^const enc = .*$`, `(?m)^const andList = .*$`} {
		m := regexp.MustCompile(re).FindString(js)
		if m == "" {
			t.Fatalf("app.js has no %s any more", re)
		}
		parts = append(parts, m)
	}
	from, to := strings.Index(js, "// ---- the machine's own check of approvals"), strings.Index(js, "const statusText = (s) =>")
	if from < 0 || to < from {
		t.Fatal("app.js has no approval part any more")
	}
	script := `"use strict";
// what the approval part of the page needs around it: nothing here is the code under test
const document = { addEventListener() {} };
const I = { shield: "<svg data-i=shield></svg>", apKey: "<svg></svg>" };
const state = { me: null, systems: [], byId: {} };
const asked = [];
const api = async (path) => { asked.push(path); return { machines: [] }; };
const render = () => {}, ago = () => "1m ago", controlWhyNot = () => "", infoBtn = (key) => "[info " + key + "]", infoList = (x) => x.join("|");
` + strings.Join(parts, "\n") + "\n" + js[from:to] + `
const fail = (m) => { console.log("FAIL " + m); process.exitCode = 1; };
const MID = "MID-on-record-AAAAAAAAAAAAAAAAAAAAAAAA", SHOWN = "SHOWN-by-the-machine-BBBBBBBBBBBBBBBBBB";
const base = { state: "required", enforced_by: "helper", issuer: "https://id.example", client_id: "pc-actions", keys: [{ kid: "k1", alg: "ES256", thumbprint: "T".repeat(43) }], users: 2, fan_window_s: 30, boot_shutdown_s: 0, set_by: "panel", set_at: "2026-11-12T10:00:00Z", proto: 2 };
const host = (a) => ({ id: "h1", name: "ALPHA", approval: a });
// the approval page's data (admins): it is there in both passes, so that a viewer's dialog is shown not to read it
const page = (m) => { apv.data = { machines: [{ id: "h1", system_id: "h1", ...m }] }; apv.at = Date.now(); };
const as = (admin) => { state.me = { can_control: admin, approval_client: true }; };
const words = (t) => t.replace(/<[^>]+>/g, " ").replace(/&#39;/g, "'").replace(/\s+/g, " ").trim();
const cases = [
  // name, block, page data, admin: [tone, word], viewer: [tone, word], what the admin's dialog must hold, what its "More" must hold
  ["required", base, { machine_id: MID, shown_id: MID, pinned: "turn-on" }, ["ok", "required"], ["ok", "required"],
    ["This machine checks a passkey approval itself before a fan, power or update action; the panel cannot skip it.", "Locked: it is changed only on the machine itself.", "Machine id", "Pinned key", "Allowed users"],
    ["verifies a Pocket ID approval (passkey) made for exactly that action. The panel cannot skip it.", "Locked: all of this is changed only on the machine, as administrator, with fairy-agent helper-trust (show, set, clear). The panel cannot change or remove it."]],
  ["required, a key Pocket ID no longer publishes", base, { machine_id: MID, shown_id: MID, stale_keys: true }, ["warn", "required · old key"], ["ok", "required"],
    ["fairy-agent helper-trust set --fetch-keys"], []],
  ["required, not this panel's values", base, { machine_id: MID, shown_id: MID, other_trust: true }, ["warn", "required · other trust"], ["ok", "required"],
    ["not this panel's values"], []],
  ["required, an agent's own check that cannot be used", { ...base, agent_error: "no trust <file>" }, { machine_id: MID, shown_id: MID }, ["warn", "required"], ["warn", "required"],
    ["The agent's own check cannot be used: no trust &lt;file&gt;"], []],
  ["required, an older approval scheme", { ...base, proto: 1 }, { machine_id: MID, shown_id: MID }, ["warn", "required"], ["warn", "required"],
    ["Agent too old: its approvals do not name the machine. Install the current agent on it."], []],
  ["required, a newer approval scheme", { ...base, proto: 3 }, { machine_id: MID, shown_id: MID }, ["warn", "required"], ["warn", "required"],
    ["This machine uses version 3 of the approval scheme; this panel knows version 2. Update the panel."], []],
  ["required, silent since the panel started", { ...base, last_known: true }, { machine_id: MID }, ["ok", "required · last known"], ["ok", "required · last known"],
    ["No answer since the panel started. Fairy sends it nothing on its own confirmation, and it is in no run."], ["It checked approvals itself when Fairy last heard from it: that is why Fairy sends it nothing on its own confirmation and it is in no run."]],
  ["off, ready", { state: "off", enforced_by: "agent", proto: 2 }, { ready: true }, ["warn", "off"], ["warn", "off"],
    ["This machine does not check approvals itself. Fan, power and update requests are confirmed in the panel only.", "Would be checked by", "Turn on", ">Ready<"],
    ["The panel confirms these requests itself (Confirm it's you) and sends them with the panel's control token.", "After it is switched on, it is changed only on the machine itself."]],
  ["off, not ready", { state: "off", enforced_by: "agent", proto: 2 }, { ready: false, why: "the panel has no control token for it" }, ["warn", "off"], ["warn", "off"],
    ["Not ready: the panel has no control token for it<", "disabled"], ["It cannot be switched on right now: the panel has no control token for it."]],
  ["off, not ready, a long reason", { state: "off", enforced_by: "agent", proto: 2 }, { ready: false, why: "nobody would be allowed to approve on it: set approval.users in config.yaml" }, ["warn", "off"], ["warn", "off"],
    ["Not ready: nobody would be allowed to approve on it<"], ["It cannot be switched on right now: nobody would be allowed to approve on it: set approval.users in config.yaml."]],
  ["unknown, the helper does not answer", { state: "unknown", enforced_by: "helper", proto: 2 }, {}, ["warn", "unknown"], ["warn", "unknown"],
    ["The machine cannot say whether it checks approvals. Start the helper (Elevated helper, next to the host name)."], ["The machine cannot say whether it checks approvals (its elevated helper is not answering). Start the helper (Elevated helper, next to the host name)."]],
  ["unknown, an old helper is running", { state: "unknown", enforced_by: "helper", proto: 2, error: "the running fairy-agent-helper is older than this agent (it knows no approvals)" }, {}, ["warn", "unknown"], ["warn", "unknown"],
    ["Its running helper is older than its agent: it refuses fan, power and update requests. Restart the helper : Stop helper, then Start helper."], ["so nothing there can check an approval: it refuses fan, power and update requests."]],
  ["some state this page does not know", { state: "later", enforced_by: "helper", proto: 2 }, {}, ["warn", "unknown"], ["warn", "unknown"],
    ["Checked by"], []],
  ["broken", { state: "broken", enforced_by: "helper", proto: 2, error: "trust file: <bad>" }, {}, ["bad", "broken"], ["bad", "broken"],
    ["Its trust file cannot be used. It refuses fan, power and update requests until that is repaired on the machine : fairy-agent helper-trust show"],
    ["This machine has a trust file it cannot use: trust file: &lt;bad&gt; . Until it is repaired on the machine it refuses fan, power and update requests. On the machine, as administrator: fairy-agent helper-trust show"]],
  ["machine id changed", { ...base, binding: "changed" }, { machine_id: MID, shown_id: SHOWN, pinned: "turn-on", pinned_by: "nora", can_accept: true, accept: "id" }, ["bad", "required · Machine id changed"], ["bad", "required · Machine id changed"],
    ["Machine id changed. It shows another machine id than the one Fairy has on record. No approvals until an admin accepts the new id.", "Accept this machine's new id", "Machine id on record", "Machine id it shows"],
    ["If nobody did, what answers at this host's address may be another machine: do not accept, look at the host.", "On record:", "It shows:"]],
  ["one machine id on two hosts", { ...base, binding: "duplicate", binding_with: ["BETA"] }, { machine_id: MID, shown_id: MID, with: ["BETA"], can_accept: true, accept: "keep" }, ["bad", "required · One machine id on two hosts"], ["bad", "required · One machine id on two hosts"],
    ["One machine id on two hosts. BETA has the same machine id: each would accept an approval made for the other. No approvals for either.", "This host keeps its machine id"],
    ["Before you press, compare the id with the machine's own: on the machine fairy-agent helper-status , line machine id: .", "The same id: BETA."]],
  ["one machine id on two hosts, the other keeps it", { ...base, binding: "duplicate", binding_with: ["BETA"], binding_held: ["BETA"] }, { shown_id: SHOWN, with: ["BETA"], can_accept: true, accept: "first" }, ["bad", "required · One machine id on two hosts"], ["bad", "required · One machine id on two hosts"],
    ["BETA keeps this machine id. No approvals for this host until it shows an id of its own.", "Give this machine a new id on the machine itself ( fairy-agent helper-trust set --new-machine-id ), then accept the new id here."], []],
  ["check gone, it shows off", { state: "off", enforced_by: "agent", proto: 2, binding: "vanished" }, { machine_id: MID, can_accept: true, accept: "off", ready: false, why: "its check is gone: accept that first" }, ["bad", "off · Check gone"], ["bad", "off · Check gone"],
    ["Check gone. It had the check on the machine itself and shows none now. Fairy sends it no fan, power or update request.", "Accept that its check is off", "Would be checked by"],
    ["Every request would then run with the panel's control token alone.", "If nobody did, what answers at this host's address may be another machine: do not accept, look at the host.", "Turn the check on again, or accept that it is off."]],
  ["check gone, the block still says required", { ...base, binding: "vanished" }, { machine_id: MID, can_accept: true, accept: "off" }, ["bad", "Check gone"], ["bad", "Check gone"],
    ["Check gone.", "Accept that its check is off"], ["If nobody did, what answers at this host's address may be another machine: do not accept, look at the host."]],
  ["agent too old for the machine id", { ...base, proto: 1, binding: "too_old" }, {}, ["bad", "required · Agent too old"], ["bad", "required · Agent too old"],
    ["Agent too old. Its approvals do not name the machine. Install the current agent on it."], ["It runs the test build of 2026-11-11. Its approvals are not tied to one machine, and it refuses this panel's."]],
  ["not on record", { ...base, set_by: "machine", binding: "no_record" }, { shown_id: SHOWN, can_accept: true, accept: "first" }, ["bad", "required · Not on record"], ["bad", "required · Not on record"],
    ["Not on record. Fairy has no machine id on record for it. No approvals until an admin accepts its id.", "Accept this machine's id", "Machine id it shows"],
    ["Fairy did not turn this check on: it was set on the machine itself, or from another panel.", "Before you accept, compare the id with the machine's own: on the machine fairy-agent helper-status , line machine id: .",
      "It does not catch a host whose own administrator is hostile.", "It shows: " + SHOWN + " . Nothing is on record."]],
  ["the record cannot be read", { ...base, binding: "unreadable" }, {}, ["bad", "required · Record unreadable"], ["bad", "required · Record unreadable"],
    ["Record unreadable. Fairy cannot read its record of machine ids (machines.json), so it asks no machine for an approval."], ["The panel's log says why. Until the file can be read, nothing is approved and no fan, power or update request is sent."]],
];
const ADMINS = [MID, SHOWN, MID.slice(0, 8), SHOWN.slice(0, 8), "data-act=\"ap-accept\"", "data-act=\"ap-open\"", "ap-on-note", "On record:", "It shows:", "The same id:", "not this panel's values", "helper-trust set --fetch-keys", "It cannot be switched on", "After it is switched on"];
let n = 0;
for (const [name, a, m, admin, viewer, shown, more] of cases) {
  n++;
  const s = host(a);
  page(m);
  for (const [who, isAdmin, want] of [["admin", true, admin], ["viewer", false, viewer]]) {
    as(isAdmin);
    const v = approvalBox(s), btn = approvalBtn(s), [body, hasMore] = approvalDialogBody(s);
    if (!v) { fail(name + ", " + who + ": no dialog"); continue; }
    if (v.tone !== want[0] || v.word !== want[1]) fail(name + ", " + who + ": the button is " + v.tone + " / " + v.word + ", want " + want.join(" / "));
    if (!btn.includes('class="btn ap-btn ' + want[0] + '"') || !btn.includes('title="Approval: ' + want[1] + '"') || !btn.includes('aria-label="Approval: ' + want[1] + '"') || !btn.endsWith(I.shield + "Approval</button>")) fail(name + ", " + who + ": the button: " + btn);
    if (!hasMore !== (v.more.length === 0) || v.more.some((x) => typeof x !== "string" || !x.trim())) fail(name + ", " + who + ": More: " + JSON.stringify(v.more));
    if (!body.includes('<span id="apd-t">Approval</span> ' + v.chip) || !body.includes('id="ap-more" hidden')) fail(name + ", " + who + ": the dialog's head or its More: " + body.slice(0, 200));
    if ((v.html.match(/<p[ >]/g) || []).length > 1) fail(name + ", " + who + ": more than one paragraph above the rows");
    const html = words(v.html), behind = words(v.more.join(" "));
    if (isAdmin) {
      for (const x of shown) if (!html.includes(x.replace(/^>|<$/g, "")) && !v.html.includes(x)) fail(name + ": the dialog lacks " + JSON.stringify(x) + " in " + JSON.stringify(html));
      for (const x of more) if (!behind.includes(x)) fail(name + ": More lacks " + JSON.stringify(x) + " in " + JSON.stringify(behind));
    } else {
      for (const x of ADMINS) if (v.html.includes(x) || v.more.join(" ").includes(x) || btn.includes(x)) fail(name + ": a viewer's dialog holds " + JSON.stringify(x));
    }
  }
}
// a host without the block: no button, no line, nothing to open
as(true);
if (approvalBox(host(null)) !== null || approvalBtn(host(undefined)) !== "" || approvalBtn(host("x")) !== "") fail("a host without the block");
// a viewer's page never asks for the approval page
if (asked.length) fail("the approval page was asked for: " + asked.join(","));
// the reason next to Turn on: up to the first colon, bracket or semicolon
for (const [why, short] of [["agent too old (approval scheme 1; this panel uses 2): install the current agent on it", "agent too old"], ["3 users would be allowed; a machine takes at most 2", "3 users would be allowed"], ["it is not answering right now", "it is not answering right now"], ["", ""]])
  if (apShortWhy(why) !== short) fail("apShortWhy(" + JSON.stringify(why) + ") = " + JSON.stringify(apShortWhy(why)));
if (!process.exitCode) console.log("ok " + n + " states");
`
	file := filepath.Join(t.TempDir(), "approval-dialog.js")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, file).CombinedOutput()
	if err != nil || !strings.HasPrefix(string(out), "ok 22 states") {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
