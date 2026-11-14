package wakerun

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Second review of build 14, R3. The agent's cancel names no action and no machine: it calls off whatever is
// pending on the machine that has the id in the panel when it is sent. A run that lets go of a machine during
// the countdown it started sends that cancel (review of build 14, F4), and two things it took for its own
// countdown were not.

const rePaired = "It was paired again during the run (its entry in the panel was replaced), so the run let go of it"

// (a) After a Re-pair the id can be another machine. The reviewer's sequence: machine A is counting down; the
// id is paired again to machine B, which has been on for days and on which an admin asked for a shutdown in 20
// minutes; machine A counts on, out of the panel's sight. The run's cancel went to B and called the admin's
// shutdown off, and the report said "its shutdown countdown was called off ... it is ON" while A went off.
//
// No cancel goes to an agent that has not shown the run's own countdown. When it never does, the report says
// that the run does not know whether the machine it woke switches off, and when that would be.
func TestAfterARePairNoCancelGoesToAnAgentThatDoesNotShowTheRunsCountdown(t *testing.T) {
	for name, c := range map[string]struct {
		other func(w *world, b *machine) // what the machine that has the id now is like
		shown string
	}{
		"another machine with a shutdown somebody asked for": {func(w *world, b *machine) {
			b.pending = &Pending{Action: "shutdown", At: w.now.Add(20 * time.Minute), DelayS: 1200}
		}, "the agent that has this id now shows another action as pending (shutdown at "},
		"another machine with a shutdown of the same length as the run's": {func(w *world, b *machine) {
			b.pending = &Pending{Action: "shutdown", At: w.now.Add(10 * time.Minute), DelayS: 120} // the length alone is not enough here: the end must fit too
		}, "the agent that has this id now shows another action as pending (shutdown at "},
		"another machine with a reboot pending": {func(w *world, b *machine) {
			b.pending = &Pending{Action: "reboot", At: w.now.Add(20 * time.Minute), DelayS: 1200}
		}, "the agent that has this id now shows another action as pending (reboot at "},
		"another machine with nothing pending": {func(*world, *machine) {}, "the agent that has this id now shows no countdown"},
		"an agent that does not answer":        {func(_ *world, b *machine) { b.unreachable = true }, "its agent did not answer under the new entry in that time"},
	} {
		w := newWorld(t)
		a := w.off("pc-a")
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		untilState(t, w, "pc-a", StShutting)
		w.tick()
		if a.pending == nil || !host(active(w), "pc-a").pendingSeen {
			t.Fatalf("%s: the fixture: no countdown, or the run has not seen it", name)
		}
		due := *host(active(w), "pc-a").ShutdownAt
		before := len(w.calls)
		// machine B takes the id; machine A counts on, where the panel does not see it
		b := &machine{info: a.info, on: true, bootedAt: w.now.Add(-72 * time.Hour), agentReadyAt: w.now.Add(-71 * time.Hour),
			version: "17-bbbbbbb", updState: "idle", shutMode: "ok", busyDone: map[string]bool{}}
		b.info.Entry++
		c.other(w, b)
		was := b.pending
		w.ms["pc-a"], w.ms["old-a"] = b, a
		w.order = append(w.order, "old-a")
		w.tick()
		if row := host(mustActive(t, w), "pc-a"); !strings.Contains(row.Text, "waiting for its agent to show the run's countdown before a cancel is sent") ||
			!logHas(row, "no cancel is sent until the agent that has this id now shows that countdown, since it may be another machine") {
			t.Errorf("%s: the row does not say what the run waits for: %s / %v", name, row.Text, logTexts(row))
		}
		h := host(w.finish(), "pc-a")
		if sent := w.calls[before:]; len(sent) != 0 {
			t.Errorf("%s: WRONG MACHINE: sent to the agent that has the id now: %v", name, sent)
		}
		if b.pending != was {
			t.Errorf("%s: what was pending on the machine that took the id was changed: %+v, was %+v", name, b.pending, was)
		}
		if !a.wentOff {
			t.Fatalf("%s: the fixture: the machine the run woke should have gone off at the end of its countdown", name)
		}
		has(t, h, StLeftOn, rePaired+" while its shutdown was counting down (due at "+clock(due)+"): "+c.shown,
			"so the run could not make sure that it is the machine it woke, and sent no cancel. The run does not know whether the machine it woke switches off: it may have switched off at "+clock(due)+".")
		if h.On != OnUnknown || strings.Contains(h.Text, "it is ON") || strings.Contains(h.Text, "called off.") {
			t.Errorf("%s: UNTRUE REPORT [%s]: %s", name, h.On, h.Text)
		}
	}
}

