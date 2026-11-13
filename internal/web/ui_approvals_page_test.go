package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The Approvals page (13 Nov, the owner: "part of it is just approvals which doesn't even have anything to do
// with updates"). What stood in the box "The check on the machine itself" on the Updates page is a page of its
// own, in the menu for the people who could use that box (can_control), and the Updates page keeps one short
// line with a link. Nothing new is asked of the panel: the page is GET /api/admin/approval, as the box was.

// jsFunc is one top-level function of app.js, as it stands there.
func jsFunc(t *testing.T, js, name string) string {
	t.Helper()
	from := strings.Index(js, "\nfunction "+name+"(")
	if from < 0 {
		from = strings.Index(js, "\nasync function "+name+"(")
	}
	if from < 0 {
		t.Fatalf("app.js has no function %s", name)
	}
	end := strings.Index(js[from+1:], "\n}\n")
	return js[from+1 : from+1+end+3]
}

// jsLines are one-line declarations of app.js (const name = ...), as they stand there.
func jsLines(t *testing.T, js string, names ...string) string {
	t.Helper()
	var parts []string
	for _, n := range names {
		m := regexp.MustCompile(`(?m)^const ` + regexp.QuoteMeta(n) + ` = .*$`).FindString(js)
		if m == "" {
			t.Fatalf("app.js has no const %s any more", n)
		}
		parts = append(parts, m)
	}
	return strings.Join(parts, "\n") + "\n"
}

// runPageScript runs a script in node; it must print "ok" and nothing else.
func runPageScript(t *testing.T, name, script string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the page's own functions are not run here")
	}
	file := filepath.Join(t.TempDir(), name+".js")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, file).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

// pageTools is what every page script here has around the code under test: the document's listeners are kept
// (click fires them with an element made from the page's own HTML), requests and "Confirm it's you" calls are
// recorded, and nothing of it is the code under test.
const pageTools = `"use strict";
const listeners = [];
const document = { addEventListener(type, fn) { listeners.push([type, fn]); }, querySelectorAll: () => [] };
const location = { hash: "#/approvals" };
const I = new Proxy({}, { get: () => "" });
const state = { me: { can_control: true, approval_client: true, role: "admin" }, systems: [], byId: Object.create(null), sites: Object.create(null), summary: null, menu: null };
const asked = [], calls = [], flashes = [], titles = [], timers = [];
let answer = () => ({});
const api = async (path, opts) => { asked.push([path, opts?.method || "GET", opts?.body || ""]); return answer(path, opts?.method || "GET"); };
const controlCall = async (pending, fn) => { calls.push(pending); return fn(); };
let renders = 0, off = "";
const render = () => { renders++; }, ago = () => "1m ago", controlWhyNot = () => off, infoBtn = () => "", infoList = (x) => x.join("|");
const flash = (msg, bad = false, details = null) => { flashes.push({ msg, bad, details }); };
const setTitle = (t, sub = "") => { titles.push([t, sub]); };
const hostIcon = () => "", schedule = (fn, ms) => { timers.push(ms); };
const confirm = () => true;
const fail = (m) => { console.log("FAIL " + m); process.exitCode = 1; };
const unesc = (t) => t.replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&amp;/g, "&");
const words = (t) => unesc(t.replace(/<[^>]+>/g, " ")).replace(/\s+/g, " ").trim();
const settle = () => new Promise((r) => setTimeout(r, 5));
// the first tag of html that has the attribute, as the element a click would land on
const tag = (html, attr) => {
  const m = html.match(new RegExp("<(?:button|a)\\b[^>]*" + attr.replace(/[.*+?^${}()|[\]\\]/g, "\\$&") + "[^>]*>"));
  if (!m) return null;
  const dataset = {};
  for (const a of m[0].matchAll(/data-([a-z-]+)="([^"]*)"/g)) dataset[a[1].replace(/-([a-z])/g, (_, c) => c.toUpperCase())] = unesc(a[2]);
  return { dataset, disabled: /\sdisabled(\s|>)/.test(m[0]), html: m[0], closest: () => null };
};
const click = async (el, sel) => {
  for (const [type, fn] of listeners) if (type === "click") await fn({ target: { closest: (s) => (s.split(",").includes(sel) ? el : null) }, preventDefault() {} });
  await settle();
};
`

