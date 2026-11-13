package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// powerPageScript is the power part of app.js for node: the page's own linkedPower, wallPower, pagePower and
// powerSourceRow are run, nothing of them is copied. What is around them is not the code under test.
func powerPageScript(t *testing.T, js, body string) string {
	t.Helper()
	var parts []string
	for _, re := range []string{`(?m)^const own = .*$`, `(?m)^const esc = .*$`, `(?m)^const enc = .*$`, `(?m)^const fmtW = .*$`, `(?m)^const upsById = .*$`, `(?m)^const infoList = .*$`} {
		m := regexp.MustCompile(re).FindString(js)
		if m == "" {
			t.Fatalf("app.js has no %s any more", re)
		}
		parts = append(parts, m)
	}
	block := func(from, to string) string {
		i := strings.Index(js, from)
		j := strings.Index(js, to)
		if i < 0 || j < i {
			t.Fatalf("app.js has no part from %q to %q any more", from, to)
		}
		return js[i:j]
	}
	return `"use strict";
const state = { power: null, history: [], systems: [], ups: { ups: [] } };
const spark = () => "", setTitle = () => {}, infoBtn = () => "";
` + strings.Join(parts, "\n") + "\n" +
		block("const linkedPower = (id) => {", "// ------------------------------------------------------------ grouping: hosts by site") +
		block("function pagePower() {", "// ------------------------------------------------------------ UPS page") + `
const fail = (m) => { console.log("FAIL " + m); process.exitCode = 1; };
const words = (t) => t.replace(/<[^>]+>/g, " ").replace(/&#39;/g, "'").replace(/\s+/g, " ").trim();
const src = (id, o) => ({ id, type: "homeassistant", name: id, status: "ok", include_in_total: true, readings: [], last_success: null, error: null, via: "relay", ...o });
const rd = (watts, system_id) => ({ source_id: "", name: "", watts, voltage: null, current: null, energy_kwh_total: null, system_id });
` + body + `
if (!process.exitCode) console.log("ok");
`
}

func runPowerPageScript(t *testing.T, name, body string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the page's own functions are not run here")
	}
	file := filepath.Join(t.TempDir(), name+".js")
	if err := os.WriteFile(file, []byte(powerPageScript(t, appJS(t), body)), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, file).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

// The host's page, panel "Power": the wall-power line from a plug an agent reads through Home Assistant. The
// row of the machine is a Proxmox node's; the plug names that row (system_id, as the panel gives it for a link
// written with either of the machine's ids). A plug that fails stays on the page with the agent's words for
// why: with another plug that reads, under the watts; alone, in place of them, and not as "No wall-power
// source linked".
func TestUIHostPageWallPowerFromAHomeAssistantPlug(t *testing.T) {
	runPowerPageScript(t, "wall", `
const row = "lab-node-pve-b";
state.power = { total_w: 52.5, sources: [
  src("relay/plug-b", { name: "PVE-B plug", system_id: row, readings: [rd(52.5, row)] }),
  src("relay/other", { name: "Other plug", system_id: "pc-x", readings: [rd(10, "pc-x")] }) ] };
let lp = linkedPower(row);
if (lp.watts !== 52.5 || lp.sources.length !== 1) fail("the node's row: " + JSON.stringify(lp));
if (words(wallPower(lp)) !== "52.5 W wall · PVE-B plug") fail("one plug that reads: " + words(wallPower(lp)));
if (linkedPower("lab-b-agent").watts !== null || words(wallPower(linkedPower("lab-b-agent"))) !== "No wall-power source linked.") fail("a row no plug names");
// two plugs, one of them refused by Home Assistant for longer than offline_after: no reading, its link and its reason
const refused = src("relay/dock", { name: "Dock plug", system_id: row, status: "unreachable", error: "home assistant refused the token (http 401)" });
state.power.sources.push(refused);
lp = linkedPower(row);
if (lp.watts !== 52.5 || lp.sources.length !== 2) fail("with a plug that fails: " + JSON.stringify(lp));
if (words(wallPower(lp)) !== "52.5 W wall · PVE-B plug, Dock plug unreachable Dock plug (via agent relay): home assistant refused the token (http 401)") fail("two plugs, one fails: " + words(wallPower(lp)));
// only the plug that fails
state.power.sources = [refused];
lp = linkedPower(row);
if (lp.watts !== null) fail("watts without a reading: " + lp.watts);
if (words(wallPower(lp)) !== "No wall-power reading from Dock plug. unreachable Dock plug (via agent relay): home assistant refused the token (http 401)") fail("only the plug that fails: " + words(wallPower(lp)));
if (!wallPower(lp).includes('<span class="chip bad">unreachable</span>')) fail("the status chip: " + wallPower(lp));
// stale: the last reading still counts, and the reason is said
state.power.sources = [src("relay/dock", { name: "Dock plug", system_id: row, status: "stale", error: "timeout", readings: [rd(30, row)] })];
if (words(wallPower(linkedPower(row))) !== "30.0 W wall · Dock plug stale Dock plug (via agent relay): timeout") fail("stale: " + words(wallPower(linkedPower(row))));
// a name and an error are text, never markup
state.power.sources = [src("relay/x", { name: "<b>x</b>", system_id: row, status: "unreachable", error: "<img src=x>" })];
if (/<b>x<\/b>|<img/.test(wallPower(linkedPower(row)))) fail("markup from a source's name or error: " + wallPower(linkedPower(row)));
`)
}
