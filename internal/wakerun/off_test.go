package wakerun

import (
	"strings"
	"testing"
	"time"
)

// Audit 5, round 3 (r3-p2-04): "switched off again" was written for a machine that is on. "Off" rested on 20 s
// without an answer once the countdown was over, and that silence could lie before the end of the countdown,
// or be one failed poll of a machine whose polling was then paused. The first two tests are the auditor's
// proofs (TestR3P2...), unchanged in what happens in them.

// The shutdown does not happen on the machine (the world's "ignore": the agent took the request, the countdown
// ran out, the system stayed up: a program that blocks the shutdown, a stop job that hangs), and the link to it
// is down from 5 s before the countdown ends for 40 s (its tunnel is the first thing a shutdown stops; or a
// drop of the link). The run wrote "switched off again" 16 s after the due time; the machine answered again
// 19 s later. TestRefusingAfterTheCountdownIsNotOff covers the same hang when the machine can still refuse the
// connection; this is the one that cannot.
func TestSilenceThatBeganBeforeTheCountdownEndedIsNotOff(t *testing.T) {
	for _, c := range []struct {
		name         string
		from, to     time.Duration // the link is down from .. to .., counted from the countdown's end
		mode         string        // what the machine does when its countdown ends
		state, power string
		want         string
	}{
		{"the auditor's: the shutdown does not happen, the link is down from 5 s before the end for 40 s", -5 * time.Second, 35 * time.Second, "ignore",
			StLeftOn, OnYes, "It is still on 5 min after its shutdown was due: the shutdown did not work. It is ON."},
		{"the same, and the link comes back 10 s after the end: 20 s of silence, 15 of them before the end", -15 * time.Second, 10 * time.Second, "ignore",
			StLeftOn, OnYes, "It is still on 5 min after its shutdown was due: the shutdown did not work. It is ON."},
		{"the link drops 2 s after the end, for 15 s: not 20 s", 2 * time.Second, 17 * time.Second, "ignore",
			StLeftOn, OnYes, "It is still on 5 min after its shutdown was due: the shutdown did not work. It is ON."},
		{"the link is down from 5 s before the end and never comes back, and the machine did go off: the run cannot tell", -5 * time.Second, time.Hour, "ok",
			StLeftOn, OnUnknown, "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and asked to switch off. It was already out of reach before its countdown ended, " +
				"and it does not answer now: Fairy cannot tell a machine that went off from a connection that dropped. Not confirmed that it is off."},
		{"a drop of the link during the countdown that is over 30 s before its end changes nothing", -60 * time.Second, -30 * time.Second, "ok",
			StOff, OnNo, "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again."},
	} {
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.shutMode = c.mode
		var due time.Time
		w.afterTick = func() {
			if pc.pending != nil {
				due = pc.pending.At
			}
			if !due.IsZero() {
				pc.unreachable = !w.now.Before(due.Add(c.from)) && w.now.Before(due.Add(c.to))
			}
		}
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		r := over(t, w)
		h := host(r, "pc-a")
		w.afterTick = nil
		pc.unreachable = false
		w.advance(2 * time.Minute)
		if h.On == OnNo && pc.on {
			t.Errorf("%s: REPORTED OFF, AND IT IS ON (%v after the countdown's end): %s", c.name, r.EndedAt.Sub(due), h.Text)
		}
		if h.State != c.state || h.On != c.power || !strings.Contains(h.Text, c.want) {
			t.Errorf("%s: [%s] power %q, %v after the countdown's end: %s", c.name, h.State, h.On, r.EndedAt.Sub(due), h.Text)
		}
		if h.On == OnNo && r.EndedAt.Sub(due) < offConfirm {
			t.Errorf("%s: \"off\" %v after the countdown's end, before the %v of silence after it can have been seen", c.name, r.EndedAt.Sub(due), offConfirm)
		}
	}
}

