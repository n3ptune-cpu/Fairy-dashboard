package web

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
)

// The bug run of audit 5 (r1-b1): what each fix put into the page, so that it does not go away unnoticed. The
// behaviour itself was measured in a browser against the demo and the test build (the report of the fixes
// has the numbers).

func appCSS(t *testing.T) string {
	t.Helper()
	b, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// r1-b1-01, r1-b1-13: a model name of 64 letters without a space, or a host with a 63-letter name, made the
// host's page up to 2.3 windows wide (908 px at 390): the value of a row, the line under the name and the name
// itself could not break.
func TestUILongWordsDoNotWidenTheHostPage(t *testing.T) {
	css := appCSS(t)
	for _, want := range []string{
		`.list-row > span:last-child { min-width: 0; overflow-wrap: anywhere; }`,
		`.detail-head h2 { margin: 0; font-size: 20px; min-width: 0; overflow-wrap: anywhere; }`,
		`.detail-head .sub { grid-column: 1 / -1; color: var(--muted); font-size: 13px; min-width: 0; overflow-wrap: anywhere; }`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css lacks %q", want)
		}
	}
}

// r1-b1-03: the box of "Set model name" took 200 characters, the limit is 64 (adminstate.MaxModelName): the
// 65th was refused only after Save. The box now stops at the limit.
func TestUIModelNameBoxStopsAtTheLimit(t *testing.T) {
	js := appJS(t)
	want := fmt.Sprintf(`<input class="input" id="mn-in" maxlength="%d" autocomplete="off"`, adminstate.MaxModelName)
	if !strings.Contains(js, want) {
		t.Errorf("app.js lacks %q", want)
	}
}

// r1-b1-05: the power dialog said "Linux rounds up to whole minutes". The agent waits the asked seconds itself
// (time.AfterFunc, then systemctl poweroff; Fairy-agent pkg/agent/power_linux.go, since its first release): 10 s
// are 10 s. Someone who relied on "at least a minute" to press Cancel had ten seconds. The sentence is gone.
func TestUIPowerDialogDoesNotSayLinuxRoundsUp(t *testing.T) {
	js := appJS(t)
	if strings.Contains(js, "rounds up") || strings.Contains(js, "whole minutes") {
		t.Error("app.js still says that a delay is rounded up")
	}
	if want := `<span class="note">a countdown with Cancel is shown</span></label>`; !strings.Contains(js, want) {
		t.Errorf("app.js lacks %q", want)
	}
}

// r1-b1-06: an Add host that failed after "Confirm it's you" (the panel could not reach GitHub) left no dialog,
// the form was lost, and the error was in the corner box for five seconds. The dialog now comes back with what
// was typed and the panel's whole sentence in it. What was typed waits in this tab's sessionStorage meanwhile.
func TestUIAddHostComesBackWithItsFormAfterAFailure(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`const AH_KEY = "fairy-addhost-form";`,
		`if (r) openEnrollDialog(r); else keepAddHost(body);`,
		`const typed = pr.path === "/api/admin/hosts" ? keptAddHost() : null;`,
		`if (!pr.ok && typed) { openAddHost(typed, pr.message || ` + "`HTTP ${pr.status}`" + `); return; }`,
		`async function openAddHost(typed = null, problem = "") {`,
		`if (!typed) keptAddHost();`, // a new dialog forgets an older form
		`$("#ah-err").textContent = problem;`,
		// the values go in as properties, and a choice only if the list still has it
		`const pick = (sel, v) => { if ([...sel.options].some((o) => o.value === text(v))) sel.value = text(v); };`,
		`$("#ah-name").value = text(typed.name); $("#ah-addr").value = text(typed.address);`,
		// the same when the action was run with "Resume" instead of by the panel
		`if (typed) openAddHost(typed, e.message); else flash(e.message, true);`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	// what is kept is read once and removed, and it is not used when it is older than ten minutes
	if !strings.Contains(js, `sessionStorage.removeItem(AH_KEY); } catch { k = null; }`) || !strings.Contains(js, `Date.now() - k.ts <= 10 * 60 * 1000 ? b : null;`) {
		t.Error("app.js keeps the typed form longer than one use, or without a limit")
	}
}

