package wakerun

import (
	"os"
	"strings"
	"testing"
	"time"
)

// armedRun starts a run with one machine with the check and lets it go until the machine waits, armed.
func armedRun(t *testing.T, set func(w *world, pc *machine)) (*world, *machine, *Run) {
	t.Helper()
	w := newWorld(t)
	pc := w.checkedOff("pc-a")
	if set != nil {
		set(w, pc)
	}
	w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
	act := w.waitFor(8*time.Minute, "it waits, armed", func(r *Run) bool { return stateOf(r, "pc-a") == StApproval && host(r, "pc-a").Arm == ArmArmed })
	return w, pc, act
}

// R5: "Cancel run" with the machines left on calls the self-switch-off of an armed machine off with the
// agent's cancel, and then goes by what the machine says: only "cancelled" (or no run at all) means that it
// stays on. A cancel that got an error or no answer is not a cancel: it is sent again, and when the machine's
// word never comes the sentence says when it may go off.
func TestCancelLeaveOnCallsTheSelfSwitchOffOffAndReadsBack(t *testing.T) {
	for name, c := range map[string]struct {
		set    func(w *world, pc *machine)
		parts  []string
		stays  bool
		cancel int // at least this many cancels
	}{
		"the cancel is answered": {nil, []string{"The run was cancelled by Bob: its switch-off was called off (its agent shows the run as cancelled). It came online after the wake. Not confirmed, not updated. It is ON."}, true, 1},
		"the agent answers 500 every time": {func(_ *world, pc *machine) {
			pc.chk.cancelSays = "the pending action is stopped, but the end of the run could not be written down"
		},
			[]string{"The run was cancelled by Bob, but its switch-off could not be called off (the pending action is stopped, but the end of the run could not be written down): it may have switched itself off (its time was about ",
				"Keep on for it in the panel, or cancel at the machine. Not confirmed, not updated.", "It does not answer now: not confirmed that it is on."}, false, 20},
		"the first cancel gets no answer and arrived": {func(_ *world, pc *machine) { pc.cancelLost = "arrives" },
			[]string{"its switch-off was called off (its agent shows the run as cancelled)", "It is ON."}, true, 2},
		"the first cancel gets no answer and did not arrive": {func(_ *world, pc *machine) { pc.cancelLost = "never" },
			[]string{"its switch-off was called off (its agent shows the run as cancelled)", "It is ON."}, true, 2},
		"no cancel ever arrives": {func(_ *world, pc *machine) { pc.cancelLost = "always" },
			[]string{"The run was cancelled by Bob, and the request to call off its switch-off got no answer (agent unreachable: timeout): it may have arrived, and then the machine stays ON; if it did not, it may have switched itself off (its time was about "}, false, 20},
		"its checker says nothing about runs afterwards": {func(w *world, pc *machine) {
			w.during = func(call string) {
				if call == "cancel pc-a" {
					pc.chk.noRuns = true
				}
			}
		}, []string{"its switch-off was called off (its agent shows no run any more)", "It is ON."}, true, 1},
	} {
		t.Run(name, func(t *testing.T) {
			w, pc, act := armedRun(t, c.set)
			if _, err := w.m.CancelRun(act.ID, "Bob", false); err != nil {
				t.Fatal(err)
			}
			done := w.finish()
			h := host(done, "pc-a")
			has(t, h, StLeftOn, c.parts...)
			n := strings.Count(w.callsFor("pc-a"), "cancel")
			if n < c.cancel || strings.Contains(w.callsFor("pc-a"), "update") || strings.Contains(w.callsFor("pc-a"), "shutdown") || strings.Contains(w.callsFor("pc-a"), "go") {
				t.Fatalf("requests: %q", w.callsFor("pc-a"))
			}
			w.advance(30 * time.Minute)
			if pc.on != c.stays {
				t.Fatalf("thirty minutes later it is on=%v, the sentence was: %s", pc.on, h.Text)
			}
		})
	}
}

