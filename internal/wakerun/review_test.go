package wakerun

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The tests of the review of 2026-11-11 (finding W1, with W2), and what belongs to them. The mistake: one
// minute without an answer was "off". A machine that restarts, or that is on and out of reach, also gives no
// answer for a minute. When it came back inside the wait with a fresh uptime, the run took it for a machine it
// had woken, updated it and switched it off. Now a machine is off for a run only after MinOff without an
// answer; less than that is "not sure", and nothing is sent.

// The reviewer's test, as written. It failed with "Woken, updated ..., and switched off again."
func TestReviewOnButUnreachableThenRestartsInsideTheWindow(t *testing.T) {
	w := newWorld(t)
	pc := w.running("pc-a") // on for three days
	pc.unreachable = true
	w.advance(90 * time.Second) // the panel shows it as offline
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	w.advance(20 * time.Second) // probe: timeout -> "off"; the relay accepts the wake (it changes nothing: the PC is on)
	// the person at the PC restarts it (or Windows Update does); after the restart its network works again
	pc.bootedAt, pc.agentReadyAt, pc.unreachable = w.now.Add(30*time.Second), w.now.Add(60*time.Second), false
	r := w.finish()
	h := host(r, "pc-a")
	t.Logf("state %s: %s; requests: %s; shutdowns %d", h.State, h.Text, w.callsFor("pc-a"), pc.shutdowns)
	if pc.shutdowns != 0 {
		t.Fatalf("a machine that was ON when the run started and when the wake was accepted was switched off")
	}
	// and more than the reviewer asked: it is not woken either, and the report says why
	if h.State != StSkipped || h.Text != NotSure || h.On != OnUntouched || len(w.calls) != 0 || !pc.on {
		t.Fatalf("%s (%s): %s; requests %v", h.State, h.On, h.Text, w.calls)
	}
}

// A machine that restarts (Windows Update, or its owner) while a run is started: its agent is silent for some
// minutes and comes back during the run's wait with an uptime of seconds. It is not shut down, not woken, not
// updated, whether or not "update it too" was ticked.
func TestRestartingMachineIsNeverTakenForOff(t *testing.T) {
	for _, quiet := range []time.Duration{61 * time.Second, 3 * time.Minute, 10 * time.Minute, MinOff - 2*time.Second} {
		for _, updateIfOn := range []bool{false, true} {
			w := newWorld(t)
			pc := w.running("pc-a")
			pc.unreachable = true // the restart begins: it does not answer
			w.advance(quiet)
			if o := w.Observe("pc-a"); o.Status != "offline" || o.Online || OffNow(o, w.now) {
				t.Fatalf("%v: the panel shows it as offline, and that is not \"off\": %+v", quiet, o)
			}
			w.start(StartRequest{Hosts: []string{"pc-a"}, UpdateIfOn: updateIfOn})
			w.advance(45 * time.Second)
			pc.bootedAt, pc.agentReadyAt, pc.unreachable = w.now.Add(-20*time.Second), w.now, false // back, 20 s after its system started
			w.advance(10 * time.Minute)
			r := w.finish()
			h := host(r, "pc-a")
			if pc.shutdowns != 0 || !pc.on {
				t.Fatalf("%v: a machine that restarted was switched off: %s", quiet, h.Text)
			}
			if h.State != StSkipped || h.Text != NotSure || h.On != OnUntouched || h.ProbedOff || h.WakeSentAt != nil || len(w.calls) != 0 {
				t.Fatalf("%v: %s (%s): %s; requests %v", quiet, h.State, h.On, h.Text, w.calls)
			}
		}
	}
}

