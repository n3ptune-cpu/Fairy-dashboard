package wakerun

import (
	"strings"
	"testing"
	"time"
)

// The last answers of a machine that switches itself off (final review of build 16, J1-A).
//
// A PC with the check is armed, nobody confirms, and it counts down. Windows counts that countdown down itself
// and, at its end, stops its services one after the other: the helper, which is what keeps the run, before
// the agent, which answers one or two more polls. Those answers say nothing about runs (no "runs", no "run").
// In the watch of the machine's own switch-off such an answer took the word "counting down" away like every
// other answer, and the silence after it was "said nothing about the run ... not confirmed that it is off",
// Left ON, for a PC that had switched itself off as its ticket said.
//
// An answer that says nothing now leaves that word in place when it is the answer of that machine going down
// (seeSelfWord): the same start of the machine, and made between ten seconds before the earliest end of its
// countdown and a minute after the latest. The rule of d4625a8 holds: silence is the machine's own switch-off
// only after an answer that showed this run counting down (or armed), and an answer that shows the run gone,
// called off or confirmed, an answer of a machine that was started again, and one from outside that time take
// the word away.

// lastWords is what a PC that counts down its own switch-off does and says around the end of its countdown.
// All moments are counted from that end. As the agent does, the PC's helper shows the run as counting down for
// a minute after the end while the PC is still up (and as called off after that minute).
type lastWords struct {
	set        func(pc *machine)           // before the run starts
	each       func(w *world, pc *machine) // before every second of the world
	askFor     int                         // the run asks for the confirmation for this many seconds (0: the usual ten minutes)
	mute       []spanOf                    // its agent's answers say nothing about runs then (the helper does not answer it)
	noUptime   *time.Duration              // its agent's answers give no uptime from then on
	upJump     time.Duration               // ... and an uptime that is this much longer than the PC's
	otherEntry bool                        // ... and come under another entry than the one the run knows
	armedOnly  bool                        // its agent never shows the countdown: the run still reads "armed" (the wait is not over by its count)
	calledOff  bool                        // the countdown was stopped at the PC, and its helper says so as soon as it answers after the end
	off        *time.Duration              // the PC is down from then on (nil: it stays on: its shutdown did not happen)
	restart    *time.Duration              // it is started again then: the run is gone on it, and its agent answers 5 s later
	none       *time.Duration              // the one answer of that second shows no run on it
	out        []spanOf                    // the link is down then
	down       *time.Duration              // the link drops then, for good
}

type spanOf struct{ from, to time.Duration } // to 0: for good

func sec(n int) *time.Duration { d := time.Duration(n) * time.Second; return &d }

