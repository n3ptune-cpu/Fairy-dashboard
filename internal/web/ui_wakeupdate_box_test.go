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
  const line = () => words((wakeUpdateBox().match(/<p class="note wu-rule info-keep">.*?<\/p>/s) || [""])[0]);
  const LEAD = "A machine takes part only after you switch it on here, one machine at a time. ";
  // no run: the number is there when not every machine that is switched on can be chosen
  wu.data = data({ hosts: [host("pc-a"), host("pc-b"), host("pc-c", { choice: "", power: "unclear", why: "not sure that it is off" }), host("pc-d", { opted_in: false })] });
  if (line() !== LEAD + "3 are switched on, 2 can be chosen right now.") fail("no run, one of three cannot be chosen: " + line());
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
