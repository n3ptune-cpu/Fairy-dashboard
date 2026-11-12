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
