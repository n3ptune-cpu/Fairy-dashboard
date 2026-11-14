package web

import "testing"

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