// "Cancel run" with "switch off again": the panel cannot switch a machine with the check off without its
// confirmation, so an armed machine switches itself off as its ticket says, and one that is not armed is ON.
func TestCancelSwitchOffAgainLeavesItToTheMachine(t *testing.T) {
	w, pc, act := armedRun(t, nil)
	if _, err := w.m.CancelRun(act.ID, "Bob", true); err != nil {
		t.Fatal(err)
	}
	if _, err := w.confirm("Ann"); err == nil || !strings.Contains(err.Error(), "this run was cancelled") {
		t.Fatalf("a confirmation after the cancel: %v", err)
	}
	done := w.finish()
	has(t, host(done, "pc-a"), StOff, "It came online after the wake. The run was cancelled by Bob: not confirmed, not updated. It switched itself off again.")
	if got := w.callsFor("pc-a"); got != "wake arm" || pc.on {
		t.Fatalf("requests: %q", got)
	}

	w = newWorld(t)
	pc = w.checkedOff("pc-a")
	pc.chk.armSays = "not_fresh"
	w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
	act = w.waitFor(8*time.Minute, "it waits", func(r *Run) bool { return stateOf(r, "pc-a") == StApproval })
	if _, err := w.m.CancelRun(act.ID, "Bob", true); err != nil {
		t.Fatal(err)
	}
	done = w.finish()
	has(t, host(done, "pc-a"), StLeftOn, "The run was cancelled by Bob with \"switch off again\". It came online after the wake. It could not be armed (run refused (not_fresh)). "+
		"Without your second confirmation Fairy cannot switch off a machine that checks approvals itself. Not confirmed, not updated. It is ON.")
	if got := w.callsFor("pc-a"); got != "wake arm" || !pc.on {
		t.Fatalf("requests: %q", got)
	}
}

// The kill switch stops the run, and the one request it still sends is the cancel: an armed machine is asked
// to stay on. A run whose journal cannot be written sends nothing at all, and says when the machine goes.
func TestAStoppedRunAndAnArmedMachine(t *testing.T) {
	w, pc, _ := armedRun(t, nil)
	w.blocked = "control is disabled by an administrator"
	done := w.finish()
	has(t, host(done, "pc-a"), StLeftOn, "the run stopped (control is disabled by an administrator): its switch-off was called off (its agent shows the run as cancelled).", "It is ON.")
	if got := w.callsFor("pc-a"); got != "wake arm cancel" || !pc.on || done.Approval != "over" {
		t.Fatalf("requests: %q approval %q", got, done.Approval)
	}

	w, pc, act := armedRun(t, nil)
	if err := os.Mkdir(w.m.path+".lock", 0o700); err != nil { // the journal cannot be written from here on
		t.Skip(err)
	}
	off := *host(act, "pc-a").SelfOffAt
	w.m.mu.Lock()
	w.m.dirty = true
	w.m.mu.Unlock()
	done = w.finish()
	has(t, host(done, "pc-a"), StLeftOn, "its journal (wake-update.json) could not be written", "It came online after the wake and is armed: it begins to switch itself off at about "+clock(off)+
		", with a notice of 2 min, unless that is cancelled (Keep on in the panel, or at the machine). Fairy no longer watches it. Not confirmed, not updated.")
	if got := w.callsFor("pc-a"); got != "wake arm" {
		t.Fatalf("a run without its journal sent %q", got)
	}
	w.advance(30 * time.Minute)
	if pc.on {
		t.Fatal("the armed machine did not switch itself off")
	}
}

// A guest that starts on an armed Proxmox node keeps it on: the run calls the node's switch-off off, while it
// waits and while the notice counts.
func TestAGuestOnAnArmedNodeKeepsItOn(t *testing.T) {
	for name, at := range map[string]time.Duration{"while it waits": 3 * time.Minute, "while the notice counts": 16*time.Minute + 30*time.Second} {
		t.Run(name, func(t *testing.T) {
			w, pc, _ := armedRun(t, nil)
			w.advance(at)
			pc.guests, pc.guestNames = 1, "vm-1"
			done := w.finish()
			has(t, host(done, "pc-a"), StLeftOn, "Kept ON: 1 guest is running on it (vm-1)", "its switch-off was called off (its agent shows the run as cancelled)", "It is ON.")
			w.advance(30 * time.Minute)
			if !pc.on {
				t.Fatal("the node went off under its guest")
			}
		})
	}
}

