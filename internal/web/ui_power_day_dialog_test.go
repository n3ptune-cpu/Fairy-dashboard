package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The dialog of "Last 24 hours" itself, run in node with a page that is just enough for it: elements that are
// found by their id, remember what was written into them and how (as text or as HTML), a focus, timers that
// run when the test says so, and answers of the panel that come when the test says so. The functions are the
// page's own (powerDayScript cuts them out of app.js).
func runPowerDayDialog(t *testing.T, name, body string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the page's own functions are not run here")
	}
	page := `
// ---- the page
const els = new Map();
let active = null, selection = "";
const under = (el, top) => { for (let p = el.parent; p; p = p.parent) if (p === top) return true; return false; };
const drop = (top) => { for (const [id, el] of els) if (under(el, top)) { els.delete(id); if (active === el) active = null; } };
function El(id, parent) {
  return { id, parent, hidden: false, dataset: {}, clientWidth: 600, html: "", text: "", htmlSets: 0, textSets: 0, added: [],
    set innerHTML(h) { drop(this); this.html = String(h); this.text = ""; this.added = []; this.htmlSets++;
      for (const m of this.html.matchAll(/<(\w+)[^>]*\sid="([^"]+)"/g)) els.set(m[2], El(m[2], this)); },
    get innerHTML() { return this.html; },
    set textContent(v) { drop(this); this.text = String(v); this.html = ""; this.added = []; this.textSets++; },
    get textContent() { return this.text + this.added.map((c) => c.textContent).join(" "); },
    append(c) { this.added.push(c); },
    contains(x) { return !!x && (x === this || under(x, this)); },
    focus() { active = this; },
    querySelector() { return null; }, querySelectorAll() { return []; } };
}
const modal = El("modal", null);
modal.hidden = true;
els.set("modal", modal);
const $ = (sel) => (sel[0] === "#" && els.has(sel.slice(1)) ? els.get(sel.slice(1)) : null);
const listeners = {};
const document = { hidden: false, get activeElement() { return active; }, addEventListener(type, fn) { (listeners[type] ||= []).push(fn); },
  createElement: () => ({ className: "", textContent: "" }), querySelectorAll: () => [] };
const window = { addEventListener() {}, getSelection: () => selection };
const send = (type, e) => { for (const fn of listeners[type] || []) fn(e); };
// ---- timers that run when the test says so
const timers = new Map();
let timerSeq = 0;
const setInterval = (fn, ms) => { timers.set(++timerSeq, { fn, ms, again: true }); return timerSeq; };
const setTimeout = (fn, ms) => { timers.set(++timerSeq, { fn, ms, again: false }); return timerSeq; };
const clearInterval = (id) => { timers.delete(id); }, clearTimeout = clearInterval;
const timersOf = (again) => [...timers.values()].filter((x) => x.again === again);
const runTimers = (again) => { for (const [id, x] of [...timers]) if (x.again === again) { if (!again) timers.delete(id); x.fn(); } };
// ---- the panel, which answers when the test says so
const calls = [];
const api = (path) => new Promise((res, rej) => calls.push({ path, res, rej }));
const settle = () => new Promise((r) => setImmediate(r));
const answer = async (i, d) => { calls[i].res(d); await settle(); };
const refuse = async (i, status, message) => { const e = new Error(message); e.status = status; calls[i].rej(e); await settle(); };
// ---- what the dialog uses of the rest of the page
let stepUpOpen = false;
const focused = [];
const focusAtTop = (sel) => { focused.push(sel); $(sel)?.focus(); };
const closeModal = () => { modal.innerHTML = ""; modal.hidden = true; };
`
	file := filepath.Join(t.TempDir(), name+".js")
	script := powerDayScript(t, appJS(t), `
const HTML = '<img src=x onerror="alert(1)"> & <b>plug</b>';
state.power = { sources: [{ id: "pc-a/plug-a", name: HTML }, { id: "pc-b/plug-b", name: "Plug B" }] };
const dayFor = (source, watts, o = {}) => ({ ...dayOf(120, () => watts), source,
  summary: { from: NOW - 7200, to: NOW, covered_s: 7200, min_w: watts, avg_w: watts, max_w: watts, min_at: NOW - 7200, max_at: NOW - 60, energy_kwh: watts / 500, energy_est_kwh: 0, energy: "counter" }, ...o });
(async () => {
`+body+`
})().catch((e) => fail("the script: " + (e && e.stack || e)));`)
	// the page above takes the place of the two lines powerDayScript has for it
	const stub = "const document = { addEventListener() {} }, window = { addEventListener() {} };\n"
	if !strings.Contains(script, stub) {
		t.Fatal("powerDayScript has no line for the document any more")
	}
	script = strings.Replace(script, stub, page, 1)
	script = strings.Replace(script, "\nif (!process.exitCode) console.log(\"ok\");\n", "\nprocess.on(\"exit\", () => { if (!process.exitCode) console.log(\"ok\"); });\n", 1)
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, file).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