// The same without any silence after the countdown: polling is paused for the machine while the panel's last
// poll of it was one that failed (a blip of the link). From then on the run read that one stale "no answer"
// for ever, and one second after the countdown's due time it wrote "switched off again".
func TestPausedPollingDuringTheCountdownIsNotOff(t *testing.T) {
	for _, mode := range []string{"ignore", "ok"} { // the shutdown does not happen (it is blocked on the machine); it does
		w := newWorld(t)
		pc := w.off("pc-a")
		pc.shutMode = mode
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		untilState(t, w, "pc-a", StShutting)
		w.advance(20 * time.Second)
		pc.unreachable = true // a blip: two polls fail
		w.advance(2 * time.Second)
		pc.paused = true // an admin pauses polling for it
		pc.unreachable = false
		h := host(over(t, w), "pc-a")
		if h.On == OnNo {
			t.Errorf("%s: REPORTED OFF without one look at it after the countdown (the machine is on: %v): %s", mode, pc.on, h.Text)
		}
		has(t, h, StLeftOn, "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and asked to switch off. Polling is paused for it, "+
			"so Fairy could not see whether it went off: not confirmed that it is off.")
		if h.On != OnUnknown {
			t.Errorf("%s: power %q, want not known", mode, h.On)
		}
	}
	// paused while it answers, and the pause ends before the countdown does: the run goes on as if nothing was
	w := newWorld(t)
	pc := w.off("pc-a")
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	w.advance(20 * time.Second)
	pc.paused = true
	w.advance(40 * time.Second)
	pc.paused = false
	has(t, host(w.finish(), "pc-a"), StOff, "and switched off again.")
}