// The usual Re-pair is of the same machine: its agent is asked again some seconds later, under the new entry,
// and shows the countdown the run started. The cancel is sent then, once, and the machine stays on, as before
// (F4). Until then nothing is sent.
func TestAfterARePairOfTheSameMachineItsCountdownIsCalledOffOnceItShows(t *testing.T) {
	for _, silent := range []int{0, 1, 7} { // seconds until the agent of the new entry has answered
		w, pc, before := letGoWhile(t, func(pc *machine) { pc.info.Entry++; pc.unreachable = true })
		for i := 0; i < silent; i++ {
			w.tick()
			if len(w.calls) != before {
				t.Fatalf("silent for %d s: a cancel was sent before the agent of the new entry showed the run's countdown: %v", silent, w.calls[before:])
			}
		}
		pc.unreachable = false
		h := host(w.finish(), "pc-a")
		if got := strings.Join(w.calls[before:], ", "); got != "cancel pc-a" {
			t.Fatalf("silent for %d s: sent after the Re-pair: %q, want one cancel", silent, got)
		}
		has(t, h, StLeftOn, rePaired+": its shutdown countdown was called off. It came online after the wake and it is ON.")
		if h.On != OnYes || !pc.on || pc.pending != nil || pc.wentOff {
			t.Fatalf("silent for %d s: reported %s; the machine is on: %v, a countdown is pending: %v", silent, h.On, pc.on, pc.pending != nil)
		}
	}
}

// A request that got no answer and whose countdown the run never saw: there is nothing the run could know its
// machine by after a Re-pair. Nothing is sent, and the report says so at once.
func TestAfterARePairARequestWithoutAnAnswerIsNotCalledOffBlind(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	pc.shutLost = "never"
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	before := len(w.calls)
	pc.info.Entry++
	// at once: there is nothing to wait for, so the run does not wait for "its countdown" under the new entry
	w.tick()
	w.tick()
	if a, hist := w.m.Snapshot(); a != nil || len(hist) != 1 || logHas(host(hist[0], "pc-a"), "until the agent that has this id now shows that countdown") {
		t.Fatalf("the run waited for a countdown it has never seen (still going: %v)", a != nil)
	}
	h := host(w.finish(), "pc-a")
	if sent := w.calls[before:]; len(sent) != 0 {
		t.Errorf("sent after the Re-pair: %v", sent)
	}
	has(t, h, StLeftOn, rePaired+" after its shutdown request got no answer (it would be due at ",
		"The run never saw a countdown of it, so it cannot tell whether the machine that has this id in the panel now is the one it woke, and sent no cancel: "+
			"it does not know whether the machine it woke switches off. If the request arrived, that machine switches off at about ")
	if h.On != OnUnknown {
		t.Errorf("power %q: %s", h.On, h.Text)
	}
}

