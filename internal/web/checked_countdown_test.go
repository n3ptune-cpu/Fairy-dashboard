package web

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// A machine that checks passkey approvals itself takes a shutdown countdown of at most ten minutes in a run
// that wakes it. The start of a run says so and starts nothing; its sentence names where the number comes
// from, so the admin knows which one to change. And the two dialogs in which the number is set are told
// about it before: Edit host for the machine's own countdown, Run timings for the panel's with the count of
// such machines that follow it.
func TestACountdownARunDoesNotTakeIsSaidWhereItIsSetAndNamedAtTheStart(t *testing.T) {
	r := newRunRig(t, "pc-a")
	start := func() string {
		t.Helper()
		x := r.req("POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}})
		if _, _, asked := approvalAsked(x); asked || x.code != 400 {
			t.Fatalf("the start: %d %s", x.code, x.raw)
		}
		d, _ := x.body["detail"].(string)
		return d
	}
	dialogs := func(when string, wantFollowers int, wantHeld bool) {
		t.Helper()
		g := r.req("GET", "/api/admin/run-timings", nil)
		if g.code != 200 || g.body["checked_hosts"] != float64(wantFollowers) || g.body["checked_max_s"] != 600.0 {
			t.Errorf("%s: Run timings: %d checked_hosts %v, checked_max_s %v", when, g.code, g.body["checked_hosts"], g.body["checked_max_s"])
		}
		for id, held := range map[string]bool{"pc-a": wantHeld, "pc-b": false} {
			g := r.req("GET", ehPath(id), nil)
			h, _ := g.body["host"].(map[string]any)
			run, _ := h["run"].(map[string]any)
			if max, has := run["checked_max_s"]; g.code != 200 || !has || (held && max != 600.0) || (!held && max != nil) {
				t.Errorf("%s: Edit host of %s: %d checked_max_s %v (%v)", when, id, g.code, max, has)
			}
		}
	}
	own := func(v int) {
		t.Helper()
		if _, _, err := r.s.Poller.Admin.EditHostOptions("admin", func(o *adminstate.HostOptions) error {
			o.SetHost("pc-a", adminstate.HostEdit{Run: adminstate.RunEdit{ShutdownDelayS: v}})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		r.s.Poller.Bump()
	}
	const lead = "PC-A checks passkey approvals itself, and in a run such a machine takes a shutdown countdown of at most 10 minutes; its countdown is 11 minutes "
	// pc-a follows the panel's countdown: it is the one machine the Run timings dialog counts
	dialogs("as started", 1, true)
	// the panel's countdown, from config.yaml
	r.s.WakeRuns.Settings.ShutdownDelay, r.s.WakeRuns.Settings.ShutdownDelayS = 660*time.Second, 660
	if got := start(); got != lead+"(the panel's, wake_update.shutdown_delay_s in config.yaml)" {
		t.Errorf("config.yaml's countdown: %q", got)
	}
	r.s.WakeRuns.Settings.ShutdownDelay, r.s.WakeRuns.Settings.ShutdownDelayS = 120*time.Second, 120
	// ... changed in Run timings
	r.rtSet(map[string]int{config.KeyShutdownDelay: 660})
	if got := start(); got != lead+"(the panel's, set in Run timings)" {
		t.Errorf("the countdown of Run timings: %q", got)
	}
	dialogs("a long countdown in Run timings", 1, true)
	// the machine's own, set in Edit host: it does not follow the panel's any more
	r.rtSet(map[string]int{config.KeyShutdownDelay: 0})
	own(660)
	if got := start(); got != lead+"(its own, set in Edit host)" {
		t.Errorf("its own countdown of Edit host: %q", got)
	}
	dialogs("its own countdown", 0, true)
	// ... and from the wake_update: block of its systems: entry
	own(0)
	o := r.s.Poller.Override("pc-a")
	if o == nil {
		t.Fatal("the fixture: pc-a has no systems: entry")
	}
	o.WakeUpdate = &config.SystemWakeUpdate{ShutdownDelayS: 660}
	r.s.Poller.Bump()
	if got := start(); got != lead+"(its own, wake_update.shutdown_delay_s of its systems: entry in config.yaml)" {
		t.Errorf("the countdown of its systems: entry: %q", got)
	}
	dialogs("a block in config.yaml", 0, true)
	if !strings.Contains(lead, spanWords(runMaxDelay)) {
		t.Fatalf("the sentence's ten minutes are not runMaxDelay: %s", spanWords(runMaxDelay))
	}
}

// The README quotes the dialogs' line and the start's four sources as the page and the panel say them.
func TestREADMESaysTheCountdownARunDoesNotTake(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme, js := strings.Join(strings.Fields(string(b)), " "), appJS(t)
	for _, in := range []struct{ readme, js string }{
		{"\"This machine checks passkey approvals itself. A run that wakes it does not start while this countdown is above 10 minutes.\"",
			`return "This machine checks passkey approvals itself. A run that wakes it does not start while this countdown is above " + rtSpan(max) + ".";`},
		{"(\"1 machine checks passkey approvals itself and uses this countdown. A run that wakes it does not start while the countdown is above 10 minutes.\")",
			`(n === 1 ? "1 machine checks passkey approvals itself and uses this countdown. A run that wakes it" : `},
		{"its message names where the number comes from: \"the panel's, set in Run timings\", \"the panel's, wake_update.shutdown_delay_s in config.yaml\", \"its own, set in Edit host\" or \"its own, wake_update.shutdown_delay_s of its systems: entry in config.yaml\".",
			`+ " does not start while the countdown is above " + rtSpan(max) + ".";`},
		{"The refusal names where the number comes from (config.yaml, Run timings, or Edit host), and those two dialogs say it before, under their fields.", ""},
	} {
		if !strings.Contains(readme, in.readme) {
			t.Errorf("the README lacks %q", in.readme)
		}
		if in.js != "" && strings.Count(js, in.js) != 1 {
			t.Errorf("app.js lacks (or has twice) %q", in.js)
		}
	}
}
