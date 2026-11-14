package web

import (
	"strings"
	"testing"
)

// The Add host dialog lists the hosts that were added in the panel and are not polled right now, each with
// its Re-pair and its Remove, and under them the lines that say why (GET /api/admin/hosts: managed[].active,
// skipped).
//
// Re-pair and Remove were on a host's own page only, and a host the panel left out at its start has no page:
// the one thing the page offered for such a host was to add it again under its name. That Add is refused now
// (it took the host's place and what "Edit host" had stored for it: re-review of Edit host round 2, H1), and its
// sentence says "Re-pair that host, or Remove it first": so the two are here, where the refusal is read.
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
const openEnrollDialog = (r) => opened.push(r), openAddHost = () => {}, closeModal = () => {}, loadManaged = () => {};
const typedDialog = (title, text, name, go, fn) => { dialogs.push([title, name, go]); return fn(name); };
state.managed = Object.create(null);
` + jsFunc(t, js, "addHostWaiting") + handler + `
(async () => {
  const d = { managed: [{ id: "box-a", name: "BOX-A", url: "http://box-a:9101", active: false }, { id: "pc-b", name: "PC-B", active: true },
    { id: "odd", name: "<b>N</b>", active: false }], skipped: ["managed host box-a: tokens unreadable (<x>): Re-pair it"] };
  const html = addHostWaiting(d);
  // the hosts that are not polled, each with its two buttons; the one that is polled is not there
  for (const id of ["box-a", "odd"]) for (const act of ["mh-repair", "mh-remove"])
    if (!html.includes('data-act="' + act + '" data-host="' + id + '"')) fail("no " + act + " for " + id);
  if (html.includes('data-host="pc-b"') || words(html).includes("PC-B")) fail("a host that is polled is listed");
  if (html.includes("<b>N</b>") || html.includes("(<x>)")) fail("a name or a line went into the page as markup");
  if (!words(html).includes("managed host box-a: tokens unreadable (<x>): Re-pair it")) fail("the line that says why is not there: " + words(html));
  if (!words(html).includes("An Add under the same name is refused.")) fail("the box does not say what an Add does: " + words(html));
  // nothing to say: no box. Only a line (an emptied list): the line, and no button
  if (addHostWaiting({ managed: [{ id: "pc-b", name: "PC-B", active: true }] }) !== "" || addHostWaiting({}) !== "") fail("a box with nothing in it");
  const only = addHostWaiting({ managed: [], skipped: ["the list of added hosts is empty"] });
  if (!words(only).includes("the list of added hosts is empty") || only.includes("data-act")) fail("a line alone: " + only);
  // with the kill switch on, the buttons are off and say why
  off = "Control is disabled by an administrator";
  if (!tag(addHostWaiting(d), 'data-act="mh-repair" data-host="box-a"').disabled || !tag(addHostWaiting(d), 'data-act="mh-remove" data-host="box-a"').disabled) fail("the buttons are not off with the kill switch on");
  off = "";

  // Re-pair from the dialog: no host's page is open; the button names its host
  enr.page = d;
  answer = (path) => (path.endsWith("/repair") ? { id: "e1", name: "BOX-A", repair: true } : { ok: true, message: "BOX-A removed" });
  await click(tag(html, 'data-act="mh-repair" data-host="box-a"'), '[data-act="mh-repair"]');
  if (JSON.stringify(asked) !== JSON.stringify([["/api/admin/hosts/box-a/repair", "POST", '{"confirm":"BOX-A"}']])) fail("Re-pair asked " + JSON.stringify(asked));
  if (opened.length !== 1 || opened[0].id !== "e1" || dialogs[0][0] !== "Re-pair BOX-A?" || calls[0].label !== "Re-pair BOX-A") fail("Re-pair: " + JSON.stringify([opened, dialogs, calls]));
  // Remove from the dialog
  asked.length = 0;
  await click(tag(html, 'data-act="mh-remove" data-host="box-a"'), '[data-act="mh-remove"]');
  if (JSON.stringify(asked) !== JSON.stringify([["/api/admin/hosts/box-a", "DELETE", '{"confirm":"BOX-A"}']])) fail("Remove asked " + JSON.stringify(asked));
  if (flashes.at(-1)?.msg !== "BOX-A removed") fail("Remove said " + JSON.stringify(flashes));
  // a button for a host the dialog's data does not have does nothing, and so does one without a host while no
  // host's page is open
  asked.length = 0;
  await click({ dataset: { act: "mh-remove", host: "nobody" }, disabled: false }, '[data-act="mh-remove"]');
  await click({ dataset: { act: "mh-remove" }, disabled: false }, '[data-act="mh-remove"]');
  if (asked.length) fail("a button without its host asked " + JSON.stringify(asked));
  // on a host's page the button is that host's, as before
  here = "pc-b"; state.byId["pc-b"] = { id: "pc-b" }; state.managed["pc-b"] = { id: "pc-b", name: "PC-B" };
  await click({ dataset: { act: "mh-remove" }, disabled: false }, '[data-act="mh-remove"]');
  if (JSON.stringify(asked) !== JSON.stringify([["/api/admin/hosts/pc-b", "DELETE", '{"confirm":"PC-B"}']])) fail("Remove on a host's page asked " + JSON.stringify(asked));
})().catch((e) => fail(String(e && e.stack || e))).finally(() => { if (!process.exitCode) console.log("ok"); });
`
	runPageScript(t, "addhost-waiting", script)
}