// How long without an answer is "off". Below MinOff nothing is sent. From MinOff on the run asks the machine
// directly and wakes it. A machine that really is off, and was for that long, is woken, updated and switched
// off again as before.
func TestOffOnlyAfterMinOffWithoutAnAnswer(t *testing.T) {
	for _, c := range []struct {
		quiet time.Duration
		off   bool
	}{
		{20 * time.Second, false}, {61 * time.Second, false}, {5 * time.Minute, false}, {MinOff - 2*time.Second, false},
		{MinOff, false}, // the first poll without an answer was a second after it went off
		{MinOff + time.Second, true}, {MinOff + time.Minute, true}, {6 * time.Hour, true},
	} {
		w := newWorld(t)
		pc := w.running("pc-a")
		pc.on = false // it is switched off, really
		w.advance(c.quiet)
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		r := w.finish()
		h := host(r, "pc-a")
		if !c.off {
			if h.State != StSkipped || h.Text != NotSure || h.On != OnUntouched || len(w.calls) != 0 || pc.on {
				t.Errorf("no answer for %v: %s: %s; requests %v", c.quiet, h.State, h.Text, w.calls)
			}
			continue
		}
		has(t, h, StOff, "Woken, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.")
		if got := w.callsFor("pc-a"); got != "wake update shutdown" || pc.on || !pc.wentOff || h.QuietForS == nil || *h.QuietForS < int64(MinOff/time.Second) {
			t.Errorf("no answer for %v: requests %q on=%v quiet_for_s=%v", c.quiet, got, pc.on, h.QuietForS)
		}
	}
}

// "Off" is what the run saw when it started, not what a wait in the run's own queue made of it: a machine
// that had been quiet for a little less than MinOff at the start is not sure, also when the run gets to it
// after MinOff has passed.
func TestQuietTimeCountsFromBeforeTheRun(t *testing.T) {
	w := newWorld(t)
	var ids []string
	for i := 0; i < 12; i++ { // the run asks and wakes one machine per second, in the order of the list
		id := "first-" + string(rune('a'+i))
		w.off(id)
		ids = append(ids, id)
	}
	pc := w.running("pc-late")
	pc.on = false
	w.advance(MinOff - 5*time.Second)
	w.start(StartRequest{Hosts: append(ids, "pc-late")})
	r := w.finish()
	h := host(r, "pc-late")
	if h.State != StSkipped || h.Text != NotSure || pc.wakes != 0 || pc.shutdowns != 0 {
		t.Fatalf("%s: %s", h.State, h.Text)
	}
	if got := h.Log[len(h.Log)-1].T.Sub(r.StartedAt); got < 10*time.Second {
		t.Fatalf("the run got to it %v after the start: this test wants that to be after MinOff has passed", got)
	}
	has(t, host(r, "first-a"), StOff)
}

// The panel must have watched. A machine the panel has never had an answer from counts from the first poll
// that failed (the panel's start, or the moment it was added): before MinOff has passed since then it is "not
// sure", after it it is off.
func TestNeverSeenMachineWaitsForThePanelToHaveWatched(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	w.silentFor(pc, 0) // the panel has only just begun to ask it
	pc.everAnswered = false
	w.advance(5 * time.Minute)
	if o := w.Observe("pc-a"); o.Status != "offline" || OffNow(o, w.now) {
		t.Fatalf("the panel's view: %+v", o)
	}
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	r := w.finish()
	if h := host(r, "pc-a"); h.State != StSkipped || h.Text != NotSure || len(w.calls) != 0 {
		t.Fatalf("5 minutes after the panel began to ask: %s: %s", h.State, h.Text)
	}
	w.advance(MinOff)
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	has(t, host(w.finish(), "pc-a"), StOff)
	// and when the panel knows nothing at all (no row of failed polls), it is not sure
	w = newWorld(t)
	pc = w.off("pc-a")
	pc.obs.QuietSince = time.Time{}
	if o := w.Observe("pc-a"); o.Status != "offline" || OffNow(o, w.now) || QuietFor(o, w.now) != 0 {
		t.Fatalf("the panel's view: %+v", o)
	}
	r, err := w.m.Start(StartRequest{Hosts: []string{"pc-a"}, By: "Ann"})
	if err != nil || r.Hosts[0].QuietForS != nil {
		t.Fatalf("start: %v %+v", err, r.Hosts[0])
	}
}

