package web

import (
	"strings"
	"testing"
)

// The second bug run of audit 5 (r2-b1): what the page's script and style sheet must hold for each fix. The
// behaviour itself was run in a browser (reports/audit5-panel-fixes-r2y); these tests keep the lines in place.

// r2-b1-04: the browser's Back with a dialog open changed the page behind it and left the dialog ("Shut down
// ALPHA?") open and usable over another page. A dialog now belongs to the address it was built on, and a
// change of address closes it the way Cancel does.
func TestUIADialogClosesWhenTheAddressChanges(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`new MutationObserver(() => { modalAt = location.hash; }).observe($("#modal"), { childList: true });`,
		`if (!$("#modal").hidden && modalAt !== location.hash) closeModal();`,
		// closeModal is Cancel for "Confirm it's you" too: the kept action is dropped, never run
		`if (stepUpOpen) { cancelStepUp(); return; }`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	// the check runs in route(), before the new page is drawn
	i, j := strings.Index(js, "function route() {"), strings.Index(js, `modalAt !== location.hash) closeModal();`)
	if i < 0 || j < i || j-i > 400 {
		t.Error("route() does not close a dialog of another address")
	}
}

// r2-b1-05: the Fans tab was built anew with every refresh, so the mode button that opened "Confirm it's you"
// was gone when the dialog closed and the focus was on nothing (12 of 12). And outside full mode, where the BMC
// tells no duty, both zones read "50%": a number nobody measured.
func TestUIFansTabKeepsItsButtonsAndShowsNoMadeUpDuty(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		"morph(body, `<div class=\"warnbox\">Writes go to the BMC.",
		`const known = f.duty[z] != null, v = known ? f.duty[z] : 50;`,
		"${known ? `${esc(v)}%` : \"–\"}</b>",
		`body.querySelectorAll("[data-zone]").forEach((r) => (r.value = r.getAttribute("value")));`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	if strings.Contains(js, "f.duty[z] ?? 50") || strings.Contains(js, "body.innerHTML = `<div class=\"warnbox\">Writes go to the BMC.") {
		t.Error("the Fans tab is built anew, or shows 50% for a duty that is not known")
	}
}

// r2-b1-06: a countdown that had run out stayed "Shut down in 0:00 · Cancel" for as long as the agent went on
// reporting the action (a minute; and the machine may never go down: a shutdown its system refuses). After its
// time the line says "requested", has no Cancel, and says "still on" when the host still answers 20 s later.
func TestUIACountdownThatRanOutSaysSo(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`const PEND_LATE = 20;`,
		`const pendingDue = (p) => (Date.now() - new Date(p.at)) / 1000;`,
		"if (due >= 0) return `<div class=\"pending-strip\" data-key=\"due\"><span><b>${esc(POWER_LABEL[p.action])}</b> requested</span>${pendingLate(s, due)}</div>`;",
		"due >= PEND_LATE && s.status === \"online\"",
		"</span><b>still on</b>`",
		"${s.power_pending ? pendingStrip(s, why) : \"\"}",
		"`<span>${esc(p.action)} requested</span>${pendingLate(s, due)}`}",
		// the second it runs out the line is drawn anew, also when no data arrives
		`.some((el) => new Date(el.dataset.countdown) <= Date.now())) render();`,
		"  pendingTick();\n}, 1000);",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	// Cancel is only in the line that still counts down
	i := strings.Index(js, "function pendingStrip(s, why) {")
	j := strings.Index(js[i:], "\n}\n")
	fn := js[i : i+j]
	due, cancel := strings.Index(fn, "if (due >= 0) return"), strings.Index(fn, `data-action="cancel"`)
	if due < 0 || cancel < due || strings.Count(fn, `data-action="cancel"`) != 1 {
		t.Error("the line of an action whose time has passed offers Cancel")
	}
}

// r2-b1-10: a rate limit's refusal (429) says how long to wait. The panel's own limit on control requests says
// it in its text; for the others the page reads Retry-After.
func TestUIARateLimitSaysHowLongToWait(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`const wait = res.status === 429 ? parseInt(res.headers.get("Retry-After"), 10) : 0;`,
		"; try again in ${wait} s`;",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
}

// r2-b1-11: while the panel was down, an action answered with the browser's words ("Failed to fetch"). Now a
// write is not sent at all when the panel does not answer a knock ("Nothing was sent." is then true), and a
// request that was sent and got no answer says so in the page's own words.
func TestUIAPanelThatIsDownIsNamedInThePagesWords(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`if (write && !opts.keepalive && (net.down || Date.now() - net.heard > NET_ASK)) {`,
		`if (net.down) throw netError("Panel not reachable. Nothing was sent.");`,
		`throw netError(write ? "Panel not reachable. No answer: it is not known whether this was done." : "Panel not reachable.");`,
		`function netKnockNow() { net.heard = Math.min(net.heard, Date.now() - NET_QUIET); net.knocked = 0; return netKnock(); }`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	// "Nothing was sent." is said only before the request is made
	if i, j := strings.Index(js, `"Panel not reachable. Nothing was sent."`), strings.Index(js, "res = await fetch(path,"); i < 0 || j < i {
		t.Error(`"Nothing was sent." must be decided before fetch() is called`)
	}
}

// r2-b1-12: texts that disagreed. "running · since 64.7h" (every other duration on the page is "2d 16h"), and
// the commit of a version in capitals in the chips ("BUILD 42 (1A2B3C4)") while the dialogs say "1a2b3c4".
func TestUITextsThatDisagreed(t *testing.T) {
	js, css := appJS(t), appCSS(t)
	for _, want := range []string{
		"${h.since ? ` · for ${fmtUptime(secsSince(h.since))}` : \"\"}",
		"new Error(`picture larger than ${Math.round(signInWall.max_bytes / 2 ** 20)} MiB`)",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	if strings.Contains(js, `.replace(" ago", "")`) {
		t.Error(`a duration is made from "ago" again ("since 64.7h")`)
	}
	if !strings.Contains(css, ".ver .chip, .table.updates td:nth-child(2) .chip { text-transform: none; }") {
		t.Error("the version chips write the commit in capitals")
	}
}