// R6: a stopped helper during a run is a warning in the machine's row and in its history: only the helper
// switches the PC off. A wait that ends while it is stopped ends with the PC ON, and the result says that.
func TestAStoppedHelperIsAWarning(t *testing.T) {
	w, pc, _ := armedRun(t, nil)
	w.advance(time.Minute)
	pc.chk.helperStopped = true
	w.advance(10 * time.Second)
	act, _ := w.m.Snapshot()
	h := host(act, "pc-a")
	if !strings.HasSuffix(h.Text, " · warning: its helper is stopped, and only the helper switches it off") || !strings.Contains(logOf(h), helperStoppedNote) {
		t.Fatalf("the row: %q\n%s", h.Text, logOf(h))
	}
	done := w.finish()
	has(t, host(done, "pc-a"), StLeftOn, "Its agent reports that its switch-off was called off (a cancel in the panel or at the machine, or the machine itself did not go through with it). Not confirmed, not updated. It is ON.")
	if !pc.on || strings.Contains(w.callsFor("pc-a"), "cancel") {
		t.Fatalf("on=%v requests %q", pc.on, w.callsFor("pc-a"))
	}
}

// A machine that is started again while it waits has no run any more (its record's live part is of the boot it
// was written in): it does not switch itself off, the row says so after two answers, and the result says ON.
func TestAMachineThatIsStartedAgainWhileItWaitsIsNotArmedAnyMore(t *testing.T) {
	w, pc, _ := armedRun(t, nil)
	w.advance(time.Minute)
	pc.bootedAt, pc.agentReadyAt = w.now, w.now.Add(20*time.Second) // a restart: a new boot
	act := w.waitFor(2*time.Minute, "the run sees that", func(r *Run) bool { return strings.Contains(logOf(host(r, "pc-a")), "not armed any more") })
	w.tick()
	act, _ = w.m.Snapshot()
	if h := host(act, "pc-a"); h.Text != "awake; it is not armed any more (its agent no longer shows the run): it stays ON unless you confirm" {
		t.Fatalf("the row: %q", h.Text)
	}
	if ws, err := w.m.Waiting(act.ID); err != nil || len(ws) != 1 || ws[0].Armed || ws[0].SelfOffAt != nil {
		t.Fatalf("what can be confirmed: %+v %v", ws, err)
	}
	done := w.finish()
	has(t, host(done, "pc-a"), StLeftOn, "Nobody confirmed within 10 min: not confirmed, not updated. It was armed, and its agent no longer shows the run (the machine was started again, or the run was ended on it): it is not armed any more. It is ON")
	w.advance(30 * time.Minute)
	if !pc.on || w.callsFor("pc-a") != "wake arm" {
		t.Fatalf("on=%v requests %q", pc.on, w.callsFor("pc-a"))
	}
}

// One answer in which the checker says nothing about runs (the helper did not answer the agent for a poll) is
// not "no run": the armed machine is still armed, and still reported as that.
func TestOneAnswerWithoutRunsChangesNothing(t *testing.T) {
	w, pc, _ := armedRun(t, nil)
	pc.chk.noRuns = true
	w.advance(30 * time.Second)
	pc.chk.noRuns = false
	done := w.finish()
	has(t, host(done, "pc-a"), StOff, "It switched itself off again.")
	if strings.Contains(logOf(host(done, "pc-a")), "not armed any more") {
		t.Fatal("answers that say nothing about runs were read as \"no run\"")
	}
}

