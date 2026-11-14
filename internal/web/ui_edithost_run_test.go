package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// "Edit host" on the page, the machine's own run timings: the group "Run timings" between "Wake-on-LAN" and
// the links. The dialog's own functions are run in node, as in ui_edithost_machine_test.go.

const ehRunFixture = ehHostFixture + `
  const runs = (a = {}, b = {}) => ({ timings: [
    { key: "off_after_s", value: 900, from: "run", base: 900, base_from: "run", min: 300, max: 3600, by: null, at: null, ...a },
    { key: "shutdown_delay_s", value: 120, from: "run", base: 120, base_from: "run", min: 10, max: 3600, by: null, at: null, ...b }] });
  const withRun = (a, b, o) => { const h = host(o); h.run = runs(a, b); return data({ host: h }); };
  const same = (got, want, what) => { if (JSON.stringify(got) !== JSON.stringify(want)) fail(what + ": " + JSON.stringify(got)); };
`

// Where the group is and how it is written: at the spot between the machine's groups and the links, every
// value through esc(), the words of the "Run timings" dialog, and the info button says what it is.
func TestUIEditHostRunTimingsIsAGroupOfTheDialog(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		"return `${ehHostView(d, draft)}${ehRunView(d, draft)}${sect(\"UPS\",", // between "Wake-on-LAN" and "UPS"
		`if (Object.keys(run.body).length) body.wake_update = run.body;`,
		`const EH_RUN = ["off_after_s", "shutdown_delay_s"];`,
		`"Run timings: this machine's own time to count as off and its own shutdown countdown in a run of Update unpowered hosts. A machine without its own follows Run timings on the Updates page. Every change to them asks you to confirm, and counts from the next run.",`,
		`"One machine can have its own time to count as off and its own countdown: Edit host, in the ⋯ menu on its page (or a wake_update: block in its systems: entry of config.yaml)."]));`,
		`el.textContent = ehRunRead(t, eh.draft);`, // while somebody types: the reading as text
	} {
		if strings.Count(js, want) != 1 {
			t.Errorf("app.js lacks (or has twice) %q", want)
		}
	}
	from, to := strings.Index(js, "\nconst EH_RUN = "), strings.Index(js, "\nfunction ehPlan(d, draft) {")
	if from < 0 || to < from {
		t.Fatal("app.js has no \"Run timings\" group in the Edit host part")
	}
	part := js[from:to]
	if strings.Contains(part, "innerHTML") || strings.Contains(part, "insertAdjacentHTML") || strings.Contains(part, "outerHTML") {
		t.Error("the group writes markup by itself")
	}
	// every value of the panel that a template of the group inserts goes through esc(); what is inserted as it
	// is, is markup the group made itself or fixed words
	view := regexp.MustCompile("esc\\(`[^`]*`\\)").ReplaceAllString(part[strings.Index(part, "function ehRunTag"):], "esc()")
	for _, m := range regexp.MustCompile(`\$\{[^{}]*\}`).FindAllString(view, -1) {
		if strings.HasPrefix(m, "${esc(") || slices.Contains([]string{`${panel ? " on" : ""}`, "${ehRunBase(t)}", "${ehRunTag(t, draft)}"}, m) {
			continue
		}
		t.Errorf("the group inserts %s without esc()", m)
	}
	// the names of the two timings are the panel's
	for _, tm := range config.MachineRunTimings() {
		if !strings.Contains(js, tm.Key+`: { label: "`+rtLabel[tm.Key]+`", `) {
			t.Errorf("app.js does not call %s %q", tm.Key, rtLabel[tm.Key])
		}
	}
}