// goDown runs one armed PC through lastWords and returns its row, the machine, and the end of its countdown.
func goDown(t *testing.T, c lastWords) (*Host, *machine) {
	t.Helper()
	w := newWorld(t)
	pc := w.checkedOff("pc-a")
	pc.shutMode = "ignore" // the model leaves it on at the countdown's end: when it goes down is this test's
	if c.set != nil {
		c.set(pc)
	}
	req := StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"}
	if c.askFor != 0 {
		req.Ticket = &Ticket{ValidS: 420, WaitS: w.m.Settings.OnlineTimeoutS + 660, NoticeS: 120, WithinS: 1500 + w.m.Settings.SettleS, ConfirmWaitS: c.askFor}
	}
	w.start(req)
	var due time.Time
	reached := func(at *time.Duration, now time.Time) bool { return at != nil && !due.IsZero() && now.Sub(due) >= *at }
	restarted := false
	var r *Run
	for i := 0; i < 2*3600 && r == nil; i++ {
		if c.each != nil {
			c.each(w, pc)
		}
		next := w.now.Add(time.Second) // what the machine is in the second that comes
		if reached(c.restart, next) && !restarted && pc.on {
			restarted = true
			pc.bootedAt, pc.agentReadyAt, pc.pending = next, next.Add(5*time.Second), nil
		}
		if reached(c.off, next) && pc.on {
			pc.on, pc.wentOff, pc.offAt = false, true, next
		}
		pc.unreachable = reached(c.down, next)
		for _, m := range c.out {
			pc.unreachable = pc.unreachable || (!due.IsZero() && next.Sub(due) >= m.from && next.Sub(due) < m.to)
		}
		tickShown(w, func() {
			if due.IsZero() {
				if pc.pending != nil && pc.chk.state == BlockOffPending {
					due = pc.pending.At
				}
			}
			o := &pc.obs
			if !o.Online {
				return
			}
			if c.armedOnly && o.Run != nil && o.Run.State == BlockOffPending {
				b := *o.Run
				b.State = BlockArmed
				o.Run = &b
			}
			if due.IsZero() {
				return
			}
			since := w.now.Sub(due)
			if o.Run != nil && o.Run.State == BlockCancelled && since < time.Minute && !c.calledOff && !restarted {
				o.Run = &RunBlock{ID: o.Run.ID, State: BlockOffPending, OffAt: due} // still pending for the agent
			}
			if c.noUptime != nil && since >= *c.noUptime {
				o.UptimeS = nil
			}
			for _, m := range c.mute {
				if since >= m.from && (m.to == 0 || since < m.to) {
					o.Runs, o.Run = false, nil
					if c.upJump != 0 && o.UptimeS != nil {
						up := *o.UptimeS + int64(c.upJump/time.Second)
						o.UptimeS = &up
					}
					if c.otherEntry {
						o.Entry++
					}
				}
			}
			if c.none != nil && since == *c.none {
				o.Runs, o.Run = true, nil
			}
		})
		if a, hist := w.m.Snapshot(); a == nil {
			r = hist[0]
		}
	}
	if r == nil {
		t.Fatalf("the run did not end: %s", describe(active(w)))
	}
	if due.IsZero() {
		t.Fatal("the model: the PC never counted down")
	}
	w.wellReported(r)
	h := host(r, "pc-a")
	t.Logf("[%s] power %q: %s", h.State, h.On, h.Text)
	if h.On == OnNo && pc.on {
		t.Errorf("REPORTED OFF, AND IT IS ON: [%s] %s", h.State, h.Text)
	}
	return h, pc
}

const (
	offAgain    = "not confirmed, not updated. It switched itself off again."
	saidNothing = " said nothing about the run, and it does not answer now: not confirmed that it is off."
)

// The ordinary end: the helper is stopped in the countdown's last seconds or at its end, the agent answers a
// little longer, and the PC is down. It switched itself off.
func TestAPCThatStopsItsHelperAndThenGoesDownSwitchedItselfOff(t *testing.T) {
	for name, c := range map[string]lastWords{
		"the helper stops two seconds before the end, and the PC is down at the end": {mute: []spanOf{{-2 * time.Second, 0}}, off: sec(0)},
		"the agent answers for five seconds after the end":                           {mute: []spanOf{{0, 0}}, off: sec(5)},
		"only the very last answer is without the helper's word":                     {mute: []spanOf{{3 * time.Second, 0}}, off: sec(4)},
		"Windows takes 45 seconds to stop everything":                                {mute: []spanOf{{-1 * time.Second, 0}}, off: sec(45)},
		"ten seconds before the end is still its last seconds":                       {mute: []spanOf{{-9 * time.Second, 0}}, off: sec(1)},
	} {
		t.Run(name, func(t *testing.T) {
			h, pc := goDown(t, c)
			if pc.on {
				t.Fatal("the model: the PC is on")
			}
			if h.State != StOff || h.On != OnNo || !strings.HasSuffix(h.Text, offAgain) {
				t.Fatalf("[%s] power %q: %s", h.State, h.On, h.Text)
			}
		})
	}
}

// The same with the world's own PC, whose countdown switches it off: the checker stops saying anything about
// runs two seconds before the end (the reviewer's sequence).
func TestTheLastAnswersOfAPCWithoutItsHelperDoNotTakeItsSwitchOffBack(t *testing.T) {
	w, pc, _ := armedRun(t, nil)
	for i := 0; i < 3600 && !(pc.pending != nil && pc.chk.state == BlockOffPending && pc.pending.At.Sub(w.now) <= 2*time.Second); i++ {
		w.tick()
	}
	if pc.pending == nil || pc.chk.state != BlockOffPending {
		t.Fatalf("the model: no countdown of its own (%q)", pc.chk.state)
	}
	pc.chk.noRuns = true
	done := w.finish()
	if pc.on || !pc.wentOff {
		t.Fatalf("the model: on=%v, went off=%v", pc.on, pc.wentOff)
	}
	has(t, host(done, "pc-a"), StOff, "It came online after the wake. Nobody confirmed within 10 min: not confirmed, not updated. It switched itself off again.")
	if h := host(done, "pc-a"); h.On != OnNo {
		t.Fatalf("power %q: %s", h.On, h.Text)
	}
}

