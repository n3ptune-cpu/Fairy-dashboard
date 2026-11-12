package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// While a machine's check needs an action now (the states whose header button is red), the host's page has one
// line under the header, in the form of the countdown's line: one short sentence and the button that opens the
// dialog. It is there for everyone who sees the host, and holds nothing of the admin's part. Green and amber
// have no line.
func TestUIApprovalRedLine(t *testing.T) {
	js, css := appJS(t), appCSS(t)
	for _, want := range []string{
		`${s.power_pending ? pendingStrip(s, why) : ""}${approvalStrip(s)}`, // under the countdown's line when both are there
		`const AP_STRIP = table({`,
		`if (!a || !((a.binding && AP_BINDING[a.binding]) || a.state === "broken")) return "";`, // exactly the states of the red button
		"return `<div class=\"pending-strip bad\" id=\"ap-strip\" data-key=\"ap-strip\"><span>${esc(text)}</span><button class=\"btn push\" type=\"button\" data-act=\"ap-dlg\" aria-haspopup=\"dialog\">${I.shield}${esc(AP_LABEL)}</button></div>`;",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	// the colours of .alert-strip.bad, without its pulse
	if !strings.Contains(css, ".pending-strip.bad { border-color: rgba(255, 93, 108, .6); background: rgba(255, 93, 108, .13); color: #ffc9ce; }") ||
		!strings.Contains(css, ".alert-strip.bad { border-color: rgba(255, 93, 108, .6); background: rgba(255, 93, 108, .13); color: #ffc9ce;") {
		t.Error("app.css: the red line has not the colours of .alert-strip.bad")
	}
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
const document = { addEventListener() {} };
const I = { shield: "<svg data-i=shield></svg>", apKey: "<svg></svg>" };
const state = { me: null, systems: [], byId: {} };
const api = async () => ({ machines: [] });
const render = () => {}, ago = () => "1m ago", controlWhyNot = () => "", infoBtn = (key) => "[info " + key + "]", infoList = (x) => x.join("|");
` + strings.Join(parts, "\n") + "\n" + js[from:to] + `
const fail = (m) => { console.log("FAIL " + m); process.exitCode = 1; };
const SHOWN = "SHOWN-by-the-machine-BBBBBBBBBBBBBBBBBB";
const on = { state: "required", enforced_by: "helper", proto: 2 };
const cases = [
  ["required", on, ""], ["off", { state: "off" }, ""], ["unknown", { state: "unknown" }, ""], ["required, last known", { ...on, last_known: true }, ""],
  ["a binding this page does not know", { ...on, binding: "later" }, ""],
  ["broken", { state: "broken", error: "trust file: <bad>" }, "Approval broken: it refuses fan, power and update requests."],
  ["changed", { ...on, binding: "changed" }, "Machine id changed: no approvals until the new id is accepted."],
  ["duplicate", { ...on, binding: "duplicate", binding_with: ["BETA"] }, "One machine id on two hosts: no approvals for either."],
  ["duplicate, the other keeps the id", { ...on, binding: "duplicate", binding_with: ["BETA", "GAMMA"], binding_held: ["BETA"] }, "One machine id on two hosts: BETA keeps it."],
  ["duplicate, a name with markup", { ...on, binding: "duplicate", binding_held: ["<b>X</b>"] }, "One machine id on two hosts: &lt;b&gt;X&lt;/b&gt; keeps it."],
  ["vanished", { state: "off", binding: "vanished" }, "Check gone: this machine shows no check now. Fairy sends it nothing."],
  ["vanished, the block still says required", { ...on, binding: "vanished" }, "Check gone: this machine shows no check now. Fairy sends it nothing."],
  ["too old", { ...on, proto: 1, binding: "too_old" }, "Agent too old: its approvals do not name the machine."],
  ["not on record", { ...on, binding: "no_record" }, "Not on record: no approvals until its machine id is accepted."],
  ["record unreadable", { ...on, binding: "unreadable" }, "Record unreadable: Fairy asks no machine for an approval."],
];
for (const admin of [true, false]) {
  state.me = { can_control: admin, approval_client: true };
  apv.data = { machines: [{ id: "h1", system_id: "h1", machine_id: "MID-on-record", shown_id: SHOWN, can_accept: true, accept: "id", with: ["BETA"] }] }; apv.at = Date.now();
  for (const [name, a, want] of cases) {
    const s = { id: "h1", name: "ALPHA", approval: a }, line = approvalStrip(s), red = approvalBox(s).tone === "bad";
    const text = (line.match(/<span>(.*?)<\/span>/) || [])[1] || "";
    if (text !== want) fail(name + ": the line is " + JSON.stringify(text) + ", want " + JSON.stringify(want));
    if (red !== (line !== "")) fail(name + ": the red line and the red button go together (button red: " + red + ")");
    if (!want) { if (line !== "") fail(name + ": a line where none belongs: " + line); continue; }
    if (!line.startsWith('<div class="pending-strip bad" id="ap-strip" data-key="ap-strip"><span>') || !line.endsWith('<button class="btn push" type="button" data-act="ap-dlg" aria-haspopup="dialog">' + I.shield + "Approval</button></div>")) fail(name + ": the line's form: " + line);
    if (want.split(" ").length > 13) fail(name + ": more than a short sentence: " + want);
    for (const x of ["MID-on-record", SHOWN, "ap-accept", "trust file"]) if (line.includes(x)) fail(name + ": the line holds " + x);
  }
}
// every alarm the page knows has its sentence
for (const k of Object.keys(AP_BINDING)) if (!AP_STRIP[k]) fail("no sentence for " + k);
for (const k of Object.keys(AP_STRIP)) if (!AP_BINDING[k] || !AP_STRIP[k].startsWith(AP_BINDING[k][0] + ": ")) fail("the sentence of " + k + " does not start with the alarm's name: " + AP_STRIP[k]);
const none = { id: "h1", name: "ALPHA" };
if (approvalStrip(none) !== "" || approvalStrip({ ...none, approval: "x" }) !== "") fail("a host without the block");
if (!process.exitCode) console.log("ok " + cases.length + " states");
`
	file := filepath.Join(t.TempDir(), "approval-strip.js")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, file).CombinedOutput()
	if err != nil || !strings.HasPrefix(string(out), "ok 15 states") {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