// (b) On the same machine. A person calls the run's shutdown off on the machine's page, inside the countdown,
// and asks for their own, 30 minutes on. Then the run has a reason to leave the machine on (it is switched off
// for runs, the run is cancelled, the kill switch, a guest starts on the node): its cancel called the person's
// shutdown off, and the report said "its shutdown countdown was called off".
//
// The agent says the length of every pending countdown, and the run knows the length of its own: a shutdown of
// another length is not the run's. No cancel is sent, and the report says whose it is and when it ends.
func TestAShutdownAPersonAskedForInThePlaceOfTheRunsIsNotCalledOff(t *testing.T) {
	reasons := map[string]func(w *world, r *Run, pc *machine){
		"It was switched off for \"Update unpowered hosts\" during the run, so the run let go of it": func(_ *world, _ *Run, pc *machine) { pc.info.OptedIn = false },
		"The run was cancelled by Bob": func(w *world, r *Run, _ *machine) {
			if _, err := w.m.CancelRun(r.ID, "Bob", false); err != nil {
				t.Fatal(err)
			}
		},
		"the run stopped (control is disabled by an administrator)":                 func(w *world, _ *Run, _ *machine) { w.blocked = "control is disabled by an administrator" },
		"Left ON: 1 guest is running on it (vm-1) since its shutdown was asked for": func(_ *world, _ *Run, pc *machine) { pc.guests, pc.guestNames = 1, "vm-1" },
	}
	for why, reason := range reasons {
		for _, seen := range []bool{true, false} { // the run had a look at the person's shutdown before, or not
			w := newWorld(t)
			pc := w.off("pc-a")
			r := w.start(StartRequest{Hosts: []string{"pc-a"}})
			untilState(t, w, "pc-a", StShutting)
			w.tick()
			w.tick()
			before := len(w.calls)
			at := w.now.Add(30 * time.Minute)
			mine := &Pending{Action: "shutdown", At: at, DelayS: 1800}
			pc.pending = mine
			if seen {
				w.tick()
			}
			reason(w, r, pc)
			h := host(w.finish(), "pc-a")
			if sent := w.calls[before:]; len(sent) != 0 || pc.pending != mine {
				t.Errorf("%s (seen before: %v): sent %v; the person's shutdown is still pending: %v", why, seen, sent, pc.pending == mine)
			}
			has(t, h, StLeftOn, why+": a shutdown is pending on it now that is not the one the run asked for (a countdown of 30 min that ends at "+clock(at)+
				", by the machine's own clock; the run asked for one of 2 min). Someone asked for it in the place of the run's, so the run sent no cancel: its agent's cancel would call that one off.",
				"It came online after the wake and it is ON now. Fairy no longer watches it: it switches off then unless someone cancels that on its page.")
			if h.On != OnYes || strings.Contains(h.Text, "was called off") {
				t.Errorf("%s (seen before: %v): UNTRUE REPORT [%s]: %s", why, seen, h.On, h.Text)
			}
			w.advance(31 * time.Minute) // and it goes off when the person asked for it, as the report says
			if pc.on || !pc.wentOff {
				t.Errorf("%s (seen before: %v): the person's shutdown did not happen", why, seen)
			}
		}
	}
}

// What stays as it was, and why the END of a countdown does not decide on the same machine: the agent gives
// another end for the same countdown after the machine's clock was corrected (setPanelClock moves both clocks
// of this world). Such a countdown is still the run's and is still called off: a node must not go off under a
// guest because its clock was set. So a shutdown of the run's own length is taken for the run's also when its
// end is another; a person who asks for exactly that in the place of the run's is the limit of this rule.
func TestACountdownOfTheRunsOwnLengthIsStillCalledOffWhenItsEndMoved(t *testing.T) {
	w, pc, before := letGoWhile(t, func(*machine) {})
	w.tick()
	setPanelClock(w, pc, -time.Hour)
	w.tick()
	pc.info.OptedIn = false
	h := host(w.finish(), "pc-a")
	if got := strings.Join(w.calls[before:], ", "); got != "cancel pc-a" || !pc.on || pc.pending != nil {
		t.Fatalf("after the machine's clock was set: sent %q, on %v", got, pc.on)
	}
	has(t, h, StLeftOn, "so the run let go of it: its shutdown countdown was called off. It came online after the wake and it is ON.")
}

// The run was calling its countdown off when the machine was taken out of the panel (Remove host, or the
// moment between the Remove and the Add of a Re-pair). The reviewer's sequence: the first cancel ARRIVES and
// its answer is lost, and before the next try the machine is gone. The report said "the run let go of it while
// its shutdown was counting down (due at ...)", as if nothing had been sent, for a machine that then stayed on.
// It says what was sent and what the run does not know.
func TestAMachineRemovedWhileItsCountdownWasBeingCalledOff(t *testing.T) {
	for _, c := range []struct {
		name   string
		break_ func(pc *machine)
		stays  bool
		want   string
	}{
		{"the cancel arrived, its answer did not", func(pc *machine) { pc.cancelLost = "always arrives" }, true,
			"The run had asked its agent to call that countdown off, and the request got no answer (agent unreachable: timeout): it may have arrived, and then the machine stays ON; if it did not, it switches off at "},
		{"no cancel arrived, and none was answered", func(pc *machine) { pc.cancelLost = "always" }, false,
			"The run had asked its agent to call that countdown off, and the request got no answer (agent unreachable: timeout): it may have arrived, and then the machine stays ON; if it did not, it switches off at "},
		{"the cancel was refused", func(pc *machine) { pc.unreachable = true }, false,
			"The run had asked its agent to call that countdown off, and the request did not get through (agent unreachable: connection failed): it may switch off at "},
	} {
		w, pc, before := letGoWhile(t, func(pc *machine) {
			pc.info.OptedIn = false
			c.break_(pc)
		})
		due := *host(mustActive(t, w), "pc-a").ShutdownAt
		w.tick() // the first cancel
		if (pc.pending == nil) != c.stays {
			t.Fatalf("%s: the fixture: the countdown is called off on the machine: %v", c.name, pc.pending == nil)
		}
		w.ms["gone"] = w.ms["pc-a"]
		delete(w.ms, "pc-a")
		w.m.Tick(context.Background())
		_, hist := w.m.Snapshot()
		if len(hist) != 1 {
			t.Fatalf("%s: the run should be over", c.name)
		}
		h := host(hist[0], "pc-a")
		if got := strings.Join(w.calls[before:], ", "); got != "cancel pc-a" {
			t.Errorf("%s: sent: %q", c.name, got)
		}
		has(t, h, StLeftOn, "It is no longer in the panel, so the run let go of it while its shutdown was counting down (due at "+clock(due)+"). "+
			c.want+clock(due)+".", "Fairy can no longer look at it.")
		if h.On != OnUnknown || strings.Contains(h.Text, "it switches off by itself") {
			t.Errorf("%s: [%s] %s", c.name, h.On, h.Text)
		}
		// what the machine then does is one of the two things the sentence names
		w.ms["pc-a"], pc.unreachable = pc, false
		delete(w.ms, "gone")
		w.advance(20 * time.Minute)
		if pc.on != c.stays {
			t.Errorf("%s: 20 minutes later the machine is on: %v", c.name, pc.on)
		}
	}
}

