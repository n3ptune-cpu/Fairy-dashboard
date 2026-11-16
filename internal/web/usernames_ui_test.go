package web

import (
	"strings"
	"testing"
)

// veilJS is the part of app.js that draws a value as dots until it is clicked, and cuts a text of the Audit page
// where it holds an address or a name: without the click listener, which needs a document.
func veilJS(t *testing.T, js string) string {
	t.Helper()
	return jsLines(t, js, "veil", "VEIL_DOTS") + jsFunc(t, js, "veiled") + jsLines(t, js, "VEIL_ADDR", "veilWordChar") +
		jsFunc(t, js, "veilParts") + jsLines(t, js, "veilHas", "veilHtml", "veilDots")
}

// namesTools is what the page's own functions for people need around them in node.
const namesTools = `"use strict";
const fail = (m) => { console.log("FAIL " + m); process.exitCode = 1; };
const unesc = (t) => t.replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&amp;/g, "&");
// what a person reads: the text between the tags
const words = (t) => unesc(t.replace(/<[^>]+>/g, " ")).replace(/\s+/g, " ").trim();
// everything a person can read, point at or copy: the text, and every tooltip and name of an element
const readable = (t) => words(t) + " | " + [...t.matchAll(/(?:title|aria-label|placeholder|alt|value)="([^"]*)"/g)].map((m) => unesc(m[1])).join(" | ");
const I = new Proxy({}, { get: () => "" });
const state = { me: null, systems: [], byId: Object.create(null) };
const ago = () => "1m ago", enc = encodeURIComponent, setTitle = () => {}, infoBtn = () => "", infoList = (x) => x.join("|");
const buttons = (t) => [...t.matchAll(/<button class="veil[^"]*"[^>]*>[^<]*<\/button>/g)].map((m) => m[0]);
`