// Opening the dialog. The source's name is the user's text (a plug's name from an agent's config): it is
// written as text, never as HTML, and so is the error of a failed request. The focus starts on Close. One
// timer asks again once a minute, however many dialogs were opened before.
func TestUIPowerDayDialogOpens(t *testing.T) {
	runPowerDayDialog(t, "day-open", `
openPowerDay("pc-a/plug-a");
const name = $("#pday-name");
if (!name || name.textContent !== HTML || name.htmlSets !== 0 || name.textSets !== 1) fail("the source's name is not written as text: as text " + (name && name.textSets) + " times, as HTML " + (name && name.htmlSets) + " times: " + (name && (name.text || name.html)));
if (modal.html.includes("<img") || modal.html.includes("onerror")) fail("the source's name is in the dialog's HTML: " + modal.html);
if (modal.hidden || !$("#pday") || !$("#pday-x")) fail("the dialog is not open");
if (!same(focused, ["#pday-x"]) || active !== $("#pday-x")) fail("the focus does not start on Close: " + JSON.stringify(focused));
if (calls.length !== 1 || calls[0].path !== "/api/power/day?source=pc-a%2Fplug-a") fail("the day that is asked for: " + JSON.stringify(calls.map((c) => c.path)));
if (!modal.html.includes("Loading…")) fail("nothing says that the day is on its way");
let every = timersOf(true);
if (every.length !== 1 || every[0].ms !== 60000) fail("the timers after opening: " + JSON.stringify(every.map((x) => x.ms)) + ", want one of 60000 ms");

// an error is the panel's or the proxy's text: written with its characters, not as HTML; and asked again
await refuse(0, 502, 'HTTP 502 <script>alert(2)</script> & more');
const body = $("#pday-body");
if (!body.html.includes('<div class="error">HTTP 502 &lt;script&gt;alert(2)&lt;/script&gt; &amp; more</div>') || body.html.includes("<script>")) fail("the error is not escaped: " + body.html);
if (timersOf(true).length !== 1) fail("after an error that may pass, the dialog does not ask again");

// a second dialog, and a third: still one timer; a source that is not listed opens nothing; nor does anything while "Confirm it's you" is open
openPowerDay("pc-b/plug-b");
openPowerDay("pc-a/plug-a");
every = timersOf(true);
if (every.length !== 1 || every[0].ms !== 60000) fail("after three dialogs: " + every.length + " timers");
const n = calls.length;
openPowerDay("pc-z/none");
stepUpOpen = true; openPowerDay("pc-b/plug-b"); stepUpOpen = false;
if (calls.length !== n || pday.id !== "pc-a/plug-a") fail("a source that is not listed, or a click under \"Confirm it's you\", opened a dialog");

// closed: the timer ends itself with its next turn, and asks for nothing
closeModal();
runTimers(true);
if (timersOf(true).length !== 0 || calls.length !== n) fail("after Close: " + timersOf(true).length + " timers, " + (calls.length - n) + " requests");
`)
}

// An answer that comes late. The day of another source than the one the dialog shows now is not drawn (the
// first dialog was closed and another opened while the request was on its way), and nothing is drawn into a
// dialog that is closed.
func TestUIPowerDayDialogDrawsOnlyItsOwnSource(t *testing.T) {
	runPowerDayDialog(t, "day-late", `
openPowerDay("pc-a/plug-a");   // request 0
openPowerDay("pc-b/plug-b");   // request 1
await answer(1, dayFor("pc-b/plug-b", 55));
if (pday.d?.source !== "pc-b/plug-b" || !words($("#pday-stats").html).includes("55.0 W")) fail("the dialog's own answer is not drawn: " + words($("#pday-body").html));
const stats = $("#pday-stats").html;
await answer(0, dayFor("pc-a/plug-a", 999));
if (pday.d?.source !== "pc-b/plug-b" || $("#pday-stats").html !== stats || words($("#pday-body").html + stats).includes("999")) fail("the late answer of another source was drawn into the dialog of pc-b/plug-b");
// the same for a late error
openPowerDay("pc-a/plug-a");   // request 2
openPowerDay("pc-b/plug-b");   // request 3
await answer(3, dayFor("pc-b/plug-b", 56));
await refuse(2, 500, "late trouble");
if (pday.err || $("#pday-body").html.includes("late trouble")) fail("the late error of another source is shown");
// closed while the request was on its way
runTimers(true);               // request 4
closeModal();
await answer(4, dayFor("pc-b/plug-b", 57));
if ($("#pday-body") || modal.html !== "" || pday.d.avg[0] === 57) fail("an answer was drawn after Close");
`)
}

