package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// wuPageScript is the "Update unpowered hosts" part of app.js with what it needs around it, for node: the
// page's own functions (wakeUpdateBox, wuRunBox, fetchWakeUpdate, ...) are run, nothing of them is copied.
// body is the test. What is around them here is not the code under test: api answers with what the test set
// with setAnswer, flash records what it was given, render counts.
func wuPageScript(t *testing.T, js, body string) string {
	t.Helper()
	var parts []string
	for _, re := range []string{`(?m)^const dict = .*$`, `(?m)^const table = .*$`, `(?m)^const esc = .*$`, `(?m)^const enc = .*$`, `(?m)^const fmtRuntime = .*$`,
		`(?m)^const buildLabel = .*$`, `(?m)^const infoList = .*$`} {
		m := regexp.MustCompile(re).FindString(js)
		if m == "" {
			t.Fatalf("app.js has no %s any more", re)
		}
		parts = append(parts, m)
	}
	from := strings.Index(js, "// ------------------------------------------------------------ Update unpowered hosts (admins, Updates page)")
	to := strings.Index(js, "// ------------------------------------------------------------ users, roles and sites (admins only)")
	if from < 0 || to < from {
		t.Fatal("app.js has no \"Update unpowered hosts\" part any more")
	}
	return `"use strict";
const document = { addEventListener() {} };
const I = { onoff: "" };
const state = { me: { can_control: true }, systems: [], byId: {} };
let page = "updates";
const current = () => ({ page });
let answer = () => ({});
const asked = [];
const api = async (path) => { asked.push(path); return answer(); };
let renders = 0;
const render = () => { renders++; };
const flashes = [];
const flash = (msg, bad = false, details = null) => { flashes.push({ msg, bad, details }); };
const ago = () => "1m ago", controlWhyNot = () => "", infoBtn = () => "", fmtSpan = (s) => s + " s";
const $ = () => null, closeModal = () => {}, controlCall = async () => null;
` + strings.Join(parts, "\n") + "\n" + js[from:to] + `
const fail = (m) => { console.log("FAIL " + m); process.exitCode = 1; };
const words = (t) => t.replace(/<[^>]+>/g, " ").replace(/&#39;/g, "'").replace(/\s+/g, " ").trim();
const settle = () => new Promise((r) => setTimeout(r, 5));
const settings = { online_timeout_s: 300, settle_s: 60, shutdown_delay_s: 120 };
const host = (id, o) => ({ id, name: id.toUpperCase(), opted_in: true, can_join: true, can_wake: true, relay: "NAS", power: "off", choice: "wake", ...o });
const runHost = (id, o) => ({ id, name: id.toUpperCase(), mode: "wake", state: "waking", on: "", text: "Wake sent.", log: [], ...o });
const run = (id, state, hosts, o) => ({ id, state, by: "nora", started_at: "2026-11-13T16:18:00Z", list_hash: "abc123", hosts, ...o });
const data = (o) => ({ hosts: [], active: null, last: [], settings, off_after_s: 900, max_hosts: 16, ...o });
(async () => {
` + body + `
  if (!process.exitCode) console.log("ok");
})();
`
}

func runWuPageScript(t *testing.T, name, body string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the page's own functions are not run here")
	}
	file := filepath.Join(t.TempDir(), name+".js")
	if err := os.WriteFile(file, []byte(wuPageScript(t, appJS(t), body)), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, file).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