// Answers without the helper's word that are not the PC going down take the word "counting down" away, as
// before: what the silence after them is, the run does not know. In every one of these the PC can be on, and
// in most of them it is.
func TestAnswersThatSayNothingAndAreNotThePCGoingDown(t *testing.T) {
	for name, c := range map[string]struct {
		lastWords
		on bool // the PC is on at the end
	}{
		// The helper is gone a minute before the end. Whether Windows still goes down, the run does not take on
		// trust: here it does not (the shutdown was stopped at the PC), and the link drops later.
		"the helper stopped a minute before the end, the PC stays on, and the link drops": {
			lastWords{mute: []spanOf{{-60 * time.Second, 0}}, down: sec(30)}, true},
		// ... and here it does go down at the end: "not known" all the same (the conservative side).
		"the helper stopped a minute before the end, and the PC goes down at the end": {
			lastWords{mute: []spanOf{{-60 * time.Second, 0}}, off: sec(0)}, false},
		// It still answers three minutes after its countdown's end: it was not switched off by it.
		"it answers without a word for three minutes after the end, and then the link drops": {
			lastWords{mute: []spanOf{{-2 * time.Second, 0}}, down: sec(180)}, true},
		// A shutdown that takes longer than the minute after the countdown's end is not told from that.
		"it goes down a hundred seconds after the end, answering until then": {
			lastWords{mute: []spanOf{{-2 * time.Second, 0}}, off: sec(100)}, false},
		// Started again in the last seconds (its countdown died with that): the agent of the new start answers
		// before its helper does, at about the countdown's time, and then the link drops.
		"started again three seconds before the end, and the link drops after its agent's first answers": {
			lastWords{restart: sec(-3), mute: []spanOf{{-3 * time.Second, 0}}, down: sec(40)}, true},
		// Without an uptime in them the answers cannot be told from those of a new start.
		"its last answers give no uptime": {
			lastWords{mute: []spanOf{{-2 * time.Second, 0}}, noUptime: sec(-2), off: sec(0)}, false},
		"none of its answers of the last half minute gives an uptime": {
			lastWords{mute: []spanOf{{-2 * time.Second, 0}}, noUptime: sec(-30), off: sec(0)}, false},
		// An uptime that grew by days in two seconds is not that PC's either.
		"its last answers give an uptime that is three days longer": {
			lastWords{mute: []spanOf{{-2 * time.Second, 0}}, upJump: 72 * time.Hour, off: sec(0)}, false},
		// Under another entry: what answers under the id is not known to be the machine the run armed.
		"its last answers came under another entry than the run knows": {
			lastWords{mute: []spanOf{{-2 * time.Second, 0}}, otherEntry: true, off: sec(1)}, false},
		// The run never read "counting down": an armed PC needs its helper to begin the switch-off at all.
		"the last word was \"armed\", then answers without a word": {
			lastWords{armedOnly: true, mute: []spanOf{{-2 * time.Second, 0}}, down: sec(3)}, true},
		// ... also when those answers come at the time a countdown that began right after that word would end
		"the last word was \"armed\", then out of reach for two minutes, then answers without a word": {
			lastWords{armedOnly: true, out: []spanOf{{-115 * time.Second, -2 * time.Second}}, mute: []spanOf{{-2 * time.Second, 0}}, down: sec(30)}, true},
	} {
		t.Run(name, func(t *testing.T) {
			h, pc := goDown(t, c.lastWords)
			if pc.on != c.on {
				t.Fatalf("the model: the PC is on=%v", pc.on)
			}
			if h.State != StLeftOn || h.On != OnUnknown || !strings.HasSuffix(h.Text, saidNothing) || !strings.Contains(h.Text, "It was switching itself off; the last answer of its agent, at ") {
				t.Fatalf("[%s] power %q: %s", h.State, h.On, h.Text)
			}
		})
	}
}

