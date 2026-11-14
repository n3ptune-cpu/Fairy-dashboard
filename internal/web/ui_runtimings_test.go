package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// "Run timings" on the page: a button in the "Update unpowered hosts" box of the Updates page (admins), and a
// dialog with one row per timing. The dialog's own functions are run in node (nothing of them is copied
// here); what is around them in the script is not the code under test.

func init() {
	// where app.js puts the action into words for "Confirm it's you" (ui_confirm_test.go)
	confirmLabels["PUT /api/admin/run-timings"] = []string{`controlCall({ kind: "request", label: "Run timings", method: "PUT", path, body: plan.body },`}
}

// rtPageScript is the "Run timings" part of app.js with what it needs around it (ehPageScript's stand-ins).
func rtPageScript(t *testing.T, js, body string) string {
	t.Helper()
	var parts []string
	for _, re := range []string{`(?m)^const dict = .*$`, `(?m)^const own = .*$`, `(?m)^const esc = .*$`, `(?m)^const infoList = .*$`} {
		m := regexp.MustCompile(re).FindString(js)
		if m == "" {
			t.Fatalf("app.js has no %s any more", re)
		}
		parts = append(parts, m)
	}
	from := strings.Index(js, "// ------------------------------------------------------------ Run timings (admins, the \"Update unpowered hosts\" box)")
	to := strings.Index(js, "// ------------------------------------------------------------ users, roles and sites (admins only)")
	if from < 0 || to < from {
		t.Fatal("app.js has no \"Run timings\" part any more")
	}
	return `"use strict";
const listeners = [];
const document = { addEventListener(type, fn) { listeners.push(type); }, querySelector: () => null };
const flashes = [];
const flash = (msg, bad = false) => { flashes.push({ msg, bad }); };
let els = {}, detached = false;
const $ = (sel) => (els[sel] ||= { innerHTML: "", textContent: "", value: "", hidden: true, disabled: false, isConnected: !detached, focus() { this.focused = true; } });
let closed = 0;
const closeModal = () => { closed++; detached = true; for (const k of Object.keys(els)) els[k].isConnected = false; };
const infos = {};
const infoBtn = (key, about, html) => { infos[key] = { about, html }; return "[info " + key + "]"; };
const asked = [];
let answer = async () => ({});
const api = (path, opts = {}) => { asked.push({ path, method: opts.method || "GET", body: opts.body ? JSON.parse(opts.body) : null, signal: opts.signal }); return answer(path, opts); };
let confirming = null;
const controlCall = async (pending, fn) => { try { return await fn(); } catch (e) { if (e.stepUp) { confirming = pending; return null; } throw e; } };
const timers = [];
const setTimeout = (fn, ms) => { timers.push({ fn, ms, on: true }); return timers.length; };
const clearTimeout = (n) => { if (timers[n - 1]) timers[n - 1].on = false; };
let fetched = 0;
const fetchWakeUpdate = () => { fetched++; };
` + strings.Join(parts, "\n") + "\n" + js[from:to] + `
const fail = (m) => { console.log("FAIL " + m); process.exitCode = 1; };
const words = (t) => t.replace(/<[^>]+>/g, " ").replace(/&#39;/g, "'").replace(/&quot;/g, '"').replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&amp;/g, "&").replace(/\s+/g, " ").trim();
const settle = () => new Promise((r) => setImmediate(r));
const timing = (key, value, o) => ({ key, value, from: "config", config: value, default: value, min: 1, max: 99999, confirm: "", by: null, at: null, ...o });
const data = (o) => ({ rev: 0, active: false, confirm_wait_s: 600, ignored: [], ignored_count: 0, timings: [
  timing("off_after_s", 900, { min: 300, max: 3600, confirm: "always" }), timing("online_timeout_s", 300, { min: 60, max: 1800, confirm: "higher" }),
  timing("settle_s", 60, { min: 10, max: 600 }), timing("shutdown_delay_s", 120, { min: 10, max: 3600, confirm: "lower" }),
  timing("agent_requests_per_min", 8, { min: 1, max: 30 }), timing("run_approval_wait_s", 600, { min: 60, max: 3600 })], ...o });
const set = (d, key, o) => ({ ...d, timings: d.timings.map((t) => (t.key === key ? { ...t, ...o } : t)) });
(async () => {
` + body + `
  if (!process.exitCode) console.log("ok");
})();
`
}

