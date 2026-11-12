package web

import (
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// r1-p2-02 (audit 5) through the routes. A machine the panel itself sent to sleep gives nothing at all, like
// one that is off, so after 15 minutes the page offered it as "Off", ticked. Woken, it resumes with the
// uptime it had (sleep does not start it again), and the run said "It was already on" and filed that as
// information.
//
// Now the page says "asleep or off" for it (the dialog lists it apart, not ticked), and a run that wakes it
// leaves it on with a sentence that says what is known, as a warning.
func TestWakeUpdateAMachineThatWasAsleepIsReportedAsAWarning(t *testing.T) {
	e := setup(t)
	f := newFarm(t, e)
	// pc-a runs, and an admin sends it to sleep from the panel
	f.on["pc-a"], f.booted["pc-a"], f.ready["pc-a"] = true, f.now.Add(-72*time.Hour), f.now
	for i := 0; i < 5; i++ {
		f.tick()
	}
	at := f.now
	e.s.Poller.NoteSleep("pc-a", &at)
	f.on["pc-a"] = false // asleep: nothing answers
	f.offline()
	f.optIn("pc-a", "pc-b")
	admin := e.cookie(controller(), 0)
	page := e.do("GET", "/api/admin/wake-update", nil, "Cookie", admin)
	if h := wuHostRow(t, page, "pc-a"); h["power"] != "off" || h["choice"] != "wake" || h["asleep"] != true {
		t.Fatalf("pc-a, sent to sleep from the panel: %v", h)
	}
	if h := wuHostRow(t, page, "pc-b"); h["power"] != "off" || h["choice"] != "wake" || h["asleep"] != nil {
		t.Fatalf("pc-b, which is off: %v", h)
	}
	// a run wakes it: it resumes, with the uptime it had
	f.wakes = false
	if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		if a, _ := e.s.WakeRuns.Snapshot(); a == nil {
			break
		}
		if len(f.requests()) > 0 && !f.on["pc-a"] {
			f.on["pc-a"], f.ready["pc-a"] = true, f.now.Add(10*time.Second)
		}
		f.tick()
	}
	run := f.finish()
	h := hostOf(run, "pc-a")
	if h.State != wakerun.StLeftOn || h.On != wakerun.OnYes || !h.NotFresh ||
		h.Text != "It answered after the wake but did not look freshly started (up for 3 days): it may have been asleep, or use Fast Startup. It was left on. It was not updated." {
		t.Fatalf("pc-a: %s (%s): %s", h.State, h.On, h.Text)
	}
	if got := strings.Join(f.requests(), ", "); got != "nas /wol" || !f.on["pc-a"] {
		t.Fatalf("only the wake was sent, and the machine is on: %s (on=%v)", got, f.on["pc-a"])
	}
	warned := false
	for _, n := range e.s.Notes.List("u2", func(notifications.Event) bool { return true }, notifications.Filter{Limit: 200}).Events {
		if n.Kind == "wakeupdate_done" {
			warned = n.Severity == notifications.Warning && strings.Contains(n.Text, "did not look freshly started")
		}
	}
	if !warned {
		t.Fatal("the run's notification must be a warning that says so")
	}
	// the page's own words for it
	js := appJS(t)
	for _, want := range []string{`h.asleep ? "asleep or off" : h.power`, `Asleep or off (sent to sleep from the panel): woken; left on if it was asleep`,
		`asleep.map((h) => tick(h, "wake", false))`, `h.already_on && !h.not_fresh`} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
}

// r1-p2-01 (audit 5): the limit the panel cannot close is said where the feature is explained (the info
// button) and in the README, in one place each; the run dialog's own warning is not made longer for it.
func TestWakeUpdateSaysItsLimit(t *testing.T) {
	js := appJS(t)
	limit := "A limit: a machine that someone switches on just before the wake, and that is still starting, looks like one the run woke. It is switched off after its update."
	if strings.Count(js, limit) != 1 {
		t.Fatalf("the info text must say the limit once: %d", strings.Count(js, limit))
	}
	i := strings.Index(js, `infoBtn("wu", "Update unpowered hosts", infoList([`)
	if i < 0 || !strings.Contains(js[i:i+2500], limit) {
		t.Fatal("the limit must be in the feature's info box")
	}
	dlg := js[strings.Index(js, `async function openWakeUpdate()`):]
	box := dlg[strings.Index(dlg, `<div class="warnbox">Fairy only switches off machines`):]
	box = box[:strings.Index(box, "</div>")]
	if n := len(strings.Fields(box)); n > 78 || strings.Contains(box, "firmware") {
		t.Fatalf("the dialog's fixed warning grew (%d words): %s", n, box)
	}
}
