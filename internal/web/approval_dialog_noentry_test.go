package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The line next to "Turn on" when the page has no machine for the row it draws. It read "Checking whether it
// can be switched on…" in all three cases, and so for good in two of them (build 13: the row of a Proxmox
// node, whose machine the list named by its agent's id). Each case has its own words now:
//
//   - the list is here and has no entry for this row: "It cannot be switched on from this dialog." (and why,
//     behind "More"); no request is made again and again for a list that will not change;
//   - the list could not be had: the error, and the next drawing asks again;
//   - it is on its way: "Checking…".
//
// The page's own functions, run in node, with a row whose id is not its machine's (a node's row).
func TestUIApprovalDialogWithoutAMachineForTheRow(t *testing.T) {
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
const I = { shield: "", apKey: "" };
const state = { me: { can_control: true, approval_client: true }, systems: [], byId: {} };
const asked = [];
let answer = () => ({ machines: [] });
const api = async (path) => { asked.push(path); return answer(); };
let renders = 0;
const render = () => { renders++; }, ago = () => "1m ago", controlWhyNot = () => "", infoBtn = () => "", infoList = (x) => x.join("|");
` + strings.Join(parts, "\n") + "\n" + js[from:to] + `
const fail = (m) => { console.log("FAIL " + m); process.exitCode = 1; };
const words = (t) => t.replace(/<[^>]+>/g, " ").replace(/&#39;/g, "'").replace(/\s+/g, " ").trim();
const settle = () => new Promise((r) => setTimeout(r, 5));
const realNow = Date.now; let skew = 0; Date.now = () => realNow() + skew;   // to step over the 30 s between two requests
const off = { state: "off", enforced_by: "agent", proto: 2 };
const row = { id: "node-n1", name: "NODE-ONE", kind: "node", approval: off };       // the row of a Proxmox node
const other = { id: "a2", name: "TWO", kind: "agent", approval: off };              // an agent that is a row of its own
state.systems = [row, other]; state.byId = { [row.id]: row, [other.id]: other };
const CHECKING = "Checking whether it can be switched on…", NONE = "It cannot be switched on from this dialog.";
const line = (s) => { const v = approvalBox(s); return [words(v.html).replace(/^.*Would be checked by the agent itself /, ""), v]; };
(async () => {
  // 1. the list is on its way: "Checking…", one request
  let [l] = line(row);
  if (l !== CHECKING || asked.length !== 1) fail("on its way: " + JSON.stringify(l) + ", requests " + asked.length);
  // 2. the list came and names the row's machine in system_id (its id is the agent's): Turn on, with the agent's id
  answer = () => ({ machines: [{ id: "agent-of-n1", system_id: "node-n1", name: "NODE-ONE", state: "off", ready: true, users: [] }, { id: "a2", system_id: "a2", name: "TWO", state: "off", ready: true, users: [] }] });
  apv.data = null; apv.at = 0; asked.length = 0; line(row); await settle();
  let v; [l, v] = line(row);
  if (l !== "Turn on Ready" || !v.html.includes('data-act="ap-open" data-id="agent-of-n1"')) fail("the node's row with its machine in the list: " + JSON.stringify(l) + " " + v.html);
  if (apvMachine(row)?.id !== "agent-of-n1" || apvMachine(other)?.id !== "a2") fail("apvMachine: " + apvMachine(row)?.id + " / " + apvMachine(other)?.id);
  // 3. the list is here and has no entry for the row (what build 13 answered for a node's row): one clear line,
  //    the reason behind "More", no Turn on, and no request for every drawing, however long it stands
  answer = () => ({ machines: [{ id: "agent-of-n1", system_id: "agent-of-n1", name: "agent", state: "off", ready: true, users: [] }, { id: "a2", system_id: "a2", name: "TWO", state: "off", ready: true, users: [] }] });
  apv.data = null; apv.at = 0; asked.length = 0; line(row); await settle();
  for (let i = 0; i < 4; i++) {
    [l, v] = line(row);
    if (l !== NONE || v.html.includes("Checking") || v.html.includes('data-act="ap-open"') || !v.html.includes('id="ap-on-note"')) fail("no entry, drawing " + i + ": " + JSON.stringify(l));
    if (!v.more.some((x) => x.startsWith("It cannot be switched on from this dialog: the panel's list of machines has no entry for this host."))) fail("no entry: More: " + JSON.stringify(v.more));
    if (line(other)[0] !== "Turn on Ready") fail("no entry: the other row lost its button");
    await settle(); skew += 61000;
  }
  if (asked.length !== 1) fail("no entry: the list was asked for " + asked.length + " times");
  // ... and when the row's block changes after that, the list is taken for old: "Checking…", not the sentence
  [l] = line({ ...row, approval: { ...off, binding: "vanished" } });
  if (!l.endsWith(CHECKING)) fail("no entry, the block changed: " + JSON.stringify(l));
  await settle();
  // 4. the list could not be had: the error is said, as text, and the next drawing asks again
  answer = () => { throw Object.assign(new Error("the panel <b>answered</b> 502"), { status: 502 }); };
  apv.data = null; apv.error = null; apv.at = 0; asked.length = 0; skew += 61000;
  [l] = line(row);
  if (l !== CHECKING) fail("failed, before the answer: " + JSON.stringify(l));
  await settle();
  [l, v] = line(row);
  if (l !== "Could not check whether it can be switched on: the panel <b>answered</b> 502" && !v.html.includes("Could not check whether it can be switched on: the panel &lt;b&gt;answered&lt;/b&gt; 502")) fail("failed: " + JSON.stringify(l) + " " + v.html);
  if (v.html.includes("<b>answered") || v.html.includes("Checking") || v.html.includes('data-act="ap-open"')) fail("failed: " + v.html);
  skew += 31000; line(row); await settle();
  if (asked.length !== 2) fail("failed: asked " + asked.length + " times, want a second request after 30 s");
  // ... and the answer that comes then is used
  answer = () => ({ machines: [{ id: "agent-of-n1", system_id: "node-n1", name: "NODE-ONE", state: "off", ready: true, users: [] }] });
  skew += 31000; line(row); await settle();
  [l] = line(row);
  if (l !== "Turn on Ready") fail("after the failure: " + JSON.stringify(l));
  // a viewer who is no admin has none of these lines, and asks for nothing
  state.me = { can_control: false, approval_client: true }; apv.data = null; asked.length = 0;
  [l, v] = line(row);
  if (v.html.includes("switched on from") || v.html.includes("Checking") || v.html.includes("Could not check") || asked.length) fail("a viewer: " + v.html);
  if (!process.exitCode) console.log("ok");
})();
`
	file := filepath.Join(t.TempDir(), "approval-noentry.js")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, file).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