// approvalsPageScript is pageTools with the approval part of app.js and the helpers it uses, and the test's body.
func approvalsPageScript(t *testing.T, js, body string) string {
	t.Helper()
	from, to := strings.Index(js, "// ---- the machine's own check of approvals"), strings.Index(js, "const statusText = (s) =>")
	if from < 0 || to < from {
		t.Fatal("app.js has no approval part any more")
	}
	return pageTools + `let page = "approvals";
const current = () => ({ page, id: page === "system" ? "alfa" : undefined });
const $ = () => null, closeModal = () => {}, closeInfo = () => {};
let stepUpOpen = null;
` + jsLines(t, js, "dict", "table", "esc", "enc", "andList", "siteOf") + js[from:to] + `
const MID = (n) => "machine-id-of-" + n + "-0000000001";
const sysRow = (id, name, o) => ({ id, name, kind: "agent", platform: "linux", status: "online", owner: "nora", approval: { state: "off", enforced_by: "agent", proto: 2 }, ...o });
const mach = (id, name, o) => ({ id, system_id: id, name, online: true, state: "off", enforced_by: "agent", ready: false, users: [], ...o });
const show = (machines, o) => { apv.data = { configured: true, machines, ready: machines.filter((m) => m.ready).length, required: machines.filter((m) => m.state === "required").length, broken: 0, max: 64, last: null, ...o }; apv.error = null; apv.at = Date.now(); return pageApprovals(); };
const rowOf = (html, key) => (html.match(new RegExp('<tr class="row" data-key="apm-' + key + '">.*?</tr>', "s")) || [""])[0];
(async () => {
` + body + `
  if (!process.exitCode) console.log("ok");
})();
`
}