// The dialog follows the day: once a minute a new answer is drawn into the parts that are there. The chart's
// element stays the one it was (it had the keyboard's focus and the place the arrow keys moved to: building it
// anew every minute threw both away). A tab that is not shown asks for nothing and catches up when it is shown
// again.
func TestUIPowerDayDialogRefreshesInPlace(t *testing.T) {
	runPowerDayDialog(t, "day-refresh", `
openPowerDay("pc-b/plug-b");
await answer(0, dayFor("pc-b/plug-b", 40));
const plot = $("#pday-plot"), read = $("#pday-read"), body = $("#pday-body"), builds = body.htmlSets;
if (!plot || !plot.html.includes("pd-line") || !read) fail("no chart after the first answer");
plot.focus();
dayPoint(NOW - 3600);          // the arrow keys moved to an hour ago
const at = read.textContent;
if (!/40\.0 W/.test(at)) fail("the rig: the readout says " + at);
for (let i = 1; i <= 3; i++) {
  runTimers(true);
  if (calls.length !== i + 1) fail("turn " + i + " of the timer asked " + (calls.length - i) + " times");
  await answer(i, dayFor("pc-b/plug-b", 40 + i));
  if ($("#pday-plot") !== plot || active !== plot) fail("after refresh " + i + " the chart is another element, or the focus is not on it any more");
  if (body.htmlSets !== builds) fail("refresh " + i + " built the dialog's body anew");
  if (pday.at !== NOW - 3600 || $("#pday-read") !== read || !read.textContent.includes(dayTime(NOW - 3600))) fail("refresh " + i + " lost the minute the readout was at: " + read.textContent);
  if (!words($("#pday-stats").html).includes((40 + i).toFixed(1) + " W") || !read.textContent.includes((40 + i).toFixed(1) + " W")) fail("refresh " + i + " did not draw the new numbers: " + words($("#pday-stats").html) + " / " + read.textContent);
}
// volts come (the plug's agent was updated): the body is built once more, with the second chart
runTimers(true);
await answer(4, dayFor("pc-b/plug-b", 44, { volts: dayOf(120, () => 246.5).avg }));
if (!$("#pday-volts") || body.htmlSets !== builds + 1) fail("a day with volts does not get its second chart");
runTimers(true);
await answer(5, dayFor("pc-b/plug-b", 45, { volts: dayOf(120, () => 246.5).avg }));
if (body.htmlSets !== builds + 1) fail("with volts, a refresh builds the body anew");

// a tab that is not shown asks for nothing
document.hidden = true;
runTimers(true); runTimers(true);
if (calls.length !== 6) fail("a tab in the background asked " + (calls.length - 6) + " times");
// shown again: at once when the last answer is a minute old, not before that
document.hidden = false;
send("visibilitychange", {});
if (calls.length !== 6) fail("shown again a moment after the last answer: asked at once");
pday.got = Date.now() - 61000;
send("visibilitychange", {});
if (calls.length !== 7) fail("shown again after more than a minute: not asked");
await answer(6, dayFor("pc-b/plug-b", 46, { volts: dayOf(120, () => 246.5).avg }));
closeModal();
pday.got = 0;
send("visibilitychange", {});
if (calls.length !== 7) fail("a tab that is shown again asks for the day of a dialog that is closed");
`)
}

