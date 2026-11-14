package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
)

// "Edit host" on the page: an entry in the ⋯ menu of a host's page (admins), and a dialog with three short
// sections. The dialog's own functions are run in node (nothing of them is copied here); what is around them
// in the script is not the code under test.

// ehPageScript is the "Edit host" part of app.js with what it needs around it. The page's `$` is a table of
// stand-in elements (one per selector, made when first asked for), api answers with what the test set, and
// the timers are kept in a list instead of running.
func ehPageScript(t *testing.T, js, body string) string {
	t.Helper()
	var parts []string
	for _, re := range []string{`(?m)^const dict = .*$`, `(?m)^const esc = .*$`, `(?m)^const enc = .*$`, `(?m)^const isHost = .*$`, `(?m)^const infoList = .*$`} {
		m := regexp.MustCompile(re).FindString(js)
		if m == "" {
			t.Fatalf("app.js has no %s any more", re)
		}
		parts = append(parts, m)
	}
	from := strings.Index(js, "// ------------------------------------------------------------ Edit host (admins, the ⋯ menu of a host's page)")
	to := strings.Index(js, "// ------------------------------------------------------------ elevated helper Stop / Start")
	if from < 0 || to < from {
		t.Fatal("app.js has no \"Edit host\" part any more")
	}
	return `"use strict";
const listeners = [];
const document = { addEventListener(type, fn) { listeners.push(type); }, querySelector: () => null };
const state = { me: { can_manage_users: true }, byId: {}, menu: "poll" };
const current = () => ({ id: "h1" });
let renders = 0;
const render = () => { renders++; };
const flashes = [];
const flash = (msg, bad = false) => { flashes.push({ msg, bad }); };
let els = {}, detached = false; // detached: the dialog was closed, what it held is no longer on the page
const $ = (sel) => (els[sel] ||= { innerHTML: "", textContent: "", hidden: true, disabled: false, isConnected: !detached, focus() {} });
let closed = 0;
const closeModal = () => { closed++; detached = true; for (const k of Object.keys(els)) els[k].isConnected = false; };
const infos = {};
const infoBtn = (key, about, html) => { infos[key] = { about, html }; return "[info " + key + "]"; };
const asked = [];
let answer = async () => ({});
const api = (path, opts = {}) => { asked.push({ path, method: opts.method || "GET", body: opts.body ? JSON.parse(opts.body) : null, signal: opts.signal }); return answer(path, opts); };
let confirming = null;
const controlCall = async (pending, fn) => { try { return await fn(); } catch (e) { if (e.stepUp) { confirming = pending; return null; } throw e; } };
const timers = [];
const setTimeout = (fn, ms) => { timers.push({ fn, ms, on: true }); return timers.length; };
const clearTimeout = (n) => { if (timers[n - 1]) timers[n - 1].on = false; };
` + strings.Join(parts, "\n") + "\n" + js[from:to] + `
const fail = (m) => { console.log("FAIL " + m); process.exitCode = 1; };
const words = (t) => t.replace(/<[^>]+>/g, " ").replace(/&#39;/g, "'").replace(/&quot;/g, '"').replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&amp;/g, "&").replace(/\s+/g, " ").trim();
const settle = () => new Promise((r) => setImmediate(r));
const ups = (id, o) => ({ id: "nas-agent/" + id, name: id.toUpperCase() + " UPS", agent: "nas-agent", agent_name: "NAS", on: false, from: "config", by: null, at: null, config_on: false, ...o });
const plug = (id, o) => ({ id: "nas-agent/" + id, name: id + " plug", agent: "nas-agent", agent_name: "NAS", linked: false, self: false, other: null, from: "config", by: null, at: null,
  config_linked: false, config_other: null, in_total: true, total_from: "config", total_by: null, total_at: null, config_in_total: true, mine: false, ...o });
const data = (o) => ({ id: "h1", name: "PC-B", agent: "pc-b", rev: 3, ups: [], plugs: [], ignored: [], leftovers: [], leftover_count: 0, ...o });
(async () => {
` + body + `
  if (!process.exitCode) console.log("ok");
})();
`
}

// runEhPageScript runs body in node and returns what it printed after "ok" lines are taken out; args are
// handed to the script (process.argv[2] ...).
func runEhPageScript(t *testing.T, name, body string, args ...string) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the page's own functions are not run here")
	}
	file := filepath.Join(t.TempDir(), name+".js")
	if err := os.WriteFile(file, []byte(ehPageScript(t, appJS(t), body)), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, append([]string{file}, args...)...).CombinedOutput()
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if err != nil || lines[len(lines)-1] != "ok" || strings.Contains(string(out), "FAIL ") {
		t.Fatalf("node: %v\n%s", err, out)
	}
	return strings.Join(lines[:len(lines)-1], "\n")
}