// One row per machine, and the row says in one or two words how its check stands: on, off, not supported (an
// agent that knows no check), offline, needs accepting. The id on record is shown while the check is on.
//
// A row accepts nothing. Accepting a machine id is a decision about which machine this is, and it is made in
// the machine's own dialog, where the id stands in full with what to compare it with: the row of a machine
// that waits for an accept has "needs accepting" and one button, "Review", which opens that dialog.
func TestUIApprovalsPageHasARowForEveryMachine(t *testing.T) {
	js := appJS(t)
	runPageScript(t, "approvals-rows", approvalsPageScript(t, js, `
  const rows = [sysRow("alfa", "ALFA", { approval: { state: "required", enforced_by: "agent", proto: 2 }, group: "Cellar" }), sysRow("bravo", "BRAVO"), sysRow("charlie", "CHARLIE", { approval: undefined, platform: "windows" }),
    sysRow("delta", "DELTA", { status: "offline" }), sysRow("echo", "ECHO", { approval: { state: "required", binding: "no_record" } }), sysRow("golf", "GOLF", { approval: { state: "required", binding: "duplicate", binding_held: ["ALFA"] } }),
    sysRow("hotel", "<b>HOTEL</b>", { owner: "<i>o</i>", platform: "<u>p</u>" })];
  state.systems = rows; state.byId = dict(rows.map((s) => [s.id, s]));
  state.sites = dict([["alfa", "Attic"], ["bravo", "Attic"], ["charlie", "Cellar"], ["delta", "Cellar"], ["echo", "Cellar"], ["golf", "Cellar"], ["hotel", "<Yard>"]]);
  const list = [
    mach("alfa", "ALFA", { state: "required", machine_id: MID("alfa"), shown_id: MID("alfa"), why: "already on: it is changed only on the machine (fairy-agent helper-trust)" }),
    mach("bravo", "BRAVO", { ready: true }),
    mach("charlie", "CHARLIE", { state: "", enforced_by: "", why: "its agent does not know this check (older than the release with it, or a Windows agent without the helper)" }),
    mach("delta", "DELTA", { online: false, why: "it is not answering right now" }),
    mach("echo", "ECHO", { state: "required", binding: "no_record", binding_text: "ECHO is not on record.", can_accept: true, accept: "first", shown_id: MID("echo"), why: "ECHO is not on record." }),
    mach("foxtrot", "FOXTROT", { state: "", online: false, system_id: "foxtrot", why: "its agent does not know this check (older than the release with it, or a Windows agent without the helper)" }),
    mach("golf", "GOLF", { state: "required", binding: "duplicate", binding_text: "ALFA keeps this machine id.", held_by: ["ALFA"], shown_id: MID("alfa"), machine_id: "" }),
    mach("hotel", "<b>HOTEL</b>", { why: "the panel has no control token for it <b>x</b>: more", shown_id: "<s>" }),
    mach("india", "INDIA", { state: "unknown", why: "its running helper is older than its agent: restart the helper (Stop, then Start, next to the host name)" }),
  ];
  const html = show(list);
  if (titles.at(-1)[0] !== "Approvals") fail("the title: " + JSON.stringify(titles.at(-1)));
  if ((html.match(/<tr class="row"/g) || []).length !== 9) fail("want 9 rows: " + (html.match(/<tr class="row"/g) || []).length);
  const cells = (key) => [...rowOf(html, key).matchAll(/<td[^>]*>(.*?)<\/td>/gs)].map((m) => words(m[1]));
  const want = {
    alfa: ["ALFA", "on", "nora", "linux", MID("alfa").slice(0, 8) + "…" + MID("alfa").slice(-6), "Approval"],
    bravo: ["BRAVO", "off ready", "nora", "linux", "", "Approval"],
    charlie: ["CHARLIE", "not supported", "nora", "windows", "", ""],
    delta: ["DELTA", "off offline", "nora", "linux", "", "Approval"],
    echo: ["ECHO", "on needs accepting", "nora", "linux", "shows " + MID("echo").slice(0, 8) + "…" + MID("echo").slice(-6), "Review"],
    foxtrot: ["FOXTROT", "offline", "", "", "", ""],
    golf: ["GOLF", "on One machine id on two hosts", "nora", "linux", "", "Approval"],
    hotel: ["<b>HOTEL</b>", "off the panel has no control token for it <b>x</b>", "<i>o</i>", "<u>p</u>", "", "Approval"],
    india: ["INDIA", "unknown its running helper is older than its agent", "", "", "", ""],
  };
  for (const [key, cols] of Object.entries(want)) if (JSON.stringify(cells(key)) !== JSON.stringify(cols)) fail(key + ": " + JSON.stringify(cells(key)));
  // nothing a machine or the panel says goes into the page as HTML
  for (const raw of ["<b>HOTEL", "<i>o</i>", "<u>p</u>", "<b>x</b>", "<Yard>", '"<s>"']) if (html.includes(raw)) fail("not escaped: " + raw);
  // the whole sentence is the tooltip of the short reason; the reason of "not supported" and of a machine never heard is the chip's
  if (!rowOf(html, "hotel").includes('title="the panel has no control token for it &lt;b&gt;x&lt;/b&gt;: more"')) fail("hotel: the whole reason is not on hover");
  if (!rowOf(html, "charlie").includes('title="its agent does not know this check (older than the release with it, or a Windows agent without the helper)">not supported</span>')) fail("charlie: " + rowOf(html, "charlie"));
  if (!rowOf(html, "foxtrot").includes('title="It has not answered since the panel started: Fairy does not know yet whether its agent has this check.">offline</span>') || rowOf(html, "foxtrot").includes("not supported")) fail("foxtrot: " + rowOf(html, "foxtrot"));
  if (!rowOf(html, "echo").includes('<span class="chip bad" title="Not on record: ECHO is not on record.">needs accepting</span>')) fail("echo: " + rowOf(html, "echo"));
  // the id in full is the tooltip of the short one
  if (!rowOf(html, "alfa").includes('<code title="' + MID("alfa") + '">')) fail("alfa: the id in full");
  // no row accepts anything: the page has no Accept button at all, and one button per row at most
  if (html.includes("ap-accept") || /Accept (this|that)/.test(html)) fail("the page has an Accept button");
  for (const key of Object.keys(want)) if ((rowOf(html, key).match(/<button/g) || []).length > 1) fail(key + " has more than one button");
  // a host whose id another host keeps has nothing to accept: its button is the plain one
  if (words(rowOf(html, "golf")).includes("Review")) fail("golf: " + words(rowOf(html, "golf")));
  // by site, A-Z, "Other" last (a machine this page has no host for)
  const sites = [...html.matchAll(/<tr class="group"[^>]*><td colspan="6">(.*?)<\/td>/g)].map((m) => unesc(m[1]));
  if (JSON.stringify(sites) !== JSON.stringify(["<Yard>", "Attic", "Cellar", "Other"])) fail("sites: " + JSON.stringify(sites));
  // the two facts, one short line each
  const line = (id) => words((html.match(new RegExp('<div id="' + id + '">.*?</div>')) || [""])[0]);
  if (line("ap-fact-lock") !== "After Turn on a machine is locked: only fairy-agent helper-trust clear , on the machine itself, removes the check.") fail("the lock: " + line("ap-fact-lock"));
  if (line("ap-fact-run") !== 'A machine with the check on is in no "Update unpowered hosts" run yet.') fail("the runs: " + line("ap-fact-run"));
  // the row's button opens that machine's own dialog (the one its page has): "Approval", and "Review" in red
  // for the machine that waits for an accept. Nothing is sent to the panel by either.
  const opened = [];
  openApproval = (id) => { opened.push(id); };
  const review = tag(rowOf(html, "echo"), 'data-act="ap-dlg"');
  if (!review || review.disabled || !review.html.includes('class="btn sm ap-btn bad"') || !review.html.includes('title="Compare and accept in the dialog of ECHO"') || tag(rowOf(html, "delta"), 'data-act="ap-dlg"').html.includes("ap-btn")) fail("Review: " + (review && review.html));
  await click(review, '[data-act="ap-dlg"]');
  await click(tag(rowOf(html, "delta"), 'data-act="ap-dlg"'), '[data-act="ap-dlg"]');
  if (JSON.stringify(opened) !== '["echo","delta"]' || calls.length || asked.length) fail("the rows' buttons: " + JSON.stringify(opened) + " " + JSON.stringify(calls) + " " + JSON.stringify(asked));
  // on a host's own page the header's button (no id of its own) opens that host's, as before
  page = "system"; opened.length = 0;
  await click({ dataset: { act: "ap-dlg" }, closest: () => null }, '[data-act="ap-dlg"]');
  if (JSON.stringify(opened) !== '["alfa"]') fail("the header's button: " + JSON.stringify(opened));
  page = "approvals";
  // control switched off by an administrator: Turn on is off and says why; the rows' buttons only open a dialog, and stay
  off = "Control is disabled by an administrator";
  const offHtml = show(list);
  if (tag(rowOf(offHtml, "echo"), 'data-act="ap-dlg"').disabled || tag(rowOf(offHtml, "delta"), 'data-act="ap-dlg"').disabled || !tag(offHtml, 'data-act="ap-open"').disabled) fail("control off: " + rowOf(offHtml, "echo"));
  if (!offHtml.includes('<div class="note warn">Control is disabled by an administrator</div>')) fail("control off: not said");
`))
}