// Seen in a real run (13 Nov): the line "3 are switched on, N can be chosen right now" on the Updates page
// changed its number again and again while the run was going. N is how many of the machines could be ticked
// for a run at this moment, made from what each machine is doing right now (off for a quarter of an hour: yes;
// just woken, or just switched off: "not sure", no; on: yes). During a run every machine of the run goes
// through all of these. And during a run the number says nothing: no run can be started while one is going.
//
// While a run is going the line says how many machines are switched on, and nothing else; without a run it
// is as it was.
func TestUIWakeUpdateCanBeChosenIsNotCountedDuringARun(t *testing.T) {
	runWuPageScript(t, "wu-can", `
  // the line is over the table of machines, in the section when it is open (it is folded until it is opened, and open by itself during a run)
  wu.open.runs = true;
  const line = () => words((wakeUpdateBox().match(/<p class="note wu-rule info-keep">.*?<\/p>/s) || [""])[0]);
  const LEAD = "A machine takes part only after you switch it on here, one machine at a time. ";
  // no run: the number is there when not every machine that is switched on can be chosen
  wu.data = data({ hosts: [host("pc-a"), host("pc-b"), host("pc-c", { choice: "", power: "unclear", why: "not sure that it is off" }), host("pc-d", { opted_in: false })] });
  if (line() !== LEAD + "3 are switched on, 2 can be chosen right now.") fail("no run, one of three cannot be chosen: " + line());
  // folded, the count is said without the table (the same words, made once)
  wu.open.runs = false;
  const folded = words((wakeUpdateBox().match(/<p class="note wu-rule" id="wu-joined">.*?<\/p>/s) || [""])[0]);
  if (folded !== "3 are switched on, 2 can be chosen right now." || line() !== "") fail("folded: " + folded + " / " + line());
  wu.open.runs = true;
  wu.data = data({ hosts: [host("pc-a"), host("pc-b")] });
  if (line() !== LEAD + "2 are switched on.") fail("no run, all can be chosen: " + line());
  wu.data = data({ hosts: [host("pc-a", { choice: "" })] });
  if (line() !== LEAD + "1 is switched on, 0 can be chosen right now.") fail("no run, one machine: " + line());
  wu.data = data({ hosts: [host("pc-a", { opted_in: false })] });
  if (line() !== LEAD + "None is switched on yet.") fail("none switched on: " + line());
  // a run is going: the three machines are woken, come online, are updated and go off, one after the other.
  // What each could be chosen for changes all the time; the line does not.
  const going = run("r1", "running", [runHost("pc-a"), runHost("pc-b"), runHost("pc-c")]);
  const seen = new Set();
  for (const choices of [["wake", "wake", "wake"], ["", "wake", "wake"], ["update_only", "", "wake"], ["update_only", "update_only", ""], ["", "update_only", "update_only"], ["", "", ""]]) {
    wu.data = data({ active: going, hosts: ["pc-a", "pc-b", "pc-c"].map((id, i) => host(id, { choice: choices[i], power: choices[i] === "wake" ? "off" : choices[i] ? "on" : "unclear" })) });
    seen.add(line());
    if (wakeUpdateBox().includes("can be chosen")) fail("during a run, choices " + JSON.stringify(choices) + ": " + line());
  }
  if (seen.size !== 1 || !seen.has(LEAD + "3 are switched on.")) fail("during a run the line changed: " + JSON.stringify([...seen]));
  // the run is over: the number is back, with what holds then (machines that were just switched off are "not sure" for a while)
  wu.data = data({ last: [run("r1", "done", [])], hosts: ["pc-a", "pc-b", "pc-c"].map((id) => host(id, { choice: "", power: "unclear" })) });
  if (line() !== LEAD + "3 are switched on, 0 can be chosen right now.") fail("after the run: " + line());
`)
}