// The dots button. Before it is pressed the value is nowhere in the markup: not in the text, not in a tooltip,
// not in an attribute. It is a button with a name that says what it shows. Pressed, it shows the value, and its
// name is the value. The dots are the same for a short and a long value.
func TestUIVeiledValueIsNowhereInThePageUntilClicked(t *testing.T) {
	js := appJS(t)
	runPageScript(t, "veil", namesTools+jsLines(t, js, "dict", "esc")+veilJS(t, js)+`
const addr = "owner@example.com", name = "Real <b>Name</b>";
let h = veiled("u:sub-n:email", addr, "e-mail address of Juniper [id sub-n]");
if (h !== '<button class="veil" type="button" data-veil="u:sub-n:email" aria-pressed="false" aria-label="Show the e-mail address of Juniper [id sub-n]" title="Show the e-mail address of Juniper [id sub-n]">••••••</button>') fail("closed: " + h);
if (h.includes(addr) || h.includes("owner") || h.includes("example.com")) fail("the address is in the closed button: " + h);
const long = veiled("k2", "a-very-long-address-of-somebody@a-long-domain.example.org", "e-mail address");
if (words(long) !== words(h) || words(h) !== "••••••") fail("the dots say how long the value is: " + words(long) + " / " + words(h));
if (veiled("k3", "", "name") !== "" || veiled("k4", null, "name") !== "" || veiled("k5", undefined, "name") !== "") fail("a value that is not there gets a button");
// pressed
veil.open["u:sub-n:email"] = true;
h = veiled("u:sub-n:email", addr, "e-mail address of Juniper [id sub-n]");
if (h !== '<button class="veil open" type="button" data-veil="u:sub-n:email" aria-pressed="true" title="Hide the e-mail address of Juniper [id sub-n]">owner@example.com</button>') fail("open: " + h);
// one value: another key is still dots
if (veiled("u:sub-s:email", "friend@example.com", "e-mail address").includes("friend")) fail("a click opened another value");
// a value is text, never markup; nor is what it is called
veil.open.n = true;
h = veiled("n", name, 'name of "x" <i>');
if (h.includes("<b>") || h.includes("<i>") || !h.includes("Real &lt;b&gt;Name&lt;/b&gt;") || !h.includes("&quot;x&quot; &lt;i&gt;")) fail("markup from a value: " + h);
// in the account menu it is an entry of the menu
h = veiled("me:email", addr, "e-mail address of your account", "menuitemcheckbox");
if (!h.includes('role="menuitemcheckbox" tabindex="-1" aria-checked="false"') || h.includes("aria-pressed") || h.includes(addr)) fail("in the menu: " + h);
// a text with something to hide in it
const known = ["owner@example.com", "Real Name", "Al"];
const cut = (t) => JSON.stringify(veilParts(t, known));
for (const [text, want] of [
  ["owner@example.com", '[["owner@example.com",true]]'],
  ["owner@example.com (unverified)", '[["owner@example.com",true],[" (unverified)",false]]'],
  ["OWNER@EXAMPLE.COM", '[["OWNER@EXAMPLE.COM",true]]'],
  ["3 refused requests by stranger@example.org not written", '[["3 refused requests by ",false],["stranger@example.org",true],[" not written",false]]'],
  ["actor=owner@example.com · dropped=3", '[["actor=",false],["owner@example.com",true],[" · dropped=3",false]]'],
  ["Real Name", '[["Real Name",true]]'],
  ["by Real Name, twice", '[["by ",false],["Real Name",true],[", twice",false]]'],
  ["Al and Alfred and AL", '[["Al",true],[" and Alfred and ",false],["AL",true]]'],   // a name only as a word of its own
  ["ci (token, owner sub-n)", '[["ci (token, owner sub-n)",false]]'],
  ["root@pam and fairy@192.0.2.7", '[["root@pam and fairy@192.0.2.7",false]]'],
  ["laptop", '[["laptop",false]]'], ["", "[]"],
]) if (cut(text) !== want) fail("veilParts(" + text + "): " + cut(text));
if (cut(null) !== "[]" || cut(undefined) !== "[]" || cut(7) !== '[["7",false]]') fail("no text");
if (veilDots("by owner@example.com (unverified): Real Name", known) !== "by •••••• (unverified): ••••••") fail("veilDots: " + veilDots("by owner@example.com (unverified): Real Name", known));
if (veilHas("laptop", known) || !veilHas("x Real Name", known)) fail("veilHas");
h = veilHtml("a:7:t", "<b>by</b> owner@example.com & Real Name", "account", known);
if (h.includes("owner@") || h.includes("Real") || !h.startsWith("&lt;b&gt;by&lt;/b&gt; <button") || buttons(h).length !== 2 || !h.includes('data-veil="a:7:t:1"') || !h.includes('data-veil="a:7:t:3"') || !h.includes("</button> &amp; <button")) fail("veilHtml: " + h);
if (!process.exitCode) console.log("ok");
`)
	// the listener: one press shows that one value, the next hides it, and neither is also a click on what the
	// button sits in
	for _, want := range []string{
		`const b = e.target.closest("[data-veil]");`,
		`b.textContent = nb.textContent;`, // drawn in place: the button stays the element it is
		`if (veil.open[k]) delete veil.open[k]; else veil.open[k] = true;`,
		`veilReset("a:"); veilReset("u:");  // a page that is opened again shows dots again`,
		`if (reset) veilReset("a:");  // a list that is loaded again shows nothing that was clicked before`,
		`veilReset("u:");  // the list is drawn again: what was clicked is dots again`,
		`veilReset("me:");  // a menu that is opened again shows dots again`,
	} {
		if strings.Count(js, want) != 1 {
			t.Errorf("app.js lacks (or has twice) %q", want)
		}
	}
	css := appCSS(t)
	for _, want := range []string{".veil {", ".veil:focus-visible { outline: 2px solid var(--accent);"} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css lacks %q", want)
		}
	}
}