// What "off" is proven with, poll by poll (Host.seeSilence, Host.seenOff). The run's own due time is 100 s on
// its steady clock; the countdown cannot have ended before 97 s.
func TestOffNeedsSilentPollsMadeAfterTheDueTime(t *testing.T) {
	type look struct {
		at    time.Duration // the run's steady clock at the look
		o     Observation
		proof bool // seenOff after it
	}
	// The poller also says since when the machine gave nothing (SinceAgo): here that is the first poll of the
	// row, every row of this table begins after an answer. A look without it would read as "not known since
	// when", and that is never proof (recheck of fix7r, RC-05; this table left it out until then).
	var from time.Duration
	silent := func(row uint64, n int, began, ended, at time.Duration) Observation {
		if n == 1 {
			from = began
		}
		return Observation{Silent: Silence{Row: row, N: n, BeganAgo: at - began, EndedAgo: at - ended, SinceAgo: at - from}}
	}
	s := time.Second
	for name, looks := range map[string][]look{
		"two polls after the due time, the first 20 s ago": {
			{102 * s, silent(1, 1, 101*s, 102*s, 102*s), false},
			{107 * s, silent(1, 2, 106*s, 107*s, 107*s), false}, // two polls, but only 6 s
			{120 * s, silent(1, 2, 106*s, 107*s, 120*s), false},
			{121 * s, silent(1, 2, 106*s, 107*s, 121*s), true},
		},
		"one poll after the due time is not enough, however long ago": {
			{102 * s, silent(1, 1, 101*s, 102*s, 102*s), false},
			{200 * s, silent(1, 1, 101*s, 102*s, 200*s), false},
		},
		"a poll that began before the due time does not count, also when it ended after it": {
			{103 * s, silent(1, 1, 99*s, 103*s, 103*s), false},
			{108 * s, silent(1, 2, 104*s, 108*s, 108*s), false}, // the first that counts
			{124 * s, silent(1, 3, 120*s, 124*s, 124*s), true},  // the second, and 20 s since the first began
		},
		"an answer between two silent polls starts the count again": {
			{102 * s, silent(1, 1, 101*s, 102*s, 102*s), false},
			{107 * s, silent(1, 2, 106*s, 107*s, 107*s), false},
			{110 * s, Observation{Online: true}, false},
			{130 * s, silent(2, 1, 129*s, 130*s, 130*s), false},
			{135 * s, silent(2, 2, 134*s, 135*s, 135*s), false},
			{149 * s, silent(2, 2, 134*s, 135*s, 149*s), true},
		},
		"another row than at the last look (the answer fell between two looks) starts the count again": {
			{102 * s, silent(1, 1, 101*s, 102*s, 102*s), false},
			{107 * s, silent(1, 2, 106*s, 107*s, 107*s), false},
			{125 * s, silent(2, 1, 124*s, 125*s, 125*s), false},
			{130 * s, silent(2, 2, 129*s, 130*s, 130*s), false},
			{144 * s, silent(2, 3, 143*s, 144*s, 144*s), true},
		},
		"a sign of life (a refused connection) is not silence": {
			{102 * s, silent(1, 1, 101*s, 102*s, 102*s), false},
			{107 * s, Observation{OnBy: OnByRefused}, false},
			{125 * s, Observation{OnBy: OnByRefused}, false},
		},
		"paused polling: the row that is read is the one from before the pause": {
			{102 * s, silent(1, 1, 101*s, 102*s, 102*s), false},
			{107 * s, silent(1, 2, 106*s, 107*s, 107*s), false},
			{125 * s, func() Observation { o := silent(1, 2, 106*s, 107*s, 125*s); o.Paused = true; return o }(), false},
		},
		"the panel stopped asking (no row any more)": {
			{102 * s, silent(1, 1, 101*s, 102*s, 102*s), false},
			{107 * s, silent(1, 2, 106*s, 107*s, 107*s), false},
			{125 * s, Observation{}, false},
		},
		"out of reach before the countdown can have ended: the silence after it proves nothing": {
			{96 * s, silent(1, 1, 92*s, 96*s, 96*s), false},
			{104 * s, silent(1, 2, 101*s, 104*s, 104*s), false},
			{110 * s, silent(1, 3, 106*s, 110*s, 110*s), false},
			{140 * s, silent(1, 4, 136*s, 140*s, 140*s), false},
		},
		"a poll between the earliest and the latest end of the countdown is neither": {
			{99 * s, silent(1, 1, 98*s, 99*s, 99*s), false},
			{104 * s, silent(1, 2, 101*s, 104*s, 104*s), false},
			{125 * s, silent(1, 3, 121*s, 125*s, 125*s), true},
		},
		// (This row was "neither" while the table gave no SinceAgo. With the real poller it has been "out of
		// reach before the end" since R7A-02: the first silent poll counts from when its request left.)
		"a poll that left before the earliest end and failed after it: out of reach before the end": {
			{99 * s, silent(1, 1, 95*s, 99*s, 99*s), false},
			{104 * s, silent(1, 2, 101*s, 104*s, 104*s), false},
			{125 * s, silent(1, 3, 121*s, 125*s, 125*s), false},
			{160 * s, silent(1, 9, 156*s, 160*s, 160*s), false},
		},
	} {
		h := &Host{endMin: 97 * s, dueMono: 100 * s}
		for i, l := range looks {
			h.seeSilence(l.o, l.at)
			if got := h.seenOff(l.at); got != l.proof {
				t.Errorf("%s: look %d (at %v): proven off %v, want %v", name, i+1, l.at, got, l.proof)
			}
		}
	}
}

// The run asks the poller to poll the machine while it watches the countdown: a machine that is polled once a
// minute would otherwise not be looked at when the countdown ends.
func TestTheRunKicksThePollerDuringTheCountdown(t *testing.T) {
	w := newWorld(t)
	w.off("pc-a")
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	untilState(t, w, "pc-a", StShutting)
	before := w.kicks["pc-a"]
	w.advance(60 * time.Second)
	if n := w.kicks["pc-a"] - before; n < 10 || n > 14 {
		t.Fatalf("%d kicks in a minute of the countdown, want one every %v", n, kickEvery)
	}
	has(t, host(w.finish(), "pc-a"), StOff, "and switched off again.")
}
