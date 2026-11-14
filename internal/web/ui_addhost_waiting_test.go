package web

import (
	"strings"
	"testing"
)

// The Add host dialog lists the hosts that were added in the panel and are not what the panel polls right now,
// as GET /api/admin/hosts gives them (managed[].active false, with why): each with what is the matter, in the
// panel's words, and with what can be done about it.
//
// A host the panel left out when it started has Re-pair and Remove. Those two were on a host's own page only,
// and such a host has no page: the one thing the page offered for it was to add it again under its name. That
// Add is refused now (it took the host's place and what "Edit host" had stored for it: re-review of Edit host
// round 2, H1), and its sentence says "Re-pair that host, or Remove it first": so the two are here.
//
// A copy that config.yaml's entry of the same id shadows is not such a host: the machine IS polled, as
// config.yaml's entry (check of the build 15 candidate, K1). Its row says so, has no Re-pair, and its Remove
// says, before and after, that only the unused copy goes. The same on the machine's own page.
func TestUIAddHostDialogListsTheHostsThatAreNotPolled(t *testing.T) {
	js := appJS(t)
	if !strings.Contains(jsFunc(t, js, "openAddHost"), "${addHostWaiting(d)}\n    <div class=\"error\" id=\"ah-err\"") {
		t.Fatal("app.js: the Add host dialog does not show the list above its error line any more")
	}
	const head = "document.addEventListener(\"click\", (e) => {\n  const t = e.target.closest('[data-act=\"add-host\"],[data-act=\"mh-repair\"],[data-act=\"mh-remove\"]');"
	from := strings.Index(js, head)
	if from < 0 {
		t.Fatal("app.js: the click handler of Re-pair and Remove is not where it was")
	}
	handler := js[from : from+strings.Index(js[from:], "\n});\n")+5]
	script := pageTools + jsLines(t, js, "esc", "enc") + `
let here = "";
const enr = { id: null, timer: null, page: null }, current = () => ({ id: here }), $ = () => null;
const opened = [], dialogs = [];
let managedLoads = 0;
const openEnrollDialog = (r) => opened.push(r), openAddHost = () => {}, closeModal = () => {}, loadManaged = () => { managedLoads++; };
const typedDialog = (title, text, name, go, fn) => { dialogs.push([title, text, name, go]); return fn(name); };
state.managed = Object.create(null);
state.me.can_manage_users = true;
` + jsFunc(t, js, "addHostWaiting") + jsFunc(t, js, "managedBox") + handler + `
// every button of html: its act, its host, whether it is switched off (the attribute, not a word in a title), its label
const buttons = (html) => [...html.matchAll(/<button\b([^>]*)>([^<]*)<\/button>/g)].map((m) => {
  const attrs = m[1].replace(/title="[^"]*"/g, "");
  return { act: (attrs.match(/data-act="([^"]*)"/) || [])[1], host: unesc((attrs.match(/data-host="([^"]*)"/) || [, ""])[1]), off: /\sdisabled(\s|$)/.test(attrs),
    title: unesc((m[1].match(/title="([^"]*)"/) || [, ""])[1]), label: m[2] };
});
(async () => {
  const d = { managed: [
    { id: "box-a", name: "BOX-A", url: "http://box-a:9101", active: false, shadowed: false, why: "its tokens file agent-box-a.json is not valid JSON" },
    { id: "pc-b", name: "PC-B", active: true, shadowed: false },
    { id: "laptop", name: "LAPTOP", active: false, shadowed: true, why: "config.yaml has an entry with the id laptop, and config.yaml wins: the machine is polled as that entry, and this copy from Add host is not used" },
    { id: 'o"d<d', name: "<b>N</b>", active: false, shadowed: false, why: "<i>why</i>" }],
    skipped: ["managed-hosts.yaml is empty (<x>)"] };
  const html = addHostWaiting(d), bs = buttons(html), text = words(html);
  // a host that was left out: Re-pair and Remove, each with its host; the copy: Remove only; a host that is polled: nothing
  const have = bs.map((b) => b.act + " " + b.host + " " + b.label).join(" | ");
  const want = 'mh-repair box-a Re-pair | mh-remove box-a Remove from the panel | mh-remove laptop Remove the unused copy | mh-repair o"d<d Re-pair | mh-remove o"d<d Remove from the panel';
  if (have !== want) fail("the buttons:\n got  " + have + "\n want " + want);
  if (text.includes("PC-B")) fail("a host that is polled is listed");
  // what is said about each, in the panel's words
  for (const w of ["Added with Add host, and not what the panel polls right now:",
    "BOX-A box-a Re-pair Remove from the panel Not polled right now: its tokens file agent-box-a.json is not valid JSON. Re-pair makes a new join code for the machine; what was changed for it in Edit host is kept. An Add under the same name is refused.",
    "LAPTOP laptop Remove the unused copy Polled, as config.yaml's entry: config.yaml has the id laptop too, and config.yaml wins. The copy made by Add host is not used. Remove takes only that copy away (its entry in the list of added hosts and its tokens): the machine stays, and so does what Edit host and the panel store for it. The copy is not paired again: the panel would refuse what the machine answers.",
    "managed-hosts.yaml is empty (<x>)"])
    if (!text.includes(w)) fail("the box does not say: " + w + "\n it says: " + text);
  if (/LAPTOP[^|]*Not polled right now/.test(text.split("Polled, as config.yaml's entry")[0])) fail("the copy is called not polled: " + text);
  // names, ids, reasons and lines go in as text: nothing of them is markup, and an id is whole in its attribute
  for (const raw of ["<b>N</b>", "<i>why</i>", "(<x>)", 'data-host="o"d']) if (html.includes(raw)) fail("went into the page as it is: " + raw);
  if (!html.includes('data-host="o&quot;d&lt;d"') || !text.includes('<b>N</b> o"d<d') || !text.includes("Not polled right now: <i>why</i>.")) fail("an odd name, id or reason is not shown as text: " + text);
  // nothing to say: no box. Only a line (an emptied list): the line, and no button
  if (addHostWaiting({ managed: [{ id: "pc-b", name: "PC-B", active: true }] }) !== "" || addHostWaiting({}) !== "") fail("a box with nothing in it");
  const only = addHostWaiting({ managed: [], skipped: ["the list of added hosts is empty"] });
  if (!words(only).includes("the list of added hosts is empty") || only.includes("data-act") || words(only).includes("Added with Add host")) fail("a line alone: " + only);
  // with the kill switch on every button of the box is off and says why; without it none is
  if (bs.some((b) => b.off || b.title)) fail("a button is off without a reason: " + JSON.stringify(bs));
  off = 'The kill switch is on (by "<admin>")';
  const offHTML = addHostWaiting(d), offs = buttons(offHTML);
  if (offs.length !== 5 || !offs.every((b) => b.off && b.title === off) || offHTML.includes("<admin>")) fail("with the kill switch on: " + JSON.stringify(offs));
  off = "";

  // Re-pair from the dialog: no host's page is open; the button names its host
  enr.page = d;
  answer = (path) => (path.endsWith("/repair") ? { id: "e1", name: "BOX-A", repair: true } : { ok: true, message: "said by the panel" });
  await click(tag(html, 'data-act="mh-repair" data-host="box-a"'), '[data-act="mh-repair"]');
  if (JSON.stringify(asked) !== JSON.stringify([["/api/admin/hosts/box-a/repair", "POST", '{"confirm":"BOX-A"}']])) fail("Re-pair asked " + JSON.stringify(asked));
  if (opened.length !== 1 || opened[0].id !== "e1" || dialogs[0][0] !== "Re-pair BOX-A?" || calls[0].label !== "Re-pair BOX-A") fail("Re-pair: " + JSON.stringify([opened, dialogs, calls]));
  // Remove from the dialog
  asked.length = 0;
  await click(tag(html, 'data-act="mh-remove" data-host="box-a"'), '[data-act="mh-remove"]');
  if (JSON.stringify(asked) !== JSON.stringify([["/api/admin/hosts/box-a", "DELETE", '{"confirm":"BOX-A"}']])) fail("Remove asked " + JSON.stringify(asked));
  if (flashes.at(-1)?.msg !== "said by the panel" || dialogs.at(-1)[0] !== "Remove BOX-A from the panel?" || location.hash !== "#/") fail("Remove: " + JSON.stringify([flashes, dialogs.at(-1), location.hash]));
  // a host with an odd id is asked for by that id, whole
  asked.length = 0;
  await click(tag(html, 'data-act="mh-remove" data-host="o&quot;d&lt;d"'), '[data-act="mh-remove"]');
  if (JSON.stringify(asked) !== JSON.stringify([["/api/admin/hosts/o%22d%3Cd", "DELETE", '{"confirm":"<b>N</b>"}']])) fail("Remove of the odd id asked " + JSON.stringify(asked));

  // the copy: its Remove says what goes and what stays, before and after, and leaves the page where it is
  asked.length = 0; location.hash = "#/system/laptop"; const loads = managedLoads;
  await click(tag(html, 'data-act="mh-remove" data-host="laptop"'), '[data-act="mh-remove"]');
  const dl = dialogs.at(-1);
  if (JSON.stringify(asked) !== JSON.stringify([["/api/admin/hosts/laptop", "DELETE", '{"confirm":"LAPTOP"}']])) fail("Remove of the copy asked " + JSON.stringify(asked));
  if (dl[0] !== "Remove the unused Add host copy of LAPTOP?" || dl[3] !== "Remove the copy" ||
    dl[1] !== "Only the copy goes: its entry in the panel's list of added hosts and its tokens. LAPTOP stays in the panel as config.yaml's entry, and nothing that is stored for it is changed.")
    fail("the confirmation of the copy's Remove: " + JSON.stringify(dl));
  if (calls.at(-1).label !== "Remove the unused Add host copy of LAPTOP" || flashes.at(-1)?.msg !== "said by the panel" || location.hash !== "#/system/laptop" || managedLoads !== loads + 1)
    fail("after the copy's Remove: " + JSON.stringify([calls.at(-1), flashes.at(-1), location.hash, managedLoads - loads]));
  // and a Re-pair button for the copy, which the page never makes, does nothing
  asked.length = 0; const nd = dialogs.length;
  await click({ dataset: { act: "mh-repair", host: "laptop" }, disabled: false }, '[data-act="mh-repair"]');
  if (asked.length || dialogs.length !== nd) fail("a Re-pair of the copy was started: " + JSON.stringify(asked));

  // a button for a host the dialog's data does not have does nothing, and so does one without a host while no
  // host's page is open; a button that is switched off does nothing either
  await click({ dataset: { act: "mh-remove", host: "nobody" }, disabled: false }, '[data-act="mh-remove"]');
  await click({ dataset: { act: "mh-remove" }, disabled: false }, '[data-act="mh-remove"]');
  await click({ dataset: { act: "mh-remove", host: "box-a" }, disabled: true }, '[data-act="mh-remove"]');
  if (asked.length || dialogs.length !== nd) fail("a button without its host asked " + JSON.stringify(asked));
  // on a host's page the button is that host's, as before; and one that names a host the dialog does not have
  // does nothing there either (it is not the page's host that goes)
  here = "pc-b"; state.byId["pc-b"] = { id: "pc-b" }; state.managed["pc-b"] = { id: "pc-b", name: "PC-B", active: true };
  await click({ dataset: { act: "mh-remove", host: "nobody" }, disabled: false }, '[data-act="mh-remove"]');
  if (asked.length || dialogs.length !== nd) fail("a button that names an unknown host removed the page's host: " + JSON.stringify(asked));
  await click({ dataset: { act: "mh-remove" }, disabled: false }, '[data-act="mh-remove"]');
  if (JSON.stringify(asked) !== JSON.stringify([["/api/admin/hosts/pc-b", "DELETE", '{"confirm":"PC-B"}']])) fail("Remove on a host's page asked " + JSON.stringify(asked));

  // the machine's own page: a host that Add host made has Re-pair and Remove; the copy has its Remove only, and says what it is
  const own = managedBox({ id: "pc-b" });
  if (buttons(own).map((b) => b.act).join(",") !== "mh-repair,mh-remove" || !words(own).includes("Added with Add host")) fail("the box of an added host: " + words(own));
  state.byId.laptop = { id: "laptop" }; state.managed.laptop = d.managed[2];
  const copy = managedBox({ id: "laptop" }), cb = buttons(copy);
  if (cb.length !== 1 || cb[0].act !== "mh-remove" || cb[0].host !== "" || cb[0].label !== "Remove the unused copy" || cb[0].off) fail("the box of the copy: " + JSON.stringify(cb));
  if (words(copy) !== "An unused copy from Add host This machine is polled as config.yaml's entry. The list of hosts added with Add host has one with the same id ( laptop ) too: config.yaml wins, and that copy is not used. Remove takes only the copy away (its entry in that list and its tokens); nothing that is stored for this machine is changed. Remove the unused copy")
    fail("the box of the copy says: " + words(copy));
  off = "The kill switch is on";
  if (!buttons(managedBox({ id: "laptop" }))[0].off) fail("the copy's Remove is not off with the kill switch on");
  off = "";
  // its Remove on the machine's page is the copy's Remove
  here = "laptop"; asked.length = 0; location.hash = "#/system/laptop";
  await click({ dataset: { act: "mh-remove" }, disabled: false }, '[data-act="mh-remove"]');
  if (JSON.stringify(asked) !== JSON.stringify([["/api/admin/hosts/laptop", "DELETE", '{"confirm":"LAPTOP"}']]) || dialogs.at(-1)[3] !== "Remove the copy" || location.hash !== "#/system/laptop")
    fail("the copy's Remove on the machine's page: " + JSON.stringify([asked, dialogs.at(-1), location.hash]));
})().catch((e) => fail(String(e && e.stack || e))).finally(() => { if (!process.exitCode) console.log("ok"); });
`
	runPageScript(t, "addhost-waiting", script)
}
