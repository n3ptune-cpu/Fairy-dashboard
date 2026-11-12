package web

import (
	"fmt"
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