// A machine on the row of a Proxmox node: the list names it by its agent's id and the row in system_id. The
// row's name, link and dialog are the node's (the dialog finds the agent's entry itself, and is where an id
// is accepted); the row is kept apart by the agent's id.
func TestUIApprovalsPageOnAProxmoxNodesRow(t *testing.T) {
	js := appJS(t)
	runPageScript(t, "approvals-node", approvalsPageScript(t, js, `
  const node = sysRow("node-n1", "NODE-ONE", { kind: "node", platform: undefined, owner: undefined, approval: { state: "required", binding: "changed" } });
  state.systems = [node]; state.byId = dict([[node.id, node]]); state.sites = dict([["node-n1", "Rack"]]);
  const html = show([mach("agent-of-n1", "NODE-ONE", { system_id: "node-n1", state: "required", binding: "changed", binding_text: "NODE-ONE shows another machine id.", can_accept: true, accept: "id", machine_id: MID("old"), shown_id: MID("new") })]);
  const row = rowOf(html, "agent-of-n1");
  if (!row) fail("no row for the node's agent: " + html);
  const cells = [...row.matchAll(/<td[^>]*>(.*?)<\/td>/gs)].map((m) => words(m[1]));
  if (JSON.stringify(cells) !== JSON.stringify(["NODE-ONE", "on needs accepting", "Proxmox", "Proxmox node", "shows " + MID("new").slice(0, 8) + "…" + MID("new").slice(-6), "Review"])) fail("the row: " + JSON.stringify(cells));
  if (!row.includes('<a href="#/system/node-n1">NODE-ONE</a>')) fail("the link is not the node's row: " + row);
  if (!html.includes('<td colspan="6">Rack</td>')) fail("the site is not the node's");
  const dlg = tag(row, 'data-act="ap-dlg"');
  if (dlg?.dataset.id !== "node-n1" || row.includes("ap-accept") || (row.match(/<button/g) || []).length !== 1) fail("Review opens " + dlg?.dataset.id + ": " + row);
  // the dialog it opens is the node's, and the dialog's own lookup finds the agent's entry there (the id an accept is sent for)
  const opened = [];
  openApproval = (id) => { opened.push(id); };
  await click(dlg, '[data-act="ap-dlg"]');
  if (JSON.stringify(opened) !== '["node-n1"]' || apvMachine(node)?.id !== "agent-of-n1" || calls.length) fail("the dialog: " + JSON.stringify(opened) + " " + apvMachine(node)?.id);
  // while the node's row is not there (its Proxmox API does not answer), the machine is listed under its agent's row
  const agent = sysRow("agent-of-n1", "PVE agent");
  state.systems = [agent]; state.byId = dict([[agent.id, agent]]); state.sites = dict();
  const alone = rowOf(show([mach("agent-of-n1", "PVE agent", { ready: true })]), "agent-of-n1");
  if (!alone.includes('<a href="#/system/agent-of-n1">PVE agent</a>') || tag(alone, 'data-act="ap-dlg"')?.dataset.id !== "agent-of-n1") fail("the agent's own row: " + alone);
  // "check gone": it waits for an accept too (that its check is off), and that is in the dialog as well
  state.systems = [node]; state.byId = dict([[node.id, node]]);
  const gone = rowOf(show([mach("agent-of-n1", "NODE-ONE", { system_id: "node-n1", state: "off", binding: "vanished", can_accept: true, accept: "off", machine_id: MID("old"), shown_id: "" })]), "agent-of-n1");
  if (!words(gone).includes("off needs accepting") || !words(gone).endsWith("Review") || gone.includes("ap-accept") || words(gone).includes("shows")) fail("check gone: " + words(gone));
`))
}

