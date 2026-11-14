package wakerun

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The machine's own word about its run decides, whatever the panel was answered when it handed the ticket over
// (independent review of 270e7ab, P1). A ticket can reach a machine twice (a proxy that repeats the request,
// anybody on a plain-HTTP path): the machine takes the first copy and answers the second with a refusal, and
// the panel holds a "no" for a machine that is armed.

// armTwice is the panel's view of such a request: the machine is armed, the panel reads the refusal reason.
type armTwice struct {
	*world
	reason string
}

func (a armTwice) Arm(_ context.Context, id, run string) (time.Duration, error) {
	w := a.world
	mc := w.ms[id]
	w.calls = append(w.calls, "arm "+id)
	w.tk.taken[id] = true
	mc.chk.arms++
	if why := w.armNow(mc, run); why != "" {
		w.t.Fatalf("the model machine did not take the ticket: %s", why)
	}
	return 0, &OpError{Code: CodeRunRefused, Reason: a.reason, Text: "run refused (" + a.reason + ")"}
}

func named(armed []ArmedHost, id string) *ArmedHost {
	for i := range armed {
		if armed[i].ID == id {
			return &armed[i]
		}
	}
	return nil
}

func TestARefusedArmWhileTheMachineShowsTheRunAsArmed(t *testing.T) {
	for _, reason := range []string{"already_armed", "run_used", "other_run"} {
		t.Run(reason+", nobody confirms", func(t *testing.T) {
			w := newWorld(t)
			pc := w.checkedOff("pc-a")
			w.m.Ops = armTwice{w, reason}
			w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
			act := w.waitFor(8*time.Minute, "it waits", func(r *Run) bool { return stateOf(r, "pc-a") == StApproval })
			if h := host(act, "pc-a"); h.Arm != ArmRefused || strings.Contains(h.Text, "stays ON") {
				t.Fatalf("right after the refusal, before its agent was read: arm %q, %q", h.Arm, h.Text)
			}
			w.advance(30 * time.Second) // polls: its agent shows the run as armed in each
			act, _ = w.m.Snapshot()
			h := host(act, "pc-a")
			if o := w.Observe("pc-a"); o.Run == nil || o.Run.ID != act.ID || o.Run.State != BlockArmed {
				t.Fatalf("the model: the machine does not show the run as armed: %+v", o.Run)
			}
			if !strings.HasPrefix(h.Text, "awake and armed: it switches itself off at "+clock(*h.SelfOffAt)+" by the machine's own clock unless you confirm") || !h.SelfOffByMachine {
				t.Fatalf("the row: %q", h.Text)
			}
			if !strings.Contains(logOf(h), "its agent shows that it is armed by this run, although its arming was answered with a refusal (run refused ("+reason+")): a ticket did arrive") {
				t.Fatalf("the log does not say what was seen:\n%s", logOf(h))
			}
			a := named(w.m.MayGoOff(w.now), "pc-a")
			if a == nil || !a.Sure || !a.Active || a.At == nil {
				t.Fatalf("the strip's list does not name it as armed: %+v", a)
			}
			if ws, err := w.m.Waiting(act.ID); err != nil || len(ws) != 1 || !ws[0].Armed {
				t.Fatalf("it is offered for the confirmation as armed: %+v %v", ws, err)
			}
			done := w.finish()
			h = host(done, "pc-a")
			has(t, h, StOff, "Nobody confirmed within 10 min: not confirmed, not updated. It switched itself off again.")
			if pc.on || strings.Contains(h.Text, "nothing of this run switches it off") {
				t.Fatalf("the report: %q (on=%v)", h.Text, pc.on)
			}
		})
	}
	t.Run("and it is confirmed like any armed machine", func(t *testing.T) {
		w := newWorld(t)
		pc := w.checkedOff("pc-a")
		w.m.Ops = armTwice{w, "already_armed"}
		w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
		w.waitFor(10*time.Minute, "the run asks", func(r *Run) bool { return r.Approval == "needed" })
		w.advance(20 * time.Second)
		if _, err := w.confirm("Ann"); err != nil {
			t.Fatal(err)
		}
		done := w.finish()
		has(t, host(done, "pc-a"), StOff, "updated from 16-aaaaaaa to 17-bbbbbbb")
		if got := w.callsFor("pc-a"); pc.on || got != "wake arm go update shutdown" {
			t.Fatalf("on=%v, requests %q", pc.on, got)
		}
	})
}