// The panel restarts during a run: tickets are gone, the run is never continued, and the report says for each
// machine with the check what is true of it: armed (when it goes, and how to keep it on), arming under way
// (not known), not armed (ON), the confirmation under way (not known).
func TestAPanelRestartWithMachinesThatCheckApprovals(t *testing.T) {
	w := newWorld(t)
	w.checkedOff("pc-armed")
	w.checkedOff("pc-refused").chk.armSays = "not_fresh"
	w.checkedOff("pc-lost").chk.armLost = "never"
	slow := w.checkedOff("pc-slow")
	slow.bootTakes = 200 * time.Second
	w.start(StartRequest{Hosts: []string{"pc-armed", "pc-refused", "pc-lost", "pc-slow"}, Latest: "17-bbbbbbb"})
	act := w.waitFor(8*time.Minute, "three wait", func(r *Run) bool {
		return stateOf(r, "pc-armed") == StApproval && stateOf(r, "pc-refused") == StApproval && stateOf(r, "pc-lost") == StApproval
	})
	off := *host(act, "pc-armed").SelfOffAt
	w.tick() // the journal is written at the end of a step
	r, evs := w.restart()
	if r.State != RunInterrupted || len(evs) != 1 || evs[0].Kind != EvInterrupted {
		t.Fatalf("after the restart: %s", describe(r))
	}
	has(t, host(r, "pc-armed"), StInterrupted, "The panel restarted while it waited for the run's second confirmation. It was armed: it switches itself off at about "+clock(off)+
		", with a notice of 2 min, unless that is cancelled (Keep on in the panel, or at the machine). A run is never continued, so it can no longer be confirmed. Not confirmed, not updated.")
	has(t, host(r, "pc-refused"), StInterrupted, "The panel restarted while it waited for the run's second confirmation. It could not be armed (run refused (not_fresh)). It was ON: nothing of this run switches it off. Not confirmed, not updated.")
	has(t, host(r, "pc-lost"), StInterrupted, "The panel restarted while the run was arming it, or after its arming got no answer. Fairy does not know whether it is armed: if it is, it switches itself off by about ",
		"If not, it is ON and stays on. Not confirmed, not updated.")
	if host(r, "pc-armed").On != OnUnknown || host(r, "pc-refused").On != OnYes || host(r, "pc-lost").On != OnUnknown {
		t.Fatalf("power: %s %s %s", host(r, "pc-armed").On, host(r, "pc-refused").On, host(r, "pc-lost").On)
	}
	for _, id := range []string{"pc-armed", "pc-refused", "pc-lost", "pc-slow"} {
		if !strings.Contains(r.Summary, host(r, id).Name+": "+host(r, id).Text) {
			t.Errorf("the summary lacks %s", id)
		}
	}
}

// The journal knows of a ticket and of a confirmation before the request leaves: a restart in that very moment
// is reported as "not known", never as "nothing was sent".
func TestARestartWhileATicketOrAConfirmationGoesOut(t *testing.T) {
	w := newWorld(t)
	w.checkedOff("pc-a")
	var during *Run
	w.during = func(call string) {
		if call == "arm pc-a" {
			during, _ = w.restart()
		}
	}
	w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
	w.waitFor(8*time.Minute, "asked", func(r *Run) bool { return r.Approval == "needed" })
	if during == nil {
		t.Fatal("no ticket went out")
	}
	has(t, host(during, "pc-a"), StInterrupted, "The panel restarted while the run was arming it, or after its arming got no answer. Fairy does not know whether it is armed")
	// the confirmation: BeginConfirm writes the journal before anything is sent
	ws, _ := w.m.Waiting(w.runID())
	if _, err := w.m.BeginConfirm(w.runID(), "Ann", []string{ws[0].ID}); err != nil {
		t.Fatal(err)
	}
	r, _ := w.restart()
	has(t, host(r, "pc-a"), StInterrupted, "The panel restarted while your confirmation was being handed to it. Fairy does not know whether it arrived: if it did, the machine is confirmed and stays ON "+
		"(a run is never continued, so nothing more is sent to it); if not, it switches itself off at about ")
	if r.Approval != "needed" || r.Trying != "Ann" || r.ConfirmedBy != "" { // confirmed is a run a machine has accepted for
		t.Fatalf("the journal: approval %q, on its way by %q, confirmed by %q", r.Approval, r.Trying, r.ConfirmedBy)
	}
	// and the run that was not restarted: nobody says what came of the confirmation, so its agent's word decides
	done := w.finish()
	has(t, host(done, "pc-a"), StOff, "Its confirmation got no answer (the panel did not learn what came of it), and its agent did not show the run as confirmed: not confirmed, not updated. It switched itself off again.")
}

