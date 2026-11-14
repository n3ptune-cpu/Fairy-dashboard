package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The strip that asks for a run's second passkey, run with node: the page's own functions (runWaitStrips,
// fetchRunWait, the click handler, paintTitle), nothing of them copied. It is on every page, counts down,
// one click asks for one confirmation, and Keep on is the cancel for one machine. It is one short line: while
// the run asks, its machines are in that line and have no strip of their own (their Keep on is in the run's rows
// on the Updates page); a machine that may switch itself off while nobody is asked has its own strip.
func TestUIRunWaitStrip(t *testing.T) {
	js := appJS(t)
	var parts []string
	for _, re := range []string{`(?m)^const esc = .*$`, `(?m)^const enc = .*$`, `(?m)^const fmtRuntime = .*$`} {
		m := regexp.MustCompile(re).FindString(js)
		if m == "" {
			t.Fatalf("app.js has no %s any more", re)
		}
		parts = append(parts, m)
	}
	from, to := strings.Index(js, "function setTitle(t, sub = \"\") {"), strings.Index(js, "// ---- end of the run's strip")
	if from < 0 || to < from {
		t.Fatal("app.js has no strip for a run's confirmation any more")
	}
	// every page: the strip is part of the alerts every page draws, and nothing in it asks which page is shown
	i := strings.Index(js, "function renderAlerts() {")
	alerts := js[i : i+strings.Index(js[i:], "\n}\n")]
	if !strings.Contains(alerts, "out.push(...runWaitStrips());") {
		t.Fatal("renderAlerts does not draw the run's strip")
	}
	j := strings.Index(js, "function runWaitStrips() {")
	if fn := js[j : j+strings.Index(js[j:], "\n}\n")]; strings.Contains(fn, "current()") {
		t.Fatal("the run's strip depends on the page that is shown")
	}
	if !strings.Contains(js, `es.addEventListener("run_wait", () => { netHeard(true); fetchRunWait(); });`) {
		t.Fatal("the stream's run_wait event does not fetch the strip's data")
	}
	script := `"use strict";
const listeners = {};
const document = { addEventListener(ev, fn) { listeners[ev] = fn; }, title: "" };
const $ = () => ({ textContent: "" });
const I = { shield: "", onoff: "" };
const state = { me: { role: "admin", can_control: true } };
let page = "overview";
const current = () => ({ page });
let tick = 0;
const wuTick = () => tick;
let answer = () => ({});
const asked = [];
const api = async (path, opt) => { asked.push((opt?.method || "GET") + " " + path); return answer(path); };
let renders = 0;
const render = () => { renders++; };
const flashes = [];
const flash = (msg, bad = false, details = null) => { flashes.push({ msg, bad, details }); };
const calls = [];
const controlCall = async (pending, fn) => { calls.push(pending); return fn(); };
const fetchWakeUpdate = () => { asked.push("fetchWakeUpdate"); };
` + strings.Join(parts, "\n") + "\n" + js[from:to] + `
const fail = (m) => { console.log("FAIL " + m); process.exitCode = 1; };
const words = (t) => t.replace(/<[^>]+>/g, " ").replace(/&#39;/g, "'").replace(/\s+/g, " ").trim();
const click = (dataset) => listeners.click({ target: { closest: () => ({ dataset, disabled: false }) } });
(async () => {
  setTitle("Overview");
  if (runWaitStrips().length !== 0 || document.title !== "Overview · Fairy") fail("something is shown before anything waits: " + document.title);
  const run = "00000000000000aa", at = new Date(Date.UTC(2026, 10, 14, 21, 42)).toISOString();
  const mine = [{ run, id: "pc-a", name: "PC-A", at, sure: true, active: true }, { run, id: "pc-b", name: "PC-B", at, latest: true, sure: false, active: true }];
  const other = { run: "00000000000000bb", id: "pc-c", name: "PC-C", at, sure: true };
  const waiting = { run, by: "admin", left_s: 600, hosts: ["PC-A", "PC-B"], armed: [...mine, other] };
  answer = () => waiting;
  await fetchRunWait();
  if (asked.join() !== "GET /api/admin/wake-update/waiting" || renders !== 1) fail("the fetch: " + asked.join() + " " + renders);
  for (const p of ["overview", "updates", "system", "settings", "audit"]) {
    page = p;
    const s = runWaitStrips();
    if (s.length !== 2) { fail(p + ": " + s.length + " strips"); continue; }
    const w = words(s[0]);
    if (w !== "Your passkey is needed PC-A, PC-B wait to be updated 10:00 left not confirmed: not updated; armed machines switch themselves off Confirm it's you") fail(p + ": " + w);
    if (!s[0].includes('data-act="rw-confirm" data-id="' + run + '"') || !s[0].includes('role="alert"')) fail(p + ": the button: " + s[0]);
  }
  const clock = new Date(at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  // a machine of another run that may switch itself off has its own strip, also while this run asks
  const c = runWaitStrips()[1];
  if (words(c) !== "PC-C switches itself off at " + clock + " armed by a run Keep on" || !c.includes('data-act="rw-keep" data-id="pc-c" data-name="PC-C"')) fail("the other run's machine: " + c);
  if (document.title !== "Passkey needed · Overview · Fairy") fail("the tab's title: " + document.title);
  // one machine waits; more than three are counted, not listed
  waiting.hosts = ["PC-A"];
  if (!words(runWaitStrips()[0]).startsWith("Your passkey is needed PC-A waits to be updated 10:00 left")) fail("one machine: " + words(runWaitStrips()[0]));
  waiting.hosts = ["PC-A", "PC-B", "PC-C", "PC-D", "PC-E"];
  if (!words(runWaitStrips()[0]).startsWith("Your passkey is needed PC-A, PC-B and 3 more wait to be updated 10:00 left")) fail("five machines: " + words(runWaitStrips()[0]));
  waiting.hosts = ["PC-A", "PC-B"];
  // somebody confirmed and no machine accepted: the strip still asks, and says who tried
  waiting.tried = "Bob";
  if (words(runWaitStrips()[0]) !== "Your passkey is needed PC-A, PC-B wait to be updated 10:00 left Bob tried: no machine accepted not confirmed: not updated; armed machines switch themselves off Confirm it's you") fail("after a try: " + words(runWaitStrips()[0]));
  delete waiting.tried;
  // the time left counts down on the page's steady clock
  tick += 61000;
  if (rwLeft() !== 539 || !runWaitStrips()[0].includes('<b id="rw-left">8:59</b>')) fail("a minute later: " + rwLeft());
  tick += 3600000;
  if (rwLeft() !== 0) fail("never below zero: " + rwLeft());
  // at zero the strip is read anew, and not once a second while the answer still names the run: every 5 s at most
  const leftEl = { textContent: "" };
  document.getElementById = (id) => (id === "rw-left" ? leftEl : null);
  asked.length = 0;
  waiting.left_s = 0; // the panel still names the run for a moment after its time
  for (let i = 0; i < 4; i++) { rwTick(); await new Promise((r) => setTimeout(r, 2)); tick += 1000; }
  if (asked.length !== 1 || leftEl.textContent !== "0:00") fail("four seconds at zero: " + asked.length + " fetches, " + leftEl.textContent);
  tick += 2000; rwTick(); await new Promise((r) => setTimeout(r, 2));
  if (asked.length !== 2) fail("after 5 s at zero: " + asked.length + " fetches");
  waiting.left_s = 600;
  await fetchRunWait();
  // one click, one confirmation: one request, kept for "Confirm it's you", and what came of it is said
  asked.length = 0;
  answer = (path) => (path.endsWith("/confirm") ? { ok: true, message: "confirmed on 1 of 2 machines", lines: ["PC-A: confirmed", "PC-B: NOT accepted (its wait was over: it is switching itself off)"], all_ok: false } : { now: at });
  await click({ act: "rw-confirm", id: run });
  await new Promise((r) => setTimeout(r, 5));
  if (calls.length !== 1 || calls[0].method !== "POST" || calls[0].path !== "/api/admin/wake-update/" + run + "/confirm" || JSON.stringify(calls[0].body) !== "{}") fail("the confirmation's request: " + JSON.stringify(calls));
  if (asked.filter((x) => x.startsWith("POST")).join() !== "POST /api/admin/wake-update/" + run + "/confirm") fail("requests: " + asked.join());
  if (flashes.length !== 1 || flashes[0].msg !== "confirmed on 1 of 2 machines" || flashes[0].bad !== true || flashes[0].details.length !== 2) fail("what the page said: " + JSON.stringify(flashes));
  if (!asked.includes("GET /api/admin/wake-update/waiting")) fail("the strip was not read again after the confirmation");
  if (runWaitStrips().length !== 0 || document.title !== "Overview · Fairy") fail("the strip stays after the run stopped asking: " + document.title);
  // Keep on: the cancel for one machine; "stays on" only when the panel says so
  for (const [stays, bad] of [[true, false], [false, true]]) {
    asked.length = 0; flashes.length = 0; calls.length = 0;
    answer = (path) => (path.endsWith("/keep-on") ? { ok: true, stays_on: stays, message: "said by the panel" } : { now: at });
    await click({ act: "rw-keep", id: "pc-a", name: "PC-A" });
    await new Promise((r) => setTimeout(r, 5));
    if (calls.length !== 1 || calls[0].path !== "/api/systems/pc-a/wake-update/keep-on" || calls[0].label !== "Keep PC-A on") fail("Keep on's request: " + JSON.stringify(calls));
    if (flashes.length !== 1 || flashes[0].msg !== "said by the panel" || flashes[0].bad !== bad) fail("Keep on, stays " + stays + ": " + JSON.stringify(flashes));
  }
  // a refusal is shown, not swallowed
  flashes.length = 0;
  answer = (path) => { if (path.endsWith("/keep-on")) throw new Error("NOT cancelled (no answer). PC-A may switch itself off at 21:42 UTC."); return { now: at }; };
  await click({ act: "rw-keep", id: "pc-a", name: "PC-A" });
  await new Promise((r) => setTimeout(r, 5));
  if (flashes.length !== 1 || !flashes[0].msg.startsWith("NOT cancelled") || flashes[0].bad !== true) fail("a failed Keep on: " + JSON.stringify(flashes));
  // nobody is asked any more (the question is over, or the panel was restarted): every machine that may still
  // switch itself off is named, each with its time and Keep on
  answer = () => ({ now: at, armed: mine.map((m) => ({ ...m, by_machine: m.sure })) });
  await fetchRunWait();
  const [a, b, ...more] = runWaitStrips();
  if (more.length || document.title !== "Overview · Fairy") fail("after the question: " + more.length + " more strips, " + document.title);
  if (words(a) !== "PC-A switches itself off at " + clock + " (by its own clock) armed by a run Keep on") fail("the armed machine: " + words(a));
  if (words(b) !== "PC-B may switch itself off by " + clock + " at the latest its arming got no answer Keep on") fail("the machine whose arming is not known: " + words(b));
  if (!a.includes('data-act="rw-keep" data-id="pc-a" data-name="PC-A"')) fail("Keep on: " + a);
  answer = () => ({ now: at, armed: [{ run, id: "pc-a", name: "PC-A", sure: true }] });
  await fetchRunWait();
  if (words(runWaitStrips()[0]) !== "PC-A switches itself off at a time Fairy does not know armed by a run Keep on") fail("no time known: " + words(runWaitStrips()[0]));
  // nobody but an admin
  state.me.role = "viewer";
  if (runWaitStrips().length !== 0) fail("a viewer sees the strip");
  if (!process.exitCode) console.log("ok");
})();
`
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the page's own functions are not run here")
	}
	file := filepath.Join(t.TempDir(), "runwait.js")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, file).CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