// A ticket the machine did refuse: the row does not say "stays ON" before an answer of its agent from after
// the request has shown that no run is on it; then it says it, as before.
func TestARefusedArmSaysStaysOnOnlyAfterItsAgentShowedNoRun(t *testing.T) {
	w := newWorld(t)
	pc := w.checkedOff("pc-a")
	pc.chk.armSays = "limits"
	w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
	act := w.waitFor(8*time.Minute, "it waits", func(r *Run) bool { return stateOf(r, "pc-a") == StApproval })
	if h := host(act, "pc-a"); h.Text != "awake; its arming was refused (run refused (limits)): waiting for its agent to show whether a run is on it all the same" {
		t.Fatalf("before its agent was read: %q", h.Text)
	}
	if strings.Contains(logOf(host(act, "pc-a")), "stays ON") {
		t.Fatalf("the log says \"stays ON\" before its agent was read:\n%s", logOf(host(act, "pc-a")))
	}
	w.advance(30 * time.Second)
	act, _ = w.m.Snapshot()
	if h := host(act, "pc-a"); h.Text != "awake; it could not be armed (run refused (limits)): it stays ON unless you confirm" {
		t.Fatalf("after its agent showed no run: %q", h.Text)
	}
	if a := named(w.m.MayGoOff(w.now), "pc-a"); a != nil {
		t.Fatalf("a machine that is not armed is named as one that may switch itself off: %+v", a)
	}
	done := w.finish()
	has(t, host(done, "pc-a"), StLeftOn, "It could not be armed (run refused (limits)). It is ON: nothing of this run switches it off.")
	if !pc.on {
		t.Fatal("the model: it went off")
	}
}

// Its checker never says anything about runs after the refusal (a helper that does not answer): the run cannot
// read that no run is on it, and does not claim it.
func TestARefusedArmWhoseAgentNeverSaysIsNotSaidToStayOn(t *testing.T) {
	w := newWorld(t)
	pc := w.checkedOff("pc-a")
	pc.chk.armSays, pc.chk.noRuns = "limits", true
	w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
	w.waitFor(8*time.Minute, "it waits", func(r *Run) bool { return stateOf(r, "pc-a") == StApproval })
	w.advance(time.Minute)
	act, _ := w.m.Snapshot()
	if h := host(act, "pc-a"); strings.Contains(h.Text, "stays ON") {
		t.Fatalf("the row: %q", h.Text)
	}
	done := w.finish()
	h := host(done, "pc-a")
	has(t, h, StLeftOn, "Its arming was refused (run refused (limits)), and its agent did not say afterwards whether a run is on it. It was ON when Fairy last saw it")
	if strings.Contains(h.Text, "nothing of this run switches it off") {
		t.Fatalf("the report claims what the run could not read: %q", h.Text)
	}
}