func runRtPageScript(t *testing.T, name, body string, args ...string) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the page's own functions are not run here")
	}
	file := filepath.Join(t.TempDir(), name+".js")
	if err := os.WriteFile(file, []byte(rtPageScript(t, appJS(t), body)), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, append([]string{file}, args...)...).CombinedOutput()
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if err != nil || lines[len(lines)-1] != "ok" || strings.Contains(string(out), "FAIL ") {
		t.Fatalf("node: %v\n%s", err, out)
	}
	return strings.Join(lines[:len(lines)-1], "\n")
}

// Where it is on the page and how it is written: a button next to the start button of "Update unpowered
// hosts"; a dialog whose values all come through esc(); the styles in the stylesheet's own colours.
func TestUIRunTimingsIsAButtonOfTheUpdateUnpoweredHostsBox(t *testing.T) {
	js, css := appJS(t), appCSS(t)
	for _, want := range []string{
		`<span class="row-gap wu-btns"><button class="btn" type="button" data-act="rt-open" title="How long a run waits, counts down and when it takes a machine for off">Run timings</button>`,
		`const t = e.target.closest('[data-act="rt-open"]');`,
		`const path = "/api/admin/run-timings";`,
		`controlCall({ kind: "request", label: "Run timings", method: "PUT", path, body: plan.body },`, // "Confirm it's you" when it is asked for
		`const stop = new AbortController(), timer = setTimeout(() => stop.abort(), 15000);`,           // no "Loading…" for ever
		`<h3><span id="rt-t">Run timings</span>${RT_INFO()}</h3>`,
		`"The times above can be changed with Run timings, next to the button.",`, // the box's own info list says where
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	from := strings.Index(js, "// ------------------------------------------------------------ Run timings (admins")
	to := strings.Index(js, "// ------------------------------------------------------------ users, roles and sites (admins only)")
	part := js[from:to]
	// every ${...} that is put into markup is esc(...), one of this part's own functions that escape, or a fixed word
	for _, m := range regexp.MustCompile(`\$\{[^{}]*\}`).FindAllString(part, -1) {
		ok := false
		for _, fine := range []string{"${esc(", "${rtTag(", "${rtFoot(", "${RT_INFO()", "${v}", "${words}", "${n}", "${p[0]}", "${rtSpan(v)}", "${t.min} to ${t.max}", "${label}", "${keep ?", "${rtReading(",
			"${panel ?", "${stored", "${any ?", "${d.active ?", "${ignored", "${plan.", "${d.timings.map", "${t.confirm ===", "${row.key}", "${r.message", "${unit}", "${v === 1 ?", "${t.min}", "${t.max}", "${p[1]}",
			"${t.by}" /* inside esc(`Changed here by ...`) */} {
			ok = ok || strings.HasPrefix(m, fine)
		}
		if !ok {
			t.Errorf("app.js, \"Run timings\": %s is put into the page without esc()", m)
		}
	}
	a := strings.Index(css, `/* "Run timings": one row per timing`)
	b := strings.Index(css[a:], ".wu-btns { flex-wrap: wrap; }")
	if a < 0 || b < 0 {
		t.Fatal("app.css has no \"Run timings\" block any more")
	}
	block := css[a : a+b]
	if m := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|rgba?\(`).FindString(block); m != "" {
		t.Errorf("the \"Run timings\" styles name a colour of their own (%s): the stylesheet's variables only", m)
	}
	for _, want := range []string{".rt-row {", ".rt-cap {", ".rt-in .input { flex: none; width: 92px;", `.rt-in .input[aria-invalid="true"] { border-color: var(--bad); }`, ".rt-read.bad { color: var(--bad); }"} {
		if !strings.Contains(block, want) {
			t.Errorf("app.css lacks %q", want)
		}
	}
}

// The words: a value's reading ("360 s = 6 minutes"), a span inside a sentence and the consequence sentences
// are made twice, by the panel (for "Confirm it's you" and the audit log) and by the page (for the dialog).
// The two say the same for every value that can be typed, and each timing has the same name in both.
func TestUIRunTimingsWordsAreThePanelsWords(t *testing.T) {
	type c struct {
		Key     string `json:"key"`
		V       int    `json:"v"`
		Reading string `json:"reading"`
		Span    string `json:"span"`
		Means   string `json:"means"`
	}
	var cases []c
	for _, tm := range config.RunTimings() {
		for _, v := range []int{tm.Min, tm.Max, tm.Default, 1, 2, 10, 45, 59, 60, 61, 90, 119, 120, 121, 300, 360, 599, 600, 900, 1740, 1800, 3599, 3600, 3601, 3660, 3661, 5400, 7200, 7322, 86400} {
			cases = append(cases, c{tm.Key, v, rtReading(tm.Key, v), rtSpan(v), rtConsequence(tm.Key, v)})
		}
	}
	var labels [][2]string // in the panel's order
	for _, tm := range config.RunTimings() {
		labels = append(labels, [2]string{tm.Key, rtLabel[tm.Key]})
		if rtLabel[tm.Key] == "" {
			t.Errorf("%s has no name", tm.Key)
		}
	}
	arg, _ := json.Marshal(map[string]any{"cases": cases, "labels": labels})
	runRtPageScript(t, "words", `
  const { cases, labels } = JSON.parse(process.argv[2]);
  for (const c of cases) {
    if (rtReading(c.key, c.v) !== c.reading) fail(c.key + " " + c.v + ": the page reads " + JSON.stringify(rtReading(c.key, c.v)) + ", the panel " + JSON.stringify(c.reading));
    if (rtSpan(c.v) !== c.span) fail(c.v + ": the page's span " + JSON.stringify(rtSpan(c.v)) + ", the panel's " + JSON.stringify(c.span));
    if (rtConsequence(c.key, c.v) !== c.means) fail(c.key + " " + c.v + ": the page says " + JSON.stringify(rtConsequence(c.key, c.v)) + ", the panel " + JSON.stringify(c.means));
  }
  if (JSON.stringify(Object.keys(RT_ROWS)) !== JSON.stringify(labels.map((l) => l[0]))) fail("the page's rows " + Object.keys(RT_ROWS) + ", the panel's timings " + labels.map((l) => l[0]));
  for (const [k, name] of labels) if (RT_ROWS[k].label !== name) fail(k + ": the page calls it " + RT_ROWS[k].label + ", the panel " + name);
`, string(arg))
	// the readings themselves, once by hand
	for v, want := range map[int]string{45: "45 s", 60: "60 s = 1 minute", 90: "90 s = 1 min 30 s", 360: "360 s = 6 minutes", 3600: "3600 s = 1 hour", 5400: "5400 s = 1 h 30 min", 3661: "3661 s = 1 h 1 min 1 s", 7200: "7200 s = 2 hours", 3605: "3605 s = 1 h 5 s"} {
		if got := rtReading(config.KeyOffAfter, v); got != want {
			t.Errorf("rtReading(%d) = %q, want %q", v, got, want)
		}
	}
	if rtReading(config.KeyRequestsPerMin, 8) != "8" || rtSpan(1) != "1 second" || rtSpan(45) != "45 seconds" || rtSpan(360) != "6 minutes" || rtSpan(90) != "1 min 30 s" {
		t.Errorf("readings: %q %q %q %q %q", rtReading(config.KeyRequestsPerMin, 8), rtSpan(1), rtSpan(45), rtSpan(360), rtSpan(90))
	}
}

// The page's plan and the panel's are the same thing. For a row of stored states and what is typed or reset
// in the dialog: the page (in node, on the panel's real answer of GET) makes a request and the lines of "This
// will change"; that very request is sent to the panel, which answers with the lines it made itself and with
// whether it asks for a confirmation of its own. The lines are equal, and the page said "asks you to confirm,
// every time" exactly when the panel does.
func TestUIRunTimingsPlanIsThePanelsPlan(t *testing.T) {
	type step struct {
		stored map[string]int    // what is stored before (the panel's decisions)
		typed  map[string]string // what is typed into the fields
		reset  []string          // the Reset buttons that are pressed
	}
	steps := []step{
		{nil, map[string]string{"off_after_s": "360"}, nil},
		{nil, map[string]string{"off_after_s": "1800"}, nil},
		{nil, map[string]string{"shutdown_delay_s": "30"}, nil},
		{nil, map[string]string{"shutdown_delay_s": "300"}, nil},
		{nil, map[string]string{"online_timeout_s": "120"}, nil},
		{nil, map[string]string{"online_timeout_s": "1800"}, nil},
		{nil, map[string]string{"settle_s": "10", "agent_requests_per_min": "30", "run_approval_wait_s": "3600"}, nil},
		{nil, map[string]string{"off_after_s": "360", "online_timeout_s": "600", "settle_s": "90", "shutdown_delay_s": "30", "agent_requests_per_min": "6", "run_approval_wait_s": "900"}, nil},
		{map[string]int{"off_after_s": 360, "shutdown_delay_s": 30}, map[string]string{"off_after_s": "900"}, nil}, // config.yaml's value: a reset
		{map[string]int{"off_after_s": 360, "shutdown_delay_s": 30}, nil, []string{"off_after_s", "shutdown_delay_s"}},
		{map[string]int{"off_after_s": 360, "shutdown_delay_s": 300}, nil, []string{"shutdown_delay_s"}}, // a reset that shortens the countdown
		{map[string]int{"online_timeout_s": 120, "settle_s": 30}, map[string]string{"settle_s": "45"}, []string{"online_timeout_s"}},
		{map[string]int{"shutdown_delay_s": 45}, map[string]string{"shutdown_delay_s": "44", "off_after_s": " 900 "}, nil},
		{map[string]int{"run_approval_wait_s": 900}, map[string]string{"run_approval_wait_s": "0900"}, nil}, // the same number, written otherwise
		{map[string]int{"agent_requests_per_min": 12}, map[string]string{"agent_requests_per_min": "8"}, nil},
	}
	for i, st := range steps {
		a := signedIn(t)
		e := a.e
		if cb := a.stepUp("", false); cb.code != 303 {
			t.Fatal("the step-up:", cb.code)
		}
		if len(st.stored) > 0 {
			e.rtSet(st.stored)
		}
		get := e.do("GET", rtPath, nil, "Cookie", a.sess)
		arg, _ := json.Marshal(map[string]any{"d": json.RawMessage(get.raw), "typed": st.typed, "reset": st.reset})
		out := runRtPageScript(t, "plan", `
  const { d, typed, reset } = JSON.parse(process.argv[2]);
  if (!rtValid(d)) fail("the page does not take the panel's answer: " + JSON.stringify(d));
  const draft = rtDraft();
  for (const k of reset || []) draft.reset[k] = true;
  for (const [k, v] of Object.entries(typed || {})) draft.text[k] = v;
  const plan = rtPlan(d, draft);
  if (Object.keys(plan.problems).length) fail("problems: " + JSON.stringify(plan.problems));
  console.log(JSON.stringify({ body: plan.body, lines: plan.lines, own: plan.own }));
`, string(arg))
		var plan struct {
			Body  map[string]any `json:"body"`
			Lines []string       `json:"lines"`
			Own   bool           `json:"own"`
		}
		if err := json.Unmarshal([]byte(out), &plan); err != nil {
			t.Fatalf("step %d: %v\n%s", i, err, out)
		}
		if len(plan.Lines) == 0 { // nothing to save: the page sends nothing, and the panel agrees
			if how, r := a.rtAsk(map[string]any{"reset": []string{}, "settle_s": get.body["timings"].([]any)[2].(map[string]any)["value"]}); how != "nothing" {
				t.Errorf("step %d: the page has nothing to change, the panel: %q %s", i, how, r.raw)
			}
			continue
		}
		how, r := a.rtAsk(plan.Body)
		var lines []string
		switch how {
		case "saved":
			lines = strs(r.body["changed"])
		case "each":
			lines = strs(r.body["detail"].(map[string]any)["items"])
		default:
			t.Fatalf("step %d: the page's request %v was answered %q: %s", i, plan.Body, how, r.raw)
		}
		if !slices.Equal(lines, plan.Lines) {
			t.Errorf("step %d (%v, typed %v, reset %v):\nthe page's lines:  %q\nthe panel's lines: %q", i, st.stored, st.typed, st.reset, plan.Lines, lines)
		}
		if plan.Own != (how == "each") {
			t.Errorf("step %d (%v, typed %v, reset %v): the page says own confirmation %v, the panel answered %q", i, st.stored, st.typed, st.reset, plan.Own, how)
		}
	}
}

// The dialog: what it shows for a timing, what a field takes, what Reset does, when Save is on, and that what
// the panel sends is never trusted as markup or as a number.
func TestUIRunTimingsDialog(t *testing.T) {
	runRtPageScript(t, "dialog", `
  // the panel's answer is checked before it is drawn
  for (const bad of [null, {}, { timings: [] }, { timings: "x" }, { timings: [{}] }, set(data(), "off_after_s", { value: "900" }), set(data(), "off_after_s", { from: "elsewhere" }),
    { ...data(), timings: [...data().timings, timing("url", 1)] }, { ...data(), timings: [...data().timings, timing("constructor", 1)] }, set(data(), "settle_s", { min: 1.5 })])
    if (rtValid(bad)) fail("rtValid takes " + JSON.stringify(bad));
  if (!rtValid(data())) fail("rtValid refuses the panel's answer");

  // as opened: six rows, config.yaml everywhere, no Reset, nothing to change
  let d = data(), draft = rtDraft();
  let html = rtView(d, draft), text = words(html);
  if ((html.match(/class="rt-row"/g) || []).length !== 6) fail("rows: " + html);
  if ((text.match(/from config\.yaml/g) || []).length !== 6 || text.includes("changed here") || text.includes("Reset") || text.includes("This will change")) fail("as opened: " + text);
  if (!text.includes("Counts as off after from config.yaml s = 15 minutes A machine that is silent for longer than 15 minutes counts as off. If it was restarting and comes up during the run, the run switches it off after its update.")) fail("the first row: " + text);
  if (!text.includes("Shutdown countdown from config.yaml s = 2 minutes") || !text.includes("Requests a minute from config.yaml to one agent")) fail("the readings: " + text);
  if (!html.includes('id="rt-off_after_s" data-rt="off_after_s" type="text" inputmode="numeric" maxlength="5"') || !html.includes('value="900"')) fail("the field: " + html);
  if (!/data-rt-hint="off_after_s"[^>]*>/.test(html) || !html.includes('class="note rt-hint warn" id="rt-h-off_after_s"')) fail("the sentence of off_after_s is not the warning: " + html);
  let plan = rtPlan(d, draft);
  if (plan.lines.length || Object.keys(plan.body).length || plan.own) fail("a plan for nothing: " + JSON.stringify(plan));

  // what a field takes: whole numbers in the timing's range
  const t0 = d.timings[0];
  for (const [typed, want] of [["360", 360], [" 360 ", 360], ["0360", 360], ["300", 300], ["3600", 3600], ["299", null], ["3601", null], ["", null], ["6m", null], ["360.5", null], ["3e2", null], ["-360", null], ["+360", null], ["0x168", null], ["١٢٣", null], ["360 s", null], ["999999", null]])
    if (rtParse(t0, typed) !== want) fail("rtParse(" + JSON.stringify(typed) + ") = " + rtParse(t0, typed) + ", want " + want);

  // typing: the reading, the tag, the sentence and the lines follow; a value that cannot be sent says its range and stops Save
  draft.text.off_after_s = "360";
  text = words(rtView(d, draft));
  if (!text.includes("Counts as off after changed here s = 6 minutes A machine that is silent for longer than 6 minutes counts as off. If it was restarting and comes up during the run, the run switches it off after its update.")) fail("after typing 360: " + text);
  plan = rtPlan(d, draft);
  if (JSON.stringify(plan.body) !== '{"off_after_s":360}' || !plan.own || plan.lines.length !== 2 || plan.lines[0] !== "Counts as off after: 360 s = 6 minutes (was 900 s = 15 minutes)") fail("the plan: " + JSON.stringify(plan));
  if (!words(rtFoot(d, draft)).includes("Asks you to confirm it's you, by itself, every time.") || !rtFoot(d, draft).includes('<li class="eh-sees">A machine that is silent for longer than 6 minutes counts as off. If it was restarting')) fail("the foot: " + rtFoot(d, draft));
  if (!words(rtFoot(d, rtDraft())).includes("A machine with its own time or countdown (Edit host, or config.yaml) keeps it.")) fail("the foot does not say that a machine's own values stay: " + rtFoot(d, rtDraft()));
  draft.text.off_after_s = "120";
  plan = rtPlan(d, draft);
  if (plan.problems.off_after_s !== "300 to 3600" || plan.lines.length || rtRead(t0, draft) !== "300 to 3600") fail("120 s: " + JSON.stringify(plan) + " " + rtRead(t0, draft));
  if (rtHint(t0, draft) !== "A machine that is silent for longer than 15 minutes counts as off. If it was restarting and comes up during the run, the run switches it off after its update.") fail("the sentence while the field holds no value: " + rtHint(t0, draft));
  draft.text.off_after_s = "900";  // back to what it was: nothing to change (the page deletes such a text; a text that says the same number is no change either)
  if (rtPlan(d, draft).lines.length) fail("the value that is in force already is a change");

  // the pacing above what an agent accepts: the sentence says so
  draft = rtDraft(); draft.text.agent_requests_per_min = "20";
  if (!rtHint(d.timings[4], draft).startsWith("An agent accepts 12 a minute: above that it refuses the rest")) fail("pacing 20: " + rtHint(d.timings[4], draft));
  if (rtPlan(d, draft).own || rtPlan(d, draft).lines[0] !== "Requests a minute: 20 (was 8)") fail("pacing 20: " + JSON.stringify(rtPlan(d, draft)));

  // a timing that was changed here: "changed here", Reset, who changed it in the hover; Reset puts config.yaml's value into the field
  d = set(set(data(), "off_after_s", { value: 360, from: "panel", by: "nora@example.com", at: "2026-11-14T10:00:00Z" }), "shutdown_delay_s", { value: 300, from: "panel" });
  draft = rtDraft();
  html = rtView(d, draft);
  if ((html.match(/data-rt-reset=/g) || []).length !== 2 || !html.includes('data-rt-reset="off_after_s" title="Back to config.yaml: 900 s = 15 minutes"') || !html.includes('title="Changed here by nora@example.com. config.yaml: 900 s = 15 minutes"')) fail("a changed timing: " + html);
  if (!words(html).includes("Changed here: used instead of config.yaml until Reset.")) fail("the note about what was changed here: " + words(html));
  draft.reset.off_after_s = true;
  if (rtText(d.timings[0], draft) !== "900" || rtTag(d.timings[0], draft).includes("Reset") || !rtTag(d.timings[0], draft).includes("from config.yaml")) fail("after Reset: " + rtText(d.timings[0], draft) + " " + rtTag(d.timings[0], draft));
  plan = rtPlan(d, draft);
  if (JSON.stringify(plan.body) !== '{"reset":["off_after_s"]}' || !plan.own || plan.lines[0] !== "Counts as off after: back to config.yaml: 900 s = 15 minutes (was 360 s = 6 minutes)") fail("the plan of a Reset: " + JSON.stringify(plan));
  // a Reset of the countdown that was made longer here: shorter again, so it asks; one that makes it longer does not
  draft = rtDraft(); draft.reset.shutdown_delay_s = true;
  if (!rtPlan(d, draft).own || rtPlan(d, draft).lines.length !== 2) fail("a Reset that shortens the countdown: " + JSON.stringify(rtPlan(d, draft)));
  const d2 = set(data(), "shutdown_delay_s", { value: 30, from: "panel" });
  if (rtPlan(d2, draft).own || rtPlan(d2, draft).lines.length !== 1) fail("a Reset that makes the countdown longer: " + JSON.stringify(rtPlan(d2, draft)));
  // a Reset of a timing nobody changed here is nothing
  draft = rtDraft(); draft.reset.settle_s = true;
  if (rtPlan(d, draft).lines.length) fail("a Reset of a timing from config.yaml");
  // config.yaml's own value, typed: the same as Reset
  draft = rtDraft(); draft.text.off_after_s = "900";
  if (JSON.stringify(rtPlan(d, draft).body) !== '{"off_after_s":900}' || !rtPlan(d, draft).lines[0].startsWith("Counts as off after: back to config.yaml: ") || rtTag(d.timings[0], draft).includes("changed here")) fail("config.yaml's value typed: " + JSON.stringify(rtPlan(d, draft)));

  // a run is going, stored values are ignored: said under the fields
  text = words(rtFoot({ ...data(), active: true, ignored: ["x", "y"], ignored_count: 7 }, rtDraft()));
  if (!text.includes("A run is going: it keeps its numbers. A change counts from the next run.") || !text.includes("7 stored values are ignored: see the panel's log.")) fail("the foot: " + text);
  if (words(rtFoot({ ...data(), ignored: ["x"], ignored_count: 1 }, rtDraft())) !== "A machine with its own time or countdown (Edit host, or config.yaml) keeps it. 1 stored value is ignored: see the panel's log.") fail("one ignored value");

  // nothing the panel sends is markup
  const evil = '<img src=x onerror=alert(1)>"\'';
  d = set(data(), "off_after_s", { value: 360, from: "panel", by: evil });
  html = rtView(d, rtDraft());
  if (html.includes("<img") || html.includes('onerror=alert(1)>"')) fail("the panel's text is put into the page as markup: " + html);

  // the info box: what is kept where, when a change counts, which changes ask
  RT_INFO();
  const info = words(infos["run-timings"].html);
  for (const want of ["config.yaml is not changed and stays the base.", "A change counts from the next run. A run that is going keeps the numbers it started with.",
    "Every change of that time asks you to confirm it's you, by itself. So do a shorter shutdown countdown and a longer wait for a woken machine.", "15 minutes is the safe value."])
    if (!info.includes(want)) fail("the info box lacks: " + want);
`)
}

// The dialog from the button to Save: it loads the panel's answer, sends exactly the plan's request, hands
// over to "Confirm it's you" when the panel asks, says what the panel said, and reads the box again.
func TestUIRunTimingsDialogFlow(t *testing.T) {
	runRtPageScript(t, "flow", `
  // the panel's answer is not what the page expects: said, nothing drawn
  answer = async () => ({ timings: "no" });
  await openRunTimings();
  if (!$("#rt-err").textContent.includes("not what this page expects") || $("#rt-body").innerHTML !== "") fail("a wrong answer: " + $("#rt-err").textContent);
  // the panel does not answer
  els = {}; detached = false;
  answer = async (path, opts) => { const e = new Error("aborted"); e.name = "AbortError"; throw e; };
  await openRunTimings();
  if ($("#rt-err").textContent !== "The panel did not answer. Close this and try again.") fail("no answer: " + $("#rt-err").textContent);
  if (timers.filter((x) => x.ms === 15000).length !== 2) fail("no time limit on the dialog's question");

  // the dialog, a change, Save
  els = {}; detached = false; asked.length = 0;
  answer = async (path, opts) => (opts.method === "PUT" ? { ok: true, message: "saved", changed: ["x"] } : data());
  await openRunTimings();
  if (asked.length !== 1 || asked[0].path !== "/api/admin/run-timings" || asked[0].method !== "GET") fail("the question: " + JSON.stringify(asked));
  if (!$("#modal").innerHTML.includes('id="rt-form"') || $("#modal").hidden || !$("#rt-body").innerHTML.includes('id="rt-settle_s"')) fail("the dialog is not drawn");
  if (!$("#rt-save").disabled) fail("Save is on with nothing to change");
  const type = (key, v) => $("#rt-body").oninput({ target: { closest: (sel) => (sel === "input[data-rt]" ? { dataset: { rt: key }, value: v } : null) } });
  type("settle_s", "90");
  if ($("#rt-save").disabled || !$("#rt-foot").innerHTML.includes("Pause before the shutdown: 90 s = 1 min 30 s (was 60 s = 1 minute)")) fail("after typing: " + $("#rt-foot").innerHTML);
  type("settle_s", "5");
  if (!$("#rt-save").disabled) fail("Save is on with a value outside its range");
  type("settle_s", " 60 ");  // the value in force again: nothing to change
  if (!$("#rt-save").disabled || rt.draft.text.settle_s !== undefined) fail("the value in force typed again is a change: " + JSON.stringify(rt.draft));
  type("settle_s", "90"); type("off_after_s", "360");
  await $("#rt-form").onsubmit({ preventDefault() {} });
  const put = asked.find((x) => x.method === "PUT");
  if (!put || JSON.stringify(put.body) !== '{"off_after_s":360,"settle_s":90}') fail("the request: " + JSON.stringify(put));
  if (closed !== 1 || flashes.at(-1)?.msg !== "Run timings: saved" || fetched !== 1) fail("after the save: closed " + closed + ", flash " + JSON.stringify(flashes.at(-1)) + ", box read again " + fetched);

  // the panel asks to confirm: the dialog hands the request over and stays out of the way
  els = {}; detached = false; asked.length = 0; closed = 0;
  answer = async (path, opts) => { if (opts.method === "PUT") { const e = new Error("confirm"); e.stepUp = true; throw e; } return set(data(), "off_after_s", { value: 360, from: "panel" }); };
  await openRunTimings();
  $("#rt-body").onclick({ target: { closest: (sel) => (sel === "[data-rt-reset]" ? { dataset: { rtReset: "off_after_s" } } : null) } });
  if ($("#rt-off_after_s").value !== "900" || !$("#rt-off_after_s").focused || $("#rt-save").disabled) fail("after Reset: field " + $("#rt-off_after_s").value + ", save disabled " + $("#rt-save").disabled);
  await $("#rt-form").onsubmit({ preventDefault() {} });
  if (!confirming || confirming.label !== "Run timings" || confirming.method !== "PUT" || confirming.path !== "/api/admin/run-timings" || JSON.stringify(confirming.body) !== '{"reset":["off_after_s"]}') fail("handed to Confirm it's you: " + JSON.stringify(confirming));
  if (closed !== 0) fail("the dialog closed itself while the confirmation is asked for");
  // a Reset of a timing that was not changed here does nothing (a button that is not there)
  $("#rt-body").onclick({ target: { closest: (sel) => (sel === "[data-rt-reset]" ? { dataset: { rtReset: "settle_s" } } : null) } });
  if (rt.draft.reset.settle_s) fail("a Reset for a timing from config.yaml");

  // the panel refuses: its sentence is shown, the dialog stays, Save is on again
  els = {}; detached = false; closed = 0;
  answer = async (path, opts) => { if (opts.method === "PUT") throw new Error("off_after_s must be 300-3600"); return data(); };
  await openRunTimings();
  type("off_after_s", "360");
  await $("#rt-form").onsubmit({ preventDefault() {} });
  if ($("#rt-err").textContent !== "off_after_s must be 300-3600" || closed !== 0 || $("#rt-save").disabled || rt.busy) fail("a refusal: " + $("#rt-err").textContent);
`)
}