// Third review of build 14, S2. A cancel had been sent, and was being tried again, when the entry was replaced:
// the wait for the new entry's agent began with a fresh state and forgot it. The reviewer's sequence: the
// machine is switched off for runs during its countdown, the run's cancel ARRIVES and its answer is lost, and
// then the machine is paired again. Its agent shows no countdown under the new entry (it was called off), the
// run waited for one until its due time, and the report said "sent no cancel ... it may have switched off at
// ..." for a machine that a cancel of the run had kept on. The sentence says what was sent.
func TestARePairWhileACancelWasBeingTriedAgainKeepsWhatWasSent(t *testing.T) {
	const letGo = "It was switched off for \"Update unpowered hosts\" during the run, so the run let go of it"
	for _, c := range []struct {
		name   string
		break_ func(pc *machine)
		stays  bool
		sent   string
		want   string
	}{
		{"the cancel arrived, its answer did not", func(pc *machine) { pc.cancelLost = "always arrives" }, true, "cancel pc-a",
			"Before the entry was replaced the run had asked its agent to call that countdown off (1 request), and a request got no answer (agent unreachable: timeout): " +
				"it may have arrived, and then the machine it woke stays ON; if it did not, it switched off at "},
		{"no cancel arrived, and none was answered", func(pc *machine) { pc.cancelLost = "always" }, false, "cancel pc-a",
			"Before the entry was replaced the run had asked its agent to call that countdown off (1 request), and a request got no answer (agent unreachable: timeout): " +
				"it may have arrived, and then the machine it woke stays ON; if it did not, it switched off at "},
		{"the cancel could not be sent", func(pc *machine) { pc.unreachable = true }, false, "cancel pc-a",
			"Before the entry was replaced the run had asked its agent to call that countdown off (1 request), and none got through (agent unreachable: connection failed). " +
				"The run does not know whether the machine it woke switches off: it may have switched off at "},
	} {
		w, pc, before := letGoWhile(t, func(pc *machine) {
			pc.info.OptedIn = false
			c.break_(pc)
		})
		due := *host(mustActive(t, w), "pc-a").ShutdownAt
		w.tick() // the first cancel
		if (pc.pending == nil) != c.stays || len(w.calls) != before+1 {
			t.Fatalf("%s: the fixture: the countdown is called off on the machine: %v; sent %v", c.name, pc.pending == nil, w.calls[before:])
		}
		// Re-pair. The machine that has the id now shows no countdown of the run: it is the same one after the
		// cancel arrived, and another machine (the run's own counts on out of sight) in the other two cases.
		now := pc
		if !c.stays {
			now = &machine{info: pc.info, on: true, bootedAt: w.now.Add(-72 * time.Hour), agentReadyAt: w.now.Add(-71 * time.Hour),
				version: "17-bbbbbbb", updState: "idle", shutMode: "ok", busyDone: map[string]bool{}}
			pc.unreachable = false
			w.ms["pc-a"], w.ms["old-a"] = now, pc
			w.order = append(w.order, "old-a")
		}
		now.info.Entry++
		w.tick()
		if row := host(mustActive(t, w), "pc-a"); !logHas(row, "no further cancel is sent (1 request went out before) until the agent that has this id now shows that countdown, since it may be another machine") {
			t.Errorf("%s: the history does not say what was sent before the wait: %v", c.name, logTexts(row))
		}
		h := host(w.finish(), "pc-a")
		if got := strings.Join(w.calls[before:], ", "); got != c.sent {
			t.Errorf("%s: sent: %q, want %q (nothing after the Re-pair)", c.name, got, c.sent)
		}
		has(t, h, StLeftOn, letGo+" while its shutdown was counting down (due at "+clock(due)+"): the agent that has this id now shows no countdown, "+
			"so the run could not make sure that it is the machine it woke, and sent no further cancel. "+c.want+clock(due)+".", "Fairy no longer watches it.")
		if h.On != OnUnknown || strings.Contains(h.Text, "sent no cancel") {
			t.Errorf("%s: UNTRUE REPORT [%s]: %s", c.name, h.On, h.Text)
		}
		if pc.on != c.stays || pc.wentOff == c.stays { // what the machine the run woke did is one of the two things the sentence names
			t.Errorf("%s: the machine the run woke is on: %v", c.name, pc.on)
		}
	}
}

