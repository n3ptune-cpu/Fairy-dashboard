package wakerun

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// The findings of audit 5 (round 1, auditor r1-p2) against this package, each as a run in the test world.

// r1-p2-05: a woken machine whose agent can no longer shut it down (its power control was switched off during
// the run) is left on, with the one sentence for it. The panel sends nothing (internal/web proves that the
// request never goes to the Proxmox API instead); the run does not try again and does not try another way.
func TestAWokenMachineWithoutItsAgentRouteIsLeftOn(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	pc.shutMode = "noroute"
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	r := w.finish()
	h := host(r, "pc-a")
	if h.State != StLeftOn || h.On != OnYes || h.Text != NoAgentRoute+" It was updated to 17-bbbbbbb." {
		t.Fatalf("%s (%s): %s", h.State, h.On, h.Text)
	}
	if got := w.callsFor("pc-a"); got != "wake update shutdown" || pc.shutdowns != 0 || !pc.on {
		t.Fatalf("asked once, nothing sent, the machine is on: %q, shutdowns sent %d, on=%v", got, pc.shutdowns, pc.on)
	}
	if got := w.steps("pc-a"); got != "wake:ok update:ok shutdown:its own agent can no longer take the request" {
		t.Fatalf("the audit steps: %s", got)
	}
}

