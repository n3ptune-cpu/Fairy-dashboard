package web

import (
	"strings"
	"testing"
)

// The Power page: a plug that is left out of the total (power_exclude_from_total: a plug at the input of a
// UPS, whose machines are already counted through the UPS) has its own line with its watts and its link, is
// marked "not in total", and the line under the total counts the sources that are counted. A plug that fails
// keeps its link and shows why. The total itself is the panel's (GET /api/power, total_w), shown as it comes:
// the page adds nothing up.
func TestUIPowerPageMarksAPlugThatIsNotInTheTotal(t *testing.T) {
	runPowerPageScript(t, "power-page", `
state.systems = [{ id: "bob-node-mini-i7", name: "MINI-i7" }, { id: "rack-host", name: "Rack host" }];
state.power = { total_w: 360, sources: [
  src("relay/ups-load", { type: "nut", name: "UPS load", readings: [rd(300, null)] }),
  src("relay/corner", { name: "Server corner plug", include_in_total: false, system_id: "rack-host", readings: [rd(345, "rack-host")] }),
  src("relay/mini", { name: "MINI-i7 plug", system_id: "bob-node-mini-i7", readings: [rd(60, "bob-node-mini-i7")] }),
  src("relay/dock", { name: "Dock plug", system_id: "bob-node-mini-i7", status: "unreachable", error: "home assistant has no such entity (http 404)" }) ] };
const html = pagePower();
const rows = html.split('<tr class="row">').slice(1).map(words);
if (rows.length !== 4) fail("rows: " + rows.length);
const row = (name) => rows.find((r) => r.startsWith(name)) || "";
if (row("Server corner plug") !== "Server corner plug via agent relay homeassistant ok 345 W – – – Rack host not in total") fail("the excluded plug: " + row("Server corner plug"));
if (row("MINI-i7 plug") !== "MINI-i7 plug via agent relay homeassistant ok 60.0 W – – – MINI-i7") fail("a counted plug: " + row("MINI-i7 plug"));
if (row("UPS load") !== "UPS load via agent relay nut ok 300 W – – – –") fail("the UPS's load: " + row("UPS load"));
if (row("Dock plug") !== "Dock plug via agent relay homeassistant unreachable home assistant has no such entity (http 404) – – – – MINI-i7") fail("the plug that fails: " + row("Dock plug"));
if ((html.match(/not in total/g) || []).length !== 1) fail("marked rows: " + (html.match(/not in total/g) || []).length);
if (!html.includes('<a href="#/system/rack-host">Rack host</a> <span class="chip">not in total</span>')) fail("the mark next to the link");
const hero = words(html.split('<div class="section-title">')[0]);
if (hero !== "Total wall power 360 W 3 sources counted · 0 samples") fail("the total: " + hero);
state.power.total_w = null;
if (!words(pagePower()).startsWith("Total wall power – ")) fail("no total: " + words(pagePower()).slice(0, 40));
`)
	// the Overview's tile and the sidebar show the same number, the panel's total
	js := appJS(t)
	for _, want := range []string{`<div class="value pwr">${fmtW(state.power?.total_w)}</div>`, `<div class="big-w">${fmtW(p.total_w)}</div>`} {
		if strings.Count(js, want) != 1 {
			t.Errorf("app.js lacks (or has twice) %q", want)
		}
	}
}