// "Turn on the check for N machines" is on the Approvals page, with the words and the dialog it had, and with
// no machine ready there is no dead button. The page says what it is doing: "Loading…" until the list is here,
// the panel's error when it cannot be had (as text), and never "Checking…" for good. A viewer has no page.
func TestUIApprovalsPageTurnOnAndStates(t *testing.T) {
	js := appJS(t)
	runPageScript(t, "approvals-turnon", approvalsPageScript(t, js, `
  const rows = [sysRow("alfa", "ALFA"), sysRow("bravo", "BRAVO")];
  state.systems = rows; state.byId = dict(rows.map((s) => [s.id, s]));
  // not here yet: "Loading…"; the page asks at once, and again every 5 s while it is open
  let html = pageApprovals();
  if (words(html) !== "Loading…") fail("before the answer: " + words(html));
  answer = () => ({ configured: true, machines: [mach("alfa", "ALFA", { ready: true }), mach("bravo", "BRAVO", { ready: true })], ready: 2, required: 0, broken: 0, last: null });
  await loadApprovals();
  if (asked.length !== 1 || asked[0][0] !== "/api/admin/approval" || asked[0][1] !== "GET") fail("the page asks: " + JSON.stringify(asked));
  if (JSON.stringify(timers) !== "[5000]") fail("asked again after: " + JSON.stringify(timers));
  html = pageApprovals();
  let b = tag(html, 'data-act="ap-open"');
  if (!b || b.disabled || !html.includes(">Turn on the check for 2 machines</button>") || b.dataset.id) fail("two ready: " + (b && b.html));
  if (!words(html).includes("2 machines are ready for the check on the machine itself.") || words(html).includes("already check")) fail("two ready, the line: " + words(html));
  if (html.includes("No machine is waiting")) fail("two ready: says none is waiting");
  // the button opens the dialog for all ready machines (no machine named, no dialog to go back to)
  const turned = [];
  openTurnOn = (only, back) => { turned.push([only, back]); };
  await click(b, '[data-act="ap-open"]');
  if (JSON.stringify(turned) !== '[["",""]]') fail("the button: " + JSON.stringify(turned));
  // one machine: "1 machine"
  html = show([mach("alfa", "ALFA", { ready: true }), mach("bravo", "BRAVO", { state: "required" })]);
  if (!html.includes(">Turn on the check for 1 machine</button>") || !words(html).includes("1 machine is ready for the check on the machine itself. 1 already checks.")) fail("one ready: " + words(html));
  // none: no button, one short line
  html = show([mach("alfa", "ALFA", { state: "required" }), mach("bravo", "BRAVO", { why: "the panel has no control token for it" })]);
  if (tag(html, 'data-act="ap-open"') || !words(html).includes("No machine is waiting for the check. 1 already checks.")) fail("none ready: " + words(html));
  // the last Turn on, as the box had it
  html = show([mach("alfa", "ALFA", { state: "required" })], { last: { at: "2026-11-13T20:00:00Z", by: "nora", results: [{ id: "alfa", name: "ALFA", outcome: "accepted", text: "switched on <b>now</b>" }] } });
  if (!words(html).includes("Last Turn on · by nora 1m ago ALFA switched on · locked switched on <b>now</b>") || html.includes("<b>now</b>")) fail("the last Turn on: " + words(html));
  // no machine at all
  html = show([]);
  if (!html.includes('<div class="empty">No machines with an agent yet.</div>') || html.includes("<table")) fail("no machines: " + words(html));
  // the panel has no approval: block, Pocket ID's keys cannot be had: said in a box, as the Updates page's box did
  html = show([mach("alfa", "ALFA")], { configured: false, keys_error: "Pocket ID <answered> 502" });
  if (!words(html).includes("This panel has no approval: block in config.yaml") || !html.includes('<div class="warnbox">Pocket ID &lt;answered&gt; 502</div>')) fail("not configured: " + words(html));
  // the list cannot be had: the error, as text; no "Checking…", no "Loading…" that stays
  apv.data = null; apv.at = 0; asked.length = 0;
  answer = () => { throw Object.assign(new Error("the panel <b>answered</b> 502"), { status: 502 }); };
  await loadApprovals();
  html = pageApprovals();
  if (html !== '<div class="error">the panel &lt;b&gt;answered&lt;/b&gt; 502</div>') fail("an error: " + html);
  // ... and with a list from before, the error stands over it
  apv.data = { configured: true, machines: [mach("alfa", "ALFA")], ready: 0, required: 0, broken: 0, last: null };
  html = pageApprovals();
  if (!html.includes('<div class="error">the panel &lt;b&gt;answered&lt;/b&gt; 502</div>') || !rowOf(html, "alfa")) fail("an error over the list: " + words(html));
  for (const never of ["Checking", "Loading"]) if (html.includes(never)) fail("the page says " + never);
  // someone who is no admin: no page, and nothing is asked
  state.me = { can_control: false, role: "viewer" }; apv.data = null; apv.error = null; asked.length = 0;
  await loadApprovals();
  html = pageApprovals();
  if (html !== '<div class="empty">The Approvals page is for admins only.</div>' || asked.length) fail("a viewer: " + html + " " + asked.length);
`))
}