// The same when the agent of the new entry then shows the run's countdown (the usual Re-pair, of the same
// machine): the cancel goes on being tried under the new entry, with what was tried before, and the countdown
// is called off.
func TestARePairWhileACancelWasBeingTriedAgainGoesOnWhenTheCountdownShows(t *testing.T) {
	w, pc, before := letGoWhile(t, func(pc *machine) { pc.info.OptedIn = false; pc.cancelLost = "always" })
	w.tick() // the first cancel: it never arrives
	if pc.pending == nil || len(w.calls) != before+1 {
		t.Fatalf("the fixture: %v", w.calls[before:])
	}
	pc.info.Entry++
	pc.cancelLost = ""
	h := host(w.finish(), "pc-a")
	if got := strings.Join(w.calls[before:], ", "); got != "cancel pc-a, cancel pc-a" || !pc.on || pc.pending != nil || pc.wentOff {
		t.Fatalf("sent %q; the machine is on: %v, a countdown is pending: %v", got, pc.on, pc.pending != nil)
	}
	has(t, h, StLeftOn, "so the run let go of it: its shutdown countdown was called off. It came online after the wake and it is ON.")
	if h.On != OnYes {
		t.Errorf("[%s] %s", h.On, h.Text)
	}
}

// A second Re-pair. The first one was of the same machine: its agent showed the run's countdown under the new
// entry, a cancel went to it and got no answer. Then the id is paired again, to another machine. That the
// agent of the second entry was recognised says nothing about the third: no cancel goes to it, whatever is
// pending there (also a shutdown of the run's own length), and the sentence has the cancel that was sent.
func TestASecondRePairAfterTheFirstWasRecognised(t *testing.T) {
	for name, theirs := range map[string]Pending{
		"a shutdown of another length":       {Action: "shutdown", At: t0.Add(30 * time.Minute), DelayS: 1200},
		"a shutdown of the run's own length": {Action: "shutdown", At: t0.Add(30 * time.Minute), DelayS: 120},
		"a reboot":                           {Action: "reboot", At: t0.Add(30 * time.Minute), DelayS: 60},
	} {
		w, a, before := letGoWhile(t, func(pc *machine) { pc.info.Entry++; pc.cancelLost = "always" })
		due := *host(mustActive(t, w), "pc-a").ShutdownAt
		w.tick()
		w.tick()
		n := len(w.calls)
		if n != before+1 || a.pending == nil {
			t.Fatalf("%s: the fixture: one cancel to the recognised machine, which did not arrive: %v", name, w.calls[before:])
		}
		b := &machine{info: a.info, on: true, bootedAt: w.now.Add(-72 * time.Hour), agentReadyAt: w.now.Add(-71 * time.Hour),
			version: "17-bbbbbbb", updState: "idle", shutMode: "ok", busyDone: map[string]bool{}}
		b.info.Entry++
		p := theirs
		b.pending = &p
		w.ms["pc-a"], w.ms["old-a"] = b, a
		w.order = append(w.order, "old-a")
		h := host(w.finish(), "pc-a")
		if len(w.calls[n:]) != 0 || b.pending != &p {
			t.Errorf("%s: WRONG MACHINE after a second Re-pair: sent %v; what was pending there is still pending: %v", name, w.calls[n:], b.pending == &p)
		}
		has(t, h, StLeftOn, rePaired+" while its shutdown was counting down (due at "+clock(due)+"): the agent that has this id now shows another action as pending ("+theirs.Action+" at ",
			"so the run could not make sure that it is the machine it woke, and sent no further cancel. Before the entry was replaced the run had asked its agent to call that countdown off (1 request), "+
				"and a request got no answer (agent unreachable: timeout): it may have arrived, and then the machine it woke stays ON; if it did not, it switched off at "+clock(due)+". Fairy no longer watches it.")
		if h.On != OnUnknown || strings.Contains(h.Text, "sent no cancel") || strings.Contains(h.Text, "Someone asked for it in the place of the run's") {
			t.Errorf("%s: UNTRUE REPORT [%s]: %s", name, h.On, h.Text)
		}
		if !a.wentOff {
			t.Errorf("%s: the fixture: the machine the run woke counts on and goes off", name)
		}
	}
}