// R11: in a run with both kinds of machines, the ones without the check go their way as they always did: they
// do not wait for the confirmation, their requests carry nothing new, and their sentences are the same.
func TestAMixedRunLeavesMachinesWithoutTheCheckAsTheyWere(t *testing.T) {
	plain := newWorld(t)
	plain.off("pc-b")
	plain.start(StartRequest{Hosts: []string{"pc-b"}, Latest: "17-bbbbbbb"})
	want := plain.finish()

	w := newWorld(t)
	w.checkedOff("pc-a")
	b := w.off("pc-b")
	w.start(StartRequest{Hosts: []string{"pc-b", "pc-a"}, Latest: "17-bbbbbbb"})
	act := w.waitFor(8*time.Minute, "asked", func(r *Run) bool { return r.Approval == "needed" })
	if got := stateOf(act, "pc-b"); got == StApproval || host(act, "pc-b").Checked {
		t.Fatalf("pc-b waits for a confirmation: %s", got)
	}
	done := w.finish() // nobody confirms: pc-b is done long before
	hb, wb := host(done, "pc-b"), host(want, "pc-b")
	if hb.State != wb.State || hb.Text != wb.Text || hb.On != wb.On || hb.Update != wb.Update || b.on {
		t.Fatalf("pc-b in a mixed run: [%s] %s\nalone: [%s] %s", hb.State, hb.Text, wb.State, wb.Text)
	}
	if got, alone := w.callsFor("pc-b"), plain.callsFor("pc-b"); got != alone || got != "wake update shutdown" {
		t.Fatalf("pc-b's requests: %q, alone %q", got, alone)
	}
	if got, alone := w.steps("pc-b"), plain.steps("pc-b"); got != alone {
		t.Fatalf("pc-b's audit steps: %q, alone %q", got, alone)
	}
	if len(hb.Log) != len(wb.Log) {
		t.Fatalf("pc-b's history has %d lines, alone %d", len(hb.Log), len(wb.Log))
	}
	for i := range hb.Log {
		if hb.Log[i].Text != wb.Log[i].Text {
			t.Errorf("pc-b's history line %d: %q, alone %q", i, hb.Log[i].Text, wb.Log[i].Text)
		}
	}
	has(t, host(done, "pc-a"), StOff, "It switched itself off again.")
}

// A machine with the check that was chosen as "on": the start's passkey answered its own challenge, and its one
// update goes out on that. Without it (and for a machine of the wake list that turns out to be on) nothing is
// sent at all.
func TestMachinesWithTheCheckThatAreOn(t *testing.T) {
	w := newWorld(t)
	a := w.running("pc-a")
	a.chk.on, a.chk.startApproved, a.chk.used = true, true, map[string]bool{}
	b := w.running("pc-b")
	b.chk.on, b.chk.used = true, map[string]bool{}
	c := w.running("pc-c")
	c.chk.on, c.chk.used = true, map[string]bool{}
	w.silentFor(c, time.Hour) // the panel takes it for off; it is on
	c.unreachable = true
	w.poll()
	r := w.start(StartRequest{Hosts: []string{"pc-c"}, UpdateOnly: []string{"pc-a", "pc-b"}, UpdateIfOn: true, Latest: "17-bbbbbbb"})
	if !host(r, "pc-a").StartApproved || host(r, "pc-b").StartApproved || r.Approval != "" {
		t.Fatalf("the start: %+v", r)
	}
	c.unreachable = false
	done := w.finish()
	has(t, host(done, "pc-a"), StStayedOn, "It was on and stays on. It was updated to 17-bbbbbbb.")
	has(t, host(done, "pc-b"), StStayedOn, "It was on and stays on. Its update did not work: it checks passkey approvals itself, and this run has no approval for it.")
	has(t, host(done, "pc-c"), StStayedOn, "Its update did not work: it checks passkey approvals itself, and the run's confirmations are for machines the run wakes: update it on its own page.")
	if w.callsFor("pc-a") != "update" || w.callsFor("pc-b") != "" || strings.Contains(w.callsFor("pc-c"), "update") || strings.Contains(w.callsFor("pc-c"), "arm") {
		t.Fatalf("requests: a %q b %q c %q", w.callsFor("pc-a"), w.callsFor("pc-b"), w.callsFor("pc-c"))
	}
	if done.Approval != "" || !a.on || !b.on || !c.on {
		t.Fatalf("nobody was asked for a second confirmation: %q", done.Approval)
	}
}