// Where it is on the page and how it is written: the menu entry, for admins and for hosts; a dialog whose
// markup holds no value of the panel (the host's name is set as text); every name in the sections through
// esc(); the styles in the stylesheet's own colours.
func TestUIEditHostIsInTheMenuOfAHostForAdmins(t *testing.T) {
	js, css := appJS(t), appCSS(t)
	for _, want := range []string{
		`${modelNameMenu(s)}${editHostMenu(s)}${wakeUpdateMenu(s)}</div>` + "`;", // in the ⋯ menu, under "Set model name"
		"function editHostMenu(s) {\n  if (!isHost(s) || !state.me?.can_manage_users) return \"\";\n  return `<button class=\"menu-item\" type=\"button\" data-act=\"edit-host\">Edit host</button>`;\n}",
		"$(\"#eh-t\").textContent = `Edit ${s.name}`;", // the title: the host's name as text
		`const t = e.target.closest('[data-act="edit-host"]');`,
		"const path = `/api/admin/hosts/${enc(s.id)}/options`;",
		"controlCall({ kind: \"request\", label: `Edit host ${s.name}`, system: s.id, method: \"PUT\", path, body: plan.body },", // "Confirm it's you" when the window has run out
		`const stop = new AbortController(), timer = setTimeout(() => stop.abort(), 15000);`,                                     // no "Loading…" for ever
		// the sections are drawn anew after a tick: the tick that was used keeps the focus (found again by section and device)
		`ticks.find((x) => x.dataset.eh === had.eh && x.value === had.value))?.focus();`,
	} {
		if strings.Count(js, want) != 1 {
			t.Errorf("app.js lacks (or has twice) %q", want)
		}
	}
	from := strings.Index(js, "// ------------------------------------------------------------ Edit host (admins")
	to := strings.Index(js, "// ------------------------------------------------------------ elevated helper Stop / Start")
	if from < 0 || to < from {
		t.Fatal("app.js has no \"Edit host\" part")
	}
	part := js[from:to]
	// the dialog's frame is fixed text: the info button is the one thing put into it
	i, j := strings.Index(part, "m.innerHTML = `"), strings.Index(part, "</form>`;")
	if i < 0 || j < i {
		t.Fatal("the dialog's frame is not where this test looks for it")
	}
	if frame := strings.ReplaceAll(part[i:j], "${EH_INFO()}", ""); strings.Contains(frame, "${") {
		t.Errorf("the dialog's frame holds an inserted value: %s", frame)
	}
	// markup is written in three places: the frame, the sections (ehView, through esc), and the emptied body when loading failed
	if n := strings.Count(part, "innerHTML"); n != 3 || strings.Contains(part, "insertAdjacentHTML") || strings.Contains(part, "outerHTML") {
		t.Errorf("the Edit host part writes markup in %d places, want 3", n)
	}
	// every value of the panel that a template of the part inserts goes through esc(): no name, id or line as it is
	view := part[strings.Index(part, "function ehView"):strings.Index(part, "function ehTick")]
	view = regexp.MustCompile("esc\\(`[^`]*`\\)").ReplaceAllString(view, "esc()") // a sentence made of names, escaped as a whole
	if m := regexp.MustCompile(`\$\{\s*((x|u|p|d|s|e|r)\.(name|agent_name|agent|id|other|by|total_by|message)|l|line|by)\s*\}`).FindAllString(view, -1); len(m) != 0 {
		t.Errorf("ehView inserts a value without esc(): %v", m)
	}
	// the errors and the result are set as text
	for _, want := range []string{`$("#eh-err").textContent = msg;`, "close(); flash(`${s.name}: ${r.message}`);"} {
		if !strings.Contains(part, want) {
			t.Errorf("the Edit host part lacks %q", want)
		}
	}
	at := strings.Index(css, `/* "Edit host": one line per UPS or plug`)
	end := strings.Index(css[max(at, 0):], "\n.modal-box .eh-shadow")
	if at < 0 || end < 0 {
		t.Fatal("app.css has no block for the Edit host dialog")
	}
	block := css[at : at+end+80]
	rest := css[:at] + css[at+end+80:]
	for _, c := range regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|rgba?\([^)]*\)|var\(--[a-z0-9-]+\)`).FindAllString(block, -1) {
		if !strings.Contains(rest, c) {
			t.Errorf("the block of the Edit host dialog brings a colour the stylesheet did not have: %s", c)
		}
	}
}

// The sections, as the page's own functions draw them: what counts and where it comes from ("from
// config.yaml" / "changed here" with Reset), who reads each device, what a tick and a Reset change in the
// dialog, the list of what Save will change, and the request that says exactly that. A viewer has no entry.
func TestUIEditHostDialogSections(t *testing.T) {
	runEhPageScript(t, "eh-view", `
  // the menu entry: admins, hosts
  const host = { id: "h1", name: "PC-B", kind: "agent" };
  if (editHostMenu(host) !== '<button class="menu-item" type="button" data-act="edit-host">Edit host</button>') fail("the menu entry: " + editHostMenu(host));
  if (editHostMenu({ ...host, kind: "node" }) === "") fail("a node has no entry");
  if (editHostMenu({ ...host, kind: "qemu" }) !== "" || editHostMenu({ ...host, kind: "lxc" }) !== "") fail("a guest has an entry");
  state.me = { can_manage_users: false, can_control: true };
  if (editHostMenu(host) !== "") fail("someone who is no admin has the entry");
  state.me = null;
  if (editHostMenu(host) !== "") fail("before /api/me answered there is an entry");
  state.me = { can_manage_users: true };

  const d = data({
    ups: [ups("rack", { on: true, from: "panel", by: "nora@example.com", config_on: false }), ups("desk", { from: "panel", by: "nora@example.com", config_on: true }),
      { ...ups("attic"), agent: "pc-b", agent_name: "PC-B" }],
    plugs: [plug("a", { linked: true, mine: true, config_linked: true }), plug("b", { other: "PC-A", config_other: "PC-A" }), plug("own", { agent: "pc-b", agent_name: "PC-B", linked: true, self: true, mine: true, config_linked: true }),
      plug("corner", { agent: "pc-b", agent_name: "PC-B", mine: true, in_total: false, total_from: "panel", total_by: "bea", config_in_total: true })] });
  let draft = ehDraft();
  let html = ehView(d, draft), text = words(html);
  // three sections with one short line each, in this order
  const at = ["UPS Ticked: this machine is on that UPS.", "Plugs Ticked: the plug's watts are this machine's.", "Total Ticked: the plug counts in the total."].map((x) => text.indexOf(x));
  if (at.some((i) => i < 0) || at[0] > at[1] || at[1] > at[2]) fail("the three sections: " + text);
  // every line: the tick, the name, who reads it, where its value comes from
  const rows = [...html.matchAll(/<div class="eh-row">(.*?)<\/div>/gs)].map((m) => ({ html: m[1], text: words(m[1]) }));
  const want = ["RACK UPS read by NAS changed here Reset", "DESK UPS read by NAS changed here Reset", "ATTIC UPS read by PC-B from config.yaml",
    "a plug read by NAS from config.yaml", "b plug read by NAS · now on PC-A from config.yaml", "own plug read by PC-B · set on the machine itself", "corner plug read by PC-B from config.yaml",
    "a plug read by NAS from config.yaml", "own plug read by PC-B from config.yaml", "corner plug read by PC-B changed here Reset"];
  if (JSON.stringify(rows.map((r) => r.text)) !== JSON.stringify(want)) fail("the lines:\n" + rows.map((r) => r.text).join("\n"));
  const ticked = rows.map((r) => / checked/.test(r.html));
  if (JSON.stringify(ticked) !== JSON.stringify([true, false, false, true, false, true, false, true, true, false])) fail("the ticks: " + JSON.stringify(ticked));
  // the plug the machine itself links cannot be unticked here; nothing else is disabled
  if (rows.filter((r) => / disabled/.test(r.html)).map((r) => r.text).join("|") !== "own plug read by PC-B · set on the machine itself") fail("disabled: " + rows.filter((r) => / disabled/.test(r.html)).map((r) => r.text));
  // Reset is about its one line and says so; who last changed something that machine reads is the source's tooltip
  if (!html.includes('data-eh-reset="ups:nas-agent/rack" title="Back to config.yaml for this line"') || !html.includes('data-eh-reset="ups:nas-agent/desk" title="Back to config.yaml for this line"')
    || !html.includes('<span class="eh-src on" title="Changed here. Last change to what NAS reads: nora@example.com">changed here</span>')
    || !html.includes('data-eh-reset="total:nas-agent/corner" title="Back to config.yaml for this line"') || (html.match(/data-eh-reset=/g) || []).length !== 3) fail("Reset and its words: " + html);
  // changed here shadows config.yaml: said in one visible line; nothing to save yet
  if (!text.includes("Changed here: used instead of config.yaml until Reset.") || !html.includes('class="note info-keep eh-shadow"')) fail("the line about config.yaml: " + text);
  if (text.includes("This will change") || ehPlan(d, draft).lines.length || JSON.stringify(ehPlan(d, draft).body) !== "{}") fail("nothing was changed, and the dialog lists changes: " + text);
  // nothing overridden: no such line
  if (words(ehView(data({ ups: [ups("rack")], plugs: [plug("a")] }), ehDraft())).includes("Changed here")) fail("the line about config.yaml without any override");

  // ticks: a UPS on, a plug moved here (its total row appears), another taken out of the total
  ehTick(d, draft, "ups", "nas-agent/desk", true);
  ehTick(d, draft, "plugs", "nas-agent/b", true);
  ehTick(d, draft, "total", "nas-agent/a", false);
  let plan = ehPlan(d, draft);
  if (JSON.stringify(plan.lines) !== JSON.stringify(["UPS DESK UPS (read by NAS): powers PC-B", "Plug a plug (read by NAS): not in the total", "Plug b plug (read by NAS): linked to PC-B (was on PC-A)"])) fail("lines: " + JSON.stringify(plan.lines));
  if (JSON.stringify(plan.body) !== JSON.stringify({ ups: [{ id: "nas-agent/desk", on: true }], plugs: [{ id: "nas-agent/a", in_total: false }, { id: "nas-agent/b", linked: true }] })) fail("body: " + JSON.stringify(plan.body));
  html = ehView(d, draft); text = words(html);
  if (!text.includes("This will change UPS DESK UPS (read by NAS): powers PC-B Plug a plug (read by NAS): not in the total Plug b plug (read by NAS): linked to PC-B (was on PC-A)")) fail("the list of changes: " + text);
  if (!/b plug read by NAS changed here(?! Reset)/.test(text)) fail("a line that was ticked in this dialog says where it will come from, without a Reset: " + text);
  // a tick that says what config.yaml says is no change of the panel's: the desk UPS (changed here to off, config.yaml: on) follows config.yaml again
  if (!/DESK UPS read by NAS from config.yaml Reset/.test(text)) fail("a tick back to config.yaml's value: " + text);
  if ((text.match(/b plug read by NAS/g) || []).length !== 2) fail("the plug that is linked here now has a line under Total: " + text);
  // a tick put back is no change
  ehTick(d, draft, "ups", "nas-agent/desk", false);
  ehTick(d, draft, "plugs", "nas-agent/b", false);
  ehTick(d, draft, "total", "nas-agent/a", true);
  if (ehPlan(d, draft).lines.length || Object.keys(draft.ups).length + Object.keys(draft.linked).length + Object.keys(draft.total).length) fail("ticks put back are still changes: " + JSON.stringify(draft));
  // the total of a plug that is no longer this machine's is not sent
  ehTick(d, draft, "total", "nas-agent/a", false);
  ehTick(d, draft, "plugs", "nas-agent/a", false);
  plan = ehPlan(d, draft);
  if (JSON.stringify(plan.body) !== JSON.stringify({ plugs: [{ id: "nas-agent/a", linked: false }] }) || JSON.stringify(plan.lines) !== JSON.stringify(["Plug a plug (read by NAS): no longer linked to PC-B"])) fail("unlinked and its total: " + JSON.stringify(plan));

  // Reset: that one line shows what config.yaml says, the tick made on it before goes, and Save says what it does
  draft = ehDraft();
  ehTick(d, draft, "ups", "nas-agent/rack", false);
  ehReset(d, draft, "ups:nas-agent/rack");
  plan = ehPlan(d, draft);
  if (JSON.stringify(plan.lines) !== JSON.stringify(["UPS RACK UPS (read by NAS): back to config.yaml: no longer powers PC-B"]) || JSON.stringify(plan.body) !== JSON.stringify({ reset: [{ id: "nas-agent/rack", field: "ups" }] })) fail("Reset: " + JSON.stringify(plan));
  html = ehView(d, draft); text = words(html);
  const after = [...html.matchAll(/<div class="eh-row">(.*?)<\/div>/gs)].slice(0, 3).map((m) => words(m[1]) + (/ checked/.test(m[1]) ? " [x]" : " [ ]"));
  // the other line that was changed here (the desk UPS) is not touched by it
  if (JSON.stringify(after) !== JSON.stringify(["RACK UPS read by NAS from config.yaml [ ]", "DESK UPS read by NAS changed here Reset [ ]", "ATTIC UPS read by PC-B from config.yaml [ ]"])) fail("after Reset: " + JSON.stringify(after));
  // a second Reset, of the desk UPS: config.yaml has it on
  ehReset(d, draft, "ups:nas-agent/desk");
  plan = ehPlan(d, draft);
  if (JSON.stringify(plan.lines) !== JSON.stringify(["UPS RACK UPS (read by NAS): back to config.yaml: no longer powers PC-B", "UPS DESK UPS (read by NAS): back to config.yaml: powers PC-B"])
    || JSON.stringify(plan.body) !== JSON.stringify({ reset: [{ id: "nas-agent/rack", field: "ups" }, { id: "nas-agent/desk", field: "ups" }] })) fail("two Resets: " + JSON.stringify(plan));
  if (!/DESK UPS read by NAS from config.yaml(?! Reset)/.test(words(ehView(d, draft))) || !/value="nas-agent\/desk" checked/.test(ehView(d, draft))) fail("the desk UPS after its Reset: " + words(ehView(d, draft)));
  // a tick on a line after its Reset is the tick alone: the later of the two counts, and only one of them is sent
  ehTick(d, draft, "ups", "nas-agent/desk", true);
  plan = ehPlan(d, draft);
  if (JSON.stringify(plan.body) !== JSON.stringify({ ups: [{ id: "nas-agent/desk", on: true }], reset: [{ id: "nas-agent/rack", field: "ups" }] })
    || JSON.stringify(plan.lines) !== JSON.stringify(["UPS RACK UPS (read by NAS): back to config.yaml: no longer powers PC-B", "UPS DESK UPS (read by NAS): powers PC-B"])) fail("Reset and then a tick: " + JSON.stringify(plan));
  ehTick(d, draft, "ups", "nas-agent/desk", false);  // back to what the panel has: no change of that line
  if (JSON.stringify(ehPlan(d, draft).body) !== JSON.stringify({ reset: [{ id: "nas-agent/rack", field: "ups" }] })) fail("a tick back after a Reset: " + JSON.stringify(ehPlan(d, draft)));
  // the total of one plug
  ehReset(d, draft, "total:nas-agent/corner");
  if (JSON.stringify(ehPlan(d, draft).lines) !== JSON.stringify(["UPS RACK UPS (read by NAS): back to config.yaml: no longer powers PC-B", "Plug corner plug (read by PC-B): back to config.yaml: in the total again"])) fail("two resets: " + JSON.stringify(ehPlan(d, draft).lines));
  // a plug: Reset says where the plug goes or comes from; a change that says what config.yaml says by now changes nothing
  const moved = data({ plugs: [plug("here", { linked: true, mine: true, from: "panel", config_other: "PC-A" }), plug("away", { other: "PC-C", from: "panel", config_linked: true }),
    plug("same", { linked: true, mine: true, from: "panel", config_linked: true, in_total: false, total_from: "panel", config_in_total: false })] });
  draft = ehDraft();
  for (const k of ["plugs:nas-agent/here", "plugs:nas-agent/away", "plugs:nas-agent/same", "total:nas-agent/same", "plugs:nas-agent/nope", "nonsense"]) ehReset(moved, draft, k);
  plan = ehPlan(moved, draft);
  if (JSON.stringify(plan.lines) !== JSON.stringify(["Plug here plug (read by NAS): back to config.yaml: no longer linked to PC-B (on PC-A again)", "Plug away plug (read by NAS): back to config.yaml: linked to PC-B (was on PC-C)",
    "Plug same plug (read by NAS): back to config.yaml: nothing changes now", "Plug same plug (read by NAS): back to config.yaml: nothing changes now"])) fail("plug resets: " + JSON.stringify(plan.lines));
  if (JSON.stringify(plan.body) !== JSON.stringify({ reset: [{ id: "nas-agent/here", field: "plugs" }, { id: "nas-agent/away", field: "plugs" }, { id: "nas-agent/same", field: "plugs" }, { id: "nas-agent/same", field: "total" }] })) fail("plug resets, the request: " + JSON.stringify(plan.body));
  // after the Reset the plug that goes back is shown where config.yaml has it
  if (!words(ehView(moved, draft)).includes("here plug read by NAS · now on PC-A from config.yaml")) fail("a plug after its Reset: " + words(ehView(moved, draft)));

  // nothing to choose from, and what the panel ignores
  text = words(ehView(data({ ignored: ["x"] }), ehDraft()));
  if (!text.includes("UPS No agent relays a UPS. Plugs No agent relays a plug. Total No plug of this machine.") || !text.includes("1 stored change is ignored: see the panel's log.")) fail("empty: " + text);
  if (!words(ehView(data({ ignored: ["x", "y"] }), ehDraft())).includes("2 stored changes are ignored: see the panel's log.")) fail("two ignored");
  // the panel names the first twenty and says how many there are: the dialog says the number of all
  if (!words(ehView(data({ ignored: Array(21).fill("x"), ignored_count: 500 }), ehDraft())).includes("500 stored changes are ignored: see the panel's log.")) fail("five hundred ignored");
  // stored changes that have no line in any dialog: how many, and Remove; pressed, Save's list says what goes
  const lo = data({ ups: [ups("rack")], leftovers: ["2 stored changes about the plug b, which NAS does not relay any more", "1 stored change kept with old-pc, which is no machine of this panel"], leftover_count: 3 });
  draft = ehDraft();
  html = ehView(lo, draft); text = words(html);
  if (!text.includes("3 stored changes are about machines or devices that are gone. Remove") || !html.includes('<button class="btn sm" type="button" data-eh-left>Remove</button>') || text.includes("This will change")) fail("leftovers: " + html);
  if (JSON.stringify(ehPlan(lo, draft)) !== JSON.stringify({ lines: [], body: {}, notes: [] })) fail("leftovers are removed without being asked: " + JSON.stringify(ehPlan(lo, draft)));
  draft.leftovers = true;
  ehTick(lo, draft, "ups", "nas-agent/rack", true);
  plan = ehPlan(lo, draft);
  if (JSON.stringify(plan.lines) !== JSON.stringify(["Removed: 2 stored changes about the plug b, which NAS does not relay any more", "Removed: 1 stored change kept with old-pc, which is no machine of this panel", "UPS RACK UPS (read by NAS): powers PC-B"])
    || JSON.stringify(plan.body) !== JSON.stringify({ ups: [{ id: "nas-agent/rack", on: true }], leftovers: true })) fail("leftovers, the plan: " + JSON.stringify(plan));
  html = ehView(lo, draft); text = words(html);
  if (!text.includes("3 stored changes are about machines or devices that are gone. Save removes them.") || html.includes("data-eh-left") || !text.includes("This will change Removed: 2 stored changes about the plug b")) fail("leftovers after Remove: " + text);
  if (!words(ehView(data({ leftovers: ["x"], leftover_count: 1 }), ehDraft())).includes("1 stored change is about machines or devices that are gone. Remove")) fail("one leftover");
  if (words(ehView(data({}), ehDraft())).includes("gone")) fail("no leftovers, and the dialog speaks of them");
  draft = ehDraft(); draft.leftovers = true;  // nothing to remove: nothing is asked
  if (JSON.stringify(ehPlan(data({}), draft).body) !== "{}") fail("leftovers asked for with none there");
  // who will see a device: one note for each site whose viewers get to see it or lose it, under the lines of "This will change"
  const vd = data({ site: "Home",
    ups: [ups("rack", { sees: ["Away"], sees_on: ["Away", "Home"], sees_off: ["Away"] }), ups("desk", { on: true, from: "panel", sees: ["Home"], sees_on: ["Home"], sees_off: ["Away"] }),
      ups("attic", { on: true, from: "panel", config_on: true, sees: ["Home", "Lab"], sees_on: ["Home", "Lab"], sees_off: ["Lab"] })],
    plugs: [plug("b", { sees: ["Away"], sees_linked: ["Home"], sees_unlinked: ["Away"], sees_config: ["Away"] }),
      plug("c", { linked: true, mine: true, from: "panel", config_other: "PC-A", sees: ["Home"], sees_linked: ["Home"], sees_unlinked: ["Away"], sees_config: ["Lab"] }),
      plug("d", { linked: true, mine: true, sees: [], sees_linked: [], sees_unlinked: [], sees_config: [] })] });
  draft = ehDraft();
  if (ehPlan(vd, draft).notes.length) fail("notes without a change: " + JSON.stringify(ehPlan(vd, draft).notes));
  ehTick(vd, draft, "ups", "nas-agent/rack", true);     // Home's viewers get to see it
  ehTick(vd, draft, "ups", "nas-agent/desk", false);    // it goes back to the reading machine's site
  ehReset(vd, draft, "ups:nas-agent/attic");            // config.yaml says the same: nothing moves
  ehTick(vd, draft, "plugs", "nas-agent/b", true);
  ehReset(vd, draft, "plugs:nas-agent/c");              // back to PC-A, of the site Lab
  ehTick(vd, draft, "plugs", "nas-agent/d", false);     // nobody but admins saw it, nobody will
  ehTick(vd, draft, "total", "nas-agent/b", false);     // the total moves nobody's view
  plan = ehPlan(vd, draft);
  const wantNotes = ["Viewers of Home will see UPS RACK UPS", "Viewers of Away will see UPS DESK UPS", "Viewers of Home will no longer see UPS DESK UPS",
    "Viewers of Home will see plug b plug", "Viewers of Away will no longer see plug b plug", "Viewers of Lab will see plug c plug", "Viewers of Home will no longer see plug c plug"];
  if (JSON.stringify(plan.notes) !== JSON.stringify(wantNotes)) fail("the notes: " + JSON.stringify(plan.notes));
  html = ehView(vd, draft);
  if ((html.match(/<li class="eh-sees">/g) || []).length !== 7 || !html.includes('<li class="eh-sees">Viewers of Home will see UPS RACK UPS</li>')
    || html.indexOf('<li class="eh-sees">') < html.lastIndexOf("<li>")) fail("the notes in the dialog, after the lines: " + html.slice(html.indexOf("This will change")));
  if (JSON.stringify(Object.keys(plan.body)) !== JSON.stringify(["ups", "plugs", "reset"])) fail("the notes are part of the request: " + JSON.stringify(plan.body));
  // an answer of an older panel (no "sees"): no notes, and nothing breaks
  if (ehPlan(data({ ups: [ups("rack")] }), (() => { const x = ehDraft(); x.ups["nas-agent/rack"] = true; return x; })()).notes.length) fail("notes from nothing");
  // a machine without an agent of its own: only the plugs linked to it are under Total
  text = words(ehView(data({ agent: null, plugs: [plug("a", { agent: "", agent_name: "?" }), plug("b", { linked: true })] }), ehDraft()));
  if ((text.match(/a plug/g) || []).length !== 1 || (text.match(/b plug/g) || []).length !== 2) fail("a node without an agent: " + text);

  // names are the panel's data: every one of them is text in the dialog, whatever it holds
  const evil = '<img src=x onerror=alert(1)>"\'&';
  const bad = data({ name: evil, ups: [ups("rack", { id: evil, name: evil, agent: evil, agent_name: evil, from: "panel", by: evil })],
    plugs: [plug("b", { id: evil + "2", name: evil, agent: evil, agent_name: evil, other: evil, total_by: evil, total_from: "panel", linked: true, from: "panel", by: evil })] });
  const bd = ehDraft();
  ehTick(bad, bd, "ups", evil, true);
  const out = ehView(bad, bd), plain = ehView(JSON.parse(JSON.stringify(bad).split(JSON.stringify(evil).slice(1, -1)).join("name")), ehDraft());
  if (out.includes("<img") || out.includes('onerror=alert(1)>"') || /="[^"]*<|'&"/.test(out.replace(/&[a-z#0-9]+;/g, ""))) fail("a name is markup: " + out);
  if ((out.match(/<[a-z]/g) || []).length < (plain.match(/<[a-z]/g) || []).length) fail("the markup broke");
  if (!words(out).includes("UPS " + evil + " (read by " + evil + "): powers " + evil)) fail("the name is not shown as it is: " + words(out));
  // what is behind the info button of the title
  EH_INFO();
  const info = words(infos["edit-host"].html);
  for (const s of ["What you change here is kept by the panel. config.yaml is not changed and stays the base.",
    "A changed line is used instead of what config.yaml says about it until Reset, also when config.yaml is edited later. Every other line follows config.yaml.",
    "A link is kept with the machine that reads the device (\"read by …\"). Reset puts back that one line.",
    "A change counts from the next poll of the machine that reads the device.",
    "A link also decides who can see a UPS or plug: the viewers of the site of a machine it is linked to or, linked to nothing, of the machine that reads it. Before you save, the list says when a change shows a device to another site's viewers or hides it from them.",
    "Stored changes about a device nobody relays any more, or about a machine that is gone, have no line here. The dialog says how many there are; Remove takes them out.",
    "Set on the machine or in config.yaml: address, tokens, power control, updates, Wake-on-LAN."]) if (!info.includes(s)) fail("the info button lacks: " + s);