// A request without an answer whose countdown never showed, a stopped run that repeats its cancel in case the
// request still arrives, and then a Re-pair: nothing further is sent (there is nothing to know the machine
// by), and the sentence does not say "sent no cancel" for the cancels that went out before.
func TestARePairAfterCancelsForALostRequestSaysThatTheyWereSent(t *testing.T) {
	w, pc := lostAndStopped(t, stops[0].stop)
	w.advance(12 * time.Second)
	sent := cancels(w)
	if sent != 2 {
		t.Fatalf("the fixture: %d cancel(s)", sent)
	}
	pc.info.Entry++
	h := host(w.finish(), "pc-a")
	if cancels(w) != sent {
		t.Errorf("sent after the Re-pair: %s", w.callsFor("pc-a"))
	}
	has(t, h, StLeftOn, stops[0].why+" after its shutdown request got no answer (it would be due at ",
		"The run never saw a countdown of it, so it cannot tell whether the machine that has this id in the panel now is the one it woke, and sent no further cancel "+
			"(2 requests had gone out before the entry was replaced, in case the shutdown request still arrives): it does not know whether the machine it woke switches off. "+
			"If the request arrived, that machine switches off at about ")
	if h.On != OnUnknown {
		t.Errorf("power %q: %s", h.On, h.Text)
	}
}

// Third review of build 14, S3: "the agent that has the id now" is meant strictly. Machine A counts down; its
// id is paired again to machine B (on for days, with a reboot somebody asked for pending). A poll of the old
// entry was on its way and finishes after the Re-pair: what the run is shown under the id is A's answer, with
// the run's own countdown in it, before B has answered once. The run took B for the machine it woke and sent
// its cancel there: B's reboot was called off, and the report said "its shutdown countdown was called off ...
// it is ON". (The poller no longer keeps such an answer: TestAPollOfAnEntryThatWasReplacedIsDropped there.
// This is the run's own look: an answer counts only when it came under the entry the panel has now.)
func TestAnAnswerThatCameUnderTheOldEntryDoesNotRecogniseTheMachine(t *testing.T) {
	w := newWorld(t)
	a := w.off("pc-a")
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	w.tick()
	if a.pending == nil || !host(active(w), "pc-a").pendingSeen {
		t.Fatal("the fixture: no countdown, or the run has not seen it")
	}
	before := len(w.calls)
	b := &machine{info: a.info, on: true, bootedAt: w.now.Add(-72 * time.Hour), agentReadyAt: w.now.Add(-71 * time.Hour),
		version: "17-bbbbbbb", updState: "idle", shutMode: "ok", busyDone: map[string]bool{}}
	b.info.Entry++
	theirs := &Pending{Action: "reboot", At: w.now.Add(20 * time.Minute), DelayS: 1200}
	b.pending = theirs
	// what is under the id is still the old entry's answer, which came in after the Re-pair
	b.obs, b.everAnswered, b.seenMono, b.lastAnswer = a.obs, true, w.mono, w.now
	if b.obs.Entry == b.info.Entry || b.obs.Pending == nil {
		t.Fatal("the fixture: the answer under the id is the old entry's, with the run's countdown")
	}
	w.ms["pc-a"], w.ms["old-a"] = b, a
	w.order = append(w.order, "old-a")
	w.m.Tick(context.Background()) // the run's step before B's own first answer
	h := host(w.finish(), "pc-a")
	if sent := w.calls[before:]; len(sent) != 0 || b.pending != theirs {
		t.Errorf("WRONG MACHINE: sent %v to the machine that has the id now; its reboot is still pending: %v", sent, b.pending == theirs)
	}
	has(t, h, StLeftOn, rePaired+" while its shutdown was counting down (due at ", "the agent that has this id now shows another action as pending (reboot at ",
		"so the run could not make sure that it is the machine it woke, and sent no cancel.")
	if h.On != OnUnknown || !a.wentOff {
		t.Errorf("[%s] the machine the run woke went off: %v: %s", h.On, a.wentOff, h.Text)
	}
}