// The menu and the router. "Approvals" is behind the right the box on the Updates page was behind, the one
// Audit and Updates are behind (can_control): a viewer has no entry. The address is a page of the router.
func TestUIApprovalsPageInTheMenuAndTheRouter(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`if (h.startsWith("#/approvals")) return { page: "approvals" };`,
		`r.page === "approvals" ? pageApprovals()`,
		`if (r.page === "approvals") loadApprovals();`,
		`${me.can_control ? item("#/approvals", I.shield, "Approvals") : ""}`,
		`if (!state.me?.can_control) return '<div class="empty">The Approvals page is for admins only.</div>';`,
		`<button class="btn" type="button" data-act="ap-accept"`, // Accept is made in one place: the machine's dialog (approvalBinding)
	} {
		if strings.Count(js, want) != 1 {
			t.Errorf("app.js lacks (or has twice) %q", want)
		}
	}
	runPageScript(t, "approvals-menu", pageTools+`
let menu = { innerHTML: "", setAttribute() {} };
const $ = () => menu, syncAcct = () => {};
`+jsLines(t, js, "esc", "demoViewLabel", "hashPart")+jsFunc(t, js, "buildAcct")+jsFunc(t, js, "current")+`
const entries = () => [...menu.innerHTML.matchAll(/<a class="menu-item"[^>]*href="([^"]*)"[^>]*>.*?<span class="name">(.*?)<\/span>/gs)].map((m) => m[1] + " " + m[2]);
state.me = { sub: "u1", name: "nora", role: "admin", can_control: true, can_manage_users: true };
buildAcct();
if (JSON.stringify(entries()) !== JSON.stringify(["#/settings Settings", "#/sessions Sessions", "#/audit Audit", "#/users Users", "#/updates Updates", "#/approvals Approvals"])) fail("an admin's menu: " + JSON.stringify(entries()));
if (!menu.innerHTML.includes('href="#/approvals" data-page="approvals"')) fail("the entry is not marked by its page");
// a viewer: Settings and Sessions, as before
state.me = { sub: "u2", name: "vera", role: "viewer", can_control: false, can_manage_users: false, sites: ["Attic"] };
buildAcct();
if (JSON.stringify(entries()) !== JSON.stringify(["#/settings Settings", "#/sessions Sessions"])) fail("a viewer's menu: " + JSON.stringify(entries()));
if (menu.innerHTML.includes("pproval")) fail("a viewer's menu names the page");
// control without user management (no such role today): the entry goes with Audit and Updates, not with Users
state.me = { sub: "u3", name: "odo", role: "admin", can_control: true, can_manage_users: false };
buildAcct();
if (JSON.stringify(entries()) !== JSON.stringify(["#/settings Settings", "#/sessions Sessions", "#/audit Audit", "#/updates Updates", "#/approvals Approvals"])) fail("control only: " + JSON.stringify(entries()));
for (const [hash, want] of [["#/approvals", "approvals"], ["#/updates", "updates"], ["#/", "overview"]]) { location.hash = hash; if (current().page !== want) fail(hash + " is the page " + current().page); }
if (!process.exitCode) console.log("ok");
`)
}