// Another run is live on the machine by its own word (the ticket was refused with other_run, and its agent
// shows that other run as armed): it may switch itself off by that run's ticket. No sentence says "stays ON",
// the strip names it with Keep on, and the report says what its agent shows.
func TestAMachineOnWhichAnotherRunIsArmedIsNotSaidToStayOn(t *testing.T) {
	w := newWorld(t)
	pc := w.checkedOff("pc-a")
	const other = "00000000000000ee"
	set := false
	w.afterTick = func() { // armed by another run's ticket as soon as it is up
		if c := w.bootOf(pc); pc.on && !set {
			set = true
			c.run, c.state = other, BlockArmed
			c.waitEnds, c.offAt = w.mono+30*time.Minute, w.now.Add(30*time.Minute)
		}
	}
	w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
	w.waitFor(8*time.Minute, "it waits", func(r *Run) bool { return stateOf(r, "pc-a") == StApproval })
	w.advance(30 * time.Second)
	act, _ := w.m.Snapshot()
	h := host(act, "pc-a")
	at := clock(w.bootOf(pc).offAt)
	if h.Arm != ArmRefused || h.Text != "awake; not armed by this run (run refused (other_run)), but its agent shows another run ("+other+") as armed: by that run it switches itself off at "+at+" by the machine's own clock. Keep on cancels that" {
		t.Fatalf("the row: arm %q, %q", h.Arm, h.Text)
	}
	a := named(w.m.MayGoOff(w.now), "pc-a")
	if a == nil || !a.Sure || !a.ByMachine || a.At == nil || !a.At.Equal(w.bootOf(pc).offAt) {
		t.Fatalf("the strip's list: %+v", a)
	}
	done := w.finish()
	h = host(done, "pc-a")
	has(t, h, StLeftOn, "It could not be armed (run refused (other_run)). It is ON, and its agent shows another run ("+other+") as armed: by that run it switches itself off at "+at+" by the machine's own clock unless that is cancelled (Keep on in the panel, or at the machine).")
	if strings.Contains(h.Text, "nothing of this run switches it off") || !h.MayGoOff {
		t.Fatalf("the report: %q may_go_off=%v", h.Text, h.MayGoOff)
	}
	if a := named(w.m.MayGoOff(w.now), "pc-a"); a == nil || a.Active {
		t.Fatalf("after the run the strip's list no longer names it: %+v", a)
	}
}

// What the run learned from the machine's own word is in the journal (independent review of 270e7ab, P6): it
// was in memory only, so after a restart of the panel a machine the run had already seen NOT armed any more
// (it was started again) was reported as "It was armed: it switches itself off at about ...", and the strip
// offered Keep on for a machine that stays on.
func TestAfterARestartAMachineThatIsNoLongerArmedIsNotSaidToSwitchItselfOff(t *testing.T) {
	w, pc, _ := armedRun(t, nil)
	w.advance(time.Minute)
	pc.bootedAt, pc.agentReadyAt = w.now, w.now.Add(20*time.Second) // started again: a new boot, no run on it
	w.waitFor(2*time.Minute, "the run sees that", func(r *Run) bool { return strings.Contains(logOf(host(r, "pc-a")), "not armed any more") })
	w.tick()
	w.tick()
	m2, r := restarted(w)
	h := host(r, "pc-a")
	has(t, h, StInterrupted, "The panel restarted while it waited for the run's second confirmation. It was armed, and its agent no longer shows the run (the machine was started again, or the run was ended on it): it is not armed any more. It was ON: nothing of this run switches it off.")
	if h.MayGoOff || strings.Contains(h.Text, "it switches itself off at") {
		t.Fatalf("after the restart: %q (may_go_off=%v)", h.Text, h.MayGoOff)
	}
	if a := named(m2.MayGoOff(w.now), "pc-a"); a != nil {
		t.Fatalf("the strip's list names it: %+v", a)
	}
	w.advance(40 * time.Minute)
	if !pc.on {
		t.Fatal("the model: it went off")
	}
}