// Tests for changed copies of the program that the second and third review of build 14 got past the tests
// (each was applied by hand, seen to fail one of these, and taken out again).

// An agent that does not say the length of its countdown (delay_s missing or 0: an agent from before the
// field). What the run does with it, decided and held here: a pending shutdown without a length cannot be told
// from the run's own, and it is taken for the run's own, as every shutdown was before the length was looked
// at. So the run's cancel is sent, also when a person has asked for that shutdown in the place of the run's:
// a cancel too many loses a shutdown somebody asked for, a cancel too few lets a machine go off that was to
// stay on (a node under a guest). The README says so with the other limit of this rule.
func TestAShutdownOfAnAgentThatDoesNotSayItsLengthIsTakenForTheRuns(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	w.tick()
	w.tick()
	before := len(w.calls)
	pc.pending = &Pending{Action: "shutdown", At: w.now.Add(30 * time.Minute)} // no DelayS
	w.tick()
	pc.info.OptedIn = false
	h := host(w.finish(), "pc-a")
	if got := strings.Join(w.calls[before:], ", "); got != "cancel pc-a" || pc.pending != nil || !pc.on {
		t.Errorf("sent %q; pending %v, on %v", got, pc.pending, pc.on)
	}
	has(t, h, StLeftOn, "so the run let go of it: its shutdown countdown was called off. It came online after the wake and it is ON.")
	if strings.Contains(h.Text, "not the one the run asked for") {
		t.Errorf("a shutdown without a length was taken for somebody else's: %s", h.Text)
	}
}

// The length that is compared is the machine's own where it has one (wake_update.shutdown_delay_s of its
// systems: entry), not the panel's: on a machine whose countdown is 30 s a person's shutdown of 2 minutes (the
// panel's length) is not the run's, and one of 30 s cannot be told from it.
func TestTheLengthThatIsComparedIsTheMachinesOwn(t *testing.T) {
	for personS, theirs := range map[int]bool{120: true, 30: false} {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.info.ShutdownDelay = 30 * time.Second
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		untilState(t, w, "pc-a", StShutting)
		w.tick()
		w.tick()
		if pc.pending == nil || pc.pending.DelayS != 30 {
			t.Fatalf("the fixture: %+v", pc.pending)
		}
		before := len(w.calls)
		mine := &Pending{Action: "shutdown", At: w.now.Add(time.Duration(personS) * time.Second), DelayS: personS}
		pc.pending = mine
		w.tick()
		pc.info.OptedIn = false
		h := host(w.finish(), "pc-a")
		if sent := len(w.calls[before:]); (sent == 0) != theirs || (pc.pending == mine) != theirs {
			t.Errorf("a person's shutdown of %d s on a machine whose own countdown is 30 s: %d cancel(s), still pending: %v: %s", personS, sent, pc.pending == mine, h.Text)
		}
		if theirs {
			has(t, h, StLeftOn, "(a countdown of 2 min that ends at ", "; the run asked for one of 30 s)")
		}
	}
}

// The run waits for the agent of a new entry to show its countdown, and the machine is taken out of the panel
// in that time. No cancel was sent, and the sentence does not say that one was: "The run had asked its agent to
// call that countdown off, and the request did not get through ()" is for a cancel that left.
func TestAMachineRemovedWhileTheRunWaitedForItsCountdownSaysNothingOfACancel(t *testing.T) {
	w, pc, before := letGoWhile(t, func(pc *machine) { pc.info.Entry++; pc.unreachable = true })
	due := *host(mustActive(t, w), "pc-a").ShutdownAt
	w.tick()
	w.tick()
	if c := host(mustActive(t, w), "pc-a").calloff; c == nil || !c.idWait || c.tries != 0 || len(w.calls) != before {
		t.Fatalf("the fixture: the run waits, and has sent nothing: %+v, %v", c, w.calls[before:])
	}
	w.ms["gone"] = pc
	delete(w.ms, "pc-a")
	w.m.Tick(context.Background())
	_, hist := w.m.Snapshot()
	if len(hist) != 1 {
		t.Fatal("the run should be over")
	}
	h := host(hist[0], "pc-a")
	has(t, h, StLeftOn, "It is no longer in the panel, so the run let go of it while its shutdown was counting down (due at "+clock(due)+").", "Fairy can no longer look at it.")
	if strings.Contains(h.Text, "had asked its agent") || strings.Contains(h.Text, "did not get through") || len(w.calls) != before {
		t.Errorf("UNTRUE: no cancel was sent (%v): %s", w.calls[before:], h.Text)
	}
}