// What the group shows, what a field takes, the lines and the request Save would make, Reset, and that an
// answer that is not the panel's draws nothing.
func TestUIEditHostRunTimingsGroup(t *testing.T) {
	runEhPageScript(t, "eh-run", ehRunFixture+`
  // an answer without host.run (an older panel): no group, and the plan as before
  let d = data({ host: host() }), draft = ehDraft();
  let html = ehView(d, draft), text = words(html);
  if (text.includes("Run timings") || html.includes("eh-rt-")) fail("a group without d.host.run: " + text);
  same(ehPlan(d, draft), { lines: [], body: {}, notes: [], problems: [] }, "a plan without d.host.run");
  // ... and one whose rows are not the panel's: nothing of it is drawn
  for (const bad of [{ timings: "x" }, { timings: [{ key: "online_timeout_s", value: 60, base: 60, min: 60, max: 1800, from: "run", base_from: "run" }] }, runs({ value: "900" }), runs({ from: "<b>" }), runs({}, { min: null }), runs({ base_from: "x" })]) {
    const h = host(); h.run = bad;
    if (ehView(data({ host: h }), ehDraft()).includes("eh-rt-") || ehRun(data({ host: h })).length) fail("rows that are not the panel's are drawn: " + JSON.stringify(bad));
  }

  d = withRun(); draft = ehDraft();
  html = ehView(d, draft); text = words(html);
  if (!text.includes("Run timings Counts as off after from Run timings s = 15 minutes Shutdown countdown from Run timings s = 2 minutes This machine's own, in a run of Update unpowered hosts. UPS")) fail("the group: " + text);
  if (!(html.indexOf("Wake-on-LAN") < html.indexOf(">Run timings<") && html.indexOf(">Run timings<") < html.indexOf(">UPS<"))) fail("the group is not between Wake-on-LAN and UPS");
  if (!html.includes('<input class="input" id="eh-rt-off_after_s" data-eh-f="run-off_after_s" type="text" inputmode="numeric" maxlength="5" autocomplete="off" spellcheck="false" value="900">')
    || !html.includes('<label for="eh-rt-shutdown_delay_s">Shutdown countdown</label>') || !html.includes('<span class="rt-read" data-eh-read="shutdown_delay_s">s = 2 minutes</span>')) fail("the fields: " + html);
  if (html.includes("data-eh-reset") || text.includes("This will change") || text.includes("Changed here") || ehCanSave(ehPlan(d, draft))) fail("nothing was changed: " + text);

  // a time typed: the line, what it means for this machine under it, the request
  ehField(d, draft, "run-off_after_s", " 360 ");
  let plan = ehPlan(d, draft);
  const means6 = "If PC-B is silent for longer than 6 minutes, it counts as off. If it was restarting and comes up during a run, the run switches it off after its update.";
  same(plan, { lines: ["Counts as off after: 360 s = 6 minutes (was 900 s = 15 minutes)", means6], body: { wake_update: { off_after_s: 360 } }, notes: [], problems: [], means: [means6] }, "a time typed");
  if (!ehCanSave(plan) || !ehPlanView(plan).includes('<li class="eh-sees">' + means6 + "</li>") || !ehPlanView(plan).includes("<li>Counts as off after: 360 s")) fail("the list of changes: " + ehPlanView(plan));
  html = ehView(d, draft);
  if (!words(ehHostTag(d, draft, "off_after_s")).includes("changed here") || !html.includes('value=" 360 "') || !html.includes('data-eh-read="off_after_s">s = 6 minutes<')
    || !words(html).includes("Changed here: used instead of config.yaml until Reset.") || html.includes("data-eh-reset")) fail("after typing: " + words(html));
  // the value it has again: no change
  ehField(d, draft, "run-off_after_s", "900");
  same(ehPlan(d, draft).lines, [], "the value it has");
  if (!words(ehHostTag(d, draft, "off_after_s")).includes("from Run timings")) fail("the tag of the value it has");
  // what cannot be sent: said with the range, nothing of it is in the request, Save is off
  for (const bad of ["299", "60", "0", "-300", "3601", "", "abc", "36 0", "1e3", "300.5", "0x200", "99999", "９００"]) {
    ehField(d, draft, "run-off_after_s", bad);
    plan = ehPlan(d, draft);
    if (plan.lines.length || "wake_update" in plan.body || plan.problems[0] !== "Counts as off after: 300 to 3600." || ehCanSave(plan) || ehRunRead(ehRun(d)[0], draft) !== "300 to 3600") fail("typed " + JSON.stringify(bad) + ": " + JSON.stringify(plan));
  }
  for (const ok of ["300", "3600", "0300"]) {
    ehField(d, draft, "run-off_after_s", ok);
    if (ehPlan(d, draft).body.wake_update?.off_after_s !== Number(ok) || ehPlan(d, draft).problems.length) fail("typed " + ok + ": " + JSON.stringify(ehPlan(d, draft)));
  }
  draft = ehDraft();
  for (const bad of ["9", "0", "3601", "five"]) {
    ehField(d, draft, "run-shutdown_delay_s", bad);
    if (ehPlan(d, draft).problems[0] !== "Shutdown countdown: 10 to 3600." || ehCanSave(ehPlan(d, draft))) fail("a countdown of " + bad + ": " + JSON.stringify(ehPlan(d, draft)));
  }
  // a longer countdown has its line; a shorter one also says what it means
  ehField(d, draft, "run-shutdown_delay_s", "300");
  same(ehPlan(d, draft), { lines: ["Shutdown countdown: 300 s = 5 minutes (was 120 s = 2 minutes)"], body: { wake_update: { shutdown_delay_s: 300 } }, notes: [], problems: [] }, "a longer countdown");
  ehField(d, draft, "run-shutdown_delay_s", "30"); ehField(d, draft, "name", "Den <b>");
  plan = ehPlan(d, draft);
  same(plan.lines, ["Name: Den <b> (was PC-B)", "Shutdown countdown: 30 s (was 120 s = 2 minutes)", "Somebody at Den <b> has 30 seconds to cancel a run's shutdown."], "a shorter countdown, next to a new name");
  same(plan.body, { name: "Den <b>", wake_update: { shutdown_delay_s: 30 } }, "the request");
  if (ehPlanView(plan).includes("<b>") || !ehPlanView(plan).includes("Somebody at Den &lt;b&gt; has 30 seconds")) fail("a name in the sentence is put into the page as markup: " + ehPlanView(plan));

  // a timing that was changed here, over config.yaml's own: "changed here" with Reset
  d = withRun({ value: 360, from: "panel", base: 600, base_from: "config", by: "nora <n@example.com>", at: "2026-11-14T10:00:00Z" }); draft = ehDraft();
  html = ehView(d, draft); text = words(html);
  if (!text.includes("Counts as off after changed here Reset s = 6 minutes") || !html.includes('<button class="btn sm" type="button" data-eh-reset="off_after_s:" title="Back to config.yaml: 600 s = 10 minutes">Reset</button>')
    || !html.includes('title="Changed here. Last change: nora &lt;n@example.com&gt;"') || !text.includes("Changed here: used instead of config.yaml until Reset.") || (html.match(/data-eh-reset/g) || []).length !== 1) fail("a stored change: " + html);
  ehReset(d, draft, "shutdown_delay_s:");  // nothing was changed there: nothing to put back
  same(draft.reset, {}, "Reset of a timing that was not changed here");
  ehField(d, draft, "run-off_after_s", "420");
  ehReset(d, draft, "off_after_s:");
  plan = ehPlan(d, draft);
  same(plan.lines, ["Counts as off after: back to config.yaml: 600 s = 10 minutes (was 360 s = 6 minutes)", "If PC-B is silent for longer than 10 minutes, it counts as off. If it was restarting and comes up during a run, the run switches it off after its update."], "Reset");
  same(plan.body, { reset: [{ field: "off_after_s" }] }, "the request of a Reset");
  html = ehView(d, draft);
  if (!html.includes('data-eh-f="run-off_after_s" type="text" inputmode="numeric" maxlength="5" autocomplete="off" spellcheck="false" value="600"') || html.includes("data-eh-reset")
    || !words(ehHostTag(d, draft, "off_after_s")).includes("from config.yaml") || words(html).includes("Changed here: used")) fail("after Reset: " + words(html));
  // typing after a Reset takes the Reset back
  ehField(d, draft, "run-off_after_s", "600");
  plan = ehPlan(d, draft);
  same(plan.body, { wake_update: { off_after_s: 600 } }, "config.yaml's own value typed over a change");
  same(plan.lines[0], "Counts as off after: back to config.yaml: 600 s = 10 minutes (was 360 s = 6 minutes)", "its line");
  if (!words(ehHostTag(d, draft, "off_after_s")).includes("from config.yaml")) fail("its tag");
  // a change over the panel's own timing goes back to "Run timings"; one that is the panel's value now changes no value
  d = withRun({}, { value: 45, from: "panel", base: 45, base_from: "run" }); draft = ehDraft();
  if (!ehView(d, draft).includes('title="Back to Run timings: 45 s"')) fail("the Reset of a change over the panel's timing: " + ehView(d, draft));
  ehReset(d, draft, "shutdown_delay_s:");
  same(ehPlan(d, draft).lines, ["Shutdown countdown: back to Run timings: nothing changes now (45 s)"], "a Reset that changes no value");
  d = withRun({}, { value: 45, from: "panel", base: 120, base_from: "run" }); draft = ehDraft();
  ehField(d, draft, "run-shutdown_delay_s", "120");
  same(ehPlan(d, draft).lines, ["Shutdown countdown: back to Run timings: 120 s = 2 minutes (was 45 s)"], "the panel's value typed over a change");
  // with the links and Wake-on-LAN in one request, in the panel's order
  d = withRun(); d.ups = [ups("rack")]; draft = ehDraft();
  ehTick(d, draft, "ups", "nas-agent/rack", true); ehField(d, draft, "wol-port", "7"); ehField(d, draft, "run-off_after_s", "1800");
  plan = ehPlan(d, draft);
  same(plan.body, { ups: [{ id: "nas-agent/rack", on: true }], wol: { macs: ["02:00:00:00:00:b1"], port: 7, relay: "relay-a" }, wake_update: { off_after_s: 1800 } }, "one request");
  same(plan.lines.slice(1), ["Counts as off after: 1800 s = 30 minutes (was 900 s = 15 minutes)", "If PC-B is silent for longer than 30 minutes, it counts as off. If it was restarting and comes up during a run, the run switches it off after its update.", "UPS RACK UPS (read by NAS): powers PC-B"], "its lines");
`)
}