// And the other way round: a machine whose arming was answered with a refusal and whose agent then showed the
// run as armed is, after a restart, a machine that was armed.
func TestAfterARestartAMachineItsAgentShowedAsArmedIsSaidToSwitchItselfOff(t *testing.T) {
	w := newWorld(t)
	w.checkedOff("pc-a")
	w.m.Ops = armTwice{w, "already_armed"}
	w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
	w.waitFor(8*time.Minute, "it waits", func(r *Run) bool { return stateOf(r, "pc-a") == StApproval })
	w.advance(30 * time.Second)
	m2, r := restarted(w)
	h := host(r, "pc-a")
	has(t, h, StInterrupted, "The panel restarted while it waited for the run's second confirmation. It was armed: it switches itself off at ")
	if a := named(m2.MayGoOff(w.now), "pc-a"); !h.MayGoOff || !h.ArmSeen || a == nil || !a.Sure {
		t.Fatalf("after the restart: may_go_off=%v arm_seen=%v, the strip's list: %+v", h.MayGoOff, h.ArmSeen, a)
	}
}

// restarted is the panel after a restart: a manager that has read the journal, and the run it found going.
func restarted(w *world) (*Manager, *Run) {
	w.t.Helper()
	m2 := New(w.m.path, w.m.Settings, w)
	m2.Now = w.m.Now
	if err := m2.Load(); err != nil {
		w.t.Fatal(err)
	}
	_, hist := m2.Snapshot()
	if len(hist) == 0 {
		w.t.Fatal("the journal holds no run")
	}
	return m2, hist[0]
}

// After a restart of the panel the report says "It was ON: nothing of this run switches it off" of a machine
// that was not armed only when the run had read from its agent that no run is on it. A restart between the
// refused arm and that first read: the journal knows the refusal and nothing else, and the report says that it
// is not known, with the time by which the machine would switch itself off.
func TestAfterARestartARefusedArmIsNotSaidToStayOnBeforeItsAgentWasRead(t *testing.T) {
	arm := func(t *testing.T, set func(pc *machine)) (*world, *machine) {
		w := newWorld(t)
		pc := w.checkedOff("pc-a")
		set(pc)
		w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
		w.waitFor(8*time.Minute, "it waits", func(r *Run) bool { return stateOf(r, "pc-a") == StApproval })
		return w, pc
	}
	for name, c := range map[string]struct {
		set func(pc *machine)
		arm string
		not string
	}{
		"its arming was refused":     {func(pc *machine) { pc.chk.armSays = "limits" }, ArmRefused, "It could not be armed (run refused (limits))."},
		"it was not handed a ticket": {func(pc *machine) { pc.guests, pc.guestNames = 1, "vm-1" }, ArmNotSent, "It was not armed (1 guest is running on it (vm-1))."},
	} {
		t.Run(name+", restart before its agent was read", func(t *testing.T) {
			w, _ := arm(t, c.set)
			m2, r := restarted(w)
			h := host(r, "pc-a")
			latest := clock(r.StartedAt.Add(r.Ticket.valid() + lateArm + r.Ticket.wait() + selfOffLate))
			want := "The panel restarted while it waited for the run's second confirmation. " + c.not + " Fairy had not read from its agent yet that no run is on it, so it does not say that it stays on: " +
				"if a run is on it all the same, it switches itself off by about " + latest + " at the latest, with a notice of 2 min, unless that is cancelled (Keep on in the panel, or at the machine). " +
				"If not, it is ON and stays on. Not confirmed, not updated."
			if h.Arm != c.arm || h.NoRunSeen || h.State != StInterrupted || h.Text != want {
				t.Fatalf("arm %q, no_run_seen %v, %s:\n got %q\nwant %q", h.Arm, h.NoRunSeen, h.State, h.Text, want)
			}
			if a := named(m2.MayGoOff(w.now), "pc-a"); !h.MayGoOff || h.On != OnUnknown || a == nil || a.Sure || !a.Latest || a.At == nil {
				t.Fatalf("may_go_off %v, on %q, the strip's list: %+v", h.MayGoOff, h.On, a)
			}
		})
		t.Run(name+", restart after its agent showed no run", func(t *testing.T) {
			w, pc := arm(t, c.set)
			w.advance(30 * time.Second)
			m2, r := restarted(w)
			h := host(r, "pc-a")
			want := "The panel restarted while it waited for the run's second confirmation. " + c.not + " It was ON: nothing of this run switches it off. Not confirmed, not updated."
			if !h.NoRunSeen || h.Text != want || h.MayGoOff || h.On != OnYes {
				t.Fatalf("no_run_seen %v, may_go_off %v, on %q:\n got %q\nwant %q", h.NoRunSeen, h.MayGoOff, h.On, h.Text, want)
			}
			if a := named(m2.MayGoOff(w.now), "pc-a"); a != nil {
				t.Fatalf("the strip's list names it: %+v", a)
			}
			w.advance(40 * time.Minute)
			if !pc.on {
				t.Fatal("the model: it went off")
			}
		})
	}
	// its checker never says anything about runs: never read, so never "nothing of this run switches it off"
	t.Run("its agent never says, restart minutes later", func(t *testing.T) {
		w, _ := arm(t, func(pc *machine) { pc.chk.armSays, pc.chk.noRuns = "limits", true })
		w.advance(3 * time.Minute)
		_, r := restarted(w)
		if h := host(r, "pc-a"); h.NoRunSeen || !h.MayGoOff || strings.Contains(h.Text, "nothing of this run switches it off") || !strings.Contains(h.Text, "Fairy had not read from its agent yet that no run is on it") {
			t.Fatalf("no_run_seen %v, may_go_off %v: %q", h.NoRunSeen, h.MayGoOff, h.Text)
		}
	})
}