// updatesPageScript is pageTools with the Updates page's own code (the agents' part and "Update unpowered hosts").
func updatesPageScript(t *testing.T, js, body string) string {
	t.Helper()
	from := strings.Index(js, "// ------------------------------------------------------------ updates (admins only)")
	to := strings.Index(js, "// ------------------------------------------------------------ users, roles and sites (admins only)")
	if from < 0 || to < from {
		t.Fatal("app.js has no Updates part any more")
	}
	return pageTools + `location.hash = "#/updates";
let page = "updates";
const current = () => ({ page });
const $ = () => null, closeModal = () => {};
const localStorage = { getItem: () => null, setItem() {} }, window = { addEventListener() {} };
const fetchPanelUpdate = async () => {};
const safeHref = (u) => esc(u), secsSince = () => 100, fmtSpan = (s) => s + " s";
let signedOut = false;
` + jsLines(t, js, "dict", "table", "esc", "enc", "fmtRuntime", "pupd", "PANEL_STEPS") + jsFunc(t, js, "panelUpdateBox") + js[from:to] + `
const agent = (id, o) => ({ id, system_id: id, name: id.toUpperCase(), version: "17-bbbbbbb", status: "online", configured: true, can_update: true, behind: true, self_update: { state: "idle" }, ...o });
const updates = (agents, o) => ({ latest: { tag: "v0.8.0", version: "18-ccccccc", verified: true }, panel_version: "18-ccccccc", panel_latest: {}, agents, behind: agents.filter((r) => r.behind).length, ...o });
(async () => {
` + body + `
  if (!process.exitCode) console.log("ok");
})();
`
}