`)
}

// The dialog from "Edit host" to Save: it loads, draws, sends only what was changed, says the result, shows
// the panel's sentence when it refuses, hands over to "Confirm it's you" when the window has run out, and
// never waits for ever.
func TestUIEditHostDialogFlow(t *testing.T) {
	runEhPageScript(t, "eh-flow", `
  const s = { id: "h 1", name: "PC-B", kind: "agent" };
  const d = () => data({ ups: [ups("rack")], plugs: [plug("a", { linked: true, mine: true })] });
  const open = async (ans) => { els = {}; detached = false; asked.length = 0; flashes.length = 0; closed = 0; confirming = null; timers.length = 0; answer = ans; const p = openEditHost(s); await settle(); return p; };
  const tick = (field, id, checked) => $("#eh-body").onchange({ target: { closest: (sel) => (sel === "input[data-eh]" ? { dataset: { eh: field }, value: id, checked } : null) } });
  const submit = () => $("#eh-form").onsubmit({ preventDefault() {} });

  // it opens at once, says the host's name as text, asks the panel, and closes the menu behind it
  let hold;
  const p0 = open(() => new Promise((res) => { hold = res; }));
  await settle();
  if ($("#modal").hidden || !$("#modal").innerHTML.includes('id="eh-form"') || $("#eh-t").textContent !== "Edit PC-B" || state.menu !== null) fail("opening: " + $("#modal").innerHTML);
  if ($("#modal").innerHTML.includes("PC-B") || !$("#modal").innerHTML.includes("Loading…") || !$("#modal").innerHTML.includes('id="eh-save" disabled')) fail("the frame: " + $("#modal").innerHTML);
  if (asked.length !== 1 || asked[0].path !== "/api/admin/hosts/h%201/options" || asked[0].method !== "GET" || !asked[0].signal) fail("the request: " + JSON.stringify(asked));
  if (timers.length !== 1 || timers[0].ms !== 15000) fail("the time limit of the answer: " + JSON.stringify(timers.map((x) => x.ms)));
  hold(d()); await p0; await settle();
  if (timers[0].on) fail("the time limit is still running after the answer");
  if (!$("#eh-body").innerHTML.includes("RACK UPS") || $("#eh-save").disabled !== true || $("#eh-err").textContent !== "") fail("drawn: " + $("#eh-body").innerHTML);
  // a submit with nothing changed sends nothing
  await submit();
  if (asked.length !== 1) fail("Save with nothing changed sent a request");
  // a tick: Save can be pressed, and the list of changes is in the dialog
  tick("ups", "nas-agent/rack", true);
  if ($("#eh-save").disabled || !words($("#eh-body").innerHTML).includes("This will change UPS RACK UPS (read by NAS): powers PC-B")) fail("after a tick: " + words($("#eh-body").innerHTML));
  // Save: one PUT with only that; the result is said with the host's name, and the dialog closes
  answer = async () => ({ ok: true, message: "links saved", changed: ["x"] });
  await submit(); await settle();
  const put = asked[1] || {};
  if (asked.length !== 2 || put.method !== "PUT" || put.path !== "/api/admin/hosts/h%201/options" || JSON.stringify(put.body) !== JSON.stringify({ ups: [{ id: "nas-agent/rack", on: true }] })) fail("the PUT: " + JSON.stringify(asked));
  if (closed !== 1 || JSON.stringify(flashes) !== JSON.stringify([{ msg: "PC-B: links saved", bad: false }])) fail("saved: closed " + closed + " " + JSON.stringify(flashes));

  // the panel refuses: its sentence is in the dialog, which stays open and can be saved again; no pop-up
  await open(async () => d());
  tick("plugs", "nas-agent/a", false);
  answer = async () => { throw Object.assign(new Error("a plug is linked by the machine itself: it cannot be unlinked here"), { status: 422 }); };
  await submit(); await settle();
  if (closed !== 0 || flashes.length || $("#eh-err").textContent !== "a plug is linked by the machine itself: it cannot be unlinked here" || $("#eh-save").disabled) fail("refused: " + $("#eh-err").textContent + " save " + $("#eh-save").disabled);
  // a change in the dialog takes the old sentence away
  tick("ups", "nas-agent/rack", true);
  if ($("#eh-err").textContent !== "") fail("the sentence stays after a change");
  // too many changes, the panel not reachable: said the same way
  for (const msg of ["too many changes; try again in 42 s", "Panel not reachable. Nothing was sent."]) {
    answer = async () => { throw Object.assign(new Error(msg), { status: 429 }); };
    await submit(); await settle();
    if ($("#eh-err").textContent !== msg || $("#eh-save").disabled || closed !== 0) fail("an error: " + $("#eh-err").textContent);
  }
  // while a Save is on its way a second one sends nothing
  let finish;
  answer = () => new Promise((res) => { finish = res; });
  const n = asked.length;
  const first = submit(); await settle();
  if (!$("#eh-save").disabled) fail("Save can be pressed while it is saving");
  await submit();
  if (asked.length !== n + 1) fail("two requests for one Save");
  finish({ ok: true, message: "links saved" }); await first; await settle();
  if (closed !== 1) fail("not closed after the save");

  // the sign-in is too old: "Confirm it's you" takes over with what was asked; the dialog shows no error and no result
  await open(async () => d());
  tick("ups", "nas-agent/rack", true);
  answer = async () => { throw Object.assign(new Error("Confirm it's you"), { status: 401, stepUp: true, detail: { code: "step_up_required" } }); };
  await submit(); await settle();
  if (!confirming || confirming.label !== "Edit host PC-B" || confirming.kind !== "request" || confirming.method !== "PUT" || confirming.path !== "/api/admin/hosts/h%201/options"
    || JSON.stringify(confirming.body) !== JSON.stringify({ ups: [{ id: "nas-agent/rack", on: true }] })) fail("handed to Confirm it's you: " + JSON.stringify(confirming));
  if (flashes.length || $("#eh-err").textContent !== "" || closed !== 0) fail("after the hand-over: " + JSON.stringify(flashes) + $("#eh-err").textContent);

  // the panel does not let this person look: its sentence, no sections, nothing to save
  await open(async () => { throw Object.assign(new Error("only admins can edit a host"), { status: 403 }); });
  if ($("#eh-err").textContent !== "only admins can edit a host" || $("#eh-body").innerHTML !== "" || $("#eh-save").disabled !== false && $("#modal").innerHTML.includes("Loading…") === false) fail("refused to look: " + $("#eh-err").textContent);
  if ($("#eh-form").onsubmit) fail("a dialog that did not load can be submitted");
  // no answer at all: after the time limit the request is given up and the dialog says so
  const p1 = open((path, opts) => new Promise((res, rej) => { opts.signal.addEventListener("abort", () => rej(Object.assign(new Error("This operation was aborted"), { name: "AbortError" }))); }));
  await settle();
  if ($("#eh-err").textContent !== "" || !timers[0]?.on) fail("before the time limit: " + $("#eh-err").textContent);
  timers[0].fn(); await p1; await settle();
  if ($("#eh-err").textContent !== "The panel did not answer. Close this and try again." || $("#eh-body").innerHTML !== "") fail("no answer: " + $("#eh-err").textContent + " | " + $("#eh-body").innerHTML);
  // "Remove" of the stored changes without a line: one click, the list says what goes, Save sends it
  await open(async () => ({ ...d(), leftovers: ["1 stored change kept with old-pc, which is no machine of this panel"], leftover_count: 1 }));
  if (!$("#eh-save").disabled) fail("leftovers alone make Save pressable before Remove was pressed");
  $("#eh-body").onclick({ target: { closest: (sel) => (sel === "[data-eh-reset], [data-eh-left]" ? { dataset: { ehLeft: "" } } : null) } });
  if ($("#eh-save").disabled || !words($("#eh-body").innerHTML).includes("This will change Removed: 1 stored change kept with old-pc, which is no machine of this panel")) fail("after Remove: " + words($("#eh-body").innerHTML));
  answer = async () => ({ ok: true, message: "links saved" });
  await submit(); await settle();
  if (JSON.stringify(asked[asked.length - 1].body) !== JSON.stringify({ leftovers: true }) || closed !== 1) fail("the PUT for the leftovers: " + JSON.stringify(asked[asked.length - 1]));
  // an answer that is not what the panel's API gives (a proxy's page, another version): said, never "Loading…" for ever
  for (const bad of [null, {}, "x", [], 7, { ups: null, plugs: null }, { ups: [], plugs: {} }, { ups: [null], plugs: [] }, { ups: [], plugs: [1] }, { ups: [{ name: "no id" }], plugs: [] }]) {
    let threw = "";
    try { await open(async () => bad); await settle(); } catch (x) { threw = String(x); }
    if (threw || $("#eh-err").textContent !== "The panel's answer is not what this page expects. Reload the page and try again." || $("#eh-body").innerHTML !== "" || $("#eh-form").onsubmit || eh.d !== null)
      fail("an answer " + JSON.stringify(bad) + ": " + (threw || $("#eh-err").textContent + " | " + $("#eh-body").innerHTML));
  }
  // Cancel closes it
  await open(async () => d());
  $("#eh-x").onclick();
  if (closed !== 1) fail("Cancel");
  // an answer that comes after the dialog was closed draws nothing
  let late;
  const p2 = open(() => new Promise((res) => { late = res; }));
  await settle();
  $("#eh-x").onclick();
  late(d()); await p2; await settle();
  if ($("#eh-body").innerHTML.includes("RACK") || $("#eh-form").onsubmit) fail("a late answer drew into a closed dialog");
  // the page drawing itself again (a new snapshot from the stream) does not touch an open dialog or its ticks
  await open(async () => d());
  tick("ups", "nas-agent/rack", true);
  const drawn = $("#eh-body").innerHTML;
  for (let i = 0; i < 3; i++) render();
  if ($("#eh-body").innerHTML !== drawn || !eh.draft.ups["nas-agent/rack"]) fail("a redraw of the page changed the dialog");
