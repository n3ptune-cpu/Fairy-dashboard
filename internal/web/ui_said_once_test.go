package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
)

// From the owner's click-through of 2026-11-12: two messages said their machine twice, "Shut down NSX-TEST-VM in
// 1 minute: shutdown in 60 s" and "Accept that the check on NSX-TEST-VM is off: NSX-TEST-VM: its check is off,
// and Fairy treats it as a machine without the check from now on." After "Confirm it's you" the page put what
// was asked in front of what happened. For a power action, Turn on and an accept it says it once now.
func TestUIMessagesSayTheirMachineOnce(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		// the result the panel kept while "Confirm it's you" was on its way
		"else if (SAID_ONCE.test(pr.path || \"\")) whenSystems(() => flash(saidOnce(pr) || `${pr.label}: ${pr.ok ? pr.message || \"done\" : `failed · ${pr.message || `HTTP ${pr.status}`}`}`, !pr.ok));",
		// the same action without a confirmation in between says the same
		`close(); state.menu = null; flash(sayOnce(s.name, r.message)); refreshCaps(s.id);`,
		`if (r) { flash(sayOnce(s.name, r.message)); refreshCaps(s.id); }`,
		// everything else keeps "<what was asked>: <what happened>"
		"else flash(`${pr.label}: ${pr.ok ? pr.message || \"done\" : `failed · ${pr.message || `HTTP ${pr.status}`}`}`, !pr.ok);",
		"if (pr.items?.length) flash(`${pr.label}: ${pr.message || (pr.ok ? \"done\" : \"not everything worked\")}`, !pr.ok, pr.items);",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the page's own functions are not run here")
	}
	var parts []string
	for _, re := range []string{`(?m)^const SAID_ONCE = .*$`, `(?m)^const endDot = .*$`, `(?m)^const sayOnce = .*$`, `(?ms)^function saidOnce\(pr\) \{.*?^\}$`} {
		m := regexp.MustCompile(re).FindString(js)
		if m == "" {
			t.Fatalf("app.js has no %s any more", re)
		}
		parts = append(parts, m)
	}
	// (audit 6, r4-p3-06) what "names its machine" means is a function of its own now; the page before it had none
	parts = append([]string{regexp.MustCompile(`(?m)^const namesIt = .*$`).FindString(js)}, parts...)
	script := `"use strict";
const state = { systems: [{ id: "vm", name: "NSX-TEST-VM" }, { id: "relay", name: "RELAY" }, { id: "nameless" },
  // (audit 6, r4-p3-06) machines whose names are letters of an ordinary result
  { id: "s", name: "s" }, { id: "in", name: "in" }, { id: "sixty", name: "60" }, { id: "min", name: "minute" }, { id: "vmx", name: "NSX" }], byId: {} };
for (const s of state.systems) state.byId[s.id] = s;
` + strings.Join(parts, "\n") + `
const fail = (m) => { console.log("FAIL " + m); process.exitCode = 1; };
const power = "/api/systems/vm/power", accept = "/api/admin/approval/accept/vm", turnOn = "/api/admin/approval/turn-on";
const cases = [
  // path, ok, what was asked, what happened, the line ("" = the usual "<asked>: <happened>")
  [power, true, "Shut down NSX-TEST-VM in 1 minute", "shutdown in 60 s", "NSX-TEST-VM: shutdown in 60 s."],
  [power, true, "Reboot NSX-TEST-VM in 5 minutes", "reboot in 300 s", "NSX-TEST-VM: reboot in 300 s."],
  [power, true, "Sleep NSX-TEST-VM", "sleep in 10 s", "NSX-TEST-VM: sleep in 10 s."],
  [power, true, "Stop (hard) NSX-TEST-VM", "stop sent to Proxmox (task queued on pve1)", "NSX-TEST-VM: stop sent to Proxmox (task queued on pve1)."],
  [power, true, "Power on (Wake-on-LAN) · NSX-TEST-VM", "magic packet sent via RELAY; give it a minute to boot", "NSX-TEST-VM: magic packet sent via RELAY; give it a minute to boot."],
  [power, true, "Cancel pending · NSX-TEST-VM", "The shutdown of NSX-TEST-VM is cancelled.", "The shutdown of NSX-TEST-VM is cancelled."],
  [power, false, "Shut down NSX-TEST-VM in 1 minute", "NSX-TEST-VM did not accept the approval (NSX-TEST-VM says: approval refused (user))", "NSX-TEST-VM did not accept the approval (NSX-TEST-VM says: approval refused (user))."],
  [power, false, "Shut down NSX-TEST-VM in 1 minute", "too many control requests; try again in 42 s", ""],
  [power, false, "Shut down NSX-TEST-VM in 1 minute", "", ""],
  ["/api/systems/gone/power", true, "Shut down GONE", "shutdown in 60 s", ""],
  ["/api/systems/nameless/power", true, "Shut down it", "shutdown in 60 s", ""],
  [accept, true, "Accept that the check on NSX-TEST-VM is off", "NSX-TEST-VM: check off, accepted.", "NSX-TEST-VM: check off, accepted."],
  [accept, true, "Accept the machine id of NSX-TEST-VM", "NSX-TEST-VM: the machine id it shows is on record now.", "NSX-TEST-VM: the machine id it shows is on record now."],
  [accept, true, "NSX-TEST-VM keeps its machine id", "NSX-TEST-VM keeps the machine id. RELAY stays flagged until it shows an id of its own (on it: fairy-agent helper-trust set --new-machine-id).", "NSX-TEST-VM keeps the machine id. RELAY stays flagged until it shows an id of its own (on it: fairy-agent helper-trust set --new-machine-id)."],
  [accept, false, "Accept the new machine id of NSX-TEST-VM", "NSX-TEST-VM: nothing was accepted. What it shows is not what the dialog showed: it changed while you were confirming. Press Approval on its page, look at what it shows now, and press the button again.", "NSX-TEST-VM: nothing was accepted. What it shows is not what the dialog showed: it changed while you were confirming. Press Approval on its page, look at what it shows now, and press the button again."],
  [accept, false, "Accept the new machine id of NSX-TEST-VM", "too many control requests; try again in 42 s", ""],
  [turnOn, true, "Turn on the check on the machine itself: 1 machine", "1 of 1 machines now check approvals themselves. NSX-TEST-VM: accepted", "1 of 1 machines now check approvals themselves. NSX-TEST-VM: accepted."],
  [turnOn, false, "Turn on the check on the machine itself: 1 machine", "0 of 1 machines now check approvals themselves. NSX-TEST-VM: proof refused", "0 of 1 machines now check approvals themselves. NSX-TEST-VM: proof refused."],
];
for (const [path, ok, label, message, want] of cases) {
  if (!SAID_ONCE.test(path)) fail(path + " is not one of the routes");
  const got = saidOnce({ path, ok, label, message, status: ok ? 200 : 409 });
  if (got !== want) fail(JSON.stringify(label + " / " + message) + ": " + JSON.stringify(got) + ", want " + JSON.stringify(want));
  // the machine's name is in the line once, unless the panel's own sentence says it twice
  if (got && got.split("NSX-TEST-VM").length - 1 > Math.max(1, message.split("NSX-TEST-VM").length - 1)) fail("said twice: " + got);
}
// (audit 6, r4-p3-06) the name counts as a whole and where the panel's sentences put it, not as letters somewhere
// in the text: a machine called "s", "in" or "60" was never named, and its refusals lost what was asked and "failed"
const whole = [
  ["/api/systems/s/power", true, "Shut down s in 1 minute", "shutdown in 60 s", "s: shutdown in 60 s."],
  ["/api/systems/in/power", true, "Shut down in in 1 minute", "shutdown in 60 s", "in: shutdown in 60 s."],
  ["/api/systems/sixty/power", true, "Shut down 60 in 1 minute", "shutdown in 60 s", "60: shutdown in 60 s."],
  ["/api/systems/min/power", true, "Power on (Wake-on-LAN) · minute", "magic packet sent via RELAY; give it a minute to boot", "minute: magic packet sent via RELAY; give it a minute to boot."],
  ["/api/systems/s/power", false, "Shut down s in 1 minute", "too many control requests; try again in 42 s", ""],
  ["/api/systems/s/power", false, "Shut down s in 1 minute", "s did not accept the approval (s says: approval refused (user))", "s did not accept the approval (s says: approval refused (user))."],
  ["/api/systems/s/power", true, "Cancel pending · s", "The shutdown of s is cancelled.", "The shutdown of s is cancelled."],
  ["/api/systems/vmx/power", true, "Shut down NSX in 1 minute", "shutdown in 60 s", "NSX: shutdown in 60 s."],
  // a refusal that names no machine keeps what was asked and "failed", whatever the machines are called
  ["/api/admin/approval/accept/s", false, "Accept the new machine id of s", "too many control requests; try again in 42 s", ""],
  [turnOn, false, "Turn on the check on the machine itself: 1 machine", "too many control requests; try again in 42 s", ""],
  [turnOn, true, "Turn on the check on the machine itself: 2 machines", "2 of 2 machines now check approvals themselves. s: accepted; in: accepted", "2 of 2 machines now check approvals themselves. s: accepted; in: accepted."],
  // a name that is the start of another name is not that name
  ["/api/admin/approval/accept/vmx", true, "Accept the machine id of NSX", "NSX: the machine id it shows is on record now.", "NSX: the machine id it shows is on record now."],
];
for (const [path, ok, label, message, want] of whole) {
  const got = saidOnce({ path, ok, label, message, status: ok ? 200 : 429 });
  if (got !== want) fail("whole name: " + JSON.stringify(label + " / " + message) + ": " + JSON.stringify(got) + ", want " + JSON.stringify(want));
}
if (sayOnce("s", "shutdown in 60 s") !== "s: shutdown in 60 s." || sayOnce("NSX", "NSX-TEST-VM: accepted") !== "NSX: NSX-TEST-VM: accepted." || sayOnce("in", "in: accepted") !== "in: accepted.") fail("sayOnce goes by letters");
// an update, a fan change, Add host, a run: untouched
for (const p of ["/api/admin/updates/vm", "/api/admin/updates", "/api/systems/vm/fan/mode", "/api/admin/hosts", "/api/admin/wake-update", "/api/systems/vm/power/x", "/api/admin/approval"]) if (SAID_ONCE.test(p)) fail(p + " must keep its usual line");
// the same action without a confirmation in between
if (sayOnce("NSX-TEST-VM", "shutdown in 60 s") !== "NSX-TEST-VM: shutdown in 60 s." || sayOnce("NSX-TEST-VM", "The shutdown of NSX-TEST-VM is cancelled.") !== "The shutdown of NSX-TEST-VM is cancelled." ||
  sayOnce("", "cancelled") !== "cancelled." || sayOnce("NSX-TEST-VM", undefined) !== "NSX-TEST-VM: done.") fail("sayOnce");
if (!process.exitCode) console.log("ok " + cases.length + " messages");
`
	file := filepath.Join(t.TempDir(), "said-once.js")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, file).CombinedOutput()
	if err != nil || !strings.HasPrefix(string(out), "ok 18 messages") {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

// "Accept that its check is off" answers in one short line. What it means ("it becomes a machine without the
// check") is what "Confirm it's you" shows before; the audit log and the notification are as they were.
func TestAnAcceptedOffAnswersInOneShortLine(t *testing.T) {
	x := newAcceptAdmin(t)
	x.setTrust(t, x.laptop)
	x.poll()
	if what, _, err := x.accept("laptop", "nora@example.test"); what != "id" || err != nil {
		t.Fatal(what, err)
	}
	x.clearTrust(t, x.laptop)
	x.poll()
	if b := x.s.Poller.BindingOf("laptop"); b.Status != approval.BindVanished {
		t.Fatalf("laptop after its trust was cleared: %+v", b)
	}
	r := x.press("laptop", map[string]any{"off": true})
	if got := actionOf(r); !strings.Contains(got, "it shows no check any more: it becomes a machine without the check") {
		t.Fatalf("the dialog must say what an accepted off means: %q", got)
	}
	res := x.confirm(t, r)
	if res["ok"] != true || res["message"] != "laptop: check off, accepted." {
		t.Fatalf("the result: %v", res)
	}
	if b := x.s.Poller.BindingOf("laptop"); b.Status == approval.BindVanished || b.Checks() {
		t.Fatalf("after the accept laptop is a machine without the check: %+v", b)
	}
	off := 0
	for _, e := range x.auditEntries("", "approval.accept") {
		if p, _ := e["params"].(map[string]any); e["target"] == "laptop" && e["outcome"] == "ok" && p["accepted"] == "off" {
			off++
		}
	}
	if off != 1 {
		t.Fatalf("audit: want one approval.accept of laptop with accepted: off, outcome ok; got %d in %v", off, x.auditEntries("", "approval.accept"))
	}
}
