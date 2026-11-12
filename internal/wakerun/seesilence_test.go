package wakerun

import (
	"testing"
	"time"
)

// Second review of fix7a (R7A-01, R7A-02): Host.seeSilence, fed by hand with what the poller gives.

func secs(s int) time.Duration { return time.Duration(s) * time.Second }

// silentRow is a look at a row of silent polls: the row's number, how many polls it has, and when the first
// poll without anything since the machine's last answer began (as the poller says it: "how long ago"). The
// latest poll of the row began and ended one second before the look.
func silentRow(row uint64, n, sinceAt, lookAt int) Observation {
	return Observation{Silent: Silence{Row: row, N: n, BeganAgo: secs(1), EndedAgo: secs(1), SinceAgo: secs(lookAt - sinceAt)}}
}

// R7A-02: the run looks at the latest poll of the row only. When more than one poll ran between two looks (a
// step of the run waited for a request to another machine, up to the request timeout), the first poll of the
// row, the one made before the countdown can have ended, was never seen: the row counted as proof of "off"
// although the machine was out of reach while its countdown was still running. The poller now says since when
// it has had nothing at all from the machine, and that decides, whatever the looks saw.
func TestARowThatBeganBeforeTheEndIsNoProofWhenALookMissedItsFirstPoll(t *testing.T) {
	h := &Host{endMin: secs(120), dueMono: secs(121)}
	h.seeSilence(Observation{Online: true}, secs(110)) // 110 s: it answers
	// 114 s: the link drops. The polls at 115 s (before the countdown can have ended) and at 123 s fail: one
	// row of two polls. The run's next look is at 124 s.
	h.seeSilence(silentRow(7, 2, 115, 124), secs(124))
	h.seeSilence(silentRow(7, 3, 115, 129), secs(129))
	h.seeSilence(silentRow(7, 6, 115, 144), secs(144))
	if !h.cutEarly || h.seenOff(secs(144)) {
		t.Errorf("a row of silent polls that began at 115 s, before the countdown can have ended (120 s), is taken as proof of \"off\": cutEarly %v, polls after the due time %d",
			h.cutEarly, h.silAfter)
	}
	// the same with every poll looked at
	g := &Host{endMin: secs(120), dueMono: secs(121)}
	g.seeSilence(Observation{Online: true}, secs(110))
	g.seeSilence(silentRow(7, 1, 115, 116), secs(116))
	g.seeSilence(silentRow(7, 2, 115, 124), secs(124))
	g.seeSilence(silentRow(7, 6, 115, 144), secs(144))
	if !g.cutEarly || g.seenOff(secs(144)) {
		t.Errorf("with every poll looked at the row must not count either")
	}
	// and a row that began after the countdown, seen the same late way, is proof
	k := &Host{endMin: secs(120), dueMono: secs(121)}
	k.seeSilence(Observation{Online: true}, secs(110))
	k.seeSilence(silentRow(7, 2, 122, 124), secs(124))
	k.seeSilence(silentRow(7, 3, 122, 129), secs(129))
	k.seeSilence(silentRow(7, 6, 122, 146), secs(146))
	if k.cutEarly || !k.seenOff(secs(146)) {
		t.Errorf("a row that began after the due time is not taken as proof of \"off\": cutEarly %v, polls after the due time %d", k.cutEarly, k.silAfter)
	}
}

// R7A-01: what takes "it was out of reach before its countdown ended" back, and what does not.
func TestOnlyAnAnswerOrASignOfLifeEndsOutOfReach(t *testing.T) {
	cut := func() *Host {
		h := &Host{endMin: secs(120), dueMono: secs(121)}
		h.seeSilence(Observation{Online: true}, secs(100))
		h.seeSilence(silentRow(7, 1, 105, 106), secs(106)) // out of reach from 105 s on
		if !h.cutEarly {
			t.Fatal("a silent poll before the countdown can have ended must set cutEarly")
		}
		return h
	}
	// what the poller does when the panel was not watching for a while: a new row, counted from one. The
	// machine has given nothing since 105 s, and the poller says so.
	for name, look := range map[string]Observation{
		"a new row of silent polls":             silentRow(8, 1, 105, 130),
		"a look at which the poller has no row": {},
		"paused polling":                        {Paused: true},
		"paused polling with a row":             {Paused: true, Silent: Silence{Row: 7, N: 4, BeganAgo: secs(1), EndedAgo: secs(1), SinceAgo: secs(25)}},
	} {
		h := cut()
		h.seeSilence(look, secs(130))
		if !h.cutEarly {
			t.Errorf("%s made the run forget that the machine was out of reach before its countdown ended", name)
		}
		if h.silAfter != 0 && look.Silent.Row != 8 {
			t.Errorf("%s: the count of the polls after the due time must start again, got %d", name, h.silAfter)
		}
		// the new row grows to more than enough polls after the due time: still no proof
		for at := 135; at <= 200; at += 5 {
			h.seeSilence(silentRow(9, (at-130)/5, 105, at), secs(at))
		}
		if h.seenOff(secs(200)) {
			t.Errorf("%s, then 65 s of silent polls after the due time: taken as proof of \"off\" for a machine that was out of reach before its countdown ended", name)
		}
	}
	for name, look := range map[string]Observation{
		"an answer":      {Online: true},
		"a sign of life": {OnBy: OnByRefused},
	} {
		h := cut()
		h.seeSilence(look, secs(110))
		if h.cutEarly {
			t.Errorf("%s must end \"out of reach\"", name)
		}
		// it goes quiet again after its countdown: that silence is proof
		for at := 125; at <= 150; at += 5 {
			h.seeSilence(silentRow(9, (at-120)/5, 122, at), secs(at))
		}
		if !h.seenOff(secs(150)) {
			t.Errorf("after %s, silence that began after the due time must count: cutEarly %v, polls after the due time %d", name, h.cutEarly, h.silAfter)
		}
	}
}