// Every wait of a machine with the check is counted on the steady clock: the panel's time of day can be set
// while a machine waits (a time server's correction after a restart), and that neither ends the wait, nor
// takes the confirmation away, nor keeps the run from seeing the machine go.
func TestThePanelsClockIsSetWhileAMachineWaits(t *testing.T) {
	for name, jump := range map[string]time.Duration{"an hour forward": time.Hour, "an hour back": -time.Hour, "ten minutes back": -10 * time.Minute} {
		for _, confirm := range []bool{false, true} {
			t.Run(name, func(t *testing.T) {
				w, ahead := jumpWorld(t)
				pc := w.checkedOff("pc-a")
				w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
				w.waitFor(8*time.Minute, "it waits, armed", func(r *Run) bool { return r.Approval == "needed" && host(r, "pc-a").Arm == ArmArmed })
				w.advance(2 * time.Minute)
				*ahead = jump
				w.advance(time.Minute)
				if act, _ := w.m.Snapshot(); act == nil || stateOf(act, "pc-a") != StApproval || act.Approval != "needed" {
					t.Fatalf("a minute after the clock was set: %s", describe(act))
				}
				if ws, err := w.m.Waiting(w.runID()); err != nil || len(ws) != 1 {
					t.Fatalf("it can still be confirmed: %v", err)
				}
				if confirm {
					if _, err := w.confirm("Ann"); err != nil {
						t.Fatal(err)
					}
					done := w.finish()
					has(t, host(done, "pc-a"), StOff, "updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.")
					return
				}
				began := w.mono
				done := w.finish()
				has(t, host(done, "pc-a"), StOff, "not confirmed, not updated. It switched itself off again.")
				// armed about a minute into the run, 16 minutes of wait, 2 of notice, and the run's own proof of "off"
				if took := w.mono - began; pc.on || took < 12*time.Minute || took > 20*time.Minute {
					t.Fatalf("on=%v, and the run ended %s after the clock was set", pc.on, took)
				}
			})
		}
	}
}