// breakJournal makes every write of the journal fail from now on, the way a full disk does (the lock file's
// place is taken by a folder); mendJournal undoes it.
func breakJournal(t *testing.T, m *Manager) {
	t.Helper()
	_ = os.Remove(m.path + ".lock")
	if err := os.Mkdir(m.path+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
}

func mendJournal(m *Manager) { _ = os.Remove(m.path + ".lock") }

// restart is the panel started again on the journal as the disk has it: what the report after a restart says.
func (w *world) restart() (*Run, []Event) {
	w.t.Helper()
	m2 := New(w.m.path, w.m.Settings, w)
	m2.Now = w.m.Now
	var got []Event
	m2.Event = func(e Event) { got = append(got, e) }
	if err := m2.Load(); err != nil {
		w.t.Fatal(err)
	}
	_, hist := m2.Snapshot()
	if len(hist) == 0 {
		w.t.Fatal("the journal holds no run")
	}
	return hist[0], got
}

// r1-p2-08, the auditor's proof: the journal cannot be written any more right after the run started. The run
// went on, woke the machine and had it online; the report after the restart, made from the last journal that
// could be written, said "The panel restarted before the run reached it. Nothing was sent to it." about a
// machine that was ON.
//
// Now a run sends nothing it could not write down first. The wake is not sent, the run stops and says why,
// and the report after a restart is true: nothing was sent.
func TestARunThatCannotWriteItsJournalSendsNothingFurther(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	pc.updTakes = 10 * time.Minute
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	breakJournal(t, w.m)
	r := w.finish()
	h := host(r, "pc-a")
	if r.State != RunStopped || !strings.HasPrefix(r.Note, "stopped: its journal (wake-update.json) could not be written: ") {
		t.Fatalf("the run: %s, %q", r.State, r.Note)
	}
	if len(w.calls) != 0 || pc.on || pc.wakes != 0 {
		t.Fatalf("nothing may be sent: %v, the machine is on: %v", w.calls, pc.on)
	}
	if h.State != StSkipped || h.On != OnUntouched || !strings.HasPrefix(h.Text, "the run stopped (its journal (wake-update.json) could not be written: ") ||
		!strings.HasSuffix(h.Text, "). Nothing was sent to it.") {
		t.Fatalf("pc-a: %s (%s): %s", h.State, h.On, h.Text)
	}
	if w.m.JournalWriteError() == nil {
		t.Fatal("the manager must know that its journal cannot be written")
	}
	// no run starts while the journal cannot be written, and nothing of the refused start stays behind
	if _, err := w.m.Start(StartRequest{Hosts: []string{"pc-a"}, By: "Ann"}); err == nil || !strings.Contains(err.Error(), "the run journal cannot be written, so no run is started (nothing was sent)") {
		t.Fatalf("a start with a journal that cannot be written: %v", err)
	}
	if a, _ := w.m.Snapshot(); a != nil || len(w.calls) != 0 {
		t.Fatalf("a refused start left a run behind: %s, %v", describe(a), w.calls)
	}
	// the panel restarts on the journal as it is on the disk: the run from before the failure
	old, evs := w.restart()
	oh := host(old, "pc-a")
	if old.State != RunInterrupted || oh.On != OnUntouched || oh.Text != "The panel restarted before the run reached it. Nothing was sent to it." || pc.wakes != 0 || len(evs) != 1 {
		t.Fatalf("after the restart: %s; pc-a (%s): %s; wakes sent: %d", old.State, oh.On, oh.Text, pc.wakes)
	}
	// the disk has room again: within a few seconds the stopped run is written down, and a run can start
	mendJournal(w.m)
	w.advance(journalRetry + 2*time.Second)
	if err := w.m.JournalWriteError(); err != nil {
		t.Fatal("the journal was not tried again:", err)
	}
	if again, _ := w.restart(); again.State != RunStopped || host(again, "pc-a").Text != h.Text {
		t.Fatalf("the journal after it could be written again: %s", describe(again))
	}
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	if r := w.finish(); host(r, "pc-a").State != StOff {
		t.Fatalf("the next run: %s", describe(r))
	}
}

// The journal fails at any moment of a run, also in the middle of a request. Whatever the moment: no request
// goes out after the first write that failed; the run stops and reports every machine as it is; and the
// report after a restart, made from what is on the disk, never says "nothing was sent" about a machine that
// got a wake, and never calls a machine untouched that the run touched.
func TestTheReportAfterARestartNeverKnowsLessThanWhatWasSent(t *testing.T) {
	check := func(t *testing.T, name string, w *world, pc *machine, sent func() int) {
		t.Helper()
		r := w.finish()
		sentAtFailure := sent()
		h := host(r, "pc-a")
		if r.State != RunStopped && r.State != RunDone {
			t.Fatalf("%s: the run: %s", name, describe(r))
		}
		if r.State == RunStopped && len(w.calls) > sentAtFailure { // a request that was on its way then is counted in
			t.Fatalf("%s: %d requests after the journal failed: %v", name, len(w.calls)-sentAtFailure, w.calls)
		}
		if r.State == RunStopped && pc.wakes > 0 && (h.On == OnUntouched || strings.Contains(h.Text, "Nothing was sent")) {
			t.Fatalf("%s: the stopped run says about a woken machine (%s): %s", name, h.On, h.Text)
		}
		old, _ := w.restart()
		oh := host(old, "pc-a")
		if pc.wakes > 0 && (oh.On == OnUntouched || strings.Contains(oh.Text, "Nothing was sent")) {
			t.Fatalf("%s: after the restart, about a machine that got %d wake(s) (%s): %s", name, pc.wakes, oh.On, oh.Text)
		}
		if pc.shutdowns > 0 && !pc.wentOff && pc.pending != nil && oh.On == OnYes && !strings.Contains(oh.Text, "shut") {
			t.Fatalf("%s: after the restart, about a machine with a countdown running (%s): %s", name, oh.On, oh.Text)
		}
		if oh.Asking != "" || oh.Text == "" || oh.On == "" {
			t.Fatalf("%s: the report: %+v", name, oh)
		}
	}
	// at every seventh second of a run that takes about five minutes
	for at := 0; at < 330; at += 7 {
		w := newWorld(t)
		pc := w.off("pc-a")
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		w.advance(time.Duration(at) * time.Second)
		if a, _ := w.m.Snapshot(); a == nil {
			break // the run is over
		}
		breakJournal(t, w.m)
		n := len(w.calls)
		check(t, fmt.Sprintf("after %d s", at), w, pc, func() int { return n })
	}
	// in the middle of each request: it was written down as going out, it arrives, and its result cannot be written
	for _, call := range []string{"wake pc-a", "update pc-a", "shutdown pc-a"} {
		w := newWorld(t)
		pc := w.off("pc-a")
		sent := -1
		w.during = func(c string) {
			if c == call && sent < 0 {
				breakJournal(t, w.m)
				sent = len(w.calls)
			}
		}
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		check(t, "during "+call, w, pc, func() int { return sent })
		if sent < 0 {
			t.Fatalf("the run never sent %q", call)
		}
		old, _ := w.restart()
		oh := host(old, "pc-a")
		want := map[string]string{
			"wake pc-a":     "The panel restarted while the run was sending a wake to it. Fairy does not know whether the wake went out: if it switched on, it is ON.",
			"update pc-a":   " An update request was going out to it; if it arrived, the agent finishes the update by itself.",
			"shutdown pc-a": "The panel restarted while the run was asking it to shut down. The run had woken it. Fairy does not know whether the request arrived: ",
		}[call]
		if !strings.Contains(oh.Text, want) || (call != "update pc-a" && oh.On != OnUnknown) {
			t.Fatalf("during %s, after the restart (%s): %s", call, oh.On, oh.Text)
		}
	}
}

// Every request of a run is in the journal, as "going out", before it leaves; between requests nothing is.
func TestEveryRequestIsWrittenDownBeforeItGoesOut(t *testing.T) {
	w := newWorld(t)
	w.off("pc-a")
	var seen []string
	w.during = func(call string) {
		jf, err := readJournal(w.m.path)
		if err != nil || jf.Active == nil {
			t.Fatalf("%s: the journal on the disk: %v", call, err)
		}
		seen = append(seen, call+" / "+jf.Active.Hosts[0].Asking)
	}
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	r := w.finish()
	if got := strings.Join(seen, ", "); got != "wake pc-a / wake, update pc-a / update, shutdown pc-a / shutdown" {
		t.Fatalf("what the journal said when each request arrived: %s", got)
	}
	if h := host(r, "pc-a"); h.State != StOff || h.Asking != "" {
		t.Fatalf("%s, asking %q", h.State, h.Asking)
	}
	if jf, _ := readJournal(w.m.path); jf.Active != nil || len(jf.History) != 1 || jf.History[0].Hosts[0].Asking != "" {
		t.Fatal("the finished run in the journal still has a request going out")
	}
}

// r1-p2-02, the auditor's proof: a machine whose uptime does not start again when it is woken (it slept or
// hibernated, or it is a Windows PC that was shut down with Fast Startup). It is off or asleep, silent for a
// day; the run wakes it; its first answer reports three days of uptime. The run said "It was already on",
// left the machine it had woken on, and filed that as information ("done · stays on").
//
// Leaving it on is right: the run cannot tell it from a machine that was on all along. Saying "it was
// already on" is not. Now it ends as left on, with a sentence that says what is known, and that is a warning.
func TestAWokenMachineThatDoesNotLookFreshlyStartedIsAWarning(t *testing.T) {
	for _, updateIfOn := range []bool{false, true} {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.wakeBoots = false // the world's own boot sets a fresh uptime; this one resumes
		w.start(StartRequest{Hosts: []string{"pc-a"}, UpdateIfOn: updateIfOn})
		for i := 0; i < 3600; i++ {
			if a, _ := w.m.Snapshot(); a == nil {
				break
			}
			if pc.wakes == 1 && !pc.on { // the run's wake packet resumes it; the system "has been up" for three days
				pc.on, pc.bootedAt, pc.agentReadyAt = true, w.now.Add(-72*time.Hour), w.now.Add(30*time.Second)
			}
			w.tick()
		}
		r := w.finish()
		h := host(r, "pc-a")
		if pc.onAtStart || pc.onAtFirstWake == nil || *pc.onAtFirstWake {
			t.Fatal("setup: the machine was not off before the run")
		}
		if !pc.on || pc.shutdowns != 0 || !h.AlreadyOn || !h.NotFresh {
			t.Fatalf("update_if_on=%v: it must be left on: on=%v shutdowns=%d %+v", updateIfOn, pc.on, pc.shutdowns, h)
		}
		want := "It answered after the wake but did not look freshly started (up for 3 days): it may have been asleep, or use Fast Startup. It was left on."
		tail, calls := " It was not updated.", "wake"
		if updateIfOn {
			tail, calls = " It was updated to 17-bbbbbbb.", "wake update"
		}
		if h.State != StLeftOn || h.On != OnYes || h.Text != want+tail || strings.Contains(h.Text, "already on") || w.callsFor("pc-a") != calls {
			t.Fatalf("update_if_on=%v: %s (%s): %s; requests %q", updateIfOn, h.State, h.On, h.Text, w.callsFor("pc-a"))
		}
		// what the panel makes of it (web.wakeRunEvent): a machine that was left on is a warning
		if ok, why := mayShutDown(h, pc.info, pc.obs, func() (int, bool, string) { return 0, true, "" }); ok || why != "it answered after the wake without looking freshly started" {
			t.Fatalf("mayShutDown: %v %q", ok, why)
		}
	}
	// cancelled, stopped and after a restart: the same sentence, never "it was already on"
	for _, how := range []string{"cancel", "cancel and switch off", "stop", "restart"} {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.wakeBoots, pc.updTakes = false, 10*time.Minute
		run := w.start(StartRequest{Hosts: []string{"pc-a"}, UpdateIfOn: true})
		for i := 0; i < 120; i++ {
			if pc.wakes == 1 && !pc.on {
				pc.on, pc.bootedAt, pc.agentReadyAt = true, w.now.Add(-72*time.Hour), w.now.Add(30*time.Second)
			}
			w.tick()
		}
		var h *Host
		switch how {
		case "cancel", "cancel and switch off":
			if _, err := w.m.CancelRun(run.ID, "Ann", how != "cancel"); err != nil {
				t.Fatal(err)
			}
			h = host(w.finish(), "pc-a")
		case "stop":
			w.blocked = "control is disabled by an administrator"
			h = host(w.finish(), "pc-a")
		case "restart":
			old, _ := w.restart()
			h = host(old, "pc-a")
		}
		wantState := map[string]string{"restart": StInterrupted}[how]
		if wantState == "" {
			wantState = StLeftOn
		}
		if h.State != wantState || h.On != OnYes || !strings.Contains(h.Text, "did not look freshly started (up for 3 days)") || strings.Contains(h.Text, "already on") || pc.shutdowns != 0 || !pc.on {
			t.Fatalf("%s: %s (%s): %s (shutdowns %d)", how, h.State, h.On, h.Text, pc.shutdowns)
		}
	}
}

// r1-p2-07, the auditor's proof: an agent that takes the shutdown and reports it as pending for the next day
// kept the run going for 1 h 6 min 31 s, until the run's own limit; no other run could start and "Update
// panel" was refused all that time, and the sentence gave the end as a time without a date.
//
// Now the run watches the countdown it asked for, plus three minutes. A later one is reported, with its date,
// and the run lets go of the machine.
func TestAnAgentCannotHoldTheRunWithAFarCountdown(t *testing.T) {
	for _, how := range []string{"in its answer to the request", "in a later poll"} {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.shutMode = "ignore"
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		start := w.now
		far := start.Add(24 * time.Hour)
		var asked time.Time
		for i := 0; i < 6*3600; i++ {
			if a, _ := w.m.Snapshot(); a == nil {
				break
			}
			if pc.pending != nil && asked.IsZero() {
				asked = w.now
			}
			if pc.pending != nil && (how == "in a later poll" && w.now.Sub(asked) >= 30*time.Second) {
				pc.pending.At = far // what the agent says from now on
			}
			w.tick()
			if how == "in its answer to the request" && pc.pending != nil {
				pc.pending.At = far
			}
		}
		r := w.finish()
		h := host(r, "pc-a")
		took := r.EndedAt.Sub(asked)
		if h.State != StLeftOn || h.On != OnYes || !pc.on || r.State != RunDone {
			t.Fatalf("%s: %s / %s (%s): %s", how, r.State, h.State, h.On, h.Text)
		}
		want := "Its agent says its shutdown is due at " + far.Format("2006-01-02 15:04:05") + " UTC, later than the 2 min the run asked for. Fairy no longer watches it: " +
			"it is ON until then, and switches off then unless someone cancels that on its page. It was updated to 17-bbbbbbb."
		if h.Text != want {
			t.Fatalf("%s: %s", how, h.Text)
		}
		if limit := w.m.Settings.ShutdownDelay + countdownSlack + 10*time.Second; took > limit {
			t.Fatalf("%s: the run held on for %v after the shutdown was asked for, want at most %v", how, took, limit)
		}
		if a, _ := w.m.Snapshot(); a != nil {
			t.Fatal("the run slot is still taken")
		}
	}
	// what an agent may do with the time: Linux rounds the countdown up to the next whole minute
	w := newWorld(t)
	pc := w.off("pc-a")
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	rounded := false
	for i := 0; i < 3600; i++ {
		if a, _ := w.m.Snapshot(); a == nil {
			break
		}
		if pc.pending != nil && !rounded {
			pc.pending.At, rounded = pc.pending.At.Truncate(time.Minute).Add(2*time.Minute), true
		}
		w.tick()
	}
	if h := host(w.finish(), "pc-a"); h.State != StOff || !rounded {
		t.Fatalf("a countdown rounded up by the agent: %s: %s", h.State, h.Text)
	}
	if when(t0, t0) != "03:00:00 UTC" || when(t0.Add(24*time.Hour), t0) != "2026-11-12 03:00:00 UTC" {
		t.Fatal("when:", when(t0, t0), when(t0.Add(24*time.Hour), t0))
	}
}