// A source that is not reported any more (404): the dialog says so, keeps the chart it has, and stops asking.
// Any other error may pass, and is asked again.
func TestUIPowerDayDialogStopsAskingForASourceThatIsGone(t *testing.T) {
	runPowerDayDialog(t, "day-gone", `
openPowerDay("pc-b/plug-b");
await answer(0, dayFor("pc-b/plug-b", 40));
const plot = $("#pday-plot");
runTimers(true);
await refuse(1, 500, "the panel is busy");
if (!$("#pday-msg").html.includes('class="error"') || timersOf(true).length !== 1 || $("#pday-plot") !== plot) fail("an error that may pass: " + $("#pday-msg").html + ", " + timersOf(true).length + " timers");
runTimers(true);
await refuse(2, 404, "power source not found");
const msg = words($("#pday-msg").html);
if (msg !== "This source is no longer reported. The chart is as it was when it was last read.") fail("what the dialog says about a source that is gone: " + msg);
if ($("#pday-msg").html.includes('class="error"') || $("#pday-body").html.includes("power source not found")) fail("the 404 is shown as an error");
if ($("#pday-plot") !== plot || !plot.html.includes("pd-line")) fail("the chart that was there is gone with the source");
if (timersOf(true).length !== 0) fail("the dialog keeps asking for a source that is gone");
document.hidden = true; document.hidden = false; pday.got = 0;
send("visibilitychange", {});
if (calls.length !== 3) fail("a tab that is shown again asks for a source that is gone");
// a source that is gone before its first answer: the sentence, and nothing else
openPowerDay("pc-a/plug-a");
await refuse(3, 404, "power source not found");
if (words($("#pday-body").html) !== "This source is no longer reported." || timersOf(true).length !== 0) fail("gone before the first answer: " + words($("#pday-body").html) + ", " + timersOf(true).length + " timers");
// opened again later (it is listed again): asked again
openPowerDay("pc-a/plug-a");
if (timersOf(true).length !== 1 || pday.gone) fail("a dialog that is opened anew does not ask again");
await answer(4, dayFor("pc-a/plug-a", 12, { summary: null, avg: [], min: [], max: [] }));
if (words($("#pday-body").html) !== "No readings in the last 24 hours.") fail("a source without readings: " + words($("#pday-body").html));
`)
}

// What opens the dialog. The button does, at once. A click anywhere else in the source's row does too, unless
// it means something else: it is on a link or a control, a key is held with it, text was marked with it, or
// it is the first or the second click of a double click (which marks a word).
func TestUIPowerDayRowClick(t *testing.T) {
	runPowerDayDialog(t, "day-click", `
const btn = { dataset: { source: "pc-b/plug-b" }, disabled: false };
const row = { querySelector: (sel) => (sel === '[data-act="power-day"]' ? btn : null) };
// a click on something in the row: what closest() finds from there
const on = (found) => ({ closest: (sel) => (sel in found ? found[sel] : null) });
const CTL = "a, button, input, select, textarea, label";
const cell = on({ "tr.row": row }), link = on({ "tr.row": row, [CTL]: {} }), button = on({ "tr.row": row, [CTL]: btn, '[data-act="power-day"]': btn });
const opened = () => { const n = calls.length; calls.length = 0; closeModal(); pday.id = null; return n; };
const click = (target, o = {}) => send("click", { target, detail: 1, pointerType: "mouse", ...o });
const pending = () => timersOf(false).filter((x) => x.ms === 250).length;

click(button);
if (opened() !== 1) fail("the button does not open the dialog at once");
btn.disabled = true; click(button); btn.disabled = false;
if (opened() !== 0) fail("a disabled button opened the dialog");

// a plain click in the row with a mouse: after 250 ms, in which no second click came
click(cell);
if (calls.length !== 0 || pending() !== 1) fail("a mouse's click in the row: opened at once, or nothing is waiting (" + pending() + ")");
runTimers(false);
if (opened() !== 1) fail("a click in the row does not open the dialog");
// a finger's tap: at once
click(cell, { pointerType: "touch" });
if (pending() !== 0 || opened() !== 1) fail("a tap in the row does not open the dialog at once");

// a double click: neither of its clicks opens
click(cell); click(cell, { detail: 2 });
if (pending() !== 0) fail("the first click of a double click is still waiting to open");
runTimers(false);
if (opened() !== 0) fail("a double click in the row opened the dialog");
// a link or a control in the row
click(link); runTimers(false);
if (opened() !== 0) fail("a click on a link in the row opened the dialog");
// a key held with the click
for (const key of ["ctrlKey", "metaKey", "shiftKey", "altKey"]) {
  click(cell, { [key]: true }); runTimers(false);
  if (opened() !== 0) fail("a click with " + key + " opened the dialog");
}
// text was marked: by this click (the end of a drag), or while the click was waiting
selection = "42.0 W";
click(cell); runTimers(false);
if (opened() !== 0) fail("the click that ends a drag over text opened the dialog");
selection = "";
click(cell); selection = "Plug B"; runTimers(false);
if (opened() !== 0) fail("a click opened the dialog although text was marked before its wait was over");
selection = "";
// a click outside of any row
click(on({})); runTimers(false);
if (opened() !== 0) fail("a click outside of a row opened a dialog");
`)
}