// A shutdown request whose answer was lost and whose countdown the run then saw: the end of that countdown is
// what the agent showed first (the request's answer never gave one). After a Re-pair of the same machine the
// run knows its countdown by it, calls it off, and the machine stays on. (Without it the run would wait for a
// countdown it could not recognise, and the machine would go off as "not known".)
func TestAfterARePairACountdownThatShowedAfterALostAnswerIsKnownByWhatTheAgentShowed(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	pc.shutLost = "arrives"
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	w.tick()
	w.tick()
	if h := host(mustActive(t, w), "pc-a"); pc.pending == nil || !h.ShutdownUnsure || !h.pendingSeen {
		t.Fatalf("the fixture: a countdown that runs after an answer that was lost, seen by the run: %+v", pc.pending)
	}
	before := len(w.calls)
	pc.info.Entry++
	h := host(w.finish(), "pc-a")
	if got := strings.Join(w.calls[before:], ", "); got != "cancel pc-a" || !pc.on || pc.pending != nil || pc.wentOff {
		t.Fatalf("sent %q; the machine is on: %v, a countdown is pending: %v", got, pc.on, pc.pending != nil)
	}
	has(t, h, StLeftOn, rePaired+": its shutdown countdown was called off. It came online after the wake and it is ON.")
}

// While the run waits for the agent of the new entry, it asks the poller to poll that agent every few seconds:
// a new entry is polled at its own pace (once a minute, or put off after failed polls), and the countdown runs.
func TestWhileTheRunWaitsAfterARePairThePollerIsAskedToPoll(t *testing.T) {
	w, pc, before := letGoWhile(t, func(pc *machine) { pc.info.Entry++; pc.unreachable = true })
	asked := w.kicks["pc-a"]
	for i := 0; i < 21; i++ {
		w.tick()
	}
	if n := w.kicks["pc-a"] - asked; n < 4 || n > 6 || len(w.calls) != before { // every kickEvery (5 s)
		t.Fatalf("in 21 s of waiting the poller was asked %d times to poll the new entry; sent %v", n, w.calls[before:])
	}
	pc.unreachable = false
	if h := host(w.finish(), "pc-a"); h.On != OnYes || !pc.on {
		t.Errorf("[%s] %s", h.On, h.Text)
	}
}

// After a Re-pair both must fit, the end and the length. The end alone is not enough either: another machine
// under the id shows a shutdown that ends at the second the run's does, asked for with another countdown (20
// minutes ago, for 20 minutes). It is not the run's: nothing is sent to that machine.
func TestAfterARePairAShutdownWithTheRunsEndAndAnotherLengthIsNotTheRuns(t *testing.T) {
	w := newWorld(t)
	a := w.off("pc-a")
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	w.tick()
	if a.pending == nil || !host(active(w), "pc-a").pendingSeen {
		t.Fatal("the fixture: no countdown, or the run has not seen it")
	}
	before := len(w.calls)
	b := &machine{info: a.info, on: true, bootedAt: w.now.Add(-72 * time.Hour), agentReadyAt: w.now.Add(-71 * time.Hour),
		version: "17-bbbbbbb", updState: "idle", shutMode: "ok", busyDone: map[string]bool{}}
	b.info.Entry++
	theirs := &Pending{Action: "shutdown", At: a.pending.At, DelayS: 1200}
	b.pending = theirs
	w.ms["pc-a"], w.ms["old-a"] = b, a
	w.order = append(w.order, "old-a")
	for i := 0; i < 30; i++ { // well inside both countdowns
		w.tick()
		if len(w.calls) != before || b.pending != theirs {
			t.Fatalf("WRONG MACHINE: %d s after the Re-pair: sent %v; its shutdown is still pending: %v", i+1, w.calls[before:], b.pending == theirs)
		}
	}
	h := host(w.finish(), "pc-a")
	if len(w.calls) != before || h.On != OnUnknown || !strings.Contains(h.Text, "so the run could not make sure that it is the machine it woke, and sent no cancel.") {
		t.Errorf("sent %v; [%s] %s", w.calls[before:], h.On, h.Text)
	}
}