// The lines the dialog lists before Save are the panel's own: for the same changes, on the panel's real
// answer, the page's list and what "Confirm it's you" shows (and the audit log gets) are the same sentences,
// the request the page makes is taken as it is, and it asks for its own confirmation.
func TestUIEditHostRunTimingsLinesAreThePanelsWords(t *testing.T) {
	r := hedSetup(t)
	r.s.Poller.Override("pc-a").WakeUpdate = &config.SystemWakeUpdate{OffAfterS: 600} // systems: - {id: pc-a, wol: ..., wake_update: {off_after_s: 600}}
	step := func(id, fields string) {
		t.Helper()
		r.a.clk.advance(time.Duration(r.s.Cfg.StepUpWindowS+5) * time.Second) // the confirmation of the step before is over
		g := r.options(id)
		file := filepath.Join(t.TempDir(), "d.json")
		if err := os.WriteFile(file, g.raw, 0o600); err != nil {
			t.Fatal(err)
		}
		out := runEhPageScript(t, "eh-run-words", `
  const d = JSON.parse(require("fs").readFileSync(process.argv[2], "utf8")), draft = ehDraft();
  if (ehRun(d).length !== 2) fail("the panel's answer has no run timings the page takes: " + JSON.stringify(d.host && d.host.run));
  for (const [field, v] of JSON.parse(process.argv[3])) { if (field === "reset") ehReset(d, draft, v); else ehField(d, draft, field, v); }
  console.log(JSON.stringify(ehPlan(d, draft)));
`, file, fields)
		var plan struct {
			Lines    []string       `json:"lines"`
			Body     map[string]any `json:"body"`
			Notes    []string       `json:"notes"`
			Problems []string       `json:"problems"`
		}
		if err := json.Unmarshal([]byte(out), &plan); err != nil || len(plan.Lines) == 0 || len(plan.Problems) != 0 {
			t.Fatalf("the page's plan for %s: %v\n%s", fields, err, out)
		}
		old := r.do("PUT", "/api/admin/hosts/"+id+"/options", plan.Body, "Content-Type", "application/json", "Cookie", r.a.sess, "X-Requested-With", "fairy")
		d, _ := old.body["detail"].(map[string]any)
		if !stepUpRequired(old) || d["per_action"] != true {
			t.Fatalf("%s on %s: the page's request %v: %d %s", fields, id, plan.Body, old.code, old.raw)
		}
		if got, want := strs(d["items"]), append(slices.Clone(plan.Lines), plan.Notes...); !slices.Equal(got, want) {
			t.Errorf("%s on %s:\nthe page lists %q\nthe panel says %q", fields, id, want, got)
		}
		if cb := r.a.confirm(pendingID(old), false); cb.code != 303 {
			t.Fatalf("the confirmation: %d", cb.code)
		}
		if res, _ := r.do("GET", "/api/me", nil, "Cookie", r.a.sess).body["pending_result"].(map[string]any); res["ok"] != true {
			t.Fatalf("%s on %s: the panel did not take the page's request: %v", fields, id, res)
		}
		_, params := r.lastChange(id)
		if got := strs(params["lines"]); !slices.Equal(got, plan.Lines) {
			t.Errorf("%s on %s, the audit entry:\nthe page lists %q\nthe panel wrote %q", fields, id, plan.Lines, got)
		}
	}
	step("pc-a", `[["run-off_after_s", "360"]]`)                                                                          // over config.yaml's own 600
	step("pc-a", `[["run-off_after_s", " 420 "], ["run-shutdown_delay_s", "20"]]`)                                        // both, the countdown shorter
	step("pc-a", `[["run-off_after_s", "600"]]`)                                                                          // config.yaml's own value typed: the change goes
	step("pc-a", `[["run-shutdown_delay_s", "300"]]`)                                                                     // a longer countdown
	step("pc-a", `[["run-off_after_s", "3600"], ["name", "Gaming PC"]]`)                                                  // next to a new name: the sentence has it
	step("pc-a", `[["reset", "off_after_s:"], ["reset", "shutdown_delay_s:"]]`)                                           // both back
	step("pc-b", `[["run-shutdown_delay_s", "45"]]`)                                                                      // over the panel's own timing
	step("pc-b", `[["run-shutdown_delay_s", "120"]]`)                                                                     // the panel's own value typed: back to Run timings
	step("pc-b", `[["run-off_after_s", "300"], ["wol-on", true], ["wol-macs", "02:00:00:00:00:b1"], ["site", "site-a"]]`) // with a site and a first Wake-on-LAN
	r.rtSet(map[string]int{config.KeyOffAfter: 300})                                                                      // "Run timings": the panel's own time is the machine's now
	step("pc-b", `[["reset", "off_after_s:"]]`)                                                                           // a Reset that changes no value
	if got := r.storedRun("pc-a") + "|" + r.storedRun("pc-b"); got != "|" {
		t.Errorf("after the steps: %q", got)
	}
}