// A machine that leaves the run while it is armed: switched off for runs (the cancel goes out), paired again
// (no cancel until the agent that has the id now shows this run), taken out of the panel (nothing can be sent:
// the sentence says when it goes).
func TestAnArmedMachineThatTheRunLetsGoOf(t *testing.T) {
	t.Run("switched off for runs", func(t *testing.T) {
		w, pc, _ := armedRun(t, nil)
		pc.info.OptedIn = false
		done := w.finish()
		has(t, host(done, "pc-a"), StLeftOn, "It was switched off for \"Update unpowered hosts\" during the run, so the run let go of it: its switch-off was called off (its agent shows the run as cancelled).", "It is ON.")
		if !pc.on {
			t.Fatal("it went off")
		}
	})
	t.Run("paired again, the same machine", func(t *testing.T) {
		w, pc, _ := armedRun(t, nil)
		pc.info.Entry++
		done := w.finish()
		has(t, host(done, "pc-a"), StLeftOn, "It was paired again during the run (its entry in the panel was replaced), so the run let go of it: its switch-off was called off (its agent shows the run as cancelled).")
		if !pc.on || strings.Count(w.callsFor("pc-a"), "cancel") != 1 {
			t.Fatalf("on=%v requests %q", pc.on, w.callsFor("pc-a"))
		}
	})
	t.Run("paired again, another machine under the id", func(t *testing.T) {
		w, pc, act := armedRun(t, nil)
		off := *host(act, "pc-a").SelfOffAt
		pc.info.Entry++
		pc.chk.noRuns = true // what answers under the id now shows no run: it is not the machine the run armed
		done := w.finish()
		has(t, host(done, "pc-a"), StLeftOn, "its entry in the panel was replaced, and the agent that has this id now never showed the run, so no cancel was sent to it: it may", clock(off))
		if strings.Contains(w.callsFor("pc-a"), "cancel") {
			t.Fatalf("a cancel went to a machine that never showed the run: %q", w.callsFor("pc-a"))
		}
	})
	t.Run("taken out of the panel", func(t *testing.T) {
		w, _, act := armedRun(t, nil)
		off := *host(act, "pc-a").SelfOffAt
		mc := w.ms["pc-a"]
		delete(w.ms, "pc-a")
		w.order = nil
		var done *Run
		for i := 0; i < 600 && done == nil; i++ {
			w.tick()
			if a, hist := w.m.Snapshot(); a == nil {
				done = hist[0]
			}
		}
		has(t, host(done, "pc-a"), StLeftOn, "It is no longer in the panel, so the run let go of it. It came online after the wake and is armed: it begins to switch itself off at about "+clock(off)+
			", with a notice of 2 min, unless that is cancelled")
		if mc.chk.arms != 1 {
			t.Fatal("arms")
		}
	})
}

// The start refuses what no confirmation could name, before anything is sent: a countdown longer than a
// machine with the check takes, a ticket whose numbers no machine takes, a run id that is none.
func TestStartWithMachinesThatCheckApprovals(t *testing.T) {
	w := newWorld(t)
	pc := w.checkedOff("pc-a")
	pc.info.ShutdownDelay = 20 * time.Minute
	req := StartRequest{Hosts: []string{"pc-a"}, By: "Ann", Checked: map[string]bool{"pc-a": true}, Ticket: &Ticket{ValidS: 420, WaitS: 960, NoticeS: 120, WithinS: 1560, ConfirmWaitS: 600}}
	if _, err := w.m.Start(req); err == nil || !strings.Contains(err.Error(), "PC-A checks approvals itself, and such a machine takes a shutdown countdown of at most 10 min in a run; its countdown is 20 min") {
		t.Fatalf("a countdown of 20 min: %v", err)
	}
	pc.info.ShutdownDelay = 0
	for name, change := range map[string]func(r *StartRequest){
		"no ticket":      func(r *StartRequest) { r.Ticket = nil },
		"valid_s 601":    func(r *StartRequest) { r.Ticket.ValidS = 601 },
		"valid_s 119":    func(r *StartRequest) { r.Ticket.ValidS = 119 },
		"wait_s 3601":    func(r *StartRequest) { r.Ticket.WaitS = 3601 },
		"notice_s 601":   func(r *StartRequest) { r.Ticket.NoticeS = 601 },
		"notice_s 29":    func(r *StartRequest) { r.Ticket.NoticeS = 29 },
		"within_s 1801":  func(r *StartRequest) { r.Ticket.WithinS = 1801 },
		"within_s 299":   func(r *StartRequest) { r.Ticket.WithinS = 299 },
		"an id of 15":    func(r *StartRequest) { r.ID = "0123456789abcde" },
		"an id in upper": func(r *StartRequest) { r.ID = "0123456789ABCDEF" },
	} {
		r := req
		tk := *req.Ticket
		r.Ticket = &tk
		change(&r)
		if _, err := w.m.Start(r); err == nil {
			t.Errorf("%s: the run started", name)
		}
	}
	req.ID = "0123456789abcdef"
	r, err := w.m.Start(req)
	if err != nil || r.ID != "0123456789abcdef" {
		t.Fatalf("the start with its id: %v", err)
	}
	if len(w.calls) != 0 {
		t.Fatalf("the refused starts sent %v", w.calls)
	}
}
