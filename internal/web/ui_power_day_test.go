package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// powerDayScript is the part of app.js that draws "Last 24 hours" for node: the page's own functions are run,
// nothing of them is copied. What is around them (the page's state, the browser) is not the code under test.
func powerDayScript(t *testing.T, js, body string) string {
	t.Helper()
	var parts []string
	for _, re := range []string{`(?m)^const table = .*$`, `(?m)^const own = .*$`, `(?m)^const esc = .*$`, `(?m)^const enc = .*$`, `(?m)^const fmtW = .*$`, `(?m)^const infoList = .*$`} {
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
process.env.TZ = "Europe/Berlin";  // the hours under the chart are local time: one zone for every machine that runs this
const state = { power: null, history: [], systems: [], ups: { ups: [] } };
const infoHTML = {};  // what each info box says, by its key
const spark = () => "", setTitle = () => {}, infoBtn = (key, about, html) => { infoHTML[key] = html; return "<i data-info=\"" + key + "\"></i>"; };
const document = { addEventListener() {} }, window = { addEventListener() {} };
` + strings.Join(parts, "\n") + "\n" +
		block("const linkedPower = (id) => {", "// ------------------------------------------------------------ grouping: hosts by site") +
		block("function pagePower() {", "// ------------------------------------------------------------ UPS page") +
		block("// ------------------------------------------------------------ power: the last 24 hours of one source", "// ------------------------------------------------------------ audit page") + `
const fail = (m) => { console.log("FAIL " + m); process.exitCode = 1; };
const words = (t) => t.replace(/<[^>]+>/g, " ").replace(/&#39;/g, "'").replace(/\s+/g, " ").trim();
const same = (a, b) => JSON.stringify(a) === JSON.stringify(b);
// a day as GET /api/power/day sends it: n minutes that end with the minute of now; w(i) null = no readings
const NOW = Date.UTC(2026, 10, 14, 15, 30, 20) / 1000;
const dayOf = (n, w, o = {}) => {
  const start = Math.floor(NOW / 60) * 60 - (n - 1) * 60, avg = [], min = [], max = [];
  for (let i = 0; i < n; i++) { const v = w(i); avg.push(v); min.push(v == null ? null : v * 0.9); max.push(v == null ? null : v * 1.5); }
  return { source: "pc-a/plug-a", step_s: 60, now: NOW, start, min, avg, max, ...o };
};
const pathOf = (svg, cls) => (new RegExp('<path class="' + cls + '" d="([^"]*)"').exec(svg) || [])[1] || "";
` + body + `
if (!process.exitCode) console.log("ok");
`
}

func runPowerDayScript(t *testing.T, name, body string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the page's own functions are not run here")
	}
	file := filepath.Join(t.TempDir(), name+".js")
	if err := os.WriteFile(file, []byte(powerDayScript(t, appJS(t), body)), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, file).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

// The chart of "Last 24 hours". A minute without readings is drawn as nothing: the line and the band are cut
// into one piece per run of minutes with readings, no piece reaches into a gap, and nothing is drawn at zero
// for a minute that has no value. The axis has round numbers, the hours under it are local time and do not
// run into each other, on a wide chart and on a phone's.
func TestUIPowerDayChartLeavesGapsAsGaps(t *testing.T) {
	runPowerDayScript(t, "day-chart", `
if (!same(dayRuns([1, null, 2, 3, null, null, 4]), [[0, 0], [2, 3], [6, 6]])) fail("dayRuns: " + JSON.stringify(dayRuns([1, null, 2, 3, null, null, 4])));
if (!same(dayRuns([]), []) || !same(dayRuns([null, null]), []) || !same(dayRuns([0, 0]), [[0, 1]])) fail("dayRuns: empty, all gaps, zeros (0 W is a reading)");

// the whole day, with five hours without readings (minutes 600..899) and one minute alone in the middle of them
const d = dayOf(1440, (i) => (i === 750 ? 80 : i >= 600 && i < 900 ? null : 100 + (i % 50)));
for (const W of [860, 330]) {
  const svg = dayChart(d, W, 260, "w"), g = dayGeom(d, W, 260, "w");
  if (/NaN|undefined|Infinity/.test(svg)) fail(W + ": the chart has a value that is not a number in it");
  const line = pathOf(svg, "pd-line"), band = pathOf(svg, "pd-band");
  const pieces = line.split("M").slice(1);
  if (pieces.length !== 3 || band.split("M").length - 1 !== 3) fail(W + ": the line has " + pieces.length + " pieces and the band " + (band.split("M").length - 1) + ", want 3 each (two stretches and the minute alone)");
  // where each piece starts and ends, as minutes
  const minute = (x) => Math.round(((x - g.L) / g.pw * 86400 + g.t0 - d.start) / 60 - 0.5);
  const ends = pieces.map((p) => { const xs = p.split("L").map((q) => parseFloat(q)); return [minute(xs[0]), minute(xs[xs.length - 1])]; });
  // the minute alone is a mark of 4 px around its place (a point of the line's width is not seen), the band a box of that width
  const alone = /^([\d.]+),([\d.]+)h4$/.exec(pieces[1]);
  if (!alone) fail(W + ": a minute alone is not drawn as a short mark: " + pieces[1]);
  else {
    if (Math.abs(+alone[1] + 2 - g.x(750)) > 0.11 || Math.abs(+alone[2] - g.y(80)) > 0.11) fail(W + ": the mark of the minute alone is not around its place: " + pieces[1] + ", the minute is at " + g.x(750).toFixed(1) + "," + g.y(80).toFixed(1));
    ends[1] = [minute(+alone[1] + 2), minute(+alone[1] + 2)];
    const box = new RegExp("M" + alone[1].replace(".", "\\.") + ",([\\d.]+)h4V([\\d.]+)h-4Z").exec(band);
    if (!box || Math.abs(+box[1] - g.y(120)) > 0.11 || Math.abs(+box[2] - g.y(72)) > 0.11) fail(W + ": the band of the minute alone is not a box from its lowest to its highest reading: " + band.split("M")[2]);
  }
  const near = (a, b) => Math.abs(a - b) <= Math.ceil(1440 / g.pw);  // a pixel is some minutes wide
  if (!near(ends[0][0], 0) || !near(ends[0][1], 599) || !near(ends[1][0], 750) || !near(ends[2][0], 900) || !near(ends[2][1], 1439)) fail(W + ": the pieces run over " + JSON.stringify(ends));
  // no point of the line lies in the gap, and none at the height of 0 W
  const y0 = g.y(0).toFixed(1);
  for (const p of pieces) for (const q of p.replace(/h4$/, "").split("L")) {
    const [x, y] = q.split(",").map(parseFloat), m = minute(x);
    if (y.toFixed(1) === y0) fail(W + ": a point at 0 W: " + q);
    if (m > 610 && m < 890 && Math.abs(m - 750) > 12) fail(W + ": a point in the gap, at minute " + m);
  }
  if ((svg.match(/class="pd-gap"/g) || []).length !== 2) fail(W + ": the stretches without readings that are marked: " + (svg.match(/class="pd-gap"/g) || []).length + ", want 2 (before and after the minute alone)");
  // the axis: round numbers from 0, the highest at or above the day's highest value (149 x 1.5 = 223.5)
  if (!same(g.ticks, [0, 50, 100, 150, 200, 250])) fail(W + ": the watts axis: " + JSON.stringify(g.ticks));
  // the hours: local time (Berlin, UTC+1 in November), every 3 hours on a wide chart and every 6 on a phone
  const hours = [...svg.matchAll(/<text class="(day)?" x="([\d.]+)" y="[\d.]+" text-anchor="(\w+)">([^<]*)</g)].map((m) => ({ day: !!m[1], x: +m[2], anchor: m[3], s: m[4] }));
  const want = W === 860 ? 8 : 4;
  if (hours.length !== want) fail(W + ": " + hours.length + " hours are named, want " + want + ": " + hours.map((h) => h.s).join(" | "));
  if (hours.filter((h) => h.day).length !== 1 || !/14/.test(hours.find((h) => h.day).s)) fail(W + ": midnight is not named by its date: " + hours.map((h) => h.s).join(" | "));
  if (!hours.some((h) => /^(12|00):00/.test(h.s))) fail(W + ": no noon among the hours: " + hours.map((h) => h.s).join(" | "));
  let end = -1e9;
  for (const h of hours) {
    const w = h.s.length * 7, left = h.anchor === "start" ? h.x : h.anchor === "end" ? h.x - w : h.x - w / 2;
    if (left < end) fail(W + ": the hours run into each other at " + h.s);
    if (left < 0 || left + w > W) fail(W + ": " + h.s + " does not fit into the chart");
    end = left + w;
  }
}
// the first minutes of a day that started an hour ago: one piece, at the right end, and the rest of the day marked
const young = dayOf(60, () => 40);
const ys = dayChart(young, 860, 260, "w"), yg = dayGeom(young, 860, 260, "w");
const first = parseFloat(pathOf(ys, "pd-line").slice(1));
if (first < yg.L + yg.pw * 0.95) fail("an hour of readings starts at x " + first + " of " + (yg.L + yg.pw));
if ((ys.match(/class="pd-gap"/g) || []).length !== 1) fail("the 23 hours before the first reading are not marked");
// a day of one value, and a day of zeros: still an axis, still a line
for (const flat of [dayOf(1440, () => 0), dayOf(1440, () => 7.5), dayOf(1, () => 5)]) {
  const s = dayChart(flat, 600, 260, "w"), g = dayGeom(flat, 600, 260, "w");
  if (/NaN|Infinity/.test(s) || g.ticks.length < 2 || g.ticks[0] !== 0 || !pathOf(s, "pd-line")) fail("a flat day: ticks " + JSON.stringify(g.ticks));
}
// a source that goes under 0 (it feeds in): the axis goes down with it
if (dayGeom(dayOf(100, (i) => i - 50), 600, 260, "w").ticks[0] > -50) fail("an axis that does not reach a negative value");
// ... and its numbers, which are longer ("-12.5k"), are not cut off at the left: the chart begins further right
// for them. Each number ends 8 px before the chart and has at most 8 px a character.
const around = (w, by) => { const x = dayOf(1440, w); return { ...x, min: x.avg.map((v) => v - by), max: x.avg.map((v) => v + by) }; };
const fed = around((i) => -12300 + (i % 7) * 10, 100);
for (const [name, day, wantL] of [["a plug", dayOf(1440, (i) => 100 + (i % 50)), 46], ["a heater", dayOf(1440, () => 2400), 46],
  ["a source that feeds in 12 kW", fed, 58], ["a source around zero", around((i) => (i % 9) / 100 - 0.04, 0.01), 50]]) {
  const g = dayGeom(day, 600, 260, "w"), svg = dayChart(day, 600, 260, "w");
  const nums = [...svg.matchAll(/<text x="([\d.]+)" y="[\d.]+" dy="0.32em" text-anchor="end">([^<]*)</g)].map((m) => ({ x: +m[1], s: m[2] }));
  if (nums.length !== g.ticks.length) fail(name + ": " + nums.length + " numbers at the axis for " + g.ticks.length + " lines");
  for (const n of nums) if (n.x !== g.L - 8 || n.x - 8 * n.s.length < 2) fail(name + ": the number " + n.s + " of the axis begins " + (n.x - 8 * n.s.length) + " px from the left edge (the chart begins at " + g.L + ")");
  if (wantL != null && g.L !== wantL) fail(name + ": the chart begins at " + g.L + " px, want " + wantL);
  if (g.L + g.pw + g.R !== 600) fail(name + ": the chart does not end 12 px before the right edge");
}
if (!dayChart(fed, 600, 260, "w").includes(">-12.5k<")) fail("the rig: no -12.5k at the axis of a source that feeds in 12 kW");
// a source that is under 0 all day still has 0 W at its axis (the watts are never drawn around the values)
if (!same(dayGeom(fed, 600, 260, "w").ticks, [-12500, -10000, -7500, -5000, -2500, 0])) fail("the axis of a source that feeds in all day: " + JSON.stringify(dayGeom(fed, 600, 260, "w").ticks));
// volts: an axis around the values, a line and no band, gaps as in the watts
const dv = { ...d, volts: d.avg.map((v, i) => (v == null ? null : 246.5 + (i % 30) / 20)) };
const vs = dayChart(dv, 860, 90, "v"), vg = dayGeom(dv, 860, 90, "v");
if (!same(vg.ticks, [244, 246, 248, 250])) fail("the volts axis: " + JSON.stringify(vg.ticks));  // 246.5 to 247.95 V, with room around it
if (pathOf(vs, "pd-band") || pathOf(vs, "pd-line").split("M").length - 1 !== 3) fail("the volts chart: band or pieces");
// the axis' steps
for (const [lo, hi, n, want] of [[0, 459, 5, [0, 100, 200, 300, 400, 500]], [0, 1004, 5, [0, 250, 500, 750, 1000, 1250]], [0, 0.4, 5, [0, 0.1, 0.2, 0.3, 0.4]],
  [228.1, 231.7, 3, [228, 230, 232]], [0, 12500, 5, [0, 2500, 5000, 7500, 10000, 12500]], [0, 1, 5, [0, 0.2, 0.4, 0.6, 0.8, 1]]]) {
  if (!same(dayTicks(lo, hi, n), want)) fail("dayTicks(" + lo + ", " + hi + "): " + JSON.stringify(dayTicks(lo, hi, n)));
}
`)
}

// The numbers around the chart: what the line above it says for a minute (also for one without readings, and
// with a current well under one ampere), the day's tiles with the energy and where it is from, and the part of
// the day that has readings when that is not all of it.
func TestUIPowerDayReadoutAndNumbers(t *testing.T) {
	runPowerDayScript(t, "day-numbers", `
const d = dayOf(1440, (i) => (i >= 600 && i < 900 ? null : 100));
d.volts = d.avg.map((v) => (v == null ? null : 246.5)); d.amps = d.avg.map((v) => (v == null ? null : 0.085));
const text = (r) => r.parts.map((p) => p[1]).join(" | ");
const at = (i) => d.start + i * 60;
let r = dayReadout(d, null);
if (r.i !== 1439 || !/^Latest · \d\d:\d\d( [AP]M)? \| 100 W \| 90.0 W – 150 W \| 246.5 V \| 0.085 A$/.test(text(r))) fail("the newest minute: " + text(r));
r = dayReadout(d, at(10) + 30);
if (r.i !== 10 || text(r).startsWith("Latest") || !text(r).endsWith("100 W | 90.0 W – 150 W | 246.5 V | 0.085 A")) fail("a minute with readings: " + text(r));
for (const t of [at(700), d.start - 7200, at(1440) + 600]) {
  r = dayReadout(d, t);
  if (r.i !== -1 || !/^\d\d:\d\d( [AP]M)? \| no readings$/.test(text(r))) fail("a minute without readings: " + text(r));
}
if (!same(dayReadout(d, at(700)).parts[1], ["pd-none", "no readings"])) fail("the gap's words: " + JSON.stringify(dayReadout(d, at(700)).parts));
// a minute whose samples were all the same has no "from – to"
const flat = { ...d, min: d.avg, max: d.avg };
if (text(dayReadout(flat, at(10))).includes("–")) fail("a range for a minute of one value: " + text(dayReadout(flat, at(10))));

const sum = (o) => ({ from: d.start, to: at(1439), covered_s: 86400, min_w: 90, avg_w: 100, max_w: 150, min_at: at(3), max_at: at(1300),
  energy_kwh: 2.4, energy_est_kwh: 0, energy: "counter", volts_min: null, volts_max: null, amps_min: null, amps_max: null, ...o });
let html = dayStats({ ...d, summary: sum() });
if (!/^Average 100 W Lowest 90.0 W \d\d:\d\d( [AP]M)? Highest 150 W \d\d:\d\d( [AP]M)? Energy 2.400 kWh$/.test(words(html))) fail("a day from the counter: " + words(html));
if (/estimated|≈|Readings for|Volts|Amps/.test(words(html))) fail("a full day from the counter says more than its numbers: " + words(html));
html = dayStats({ ...d, summary: sum({ energy: "estimated", energy_est_kwh: 2.4 }) });
if (!words(html).endsWith("Energy ≈ 2.400 kWh estimated")) fail("an estimated day: " + words(html));
html = dayStats({ ...d, summary: sum({ energy: "mixed", energy_est_kwh: 0.9 }) });
if (!words(html).endsWith("Energy ≈ 2.400 kWh partly estimated")) fail("a partly estimated day: " + words(html));
// what the info box of Energy says about it: how much is estimated
let about = words(infoHTML["pday-energy"]);
if (!about.includes("0.900 kWh of it are estimated from the watts")) fail("the info box of a partly estimated day: " + about);
// a day of the counter from which a time without readings is left out (mixed, nothing estimated): the box
// says that, and not "0.000 kWh of it are estimated"
html = dayStats({ ...d, summary: sum({ energy: "mixed", energy_est_kwh: 0 }) });
about = words(infoHTML["pday-energy"]);
if (!words(html).endsWith("Energy ≈ 2.400 kWh partly estimated")) fail("a day with a time left out: " + words(html));
if (/kWh of it are estimated/.test(about) || !about.includes("From the device's own kWh counter, but not for all of the time: over a time without readings the counter went up by more than the device could have used, and that time is left out")) fail("the info box of a day with a time left out: " + about);
// and every kind of day says the same about time without readings
for (const energy of ["counter", "mixed", "estimated"]) {
  dayStats({ ...d, summary: sum({ energy, energy_est_kwh: energy === "counter" ? 0 : 0.9 }) });
  about = words(infoHTML["pday-energy"]);
  if (!about.includes("Time without readings is counted where the device's counter went on through it by an amount the device could have used. Otherwise that time is left out, and the number is marked as partly estimated: the device used more.") || about.includes("Time without readings is not counted")) fail(energy + ": the info box about time without readings: " + about);
}
// a counter that started again at 0 after an outage: a small number keeps its digits
html = dayStats({ ...d, summary: sum({ energy_kwh: 0.004 }) });
if (!words(html).endsWith("Energy 0.004 kWh")) fail("a small energy: " + words(html));
// volts, a small current, and 19 of 24 hours
html = dayStats({ ...d, summary: sum({ covered_s: 19 * 3600 + 120, volts_min: 243.1, volts_max: 247.9, amps_min: 0.085, amps_max: 0.412 }) });
if (!words(html).endsWith("Volts 243.1 – 247.9 V Amps 0.085 – 0.412 A Readings for 19 h 2 min of the 24 hours")) fail("volts, amps and a part of the day: " + words(html));
html = dayStats({ ...d, summary: sum({ covered_s: 86400 - 600, amps_min: 2.5, amps_max: 2.5 }) });
if (!words(html).endsWith("Amps 2.50 A") || /Readings for/.test(html)) fail("one current all day, and ten minutes missing: " + words(html));
if (daySpan(45 * 60) !== "45 min" || daySpan(3 * 3600) !== "3 h") fail("daySpan");
`)
}

// Volts, amperes and the kWh counter next to the live watts: on the Power page (their columns) and in a
// host's Power box (one line per linked source). A current of 85 mA is not "0.09" and a counter that started
// again at 0.004 kWh not "0.00". Each source has the button for its last 24 hours, with its id whole in it;
// a name is text, never markup.
func TestUIPowerValuesAndTheDayButton(t *testing.T) {
	runPowerDayScript(t, "day-button", `
for (const [v, want] of [[0.085, "0.085"], [0.9994, "0.999"], [1, "1.00"], [2.5, "2.50"], [12.345, "12.35"], [0, "0.000"], [null, "–"], [undefined, "–"]]) if (fmtAmps(v) !== want) fail("fmtAmps(" + v + ") = " + fmtAmps(v));
for (const [v, want] of [[0.004, "0.004"], [0, "0.000"], [9.9994, "9.999"], [12.345, "12.35"], [1234.5, "1234.5"], [null, "–"], [undefined, "–"]]) if (fmtKWh(v) !== want) fail("fmtKWh(" + v + ") = " + fmtKWh(v));
const src = (id, o) => ({ id, type: "homeassistant", name: id, status: "ok", include_in_total: true, readings: [], last_success: null, error: null, via: "pc-b", ...o });
const rd = (o) => ({ source_id: "", name: "", watts: 40, voltage: null, current: null, energy_kwh_total: null, system_id: "pc-a", ...o });
state.systems = [{ id: "pc-a", name: "PC-A" }];
state.power = { total_w: 80, sources: [
  src("pc-b/plug-a", { name: "Plug A", system_id: "pc-a", readings: [rd({ voltage: 246.5, current: 0.085, energy_kwh_total: 0.004 })] }),
  src("pc-b/plug-b", { name: "Plug <b>B</b>", system_id: "pc-a", readings: [rd({})] }),
  src('pc-b/x"y', { name: "Odd", system_id: "pc-a", status: "unreachable", error: "timeout" }) ] };
const rows = pagePower().split('<tr class="row">').slice(1);
if (rows.length !== 3) fail("rows: " + rows.length);
if (words(rows[0]) !== "Plug A via agent pc-b homeassistant ok 40.0 W 246.5 0.085 0.004 PC-A") fail("a plug with volts, amperes and a counter: " + words(rows[0]));
if (words(rows[1]) !== "Plug &lt;b&gt;B&lt;/b&gt; via agent pc-b homeassistant ok 40.0 W – – – PC-A") fail("a plug with watts only: " + words(rows[1]));
for (const [i, id] of [[0, "pc-b/plug-a"], [1, "pc-b/plug-b"], [2, "pc-b/x&quot;y"]]) {
  const btn = (/<button class="day-btn"[^>]*>/.exec(rows[i]) || [""])[0];
  if (!btn.includes('data-act="power-day"') || !btn.includes('data-source="' + id + '"') || !btn.includes('title="Last 24 hours"') || !btn.includes('type="button"')) fail("the button of row " + i + ": " + btn);
}
if (!rows[1].includes('aria-label="Last 24 hours of Plug &lt;b&gt;B&lt;/b&gt;"') || /<b>B<\/b>/.test(rows[1])) fail("a name with markup in the button: " + rows[1]);
// the host's Power box
const more = wallMore(linkedPower("pc-a"));
const lines = more.split('<div class="list-row wall-src">').slice(1);
if (lines.length !== 3) fail("lines in the host's box: " + lines.length);
if (words(lines[0]) !== "Plug A 246.5 V · 0.085 A · 0.004 kWh") fail("the line of a plug with everything: " + words(lines[0]));
if (words(lines[1]) !== "Plug &lt;b&gt;B&lt;/b&gt;" || words(lines[2]) !== "Odd") fail("lines without values: " + words(lines[1]) + " / " + words(lines[2]));
if ((more.match(/data-act="power-day"/g) || []).length !== 3 || !lines[2].includes('data-source="pc-b/x&quot;y"')) fail("the buttons in the host's box");
if (wallMore(linkedPower("pc-z")) !== "") fail("a host without a source has lines");
`)
}