`)
}

// The lines the dialog shows before Save are the panel's own lines: for the same ticks, on the panel's real
// answer, the page's list of changes and what the panel answers (and writes to the audit log, and shows in
// "Confirm it's you") are the same sentences, and the request the page makes is taken as it is.
func TestUIEditHostListsTheChangesInThePanelsWords(t *testing.T) {
	r := ehSetup(t)
	// three sites, so that links move who sees a device: PVE-A is of Away, PVE-B of Home, the NAS (which reads
	// every device) of Lab
	_ = r.s.Access.EnsureSites([]string{"Home", "Away", "Lab"})
	for id, site := range map[string]string{ehNodeB: "Home", ehNodeA: "Away", ehNAS: "Lab"} {
		if _, err := r.s.Access.Assign(id, site); err != nil {
			t.Fatal(err)
		}
	}
	notes := 0
	step := func(id, ticks string) {
		t.Helper()
		g := r.options(id)
		file := filepath.Join(t.TempDir(), "d.json")
		if err := os.WriteFile(file, g.raw, 0o600); err != nil {
			t.Fatal(err)
		}
		out := runEhPageScript(t, "eh-words", `
  const d = JSON.parse(require("fs").readFileSync(process.argv[2], "utf8")), draft = ehDraft();
  for (const [kind, key, v] of JSON.parse(process.argv[3])) { if (kind === "reset") ehReset(d, draft, key); else if (kind === "left") draft.leftovers = true; else ehTick(d, draft, kind, key, v); }
  console.log(JSON.stringify(ehPlan(d, draft)));
