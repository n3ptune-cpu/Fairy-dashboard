package web

import (
	"strings"
	"testing"
)

// Keep on where an admin looks for it: in the row of the machine in its run, for as long as the machine may
// switch itself off by that run's ticket (the strip's data says which). The page's own functions, run with node.
func TestUIRunRowHasKeepOnWhileTheMachineMaySwitchItselfOff(t *testing.T) {
	runWuPageScript(t, "runrow", `
  const id = "00000000000000aa";
  const r = run(id, "running", [runHost("pc-a", { state: "awaiting_approval", checked: true, text: "awake and armed" }), runHost("pc-b", { state: "waking" })], { approval: "needed" });
  const keeps = (html) => html.split('data-act="rw-keep"').length - 1;
  // nothing is known of armed machines (the strip's data is not there, or names nobody): no button
  if (keeps(wuRunBox(r, true)) !== 0) fail("Keep on without the strip's data");
  globalThis.rw = { data: { run: id, armed: [] } };
  if (keeps(wuRunBox(r, true)) !== 0) fail("Keep on while nobody is armed");
  // PC-A may switch itself off by this run's ticket: its row has the button, and no other row has
  rw.data.armed = [{ run: id, id: "pc-a", name: "PC-A", sure: true, active: true }];
  const box = wuRunBox(r, true), rows = box.split('data-key="wuh-').slice(1);
  if (rows.length !== 2 || keeps(box) !== 1) fail("rows and buttons: " + rows.length + ", " + keeps(box));
  if (!rows[0].includes('<button class="btn sm push" type="button" data-act="rw-keep" data-id="pc-a" data-name="PC-A">Keep on</button>')) fail("PC-A's row: " + rows[0]);
  if (words(rows[0]).indexOf("Keep on") > words(rows[0]).indexOf("Wake")) fail("the button is not in the row's first line: " + words(rows[0]));
  // the question over, the machine still armed: the button stays in its row
  rw.data.run = "";
  if (keeps(wuRunBox({ ...r, approval: "over" }, true)) !== 1) fail("no Keep on after the question");
  // a row of another run with the same machine (an earlier report) has none
  const earlier = run("00000000000000bb", "done", [runHost("pc-a", { state: "off", on: "off" })]);
  if (keeps(wuRunBox(earlier, false)) !== 0) fail("Keep on in another run's row");
  // the box does not say again what the strip over the page says
  if (/strip at the top/.test(wuRunBox(r, true))) fail("the run's box repeats the strip");
`)
}

// A machine with the check that nobody confirmed ends as planned (off again, or it stays on) but was not
// updated: its chip says so in amber, its Update step is red, and the run counts it as one to look at, like a
// machine whose update failed. A machine without the check, and one that was updated, are as before.
func TestUIRunRowSaysNotUpdatedForAMachineNobodyConfirmed(t *testing.T) {
	runWuPageScript(t, "notupdated", `
  const at = "2026-11-13T16:20:00Z";
  const woke = { state: "off", on: "off", wake_sent_at: at, online_at: at, shutdown_at: at };
  const r = run("00000000000000aa", "done", [
    runHost("pc-a", { ...woke, checked: true, text: "It came online after the wake. Nobody confirmed within 10 min: not confirmed, not updated. It switched itself off again." }),
    runHost("pc-b", { ...woke, update: "ok", text: "Came online after the wake, updated, and switched off again." }),
    runHost("pc-c", { ...woke, checked: true, update: "ok", text: "Came online after the wake, confirmed, updated, and switched off again." }),
    runHost("pc-d", { mode: "update_only", already_on: true, state: "stayed_on", on: "on", checked: true, text: "It was on. Nobody confirmed: not confirmed, not updated. It stays on." }),
    runHost("pc-e", { state: "skipped", on: "untouched", checked: true, text: "Left out." }),
  ], { ended_at: at });
  const rows = wuRunBox(r, false).split('data-key="wuh-').slice(1);
  const chip = (row) => (row.match(/<span class="chip ([a-z]*)">([^<]*)<\/span>/) || []).slice(1).join("|");
  const want = ["warn|done · off again · not updated", "ok|done · off again", "ok|done · off again", "warn|done · stays on · not updated", "|skipped"];
  rows.forEach((row, i) => { if (chip(row) !== want[i]) fail("row " + i + ": " + chip(row)); });
  if (!rows[0].includes('<span class="bad">Update</span>') || !rows[1].includes('<span class="done">Update</span>') || !rows[3].includes('<span class="bad">Update</span>')) fail("the Update step: " + rows[0]);
  if (r.hosts.filter(wuNeedsLook).length !== 2) fail("to look at: " + r.hosts.filter(wuNeedsLook).map((h) => h.id));
  if (!/2 to look at/.test(words(wuLastLine(r)))) fail("the folded line: " + words(wuLastLine(r)));
  // a machine with the check that is still in its run is not "not updated" yet
  const live = run("00000000000000bb", "running", [runHost("pc-a", { state: "awaiting_approval", checked: true, wake_sent_at: at, online_at: at })]);
  if (/not updated/.test(wuRunBox(live, true)) || wuRunBox(live, true).includes('<span class="bad">Update</span>')) fail("a machine that still waits: " + wuRunBox(live, true));
`)
}

// What the Updates page says about machines with the check before a run: one short line under the section (the
// rest behind its info button, with no number the configuration may have changed), and in the run's dialog a
// few words on the line of each such machine that would be woken.
func TestUIRunsOfCheckedMachinesAreSaidShortly(t *testing.T) {
	js := appJS(t)
	runWuPageScript(t, "checkednote", `
  const note = () => words((wakeUpdateBox().match(/<div class="note" id="wu-chk">.*?<\/div>/s) || [""])[0]);
  wu.data = data({ hosts: [host("pc-a", { checked: true }), host("pc-b")] });
  if (note() !== "A machine with the check on asks for a second passkey once it is up. Approvals") fail("the line: " + note());
  wu.data = data({ hosts: [host("pc-b")] });
  if (note() !== "") fail("the line without such a machine: " + note());
`)
	i := strings.Index(js, `infoBtn("wu-chk", "Machines with the check in a run", infoList([`)
	if i < 0 {
		t.Fatal("the line about machines with the check has no info button")
	}
	more := js[i : i+strings.Index(js[i:], "]))")]
	for _, want := range []string{"signs a ticket", "switches itself off again by itself unless you confirm", "strip on every page", "one update and one shutdown",
		"Not confirmed: not updated", "Keep on"} {
		if !strings.Contains(more, want) {
			t.Errorf("behind the info button nothing says %q", want)
		}
	}
	if strings.Contains(more, "10 minutes") || strings.Contains(more, "600") {
		t.Error("the info text has a time of its own: the time to confirm is the configuration's, and the strip counts it")
	}
	dlg := js[strings.Index(js, `async function openWakeUpdate()`):]
	dlg = dlg[:strings.Index(dlg, "m.hidden = false;")]
	if !strings.Contains(dlg, `${kind === "wake" && h.checked ? " · asks for a second passkey once it is up" : ""}`) {
		t.Error("the dialog's line of a machine with the check that is woken does not say that a second passkey is asked for")
	}
}