// r1-b1-07, r1-b1-08: at phone width the page's title was cut ("Upd…") while the grey sub-title next to it
// kept its room, and the buttons of the Updates page's two tables (Switch on, Update) were outside the window,
// with nothing to show that the table scrolls.
func TestUIPhoneWidthTitleAndUpdatesButtons(t *testing.T) {
	css := appCSS(t)
	for _, want := range []string{
		"text-overflow: ellipsis;\n  flex-shrink: 1000; }",
		`.table.updates td.num:last-child, .table.wu-machines td.num:last-child { position: sticky; right: 0; background: var(--panel);`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css lacks %q", want)
		}
	}
	// the sticky button column is for narrow windows only
	i, j := strings.Index(css, ".table.wu-machines { min-width: 560px; }"), strings.Index(css, ".table.updates td.num:last-child, .table.wu-machines td.num:last-child")
	if i < 0 || j < i || strings.Contains(css[i:j], "\n}\n") {
		t.Error("the sticky button column is not inside the phone-width block of the Updates page")
	}
}

// r1-b1-09: a button beside a text box or a list was 2 px lower than its neighbour (34 px next to 36 px) on
// Notifications, Audit, Settings and Users, and in the Add host dialog a box (36 px) stood beside a list (33
// px). A box and a list are now as high as a button. Measured in a browser: 63 rows of controls, none unequal.
func TestUIBoxesAndListsAreAsHighAsAButton(t *testing.T) {
	css := appCSS(t)
	for _, want := range []string{
		`.input, .select { height: 34px; }`,
		// a button's 34 px: 16 px of text + 2 x 8 px + the border
		`padding: 8px 12px; border-radius: var(--radius); font-size: 12.5px; font-weight: 600; cursor: pointer; font-family: inherit; line-height: 1.28; }`,
		// the small lists inside a table and the demo's switcher keep their own size
		`.table.users .select, .table.assign .select { padding: 4px 8px; font-size: 12.5px; height: auto; }`,
		`.demo-view .select { width: 100%; padding: 4px 8px; font-size: 12px; height: auto; }`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css lacks %q", want)
		}
	}
}

// r1-b1-10: the head of a host's page said "16 threads", its Resources card "16 cores", and the Overview added
// the same numbers up as "of 134 cores". The number is the machine's logical processors (the agent reports
// runtime.NumCPU, Proxmox maxcpu): threads. One word for it everywhere.
func TestUIOneWordForTheProcessorCount(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		"s.cores && `${s.cores} threads`, s.mem_total && fmtBytes(s.mem_total)]", // a Proxmox node's head
		"[model(s), s.os, s.cores && `${s.cores} threads`]",                      // a host's head
		"` · ${esc(s.cores)} threads` : \"\"}`)}",                                // Resources
		"<small> of ${esc(t.cores)} threads</small>",                             // Overview
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	if m := regexp.MustCompile("cores[)}]*\\} cores|\\d cores").FindAllString(js, -1); len(m) != 0 {
		t.Errorf("app.js still calls the number cores: %v", m)
	}
}

// r1-b1-11: the report of a cancelled run. A machine the run sent nothing to is "skipped · not touched" (the
// state comes from the run, internal/wakerun), the note under the heading speaks of woken machines only when
// the run woke one, and "over 2s ago" (which reads as "more than") is "ended 2s ago".
func TestUICancelledRunReport(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`skipped: ["", "skipped"]`, `untouched: ["", "not touched"]`, `stayed_on: ["ok", "done · stays on"]`,
		"`, ended <span data-ago=\"${esc(r.ended_at)}\">${ago(r.ended_at)}</span>`",
		`Cancelled by ${esc(c.by)}: ${!r.hosts.some((h) => h.wake_sent_at) ? "nothing else is started."`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	if strings.Contains(js, ", over <span") {
		t.Error(`app.js still says "over" for the end of a run`)
	}
}