// A PC that answers without a word for minutes after its countdown's end and never goes silent is ON, and is
// reported as that when the run stops watching.
func TestAPCThatAnswersWithoutAWordAndNeverGoesSilentIsOn(t *testing.T) {
	h, pc := goDown(t, lastWords{mute: []spanOf{{-2 * time.Second, 0}}})
	if !pc.on {
		t.Fatal("the model: the PC went off")
	}
	if h.State != StLeftOn || h.On != OnYes || !strings.HasSuffix(h.Text, "It was to switch itself off and is still on 5 min after that; its agent says nothing about the run. It is ON.") {
		t.Fatalf("[%s] power %q: %s", h.State, h.On, h.Text)
	}
}

// An answer that says nothing, and then an answer that says something: the later word counts.
func TestALaterAnswerThatSaysSomethingCounts(t *testing.T) {
	t.Run("the helper is started again while it counts down, and the PC goes down at the end", func(t *testing.T) {
		// (the early answers without a word took "counting down" away; the helper's word brings it back)
		h, pc := goDown(t, lastWords{mute: []spanOf{{-60 * time.Second, -30 * time.Second}}, off: sec(0)})
		if pc.on || h.State != StOff || h.On != OnNo || !strings.HasSuffix(h.Text, offAgain) {
			t.Fatalf("[%s] power %q (on: %v): %s", h.State, h.On, pc.on, h.Text)
		}
	})
	t.Run("the helper answers again after the end and says that the switch-off was called off", func(t *testing.T) {
		h, pc := goDown(t, lastWords{mute: []spanOf{{-2 * time.Second, 3 * time.Second}}, calledOff: true})
		if !pc.on || h.State != StLeftOn || h.On != OnYes || !strings.Contains(h.Text, "Its agent reports that its switch-off was called off") {
			t.Fatalf("[%s] power %q (on: %v): %s", h.State, h.On, pc.on, h.Text)
		}
	})
	t.Run("one answer after them shows no run, and then the link drops", func(t *testing.T) {
		// the rule of d4625a8, after answers that said nothing: one answer that shows no run is not "not armed
		// any more", and the silence after it is not the machine's own switch-off
		h, pc := goDown(t, lastWords{mute: []spanOf{{-2 * time.Second, 3 * time.Second}}, none: sec(3), down: sec(4)})
		if !pc.on || h.State != StLeftOn || h.On != OnUnknown || !strings.HasSuffix(h.Text, " showed no run on it, and it does not answer now: not confirmed that it is off.") {
			t.Fatalf("[%s] power %q (on: %v): %s", h.State, h.On, pc.on, h.Text)
		}
	})
}

// "Before the end" is counted from the latest answer that still showed the run as armed, not only from the
// arm request: a ticket that arrived 100 s after its request left makes the countdown end 100 s later than
// the earliest the request allows. A helper that stops a minute before that end is not the PC going down.
func TestTheEndOfACountdownIsCountedFromTheLastAnswerThatShowedArmed(t *testing.T) {
	late := func(pc *machine) { pc.chk.armLost, pc.chk.armLate = "late", 100*time.Second }
	t.Run("the helper stopped a minute before the end, the PC stays on, and the link drops", func(t *testing.T) {
		h, pc := goDown(t, lastWords{set: late, mute: []spanOf{{-60 * time.Second, 0}}, down: sec(30)})
		if !pc.on || h.State != StLeftOn || h.On != OnUnknown || !strings.HasSuffix(h.Text, saidNothing) {
			t.Fatalf("[%s] power %q (on: %v): %s", h.State, h.On, pc.on, h.Text)
		}
	})
	t.Run("the helper stops two seconds before the end, and the PC is down at the end", func(t *testing.T) {
		h, pc := goDown(t, lastWords{set: late, mute: []spanOf{{-2 * time.Second, 0}}, off: sec(0)})
		if pc.on || h.State != StOff || h.On != OnNo || !strings.HasSuffix(h.Text, "It switched itself off again.") {
			t.Fatalf("[%s] power %q (on: %v): %s", h.State, h.On, pc.on, h.Text)
		}
	})
}