// The Audit page: the user name first, then what the entry itself was written with (an address or a name, as
// dots), then the account id with the note about what an account can choose. An address or a name in the target,
// the details or the result is dots too. Nothing of it is in the page until it is clicked.
func TestUIAuditPageNamesByUsernameAndHidesTheRest(t *testing.T) {
	js := appJS(t)
	from := strings.Index(js, "// ------------------------------------------------------------ audit page (control group only)")
	to := strings.Index(js, "document.addEventListener(\"submit\", (e) => {\n  if (e.target.id !== \"audit-form\") return;")
	if from < 0 || to < from {
		t.Fatal("app.js has no audit page any more")
	}
	runPageScript(t, "audit-names", namesTools+jsLines(t, js, "dict", "esc")+veilJS(t, js)+`
let answer = {};
const resets = [];
const api = async () => answer, render = () => {}, veilReset = (prefix) => resets.push(prefix);
`+js[from:to]+`
state.me = { can_control: true };
audit.known = ["owner@example.com", "friend@example.com", "Real Name", "Other Person", "old@example.com"];
audit.entries = [
  // written by this version: the user name is in the entry
  { id: 1, ts: "2026-11-16T10:00:00Z", actor: "owner@example.com", sub: "sub-n", username: "Juniper", who: "Juniper", action: "power.shutdown", target: "tower", params: { delay_s: 60 }, outcome: "ok", source: "ui", ip: "192.0.2.1" },
  // written by an older version: the address and the subject
  { id: 2, ts: "2026-11-15T10:00:00Z", actor: "friend@example.com (unverified)", sub: "sub-s", who: "PineTop", action: "user.set_role", target: "old@example.com", params: { sub: "sub-o", role: "viewer", before: "pending" }, outcome: "ok", source: "ui", ip: "192.0.2.2" },
  // an account without a user name
  { id: 3, ts: "2026-11-15T09:00:00Z", actor: "old@example.com", sub: "sub-o", who: "(address hidden)", action: "auth.login", target: "", params: {}, outcome: "ok", source: "ui", ip: "192.0.2.3" },
  // what the throttle wrote: an address in the details and in the result
  { id: 4, ts: "2026-11-15T08:00:00Z", actor: "fairy", sub: "sub-s", who: "PineTop", action: "audit.suppressed", target: "", params: { dropped: 3, actor: "friend@example.com (unverified)", sub: "sub-s" }, outcome: "3 refused requests (auth.denied) by friend@example.com (unverified) not written", source: "fairy" },
  // an API token, the command line, a failed sign-in of nobody the panel knows
  { id: 5, ts: "2026-11-15T07:00:00Z", actor: "ci (token, owner sub-n)", sub: "token:1", who: "ci (token, owner Juniper)", action: "poll.set", target: "laptop", params: {}, outcome: "ok", source: "token:ci" },
  { id: 6, ts: "2026-11-15T06:00:00Z", actor: "root", sub: null, who: "root", action: "user.set_role", target: "Real Name", params: {}, outcome: "ok", source: "cli" },
  { id: 7, ts: "2026-11-15T05:00:00Z", actor: "stranger@example.org", sub: "x9", who: "(address hidden)", action: "auth.login", target: "", params: {}, outcome: "denied: not in an allowed group", source: "ui" },
];
const hidden = [...audit.known, "stranger@example.org"];
let html = pageAudit();
const rows = html.split('<tr class="row"').slice(1);
if (rows.length !== 7) fail("rows: " + rows.length);
// nothing private is in the page: not as text, not in a tooltip, not in an attribute
for (const p of hidden) if (html.toLowerCase().includes(p.toLowerCase())) fail("the page holds " + p + " before a click");
const who = (r) => words(r.split("<td>")[1].split("</td>")[0]);
const want = ["Juniper •••••• id sub-n", "PineTop •••••• (unverified) id sub-s", "(address hidden) •••••• id sub-o", "PineTop fairy id sub-s",
  "ci (token, owner Juniper) ci (token, owner sub-n) id token:1", "root", "(address hidden) •••••• id x9"];
rows.forEach((r, i) => { if (who(r) !== want[i]) fail("who of entry " + (i + 1) + ": " + who(r)); });
// the user name is first, in bold; the entry's own text is a button under it
if (!rows[0].includes('<td><b>Juniper</b><div class="note"><button class="veil" type="button" data-veil="a:1:w:0" aria-pressed="false" aria-label="Show the address or name this entry was written with"')) fail("the first entry: " + rows[0]);
// the note of the account id says what an account can choose: the user name too
const note = unesc((rows[0].match(/<div class="note" title="([^"]*)">id sub-n<\/div>/) || [])[1] || "");
if (!note.includes("the one part of this entry the owner of the account cannot choose") || !note.includes("The user name above is what the account calls itself, like the address or name behind the dots") || !note.includes("'(unverified)' means Pocket ID did not verify that address")) fail("the note of the id: " + note);
// the target of a change on the Users page is the account: dots; a host is its link
if (!rows[1].includes('data-veil="a:2:t:0"') || !rows[1].includes('aria-label="Show the account this entry is about"') || !rows[5].includes('data-veil="a:6:t:0"')) fail("a target that is an account: " + rows[1]);
if (!words(rows[0]).includes("tower")) fail("a target that is a host: " + words(rows[0]));
// details and result of the throttle's entry
if (!words(rows[3]).includes("dropped=3 · actor= •••••• (unverified) · sub=sub-s") || !rows[3].includes('data-veil="a:4:d:1"') || rows[3].includes("audit-det")) fail("details with an address: " + words(rows[3]));
if (!rows[3].includes('title="3 refused requests (auth.denied) by •••••• (unverified) not written"')) fail("a result with an address: " + rows[3]);
// details without anything to hide are as before
if (!rows[1].includes('<div class="note">sub=sub-o · role=viewer · before=pending</div>')) fail("plain details: " + rows[1]);
// a click shows that one value, and only that one
veil.open["a:2:w:0"] = true;
html = pageAudit();
if (!html.includes('data-veil="a:2:w:0" aria-pressed="true" title="Hide the address or name this entry was written with">friend@example.com</button> (unverified)')) fail("after a click: " + html.split('<tr class="row"')[2]);
if ((html.match(/friend@example\.com/g) || []).length !== 1) fail("one click showed the address in " + (html.match(/friend@example\.com/g) || []).length + " places");
for (const p of hidden.filter((x) => x !== "friend@example.com")) if (html.includes(p)) fail("a click on one value showed " + p);
// the filter box does not say the address back in a tooltip of its own making, and tells what it finds
if (!html.includes('placeholder="User name, token or source…"')) fail("the filter box");
// somebody who is no admin has no page
state.me = { can_control: false };
if (pageAudit() !== '<div class="empty">The audit log is visible to admins only.</div>') fail("no admin");
// loading the list: what the server says is to hide is kept for the drawing (a name does not look like anything),
// and what was clicked before is dots again; a list that is added to keeps what is shown
(async () => {
  state.me = { can_control: true };
  answer = { entries: [{ id: 21, ts: "2026-11-16T10:00:00Z", actor: "root", who: "root", action: "user.remove", target: "Quiet Name", params: { was: "Quiet Name" }, outcome: "ok", source: "cli" }], next: 5, private: ["Quiet Name"] };
  await loadAudit(true);
  if (audit.known.join("|") !== "Quiet Name" || resets.join("|") !== "a:") fail("after loading: known " + audit.known + ", reset " + resets);
  const page = pageAudit();
  if (page.includes("Quiet Name") || !page.includes('data-veil="a:21:t:0"') || !page.includes('data-veil="a:21:d:1"')) fail("a name the server says is an account's: " + page.split('<tr class="row"')[1]);
  await loadAudit(false);
  if (resets.length !== 1 || audit.entries.length !== 2) fail("loading more: reset " + resets + ", entries " + audit.entries.length);
  answer = { entries: [], next: null };  // an answer without the list
  await loadAudit(true);
  if (audit.known.length !== 0) fail("an answer without the list: " + audit.known);
  if (!process.exitCode) console.log("ok");
})();
`)
}