// r1-b1-12: one word for one kind of event. With "Critical only" a pop-up came for an offline host (Settings:
// "Critical: a host goes offline, ..."), but it read "warning:", the list called it "warning", and the
// list's filter "Critical" did not find it. The pop-up and the list now say "critical" for what Settings
// calls critical; the filter does the same on the server (internal/notifications).
func TestUICriticalMeansTheSameInSettingsPopupsAndTheList(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`const noteSev = (e) => (e.severity === "critical" || e.alarm === true || e.urgent === true ? "critical" : e.severity);`,
		`<span class="sev ${esc(noteSev(e))}" aria-hidden="true"></span>${esc(noteSev(e))}</td>`,
		`it.el = popEl(it, n.urgent ? "critical" : "warning", "!",`,
		`"Critical: a host goes offline, a temperature passes its limit, a UPS goes on battery, runs low or cannot be reached."`,
		`popAdd({ sev: e.severity, urgent: e.urgent === true || e.severity === "critical",`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
}

// r1-b1-15: #/system/%E0%A4%A (a broken %-escape) threw "URI malformed" in every render and left an empty
// page. It is now an id no system has: "Unknown system.", as for any other unknown id.
func TestUIBrokenEscapeInTheAddressIsAnUnknownSystem(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`const hashPart = (v) => { try { return decodeURIComponent(v); } catch { return v; } };`,
		`if (m) return { page: "system", id: hashPart(m[1]), tab: m[2] || "overview" };`,
		`Unknown system.`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	// no other place decodes a part of the address without the catch
	if n := strings.Count(js, "decodeURIComponent("); n != 1 {
		t.Errorf("decodeURIComponent is called in %d places, want only in hashPart", n)
	}
}

// r1-b1-14: with the keyboard, Tab left an open dialog and walked through the page behind it (Enter on the
// sidebar's "Collapse" worked behind "Set model name"); two dialogs opened with the focus on the button behind
// them or on nothing; after Escape the focus was on nothing. Every dialog is built into #modal, so one place
// does it for all: Tab stays inside, the focus goes in on open and back to the opener on close.
func TestUIDialogsKeepTheKeyboardFocus(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`const modalKeys = { open: false, opener: null, sel: "", last: null, lastSel: "" };`,
		`}).observe($("#modal"), { attributes: true, attributeFilter: ["hidden"] });`,
		// in on open, unless the dialog put the focus somewhere in itself
		`setTimeout(() => { if (!m.hidden && !m.contains(document.activeElement)) modalStops()[0]?.focus(); }, 30);`,
		// back on close, unless something else has the focus by then
		`if (document.activeElement && document.activeElement !== document.body) return;`,
		`(o?.isConnected ? o : sel ? document.querySelector(sel) : null)?.focus();`,
		// Tab and Shift+Tab turn round at the ends, and come in from outside
		`if (e.key !== "Tab" || m.hidden || info.key) return;`,
		`if (!m.contains(a)) { e.preventDefault(); (e.shiftKey ? last : first).focus(); }`,
		`else if (e.shiftKey && a === first) { e.preventDefault(); last.focus(); }`,
		`else if (!e.shiftKey && a === last) { e.preventDefault(); first.focus(); }`,
		// a dialog opened from a menu's item goes back to the menu's button
		`const btn = e.target.closest(".menu-item") ? e.target.closest(".menu-wrap")?.querySelector('[data-act="menu"][data-menu]') : null;`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	// one place: no dialog has a Tab handler of its own (the other one closes the account menu)
	if n := strings.Count(js, `e.key !== "Tab"`) + strings.Count(js, `e.key === "Tab"`); n != 2 || !strings.Contains(js, `else if (e.key === "Tab") closeAcct(false);`) {
		t.Errorf("Tab is handled in %d places, want the dialogs' one and the account menu's", n)
	}
}