// A run that has no moment of its own for the arming cannot tell where the end of the machine's countdown is,
// and keeps nothing. Here the run sent the PC no ticket at all (the ticket was too old when the PC was up),
// and its agent shows this run as counting down all the same: the first word the run reads of it. The helper
// is stopped in the last two seconds, the PC goes down at the end: "not known".
func TestARunWithoutAMomentForTheArmingKeepsNoWord(t *testing.T) {
	w := newWorld(t)
	pc := w.checkedOff("pc-a")
	pc.bootTakes = 150 * time.Second // up after the ticket's two minutes
	r := w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb",
		Ticket: &Ticket{ValidS: 120, WaitS: w.m.Settings.OnlineTimeoutS + 660, NoticeS: 120, WithinS: 1500 + w.m.Settings.SettleS, ConfirmWaitS: 600}})
	const notice = 120 * time.Second
	var began time.Time // its agent shows the countdown from then on
	var done *Run
	for i := 0; i < 3600 && done == nil; i++ {
		if next := w.now.Add(time.Second); !began.IsZero() && next.Sub(began) >= notice && pc.on {
			pc.on, pc.wentOff, pc.offAt = false, true, next
		}
		tickShown(w, func() {
			o := &pc.obs
			if !o.Online {
				return
			}
			if h := host(active(w), "pc-a"); began.IsZero() && h.State == StApproval && h.OnlineAt != nil && w.now.Sub(*h.OnlineAt) >= time.Minute {
				began = w.now
			}
			switch {
			case began.IsZero(), w.now.Sub(began) >= notice-2*time.Second:
				o.Runs, o.Run = false, nil // the helper does not answer the agent
			default:
				o.Runs, o.Run = true, &RunBlock{ID: r.ID, State: BlockOffPending, OffAt: began.Add(notice)}
			}
		})
		if a, hist := w.m.Snapshot(); a == nil {
			done = hist[0]
		}
	}
	if done == nil || began.IsZero() {
		t.Fatalf("the run did not end, or the PC never counted down (%v): %s", began, describe(active(w)))
	}
	h := host(done, "pc-a")
	if pc.on || h.Arm != ArmNotSent || h.State != StLeftOn || h.On != OnUnknown || !strings.HasSuffix(h.Text, saidNothing) {
		t.Fatalf("[%s] power %q, arm %q (the PC is on: %v): %s\n  history: %s", h.State, h.On, h.Arm, pc.on, h.Text, logOf(h))
	}
}

// The latest answer that showed the run as armed counts also when the run read it while a confirmation was on
// its way and it did not look at anything else (stepAwaiting). The ticket arrived 50 s after its request left;
// a confirmation is begun 26 s before the earliest time the PC can begin to switch itself off and never comes
// back (90 s); in that time the PC still shows "armed" for 50 s, and then counts down. The watch begins with
// the countdown already running. A helper that stops 40 s before that countdown's end is not the PC going
// down: counted from the arm request alone, the end would be 50 s too early, and these answers "its last".
func TestTheLastArmedAnswerCountsAlsoWhileAConfirmationIsOnItsWay(t *testing.T) {
	var armAt time.Duration // steady: the arm request left
	begun := false
	h, pc := goDown(t, lastWords{
		set:    func(pc *machine) { pc.chk.armLost, pc.chk.armLate = "late", 50*time.Second },
		askFor: 1500, // the run still asks when the PC is about to switch itself off
		each: func(w *world, pc *machine) {
			a := active(w)
			if a == nil {
				return
			}
			if armAt == 0 && strings.Contains(w.callsFor("pc-a"), "arm") {
				armAt = w.mono
			}
			if armAt != 0 && !begun && w.mono >= armAt+a.Ticket.wait()-26*time.Second {
				begun = true
				if got, err := w.m.BeginConfirm(a.ID, "Ann", []string{"pc-a"}); err != nil || len(got) != 1 {
					t.Fatalf("the rig: the confirmation was not begun: %v %v", got, err)
				}
			}
		},
		mute: []spanOf{{-40 * time.Second, 0}}, down: sec(30)})
	if !begun || !pc.on || h.State != StLeftOn || h.On != OnUnknown || !strings.HasSuffix(h.Text, saidNothing) {
		t.Fatalf("[%s] power %q (begun: %v, the PC is on: %v): %s\n  history: %s", h.State, h.On, begun, pc.on, h.Text, logOf(h))
	}
	if !logHas(h, "the panel did not learn what came of its confirmation") || logHas(h, "Nobody confirmed in time: it is switching itself off") {
		t.Fatalf("the rig: the watch did not begin after the confirmation's time, with the countdown running:%s", logOf(h))
	}
}