// The Updates page without the approval controls: no "Turn on the check", no list of checks, no Accept, and
// nothing of it asked from the panel. What it still says about the check is one short line with a link, where
// it matters for an update: a machine with the check on asks for the passkey at Update, a machine with the
// check on is in no run, and a broken check (the update is refused) is the row's own red sign, which leads to
// the machine's dialog. Update and Update all are what they were.
func TestUIUpdatesPageWithoutTheApprovalControls(t *testing.T) {
	js := appJS(t)
	from := strings.Index(js, "// ------------------------------------------------------------ updates (admins only)")
	to := strings.Index(js, "// ------------------------------------------------------------ users, roles and sites (admins only)")
	if from < 0 || to < from {
		t.Fatal("app.js has no Updates part any more")
	}
	for _, gone := range []string{"approvalBanner", "pageApprovals(", "fetchApproval", `data-act="ap-open"`, `data-act="ap-accept"`, "apAcceptBtn", "ap-machines", "Turn on the check", "turnOnResults", "The check on the machine itself", "passkey on the machine", "/api/admin/approval"} {
		if strings.Contains(js[from:to], gone) {
			t.Errorf("the Updates page's code still has %q", gone)
		}
	}
	if strings.Contains(js, "function approvalBanner") {
		t.Error("app.js still has the Updates page's box (approvalBanner): it is the Approvals page now, not a copy")
	}
	runPageScript(t, "updates-no-approval", updatesPageScript(t, js, `
  const rows = [{ id: "pc-a", name: "PC-A" }, { id: "node-n1", name: "NODE-ONE" }];
  state.systems = rows; state.byId = dict(rows.map((s) => [s.id, s]));
  const host = (id, o) => ({ id, name: id.toUpperCase(), opted_in: false, can_join: true, can_wake: true, relay: "NAS", power: "off", choice: "", ...o });
  const settings = { online_timeout_s: 300, settle_s: 60, shutdown_delay_s: 120 };
  wu.data = { hosts: [host("pc-a", { approval: "required", why: "needs approval: not supported in runs yet" }), host("pc-b")], active: null, last: [], settings, off_after_s: 900, max_hosts: 16 };
  upd.data = updates([agent("pc-a", { approval: "required" }), agent("pc-b"), agent("agent-of-n1", { system_id: "node-n1", name: "NODE-ONE", approval: "broken", can_update: false, why: "its check of approvals is broken" }), agent("<b>pc-d</b>", { behind: false })]);
  updIndex();
  const html = pageUpdates();
  for (const gone of ["ap-open", "ap-accept", "ap-machines", "Turn on", "is waiting for the check", "ready for the check", "passkey on the machine", "helper-trust clear", "cannot be in a run yet"]) if (html.includes(gone)) fail("the Updates page still has: " + gone);
  // one short line each, with the link, where an update meets the check
  const links = [...html.matchAll(/<div class="note" id="([a-z-]+)">(.*?)<\/div>/g)].filter((m) => m[2].includes('href="#/approvals"')).map((m) => m[1] + ": " + words(m[2]));
  if (JSON.stringify(links.sort()) !== JSON.stringify(["upd-appr: A machine with the check on asks for your passkey at every Update. Approvals", "wu-appr: A machine with the check on is in no run yet. Approvals"])) fail("the lines: " + JSON.stringify(links));
  // a broken check is the row's own sign, and leads to the machine's dialog (the node's row for a node's agent)
  const broken = tag(html, 'data-ap-go="node-n1"');
  if (!broken || !broken.html.includes('class="chip bad" href="#/system/node-n1"') || !html.includes(">approval broken</a>")) fail("approval broken: " + (broken && broken.html));
  // without a machine that has the check, the page does not name it at all
  wu.data.hosts = [host("pc-b")];
  upd.data = updates([agent("pc-b")]);
  if (/approv|passkey/i.test(pageUpdates())) fail("no machine has the check, and the page names it: " + words(pageUpdates()).match(/.{40}(approv|passkey).{40}/i)?.[0]);
  // Update and Update all are there for every machine, and send what they sent
  upd.data = updates([agent("pc-a", { approval: "required" }), agent("pc-b"), agent("pc-c", { can_update: false, why: "it is offline", status: "offline" })]);
  const page2 = pageUpdates();
  const one = tag(page2, 'data-act="upd-one" data-id="pc-a"'), all = tag(page2, 'data-act="upd-all"'), no = tag(page2, 'data-act="upd-one" data-id="pc-c"');
  if (!one || one.dataset.act !== "upd-one" || one.disabled || !no || !no.disabled || !no.html.includes('title="it is offline"')) fail("the rows' buttons: " + (one && one.html) + (no && no.html));
  if (!all || all.disabled || !page2.includes("Update all (2)</button>")) fail("Update all: " + (all && all.html));
  answer = (path, method) => (method !== "POST" ? (path === "/api/admin/updates" ? upd.data : wu.data) : path === "/api/admin/updates/pc-a" ? { message: "started" } : { message: "2 started", results: [{ id: "pc-a" }], all_ok: true, lines: [] });
  signedOut = true;  // (the page's background timer is not started here)
  await click(one, '[data-act="upd-one"]');
  await click(all, '[data-act="upd-all"]');
  const sent = calls.map((c) => c.method + " " + c.path + " " + c.label);
  if (JSON.stringify(sent) !== JSON.stringify(["POST /api/admin/updates/pc-a Update PC-A", "POST /api/admin/updates Update all agents"])) fail("what Update sends: " + JSON.stringify(sent));
  if (JSON.stringify(flashes.map((f) => f.msg)) !== JSON.stringify(["Update PC-A: started", "Update all agents: 2 started"])) fail("what Update says: " + JSON.stringify(flashes));
  if (asked.some(([p]) => p.startsWith("/api/admin/approval"))) fail("the Updates page asks for the approval list: " + JSON.stringify(asked));
`))
}