`, file, ticks)
		var plan struct {
			Lines []string       `json:"lines"`
			Body  map[string]any `json:"body"`
			Notes []string       `json:"notes"`
		}
		if err := json.Unmarshal([]byte(out), &plan); err != nil || len(plan.Lines) == 0 {
			t.Fatalf("the page's plan for %s: %v\n%s", ticks, err, out)
		}
		p := r.ok(id, plan.Body)
		if got := strs(p.body["changed"]); !slices.Equal(got, plan.Lines) {
			t.Errorf("%s on %s:\nthe page lists %q\nthe panel says %q", ticks, id, plan.Lines, got)
		}
		if got := strs(p.body["visibility"]); !slices.Equal(got, plan.Notes) {
			t.Errorf("%s on %s, who will see what:\nthe page notes %q\nthe panel says %q", ticks, id, plan.Notes, got)
		}
		notes += len(plan.Notes)
		r.poll()
	}
	// stored changes without a line (an id that is no agent's, a row that is no machine's), removed with a tick
	if _, _, err := r.s.Poller.Admin.EditHostOptions("earlier", func(o *adminstate.HostOptions) error {
		o.Agents["gone-pc"] = adminstate.HostOption{Exclude: map[string]bool{"x": true, "y": false}}
		o.Agents[ehNAS] = adminstate.HostOption{UPS: map[string]map[string]bool{"rack": {"gone-pc": true}}, Links: map[string]string{"corner": "gone-pc"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	step(ehNodeA, `[["left"], ["ups", "nas-agent/desk", true]]`)
	step(ehNodeA, `[["ups", "nas-agent/desk", false]]`)
	if r.storedOptions()["agents"].(map[string]any)["gone-pc"] != nil || r.storedAgent(ehNAS) != nil {
		t.Fatalf("the leftovers after the page removed them: %v", r.storedOptions())
	}
	step(ehNodeB, `[["ups", "nas-agent/rack", true], ["ups", "nas-agent/desk", true]]`)
	step(ehNodeB, `[["plugs", "nas-agent/b-plug", true], ["total", "nas-agent/b-plug", false]]`)
	step(ehNodeB, `[["plugs", "nas-agent/a-plug", true]]`) // moved from PVE-A: "(was on PVE-A)"
	step(ehNodeB, `[["plugs", "nas-agent/own", true]]`)    // the NAS's own plug: "(was on NAS)"
	step(ehNodeA, `[["ups", "nas-agent/rack", false], ["plugs", "nas-agent/a-plug", true]]`)
	step(ehNAS, `[["total", "nas-agent/corner", true], ["ups", "nas-agent/desk", true]]`)
	step(ehNodeB, `[["reset", "ups:nas-agent/rack"], ["reset", "ups:nas-agent/desk"], ["reset", "plugs:nas-agent/own"], ["total", "nas-agent/b-plug", true]]`)
	step(ehNodeA, `[["reset", "plugs:nas-agent/a-plug"], ["reset", "ups:nas-agent/rack"], ["ups", "nas-agent/desk", true]]`)
	step(ehNAS, `[["reset", "total:nas-agent/corner"], ["reset", "ups:nas-agent/desk"], ["plugs", "nas-agent/b-plug", true]]`)
	step(ehNodeB, `[["reset", "plugs:nas-agent/b-plug"], ["ups", "nas-agent/desk", true]]`) // b-plug: given to the NAS since; the Reset on PVE-B's page is not offered, and a page that asks it changes nothing
	if notes < 12 {
		t.Errorf("the steps made %d notes about who sees what: too few to compare the page's with the panel's", notes)
	}
}
