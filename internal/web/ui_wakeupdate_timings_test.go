package web

import (
	"strings"
	"testing"
)

// The dialog of "Update unpowered hosts" names the panel's timings in its sentences ("a countdown of 2 min",
// "counts as off only after 15 min without an answer"). A machine with timings of its own (the wake_update:
// block of its systems: entry) would be described wrongly by them, so its own line in the dialog says what
// holds for it, and only that: a machine without a block, and a value that is the panel's, add nothing. The
// API leaves both keys out of such a machine's entry (TestWakeUpdateAMachinesOwnTimings).
func TestUIWakeUpdateDialogSaysAMachinesOwnTimings(t *testing.T) {
	runWuPageScript(t, "wu-own-timings", `
  const same = (got, want, what) => { if (got !== want) fail(what + ": " + JSON.stringify(got) + ", want " + JSON.stringify(want)); };
  same(wuOwnTimings(host("pc-a")), "", "a machine without timings of its own");
  same(wuOwnTimings(host("pc-a", { off_after_s: 360, shutdown_delay_s: 30 })), " · counts as off after 360 s without an answer · its countdown: 30 s", "both of its own");
  same(wuOwnTimings(host("pc-a", { off_after_s: 360 })), " · counts as off after 360 s without an answer", "its own off_after_s only");
  same(wuOwnTimings(host("pc-a", { shutdown_delay_s: 30 })), " · its countdown: 30 s", "its own countdown only");
`)
	// the line of a machine that would be woken is where it is said: a machine that is on is never switched
	// off by a run and is not judged "off", so neither number means anything in its line
	if js := appJS(t); !strings.Contains(js, `${kind === "wake" ? wuOwnTimings(h) : ""}</span></span></label>`) {
		t.Error("the dialog's line of a machine to wake does not say the machine's own timings any more")
	}
}