// A machine that refuses the connection is on: its network stack answered, only its agent does not listen
// (the service is stopped, or it is being updated). Nothing is sent to it, however long its agent has been
// silent, and when its agent comes back during the run it is not touched. Here the panel's polls got no
// answer at all and only the run's direct question is refused (the poller's own view of a refusal is OnBy,
// tested below).
func TestMachineThatRefusesTheConnectionIsOn(t *testing.T) {
	for _, updateIfOn := range []bool{false, true} {
		w := newWorld(t)
		pc := w.running("pc-a")
		pc.agentDown = true
		w.advance(MinOff + 5*time.Minute)
		if o := w.Observe("pc-a"); !OffNow(o, w.now) {
			t.Fatalf("the panel's view is \"off\" (no answer for 20 minutes): %+v", o)
		}
		w.start(StartRequest{Hosts: []string{"pc-a"}, UpdateIfOn: updateIfOn})
		w.advance(30 * time.Second)
		pc.agentDown = false
		w.advance(5 * time.Minute)
		r := w.finish()
		h := host(r, "pc-a")
		if h.State != StSkipped || h.Text != refusedSentence || h.On != OnYes || !h.AlreadyOn || h.ProbedOff {
			t.Fatalf("%s (%s): %s", h.State, h.On, h.Text)
		}
		if len(w.calls) != 0 || pc.shutdowns != 0 || !pc.on {
			t.Fatalf("requests: %v", w.calls)
		}
		// and the rule itself says no, whatever else the record holds
		wake, online, up := w.now, w.now.Add(time.Minute), int64(5)
		h.ProbedOff, h.WakeSentAt, h.Wakes, h.OnlineAt, h.BootUptimeS = true, &wake, 1, &online, &up
		if ok, why := mayShutDown(h, HostInfo{OptedIn: true}, Observation{Online: true}, func() (int, bool, string) { return 0, true, "" }); ok || !strings.Contains(why, "already on") {
			t.Fatalf("mayShutDown: %v %q", ok, why)
		}
	}
}

// A Proxmox node whose agent is silent while the Proxmox API shows the node as online is on.
func TestMachineThatSomethingElseShowsAsOn(t *testing.T) {
	w := newWorld(t)
	pc := w.running("pve")
	pc.agentDown, pc.onBy, pc.probeSays = true, "the Proxmox API shows it as online", "timeout"
	w.advance(MinOff + time.Minute)
	if o := w.Observe("pve"); OffNow(o, w.now) {
		t.Fatalf("the panel's view must not be \"off\": %+v", o)
	}
	r0, err := w.m.Start(StartRequest{Hosts: []string{"pve"}, By: "Ann"})
	if err != nil || r0.Hosts[0].QuietForS != nil {
		t.Fatalf("start: %v; quiet_for_s %v", err, r0.Hosts[0].QuietForS)
	}
	r := w.finish()
	h := host(r, "pve")
	if h.State != StSkipped || h.Text != "It is on (the Proxmox API shows it as online), and its agent does not answer. Nothing was sent to it and it stays on." ||
		h.On != OnYes || !h.AlreadyOn || len(w.calls) != 0 {
		t.Fatalf("%s (%s): %s; requests %v", h.State, h.On, h.Text, w.calls)
	}
}

// The sentences are fixed: an owner who reads a report twice reads the same words.
func TestSentencesForOnOffAndNotSure(t *testing.T) {
	if NotSure != "Not sure that it is off, so nothing was sent to it: Fairy takes a machine for off only after 15 min without an answer (it may be restarting, or be on and out of reach)." {
		t.Errorf("not sure: %s", NotSure)
	}
	if MinOff != 15*time.Minute {
		t.Errorf("MinOff is %v", MinOff)
	}
	if got := fmt.Sprintf(onByText, OnByRefused); got != refusedSentence {
		t.Errorf("refused: %s", got)
	}
}

const refusedSentence = "It is on (the machine refused the connection), and its agent does not answer. Nothing was sent to it and it stays on."
