package web

import (
	"strings"
	"testing"
)

// The line about a countdown a run does not take for a machine that checks passkey approvals itself, in the
// two dialogs that set the number. It is one line, and it is there only then: the countdown in the field (or
// the one in force, while the field holds nothing that can be sent) is above the panel's limit, and a
// machine it matters for is there. The dialogs' own functions are run in node.

func TestUIRunTimingsSaysACountdownARunDoesNotTake(t *testing.T) {
	runRtPageScript(t, "rt-checked", `
  const one = "1 machine checks passkey approvals itself and uses this countdown. A run that wakes it does not start while the countdown is above 10 minutes.";
  const two = "2 machines check passkey approvals themselves and use this countdown. A run that wakes one of them does not start while the countdown is above 10 minutes.";
  const shown = (d, typed) => { const draft = rtDraft(); if (typed !== undefined) draft.text.shutdown_delay_s = typed; const h = rtFoot(d, draft); return h.includes('id="rt-checked"') ? words(h.slice(h.indexOf('<p class="note warn eh-ignored" id="rt-checked">')).split("</p>")[0]) : ""; };
  const long = (o) => set(data(o), "shutdown_delay_s", { value: 900, from: "panel" });
  for (const [what, d, typed, want] of [
    ["an answer without the count", data(), "900", ""],
    ["no such machine", data({ checked_hosts: 0, checked_max_s: 600 }), "900", ""],
    ["two minutes in force", data({ checked_hosts: 2, checked_max_s: 600 }), undefined, ""],
    ["ten minutes typed", data({ checked_hosts: 2, checked_max_s: 600 }), "600", ""],
    ["601 s typed", data({ checked_hosts: 2, checked_max_s: 600 }), "601", two],
    ["601 s typed, one machine", data({ checked_hosts: 1, checked_max_s: 600 }), " 601 ", one],
    ["15 minutes in force", long({ checked_hosts: 1, checked_max_s: 600 }), undefined, one],
    ["15 minutes in force, the field holds no number", long({ checked_hosts: 2, checked_max_s: 600 }), "x", two],
    ["15 minutes in force, two typed", long({ checked_hosts: 2, checked_max_s: 600 }), "120", ""],
    ["no limit in the answer", long({ checked_hosts: 2 }), undefined, ""],
    ["another limit", long({ checked_hosts: 1, checked_max_s: 1200 }), undefined, ""]])
    if (shown(d, typed) !== want) fail(what + ": " + JSON.stringify(shown(d, typed)));
  // after a Reset the field shows config.yaml's value, and the line follows it
  let draft = rtDraft(); draft.reset.shutdown_delay_s = true;
  if (rtFoot(long({ checked_hosts: 1, checked_max_s: 600 }), draft).includes("rt-checked")) fail("the line stays after a Reset to two minutes");
  // the count and the limit are whole numbers, or the answer is not drawn at all
  for (const bad of [{ checked_hosts: "<img src=x onerror=alert(1)>" }, { checked_hosts: 1.5 }, { checked_max_s: "600" }, { checked_hosts: [1] }, { checked_max_s: {} }])
    if (rtValid(data(bad))) fail("rtValid takes " + JSON.stringify(bad));
  if (!rtValid(data({ checked_hosts: 3, checked_max_s: 600 })) || !rtValid(data({ checked_hosts: null }))) fail("rtValid refuses the panel's answer");
`)
	js := appJS(t)
	for _, want := range []string{
		"    ${rtChecked(d, draft) ? `<p class=\"note warn eh-ignored\" id=\"rt-checked\">${esc(rtChecked(d, draft))}</p>` : \"\"}\n", // its words go through esc()
		`"A machine that checks passkey approvals takes a countdown of at most 10 minutes in a run.",`,                                // the info list says the rule
	} {
		if strings.Count(js, want) != 1 {
			t.Errorf("app.js lacks (or has twice) %q", want)
		}
	}
}

func TestUIEditHostSaysACountdownARunDoesNotTake(t *testing.T) {
	runEhPageScript(t, "eh-checked", ehRunFixture+`
  const line = "This machine checks passkey approvals itself. A run that wakes it does not start while this countdown is above 10 minutes.";
  const held = (b, max) => { const d = withRun({}, b); if (max !== undefined) ehHost(d).run.checked_max_s = max; return d; };
  const shown = (d, draft) => {
    const html = ehView(d, draft), i = html.indexOf('<p class="note warn eh-ignored" id="eh-run-checked"');
    if (i < 0 || html.indexOf('id="eh-run-checked"', i + 60) >= 0) fail("the line's place is not there once: " + html);
    const p = html.slice(i).split("</p>")[0], hidden = p.startsWith('<p class="note warn eh-ignored" id="eh-run-checked" hidden>');
    if (hidden !== (words(p) === "")) fail("hidden and its words do not go together: " + p);
    return words(p);
  };
  const typed = (d, v) => { const draft = ehDraft(); ehField(d, draft, "run-shutdown_delay_s", v); return draft; };
  const own = { value: 900, from: "panel" };
  for (const [what, d, draft, want] of [
    ["a machine without the check", held(own), ehDraft(), ""],
    ["null in the answer", held(own, null), ehDraft(), ""],
    ["two minutes", held({}, 600), ehDraft(), ""],
    ["its own 15 minutes", held(own, 600), ehDraft(), line],
    ["its own 15 minutes, ten typed", held(own, 600), typed(held(own, 600), "600"), ""],
    ["601 s typed", held({}, 600), typed(held({}, 600), "601"), line],
    ["its own 15 minutes, no number in the field", held(own, 600), typed(held(own, 600), "9"), line],
    ["15 minutes of Run timings", held({ value: 900, base: 900 }, 600), ehDraft(), line],
    ["a limit that is no number", held(own, "600"), ehDraft(), ""]])
    if (shown(d, draft) !== want) fail(what + ": " + JSON.stringify(shown(d, draft)));
  // after a Reset the field shows what counts without the change, and the line follows it
  let d = held(own, 600), draft = ehDraft();
  ehReset(d, draft, "shutdown_delay_s:");
  if (shown(d, draft) !== "") fail("the line stays after a Reset to two minutes");
`)
	js := appJS(t)
	for _, want := range []string{
		`<p class="note warn eh-ignored" id="eh-run-checked"${ehRunChecked(d, draft) ? "" : " hidden"}>${esc(ehRunChecked(d, draft))}</p>` + "`;",
		// while somebody types, the line's words are set as text: no markup is written
		"    const held = $(\"#eh-run-checked\");  // the line about a countdown a run does not take: its words, or none\n    if (held) { held.textContent = ehRunChecked(eh.d, eh.draft); held.hidden = !held.textContent; }\n",
	} {
		if strings.Count(js, want) != 1 {
			t.Errorf("app.js lacks (or has twice) %q", want)
		}
	}
}