// The Users page: a row names the person by the user name; the name and the address are dots until clicked; the
// confirmations and the labels of the changes name the account by user name and id. The Sessions page's list of
// everyone's sessions and the account menu name by user name too; one's own address in the menu is dots.
func TestUIUsersSessionsAndMenuNameByUsername(t *testing.T) {
	js := appJS(t)
	from := strings.Index(js, "const ROLE_LABEL = table(")
	to := strings.Index(js, "async function adminCall(")
	sfrom := strings.Index(js, "const sessWhen = ")
	sto := strings.Index(js, "function pageSessions()")
	if from < 0 || to < from || sfrom < 0 || sto < sfrom {
		t.Fatal("app.js has no Users or Sessions page any more")
	}
	runPageScript(t, "users-names", namesTools+jsLines(t, js, "dict", "table", "esc", "meShown", "demoViewLabel")+veilJS(t, js)+`
const admin = { data: null, error: null }, browserOf = () => "", fmtSpan = () => "";
let menu = { innerHTML: "", attrs: {}, setAttribute(k, v) { this.attrs[k] = v; } };
const $ = () => menu, syncAcct = () => {};
`+js[from:to]+js[sfrom:sto]+jsFunc(t, js, "buildAcct")+`
state.me = { can_manage_users: true, role: "admin" };
const user = (o) => ({ role: "viewer", sites: [], last_seen: "2026-11-16T10:00:00Z", via: "first sign-in", you: false, email_verified: true, ...o });
admin.data = { roles: ["admin", "viewer", "pending"], sites: [{ name: "Home", hosts: 1, users: 1, in_config: false }], targets: [], users: [
  user({ sub: "sub-n", username: "Juniper", shown_as: "Juniper", named: true, same_username: ["9a1b2c3d-wanda"], username_note: "same_username", email: "owner@example.com", name: "Real Name", role: "admin", you: true, via: "bootstrap_admins" }),
  user({ sub: "sub-s", username: "PineTop", shown_as: "PineTop", named: true, same_username: [], username_note: "", email: "friend@example.com", email_verified: false, name: "Other Person", via: "panel by Juniper" }),
  user({ sub: "sub-o", shown_as: "(address hidden)", named: false, same_username: [], username_note: "", email: "old@example.com", name: "Old Timer" }),
  user({ sub: "sub-x", username: "owner@example.com", shown_as: "(address hidden)", named: false, same_username: [], username_note: "is_address", email: "x@example.com", name: "Ex" }),  // a user name that is an address: not shown as one
  user({ sub: "sub-z", shown_as: "sub-z", named: false, same_username: [], username_note: "", email: "", name: "" }),
  // an account that waits and has the admin's user name in other letters: shown with the start of its id
  user({ sub: "9a1b2c3d-wanda", username: "juniper", shown_as: "juniper · id 9a1b2c", named: true, same_username: ["sub-n"], username_note: "same_username", email: "wanda@example.org", name: "Waiting Wanda", role: "pending" }),
  user({ sub: "sub-y", username: "sub-n", shown_as: "(address hidden)", named: false, same_username: [], username_note: "is_account_id", email: "y@example.com", name: "" }),
] };
const hidden = ["owner@example.com", "friend@example.com", "old@example.com", "x@example.com", "Real Name", "Other Person", "Old Timer", "Real", "Timer", "wanda@example.org", "Waiting Wanda"];
let html = pageUsers();
for (const p of hidden) if (html.includes(p)) fail("the Users page holds " + p + " before a click");
const rows = html.split('<tr class="row" data-key="u-').slice(1, 8).map((r) => r.split("</td>")[0]);
const want = ["Juniper you same user name as id 9a1b2c3d-wanda name •••••• e-mail •••••• id sub-n", "PineTop name •••••• e-mail •••••• unverified id sub-s",
  "no user name yet name •••••• e-mail •••••• id sub-o", "user name hidden name •••••• e-mail •••••• id sub-x", "no user name yet id sub-z",
  "juniper · id 9a1b2c same user name as id sub-n name •••••• e-mail •••••• id 9a1b2c3d-wanda", "user name hidden e-mail •••••• id sub-y"];
rows.forEach((r, i) => { if (words(r.slice(r.indexOf(">") + 1)) !== want[i]) fail("row " + (i + 1) + ": " + words(r.slice(r.indexOf(">") + 1))); });
if (!rows[0].includes('<b>Juniper</b>') || !rows[2].includes('<b class="muted" title="The panel has no user name for this account yet')) fail("the name of a row: " + rows[0] + rows[2]);
// a row never says "no user name yet" (and "it gets it at the next sign-in") for an account that has one: it says
// which account has the same, or why the panel does not show it
if (!rows[5].includes('<b>juniper · id 9a1b2c</b>') || !rows[5].includes('<span class="sub warn-t" title="More than one account has this user name.') || rows[5].includes("no user name")) fail("the row of the second account with a user name: " + rows[5]);
if (!rows[0].includes('<span class="sub warn-t" title="More than one account has this user name.')) fail("the row of the first account with that user name: " + rows[0]);
if (!rows[3].includes('title="This account&#39;s user name is an e-mail address') && !rows[3].includes("title=\"This account's user name is an e-mail address")) fail("a user name that is an address: " + rows[3]);
if (!rows[6].includes("user name is another account") || rows[6].includes("next sign-in")) fail("a user name that is another account's id: " + rows[6]);
if (rows[1].includes("same user name") || rows[2].includes("same user name")) fail("a row without a second account says there is one");
// an answer of a panel that does not send named yet is read as before
if (!userNamed({ username: "A", shown_as: "A" }) || userNamed({ username: "A", shown_as: "(address hidden)" }) || userNamed({ shown_as: "x" }) || userNamed(null)) fail("userNamed without named");
// each value is a button that says whose what it shows, by user name and id
const b = buttons(rows[1]);
if (b.length !== 2 || !b[0].includes('data-veil="u:sub-s:name"') || !b[0].includes('aria-label="Show the name of PineTop [id sub-s]"') ||
  !b[1].includes('data-veil="u:sub-s:email"') || !b[1].includes('aria-label="Show the e-mail address of PineTop [id sub-s]"')) fail("the buttons of a row: " + b.join(" "));
if (!buttons(rows[2])[1].includes('aria-label="Show the e-mail address of the account [id sub-o]"')) fail("the button of an account without a user name: " + buttons(rows[2])[1]);
// the confirmation of Remove, and with it the label of the change ("Confirm it's you" shows it)
const rm = [...html.matchAll(/data-act="u-remove" data-sub="([^"]*)" data-name="([^"]*)"/g)].map((m) => m[1] + ": " + unesc(m[2]));
if (JSON.stringify(rm) !== JSON.stringify(["sub-n: Juniper [id sub-n]", "sub-s: PineTop [id sub-s]", "sub-o: the account [id sub-o]", "sub-x: the account [id sub-x]", "sub-z: the account [id sub-z]",
  "9a1b2c3d-wanda: juniper · id 9a1b2c [id 9a1b2c3d-wanda]", "sub-y: the account [id sub-y]"])) fail("Remove: " + JSON.stringify(rm));
if (userIdent(admin.data.users[1]) !== "PineTop [id sub-s]" || userIdent(null) !== "this user") fail("userIdent");
// how they got their role, as the server gave it
if (!words(html).includes("panel by Juniper")) fail("via");
// one click
veil.open["u:sub-s:email"] = true;
html = pageUsers();
if (!html.includes('aria-pressed="true" title="Hide the e-mail address of PineTop [id sub-s]">friend@example.com</button>')) fail("after a click");
for (const p of hidden.filter((x) => x !== "friend@example.com")) if (html.includes(p)) fail("a click on one value showed " + p);
delete veil.open["u:sub-s:email"];

// everyone's sessions
const sess = (o) => ({ id: "h1", current: false, browser: "Firefox on Windows", user_agent: "UA", ip: "192.0.2.1", signed_in_ip: "192.0.2.1", also_from: [], signed_in: "2026-11-16T09:00:00Z", last_active: "2026-11-16T10:00:00Z", location: "", ...o });
let row = sessRow(sess({ user: { sub: "sub-s", username: "PineTop", you: false } }), true);
if (words(row.split("</td>")[0]) !== "PineTop id sub-s") fail("a session's account: " + words(row.split("</td>")[0]));
if (!row.includes('data-label="PineTop [id sub-s]: Firefox on Windows (192.0.2.1)"')) fail("the label of a sign-out: " + row);
row = sessRow(sess({ user: { sub: "sub-o", username: "(address hidden)", you: false } }), true);
if (words(row.split("</td>")[0]) !== "(address hidden) id sub-o") fail("a session of an account without a user name: " + words(row.split("</td>")[0]));
// ... whose sign-out is told apart from every other such account's by the id
if (!row.includes('data-label="(address hidden) [id sub-o]: Firefox on Windows (192.0.2.1)"')) fail("the label of a sign-out of an account without a user name: " + row);
row = sessRow(sess({ user: { sub: "9a1b2c3d-wanda", username: "juniper · id 9a1b2c", you: false } }), true);
if (words(row.split("</td>")[0]) !== "juniper · id 9a1b2c id 9a1b2c3d-wanda" || !row.includes('data-label="juniper · id 9a1b2c [id 9a1b2c3d-wanda]: Firefox')) fail("a session of the second account with a user name: " + row);
row = sessRow(sess({ user: { sub: "sub-n", username: "Juniper", you: true } }), true);
if (!row.includes('data-label="Firefox on Windows (192.0.2.1)"')) fail("the label of a sign-out of one's own session: " + row);
// a list from a panel that still sends name and email does not print them
row = sessRow(sess({ user: { sub: "sub-s", name: "Other Person", email: "friend@example.com", you: false } }), true);
if (row.includes("Other Person") || row.includes("friend@example.com") || words(row.split("</td>")[0]) !== "sub-s id sub-s") fail("name and email of a session: " + row);

// the account menu: the user name, the role, and one's own address as dots
state.me = { sub: "sub-n", shown_as: "Juniper", preferred_username: "Juniper", name: "Real Name", email: "owner@example.com", role: "admin", can_control: true, can_manage_users: true, all_sites: true };
buildAcct();
let m = menu.innerHTML;
if (m.includes("Real Name") || m.includes("owner@example.com")) fail("the menu holds the name or the address: " + m);
if (!m.includes('<span class="avatar">J</span>') || !m.includes("<b>Juniper</b>") || !words(m).startsWith("J Juniper admin e-mail •••••• Settings")) fail("the menu: " + words(m));
if (!m.includes('data-veil="me:email" role="menuitemcheckbox" tabindex="-1" aria-checked="false" aria-label="Show the e-mail address of your account"')) fail("the button of one's own address: " + m);
if (menu.attrs["aria-label"] !== "Signed in as Juniper (admin)") fail("the menu's name: " + menu.attrs["aria-label"]);
veil.open["me:email"] = true;
buildAcct();
if (!menu.innerHTML.includes('aria-checked="true" title="Hide the e-mail address of your account">owner@example.com</button>') || menu.innerHTML.includes("Real Name")) fail("the menu after a click: " + menu.innerHTML);
delete veil.open["me:email"];
// an account without a user name: the words the panel uses for it, never the name; a viewer's sites stay
state.me = { sub: "sub-o", shown_as: "(address hidden)", name: "Old Timer", email: "old@example.com", role: "viewer", sites: ["Home"] };
buildAcct();
m = menu.innerHTML;
if (m.includes("Old Timer") || m.includes("old@example.com") || !m.includes("<b>(address hidden)</b>") || !m.includes('<span class="avatar">A</span>') || !words(m).includes("e-mail •••••• Sees: Home")) fail("the menu of an account without a user name: " + words(m));
// an answer of /api/me without shown_as (a panel that is being updated): the user name of the session, else the id
if (meShown({ sub: "s", preferred_username: "nep", name: "Real Name", email: "owner@example.com" }) !== "nep" || meShown({ sub: "s", name: "Real Name", email: "o@example.com" }) !== "s" || meShown(null) !== "?") fail("meShown");
if (!process.exitCode) console.log("ok");
`)
	for _, want := range []string{
		`$("#waiting-who").textContent = meShown(state.me);`,
		`(focus === "last" ? items[items.length - 1] : items.find((x) => !x.dataset.veil) || items[0])?.focus();`,
	} {
		if strings.Count(js, want) != 1 {
			t.Errorf("app.js lacks (or has twice) %q", want)
		}
	}
	// no line of the page prints a person's address or name any more, but through the dots
	// (u.name alone is also a UPS's name and an approver's shown name, so it is not in this list: the Users page's
	// row is run above)
	for _, never := range []string{"esc(u.email", "esc(u?.email", "esc(u?.name", "me.name", "state.me.email", "state.me.name", "esc(e.actor)", "${u.email ||"} {
		if strings.Contains(js, never) {
			t.Errorf("app.js prints a person's address or name: %q", never)
		}
	}
}