// The run reads the machine's agent before it ends a machine on a refused arming alone. The question is over
// seconds after the arm request (the machine refused its confirmation for good): the machine is not ended at
// once as "its agent did not say afterwards whether a run is on it". Its agent gets lookFor from the arm
// request to say; what it says then decides, and only its silence ends the machine with that sentence.
func TestARefusedArmIsNotEndedBeforeItsAgentCouldSay(t *testing.T) {
	const waiting = "awake; its arming was refused (run refused (limits)): waiting for its agent to show whether a run is on it all the same"
	over := func(t *testing.T) (*world, *machine, time.Duration) {
		t.Helper()
		w := newWorld(t)
		pc := w.checkedOff("pc-a")
		pc.chk.armSays, pc.chk.noRuns, pc.chk.goSays = "limits", true, "run_over"
		w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
		w.waitFor(8*time.Minute, "the run asks", func(r *Run) bool { return r.Approval == "needed" })
		asked := w.mono // the arm request left a moment before
		if _, err := w.confirm("Ann"); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			w.tick()
			cur, _ := w.m.Snapshot()
			if cur == nil || cur.Approval != "over" || host(cur, "pc-a").State != StApproval || host(cur, "pc-a").Text != waiting {
				t.Fatalf("%d s after the question was over: %s", i+1, describe(cur))
			}
		}
		return w, pc, asked
	}
	t.Run("its agent does not say", func(t *testing.T) {
		w, _, asked := over(t)
		done := w.waitFor(5*time.Minute, "pc-a ended", func(r *Run) bool { return host(r, "pc-a").Final() })
		if d := w.mono - asked; d < lookFor-2*time.Second || d > lookFor+3*time.Second {
			t.Fatalf("its agent was waited for for %s from the arm request, want %s", d, lookFor)
		}
		has(t, host(done, "pc-a"), StLeftOn, "Its arming was refused (run refused (limits)), and its agent did not say afterwards whether a run is on it. It was ON when Fairy last saw it")
	})
	t.Run("its agent says in that time that no run is on it", func(t *testing.T) {
		w, pc, _ := over(t)
		pc.chk.noRuns = false
		done := w.waitFor(lookFor, "pc-a ended", func(r *Run) bool { return host(r, "pc-a").Final() })
		has(t, host(done, "pc-a"), StLeftOn, "It could not be armed (run refused (limits)). It is ON: nothing of this run switches it off.")
	})
}
