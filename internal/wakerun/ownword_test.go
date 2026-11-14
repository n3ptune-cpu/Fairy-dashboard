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