// Seen in a real run (13 Nov): when the run was over, nothing said so. The run's end makes a notification, of
// severity info, and pop-ups show critical events and warnings only; the page itself only changed the boxes.
//
// The page that saw a run going says once that it is over, with the line every result of an action is said
// with (flash): how it ended, how many machines, how many need a look, and each machine's sentence behind
// "Details" (so the line stays until it is closed). A run that ended while no page watched is not announced
// when a page loads, like every pop-up: it is the "Last run" on the Updates page and in Notifications.
func TestUIWakeUpdateSaysOnceThatTheRunIsOver(t *testing.T) {
	js := appJS(t)
	// on another page than Updates the page asks when a notification came (the end of a run makes one)
	if want := `if (changed && wuLive && current().page !== "updates") fetchWakeUpdate();`; strings.Count(js, want) != 1 {
		t.Errorf("app.js lacks (or has twice) %q", want)
	}
	runWuPageScript(t, "wu-over", `
  const off = (id) => runHost(id, { state: "off", on: "off", update: "ok", text: "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again." });
  const going = run("r1", "running", [runHost("pc-a"), runHost("pc-b"), runHost("<b>pc-c</b>")]);
  const over = run("r1", "done", [off("pc-a"), off("pc-b"), off("<b>pc-c</b>")], { ended_at: "2026-11-13T16:25:00Z" });
  const get = async (d) => { answer = () => d; await fetchWakeUpdate(); await settle(); };
  // the page opens while nothing runs, and an older run is on it: nothing is announced
  await get(data({ last: [run("r0", "done", [off("pc-a")])] }));
  if (flashes.length || wuLive !== "") fail("a run that was over when the page loaded: " + JSON.stringify(flashes));
  // a run is going: nothing yet, however often the page asks
  for (let i = 0; i < 3; i++) await get(data({ active: going, last: [run("r0", "done", [off("pc-a")])] }));
  if (flashes.length || wuLive !== "r1") fail("while it runs: " + JSON.stringify(flashes) + " live " + wuLive);
  // it is over: said once
  for (let i = 0; i < 3; i++) await get(data({ last: [over, run("r0", "done", [off("pc-a")])] }));
  if (flashes.length !== 1) fail("the run is over: said " + flashes.length + " times");
  let f = flashes[0] || {};
  if (f.msg !== "Update unpowered hosts: run finished · 3 machines" || f.bad !== false) fail("the line: " + JSON.stringify(f));
  if (JSON.stringify(f.details) !== JSON.stringify(["PC-A: " + off("pc-a").text, "PC-B: " + off("pc-b").text, "<B>PC-C</B>: " + off("x").text])) fail("Details: " + JSON.stringify(f.details));
  if (wuLive !== "") fail("the run is still taken for going: " + wuLive);
  // a run that needs a look: said as that, in red
  flashes.length = 0;
  const bad = run("r2", "done", [off("pc-a"), runHost("pc-b", { state: "left_on", on: "on", text: "It is still on 5 minutes after its shutdown was due: the shutdown did not work. It is ON." }),
    runHost("pc-c", { state: "off", on: "off", update: "failed", text: "Came online after the wake, was not updated, and switched off again." }), runHost("pc-d", { state: "gave_up", on: "unknown", text: "It did not come online." })]);
  await get(data({ active: run("r2", "running", [runHost("pc-a")]) }));
  await get(data({ last: [bad, over] }));
  f = flashes[0] || {};
  if (flashes.length !== 1 || f.msg !== "Update unpowered hosts: run finished · 4 machines · 3 to look at" || f.bad !== true || f.details.length !== 4) fail("a run with machines to look at: " + JSON.stringify(flashes));
  // cancelled, stopped: the word of the run's chip; one machine
  for (const [state, msg, red] of [["cancelled", "Update unpowered hosts: run cancelled · 1 machine", false], ["stopped", "Update unpowered hosts: run stopped · 1 machine", true], ["interrupted", "Update unpowered hosts: run interrupted · 1 machine", true]]) {
    flashes.length = 0;
    await get(data({ active: run("r-" + state, "running", [runHost("pc-a")]) }));
    await get(data({ last: [run("r-" + state, state, [runHost("pc-a", { state: "skipped", on: "untouched", text: "Not started." })])] }));
    if (flashes.length !== 1 || flashes[0].msg !== msg || flashes[0].bad !== red) fail(state + ": " + JSON.stringify(flashes));
  }
  // the next run was started before the page asked again: the one that ended is said, the new one is the one going
  flashes.length = 0;
  await get(data({ active: run("r5", "running", [runHost("pc-a")]) }));
  await get(data({ active: run("r6", "running", [runHost("pc-b")]), last: [run("r5", "done", [off("pc-a")])] }));
  if (flashes.length !== 1 || flashes[0].msg !== "Update unpowered hosts: run finished · 1 machine" || wuLive !== "r6") fail("two runs: " + JSON.stringify(flashes) + " live " + wuLive);
  // a run that is in no list any more: nothing to say, and nothing breaks
  flashes.length = 0;
  await get(data({}));
  if (flashes.length || wuLive !== "") fail("a run that is gone: " + JSON.stringify(flashes));
  // an answer that failed changes nothing about what the page has seen
  await get(data({ active: run("r7", "running", [runHost("pc-a")]) }));
  answer = () => { throw Object.assign(new Error("502"), { status: 502 }); };
  await fetchWakeUpdate(); await settle();
  if (flashes.length || wuLive !== "r7") fail("a failed answer: " + JSON.stringify(flashes) + " live " + wuLive);
  await get(data({ last: [run("r7", "done", [off("pc-a")])] }));
  if (flashes.length !== 1) fail("after a failed answer the end is not said");
  // someone who is no admin asks for nothing and is told nothing
  flashes.length = 0; asked.length = 0; state.me = { can_control: false };
  await fetchWakeUpdate();
  if (asked.length || flashes.length) fail("a viewer: " + asked.length + " requests");
`)
}
