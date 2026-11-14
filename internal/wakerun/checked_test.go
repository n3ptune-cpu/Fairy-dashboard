package wakerun

import (
	"strings"
	"testing"
	"time"
)

// A machine that checks passkey approvals itself, the whole way: woken, armed with the ticket of the start
// (one request), the run asks once for the second confirmation when it is up, a person confirms, the machine
// accepts that itself; then its update and its shutdown, each naming the run. Six requests, in that order.
func TestCheckedMachineConfirmedUpdatedAndSwitchedOff(t *testing.T) {
	w := newWorld(t)
	pc := w.checkedOff("pc-a")
	r := w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
	if r.Ticket == nil || r.Ticket.ValidS != 420 || r.Ticket.WaitS != 960 || r.Ticket.NoticeS != 120 || !host(r, "pc-a").Checked {
		t.Fatalf("the run's ticket: %+v", r.Ticket)
	}
	act := w.waitFor(5*time.Minute, "it waits for the confirmation", func(r *Run) bool { return r.Approval == "needed" })
	h := host(act, "pc-a")
	if h.State != StApproval || h.Arm != ArmArmed || h.ArmedAt == nil || h.SelfOffAt == nil || h.To != "17-bbbbbbb" || act.ConfirmBy == nil {
		t.Fatalf("waiting: %+v", h)
	}
	if got := h.SelfOffAt.Sub(*h.ArmedAt); got != 960*time.Second {
		t.Fatalf("it switches itself off %s after it was armed, the ticket says 960 s", got)
	}
	if !strings.HasPrefix(h.Text, "awake and armed: it switches itself off at about "+clock(*h.SelfOffAt)+" unless you confirm") {
		t.Fatalf("the row: %q", h.Text)
	}
	if got := w.callsFor("pc-a"); got != "wake arm" {
		t.Fatalf("before the confirmation the run sent %q", got)
	}
	if ws, err := w.m.Waiting(act.ID); err != nil || len(ws) != 1 || ws[0].To != "17-bbbbbbb" || ws[0].ShutdownDelayS != 120 || ws[0].WithinS != 1560 || !ws[0].Armed {
		t.Fatalf("what can be confirmed: %+v %v", ws, err)
	}
	w.advance(3 * time.Minute) // the admin takes a while
	if got := w.callsFor("pc-a"); got != "wake arm" {
		t.Fatalf("while it waits the run sent %q", got)
	}
	if begun, err := w.confirm("Ann"); err != nil || len(begun) != 1 {
		t.Fatalf("the confirmation: %v %v", begun, err)
	}
	done := w.finish()
	h = host(done, "pc-a")
	has(t, h, StOff, "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.")
	if got := w.callsFor("pc-a"); got != "wake arm go update shutdown" {
		t.Fatalf("requests: %q", got)
	}
	if got := w.steps("pc-a"); got != "wake:ok arm:ok confirm:ok update:ok shutdown:ok" {
		t.Fatalf("the audit steps: %q", got)
	}
	if done.Approval != "given" || done.ConfirmedBy != "Ann" || pc.on || h.Confirm != ConfirmOK {
		t.Fatalf("the run: approval %q by %q", done.Approval, done.ConfirmedBy)
	}
	asked, over := 0, 0
	for _, e := range w.events {
		switch e.Kind {
		case EvApproval:
			asked++
		case EvApprovalEnd:
			over++
		}
	}
	if asked != 1 || over != 1 {
		t.Fatalf("the run asked %d times and ended the question %d times", asked, over)
	}
}

// Nobody gives the second confirmation: the armed machine switches itself off, the run sends it nothing at
// all after the ticket, and says "off" only when it has seen it go.
func TestNobodyConfirmsAndTheArmedMachineSwitchesItselfOff(t *testing.T) {
	w := newWorld(t)
	pc := w.checkedOff("pc-a")
	w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
	act := w.waitFor(20*time.Minute, "it is switching itself off", func(r *Run) bool { return stateOf(r, "pc-a") == StSelfOff })
	if act.Approval != "over" || !pc.on {
		t.Fatalf("the question is over (%q) and the machine still counts its notice (on=%v)", act.Approval, pc.on)
	}
	if h := host(act, "pc-a"); !strings.HasPrefix(h.Text, "Nobody confirmed within 10 min: it is switching itself off (a notice of 2 min counts on it from about ") {
		t.Fatalf("the row: %q", h.Text)
	}
	if _, err := w.confirm("Ann"); err == nil || !strings.Contains(err.Error(), "does not ask for a confirmation (any more)") {
		t.Fatalf("a confirmation after its time: %v", err)
	}
	done := w.finish()
	has(t, host(done, "pc-a"), StOff, "It came online after the wake. Nobody confirmed within 10 min: not confirmed, not updated. It switched itself off again.")
	if got := w.callsFor("pc-a"); got != "wake arm" {
		t.Fatalf("requests: %q", got)
	}
	if pc.on || !pc.wentOff || host(done, "pc-a").On != OnNo || pc.version != "16-aaaaaaa" {
		t.Fatalf("the machine: on=%v version %s", pc.on, pc.version)
	}
	if got := w.steps("pc-a"); got != "wake:ok arm:ok self_off:ok" {
		t.Fatalf("the audit steps: %q", got)
	}
}

// R1: the ticket goes only to a machine the run's own proof says it woke (the evidence of mayShutDown), with
// nothing pending and no guests. Every other machine is not armed, waits for the confirmation all the same,
// and is ON when nobody gives it.
func TestArmOnlyWithTheRunsOwnProof(t *testing.T) {
	for name, c := range map[string]struct {
		set func(w *world, mc *machine)
		why string
	}{
		"its wake request got no answer": {func(_ *world, mc *machine) { mc.wakeLost = "arrives"; mc.wakeBoots = true },
			"the run's wake request for it got no answer, so Fairy cannot tell that the run woke it"},
		"it reports no uptime": {func(_ *world, mc *machine) { mc.noUptime = true }, "its agent reports no uptime, so Fairy cannot tell that it started after the wake"},
		"a guest runs on it":   {func(_ *world, mc *machine) { mc.guests, mc.guestNames = 1, "vm-1" }, "1 guest is running on it (vm-1)"},
		"its guests are unseen": {func(_ *world, mc *machine) { mc.guestsWhy = "its Proxmox API has not answered since it came online" },
			"Fairy cannot see whether guests run on it (its Proxmox API has not answered since it came online)"},
		"a list of its guests never comes": {func(_ *world, mc *machine) { mc.list, mc.listDown = true, true }, "Fairy cannot see whether guests run on it"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			pc := w.checkedOff("pc-a")
			if name == "its wake request got no answer" {
				// every wake request of the run goes unanswered; the first one does start the machine
				w.during = func(call string) {
					if call == "wake pc-a" && !pc.on {
						pc.on, pc.bootedAt, pc.agentReadyAt = true, w.now.Add(pc.post), w.now.Add(pc.bootTakes)
					}
				}
				pc.wakeLost = "always"
			} else {
				c.set(w, pc)
			}
			w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
			act := w.waitFor(8*time.Minute, "it waits", func(r *Run) bool { return stateOf(r, "pc-a") == StApproval })
			h := host(act, "pc-a")
			if h.Arm != ArmNotSent || !strings.Contains(h.ArmText, c.why) || !strings.HasPrefix(h.Text, "awake; it was not armed (") || !strings.HasSuffix(h.Text, "): it stays ON unless you confirm") {
				t.Fatalf("not armed: %q %q / %q", h.Arm, h.ArmText, h.Text)
			}
			done := w.finish()
			has(t, host(done, "pc-a"), StLeftOn, "Nobody confirmed within 10 min: not confirmed, not updated. It was not armed ("+c.why, "It is ON: nothing of this run switches it off.")
			if strings.Contains(w.callsFor("pc-a"), "arm") || strings.Contains(w.callsFor("pc-a"), "update") || strings.Contains(w.callsFor("pc-a"), "shutdown") {
				t.Fatalf("requests: %q", w.callsFor("pc-a"))
			}
			if !pc.on {
				t.Fatal("it went off")
			}
		})
	}
	// mayArm reads the evidence itself: every "no" of the proof is its "no"
	yes, up := true, int64(30)
	at := func(s int) *time.Time { x := t0.Add(time.Duration(s) * time.Second); return &x }
	good := func() *Host {
		return &Host{Mode: ModeWake, StartStatus: "offline", QuietForS: ptr(int64(3600)), ProbedOff: true, WakeSentAt: at(0), Wakes: 1, OnlineAt: at(40), BootUptimeS: &up,
			wakeMono: 0, onlineMono: 40 * time.Second}
	}
	info, obs := HostInfo{OptedIn: true}, Observation{Online: true}
	none := func() (int, bool, string) { return 0, true, "" }
	if ok, why := mayArm(good(), info, obs, none); !ok {
		t.Fatalf("a machine the run woke: %s", why)
	}
	_ = yes
	for name, change := range map[string]func(h *Host, i *HostInfo, o *Observation){
		"chosen as on":                func(h *Host, _ *HostInfo, _ *Observation) { h.Mode = ModeUpdateOnly },
		"not fresh":                   func(h *Host, _ *HostInfo, _ *Observation) { h.NotFresh = true },
		"already on":                  func(h *Host, _ *HostInfo, _ *Observation) { h.AlreadyOn = true },
		"not offline at the start":    func(h *Host, _ *HostInfo, _ *Observation) { h.StartStatus = "stale" },
		"not quiet long enough":       func(h *Host, _ *HostInfo, _ *Observation) { h.QuietForS = ptr(int64(60)) },
		"not asked directly":          func(h *Host, _ *HostInfo, _ *Observation) { h.ProbedOff = false },
		"no wake":                     func(h *Host, _ *HostInfo, _ *Observation) { h.WakeSentAt, h.Wakes = nil, 0 },
		"only an unanswered wake":     func(h *Host, _ *HostInfo, _ *Observation) { h.Wakes, h.WakeUnsure = 0, true },
		"not seen online":             func(h *Host, _ *HostInfo, _ *Observation) { h.OnlineAt = nil },
		"no uptime":                   func(h *Host, _ *HostInfo, _ *Observation) { h.BootUptimeS = nil },
		"an uptime that cannot be":    func(h *Host, _ *HostInfo, _ *Observation) { h.BootUptimeS = ptr(int64(-4)) },
		"up for longer than the wake": func(h *Host, _ *HostInfo, _ *Observation) { h.BootUptimeS = ptr(int64(41)) },
		"up for longer, steady clock": func(h *Host, _ *HostInfo, _ *Observation) { h.onlineMono = 20 * time.Second },
		"switched off for runs":       func(_ *Host, i *HostInfo, _ *Observation) { i.OptedIn = false },
		"not answering":               func(_ *Host, _ *HostInfo, o *Observation) { o.Online = false },
		"something pending":           func(_ *Host, _ *HostInfo, o *Observation) { o.Pending = &Pending{Action: "reboot", At: t0} },
	} {
		h, i, o := good(), info, obs
		change(h, &i, &o)
		if ok, why := mayArm(h, i, o, none); ok || why == "" {
			t.Errorf("%s: armed", name)
		}
	}
	if ok, _ := mayArm(good(), info, obs, func() (int, bool, string) { return 1, true, "vm" }); ok {
		t.Error("a node with a guest: armed")
	}
	if ok, _ := mayArm(good(), info, obs, func() (int, bool, string) { return 0, false, "why" }); ok {
		t.Error("a node whose guests the panel cannot see: armed")
	}
}

func ptr[T any](v T) *T { return &v }

// Every reason a machine gives for not taking the ticket: it is not armed, the reason is in its row and in
// its result, the ticket is not sent again, and the machine is ON when nobody confirms.
func TestArmRefusedForEachReason(t *testing.T) {
	for _, reason := range []string{"clock_back", "not_fresh", "started_before", "before_record", "already_armed", "run_used", "power_pending", "limits", "other_run", "clock"} {
		t.Run(reason, func(t *testing.T) {
			w := newWorld(t)
			pc := w.checkedOff("pc-a")
			pc.chk.armSays = reason
			w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
			w.waitFor(8*time.Minute, "it waits", func(r *Run) bool { return stateOf(r, "pc-a") == StApproval })
			w.advance(30 * time.Second) // "stays ON" is said once its agent, asked after the refusal, has shown no run (ownword_test.go)
			act, _ := w.m.Snapshot()
			h := host(act, "pc-a")
			if h.Arm != ArmRefused || h.Text != "awake; it could not be armed (run refused ("+reason+")): it stays ON unless you confirm" || h.SelfOffAt != nil {
				t.Fatalf("refused: %q / %q", h.Arm, h.Text)
			}
			done := w.finish()
			has(t, host(done, "pc-a"), StLeftOn, "It came online after the wake. Nobody confirmed within 10 min: not confirmed, not updated. It could not be armed (run refused ("+reason+")). It is ON: nothing of this run switches it off.")
			if got := w.callsFor("pc-a"); got != "wake arm" || pc.chk.arms != 1 || !pc.on {
				t.Fatalf("requests: %q (arms %d, on %v)", got, pc.chk.arms, pc.on)
			}
		})
	}
}

// A machine that was not armed can still be confirmed: the confirmation is the machine's own, and it lets the
// update and the shutdown through.
func TestAMachineThatCouldNotBeArmedIsConfirmedAllTheSame(t *testing.T) {
	w := newWorld(t)
	pc := w.checkedOff("pc-a")
	pc.chk.armSays = "clock_back"
	w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
	w.waitFor(8*time.Minute, "asked", func(r *Run) bool { return r.Approval == "needed" })
	if ws, err := w.m.Waiting(w.runID()); err != nil || len(ws) != 1 || ws[0].Armed || ws[0].SelfOffAt != nil {
		t.Fatalf("waiting: %+v %v", ws, err)
	}
	if _, err := w.confirm("Ann"); err != nil {
		t.Fatal(err)
	}
	done := w.finish()
	has(t, host(done, "pc-a"), StOff, "updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.")
	if got := w.callsFor("pc-a"); got != "wake arm go update shutdown" || pc.on {
		t.Fatalf("requests: %q", got)
	}
}

// R4: a ticket whose request got no answer is never sent again. What the machine's agent shows decides: armed
// (the request arrived, at once or late), or nothing (it never arrived).
func TestAnArmThatGotNoAnswerIsNotSentAgain(t *testing.T) {
	t.Run("it arrived", func(t *testing.T) {
		w := newWorld(t)
		pc := w.checkedOff("pc-a")
		pc.chk.armLost = "arrives"
		w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
		act := w.waitFor(8*time.Minute, "its agent shows it as armed", func(r *Run) bool { return host(r, "pc-a").SelfOffByMachine })
		h := host(act, "pc-a")
		if h.Arm != ArmUnanswered || h.SelfOffAt == nil || !strings.Contains(h.Text, "awake and armed: it switches itself off at "+clock(*h.SelfOffAt)+" by the machine's own clock unless you confirm") {
			t.Fatalf("%q %q", h.Arm, h.Text)
		}
		done := w.finish()
		has(t, host(done, "pc-a"), StOff, "Nobody confirmed within 10 min: not confirmed, not updated. It switched itself off again.")
		if got := w.callsFor("pc-a"); got != "wake arm" || pc.chk.arms != 1 || pc.on {
			t.Fatalf("requests: %q", got)
		}
	})
	t.Run("it arrived late", func(t *testing.T) {
		w := newWorld(t)
		pc := w.checkedOff("pc-a")
		pc.chk.armLost, pc.chk.armLate = "late", 150*time.Second
		w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
		w.waitFor(8*time.Minute, "its agent shows it as armed", func(r *Run) bool { return host(r, "pc-a").SelfOffByMachine })
		done := w.finish()
		has(t, host(done, "pc-a"), StOff, "It switched itself off again.")
		if got := w.callsFor("pc-a"); got != "wake arm" || pc.on {
			t.Fatalf("requests: %q", got)
		}
	})
	t.Run("it never arrived", func(t *testing.T) {
		w := newWorld(t)
		pc := w.checkedOff("pc-a")
		pc.chk.armLost = "never"
		w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
		act := w.waitFor(8*time.Minute, "it waits", func(r *Run) bool { return stateOf(r, "pc-a") == StApproval })
		if h := host(act, "pc-a"); h.Text != "awake; the arming got no answer, so Fairy does not know yet whether it is armed: waiting for you to confirm" {
			t.Fatalf("%q", h.Text)
		}
		done := w.finish()
		has(t, host(done, "pc-a"), StLeftOn, "Nobody confirmed within 10 min: not confirmed, not updated. Its arming got no answer (agent unreachable: timeout), and its agent never showed the run: it is not armed. It is ON: nothing of this run switches it off.")
		if got := w.callsFor("pc-a"); got != "wake arm" || !pc.on {
			t.Fatalf("requests: %q on=%v", got, pc.on)
		}
		w.advance(30 * time.Minute)
		if !pc.on {
			t.Fatal("it went off after the run said that nothing switches it off")
		}
	})
}

// R4: the ticket is good for 7 minutes. A machine that is up later is not armed, nothing is sent to it, and
// the sentence says why.
func TestAMachineThatIsUpAfterTheTicketsTimeIsNotArmed(t *testing.T) {
	w := newWorld(t)
	w.m.Settings = NewSettings(900, 60, 120, 8)
	pc := w.checkedOff("pc-a")
	pc.bootTakes = 500 * time.Second
	w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
	act := w.waitFor(12*time.Minute, "it waits", func(r *Run) bool { return stateOf(r, "pc-a") == StApproval })
	h := host(act, "pc-a")
	if h.Arm != ArmNotSent || h.Text != "awake; it was not armed (the ticket of the start was older than 7 min when it was up): it stays ON unless you confirm" || pc.chk.arms != 0 {
		t.Fatalf("%q %q (requests that reached it: %d)", h.Arm, h.Text, pc.chk.arms)
	}
	done := w.finish()
	has(t, host(done, "pc-a"), StLeftOn, "It was not armed (the ticket of the start was older than 7 min when it was up). It is ON")
}

// R7: a run is confirmed once: by the first confirmation a machine accepts. No second one is asked for by
// itself, whatever came of the first, and a machine it did not reach is not updated. A confirmation that no
// machine accepted leaves the question open for a person to try again (tryagain_test.go), except for a machine
// whose answer says that no confirmation can help it any more (too late, the run is over on it, another run).
func TestTheRunIsConfirmedOnce(t *testing.T) {
	for name, c := range map[string]struct {
		set   func(mc *machine)
		state string
		parts []string
		calls string
		on    bool
	}{
		"too late": {func(mc *machine) { mc.chk.goSays = "too_late" }, StOff,
			[]string{"Its confirmation was not accepted (run refused (too_late)): not confirmed, not updated. It switched itself off again."}, "wake arm go", false},
		"the run is over on it": {func(mc *machine) { mc.chk.goSays = "run_over" }, StOff,
			[]string{"Its confirmation was not accepted (run refused (run_over)): not confirmed, not updated. It switched itself off again."}, "wake arm go", false},
		"another run": {func(mc *machine) { mc.chk.goSays = "other_run" }, StOff, []string{"run refused (other_run)", "It switched itself off again."}, "wake arm go", false},
		"limits":      {func(mc *machine) { mc.chk.goSays = "limits" }, StOff, []string{"run refused (limits)", "It switched itself off again."}, "wake arm go", false},
		"no answer, and it did not arrive": {func(mc *machine) { mc.chk.goLost = "never" }, StOff,
			[]string{"Its confirmation got no answer (agent unreachable: timeout), and its agent did not show the run as confirmed: not confirmed, not updated. It switched itself off again."}, "wake arm go", false},
		"no answer, and it arrived": {func(mc *machine) { mc.chk.goLost = "arrives" }, StOff, []string{"updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again."}, "wake arm go update shutdown", false},
		"refused, and it was not armed": {func(mc *machine) { mc.chk.goSays, mc.chk.armSays = "run_over", "already_armed" }, StLeftOn,
			[]string{"Its confirmation was not accepted (run refused (run_over)): not confirmed, not updated. It could not be armed (run refused (already_armed)). It is ON"}, "wake arm go", true},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			pc := w.checkedOff("pc-a")
			c.set(pc)
			w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
			w.waitFor(8*time.Minute, "asked", func(r *Run) bool { return r.Approval == "needed" })
			if _, err := w.confirm("Ann"); err != nil {
				t.Fatal(err)
			}
			if name == "limits" { // no machine accepted, and nothing says that a confirmation cannot help: the question is open
				if act, _ := w.m.Snapshot(); act.Approval != "needed" || host(act, "pc-a").Confirm != "" {
					t.Fatalf("after a confirmation no machine accepted: %q / %q", act.Approval, host(act, "pc-a").Confirm)
				}
				done := w.finish() // nobody tries again
				has(t, host(done, "pc-a"), c.state, "The confirmation by Ann was not accepted (run refused (limits)), and nobody confirmed after that: not confirmed, not updated. It switched itself off again.")
				if got := w.callsFor("pc-a"); got != c.calls || pc.chk.gos != 1 {
					t.Fatalf("requests: %q, confirmations sent: %d", got, pc.chk.gos)
				}
				return
			}
			w.tick()
			if _, err := w.confirm("Ann"); err == nil {
				t.Fatal("a second confirmation was taken")
			}
			if _, err := w.m.BeginConfirm(w.runID(), "Bob", []string{"pc-a"}); err == nil {
				t.Fatal("a second BeginConfirm")
			}
			done := w.finish()
			has(t, host(done, "pc-a"), c.state, c.parts...)
			if got := w.callsFor("pc-a"); got != c.calls || pc.on != c.on || pc.chk.gos != 1 {
				t.Fatalf("requests: %q on=%v confirmations sent: %d", got, pc.on, pc.chk.gos)
			}
		})
	}
}

// A machine is no longer offered 20 s before it switches itself off, and what is not offered gets nothing. The
// run's question ends at that moment of the first machine that waits (confirmby_test.go), so that the strip
// never counts a time in which one of them can no longer be confirmed; the rule for the single machine stays.
func TestAMachineIsNotOfferedShortlyBeforeItSwitchesItselfOff(t *testing.T) {
	w := newWorld(t)
	w.checkedOff("pc-a")
	b := w.checkedOff("pc-b")
	b.bootTakes = 290 * time.Second // armed almost five minutes after pc-a
	// (a ticket with a shorter wait than the panel signs, so that pc-a's time comes before the configured wait is over)
	w.start(StartRequest{Hosts: []string{"pc-a", "pc-b"}, Latest: "17-bbbbbbb", Ticket: &Ticket{ValidS: 420, WaitS: 700, NoticeS: 120, WithinS: 1560, ConfirmWaitS: 600}})
	act := w.waitFor(8*time.Minute, "asked", func(r *Run) bool { return r.Approval == "needed" })
	first := *host(act, "pc-a").SelfOffAt
	if want := first.Add(-confirmMargin); act.ConfirmBy.Sub(want).Abs() > 2*time.Second {
		t.Fatalf("the question ends at %s; pc-a can be confirmed until %s", clock(*act.ConfirmBy), clock(want))
	}
	for w.now.Before(first.Add(-confirmMargin - 3*time.Second)) {
		w.tick()
	}
	if ws, err := w.m.Waiting(w.runID()); err != nil || len(ws) != 2 {
		t.Fatalf("3 s before the question ends: %+v %v", ws, err)
	}
	// the rule for the single machine, by itself: from 20 s before its own time on it is not offered
	live := w.m.active
	pa, pb := host(live, "pc-a"), host(live, "pc-b")
	if at := pa.chk.offMin - confirmMargin; !pa.offerable(live, at) || pa.offerable(live, at+time.Second) || !pb.offerable(live, at+time.Second) {
		t.Fatalf("offerable around pc-a's limit: pc-a %v then %v, pc-b %v", pa.offerable(live, at), pa.offerable(live, at+time.Second), pb.offerable(live, at+time.Second))
	}
	for w.now.Before(first.Add(-10 * time.Second)) {
		w.tick()
	}
	if ws, err := w.m.Waiting(w.runID()); err == nil {
		t.Fatalf("10 s before pc-a goes the run still offers: %+v", ws)
	}
	if _, err := w.m.BeginConfirm(w.runID(), "Ann", []string{"pc-a", "pc-b"}); err == nil {
		t.Fatal("a confirmation was begun 10 s before pc-a goes")
	}
	done := w.finish()
	for _, id := range []string{"pc-a", "pc-b"} {
		has(t, host(done, id), StOff, "Nobody confirmed within "+span(time.Duration(done.ConfirmForS)*time.Second)+": not confirmed, not updated. It switched itself off again.")
		if got := w.callsFor(id); !strings.HasSuffix(got, "wake arm") || strings.Contains(got, "go") || strings.Contains(got, "update") || strings.Contains(got, "shutdown") {
			t.Fatalf("%s: %q", id, got)
		}
	}
}

// R8: the run's one update is asked for once. If it does not work the run goes on to the shutdown and says
// "NOT updated"; a request the machine no longer lets through is not repeated either.
func TestTheRunsUpdateIsAskedForOnce(t *testing.T) {
	t.Run("it fails", func(t *testing.T) {
		w := newWorld(t)
		pc := w.checkedOff("pc-a")
		pc.updMode = "fail"
		w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
		w.waitFor(8*time.Minute, "asked", func(r *Run) bool { return r.Approval == "needed" })
		if _, err := w.confirm("Ann"); err != nil {
			t.Fatal(err)
		}
		done := w.finish()
		has(t, host(done, "pc-a"), StOff, "NOT updated (downloading the release: connection reset), and switched off again.")
		if got := w.callsFor("pc-a"); got != "wake arm go update shutdown" || pc.on {
			t.Fatalf("requests: %q", got)
		}
	})
	t.Run("the machine does not let it through", func(t *testing.T) {
		w := newWorld(t)
		pc := w.checkedOff("pc-a")
		w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
		w.waitFor(8*time.Minute, "asked", func(r *Run) bool { return r.Approval == "needed" })
		w.during = func(call string) {
			if call == "update pc-a" {
				pc.chk.updates = 0 // used up on the machine (somebody was faster), or its time is over
			}
		}
		if _, err := w.confirm("Ann"); err != nil {
			t.Fatal(err)
		}
		done := w.finish()
		has(t, host(done, "pc-a"), StOff, "NOT updated (its agent did not let the run's update through (approval required: confirm this action with Pocket ID; the run it names does not cover it: its one update was used)), and switched off again.")
		if got := w.callsFor("pc-a"); got != "wake arm go update shutdown" || pc.on {
			t.Fatalf("requests: %q", got)
		}
	})
	t.Run("the machine does not let the shutdown through", func(t *testing.T) {
		w := newWorld(t)
		pc := w.checkedOff("pc-a")
		w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
		w.waitFor(8*time.Minute, "asked", func(r *Run) bool { return r.Approval == "needed" })
		w.during = func(call string) {
			if call == "shutdown pc-a" {
				pc.chk.withinEnds = 0 // the time the confirmation named is over
				w.runTick(pc)
			}
		}
		if _, err := w.confirm("Ann"); err != nil {
			t.Fatal(err)
		}
		done := w.finish()
		has(t, host(done, "pc-a"), StLeftOn, "Left ON: its agent did not let the run's shutdown through (approval required: confirm this action with Pocket ID; the run it names does not cover it: no run is confirmed on this machine in this boot). "+
			"The confirmation covers one shutdown, for 26 min and until the machine is started again. It was updated to 17-bbbbbbb.")
		if got := w.callsFor("pc-a"); got != "wake arm go update shutdown" || !pc.on {
			t.Fatalf("requests: %q", got)
		}
	})
	t.Run("nothing newer, no version named", func(t *testing.T) {
		w := newWorld(t)
		pc := w.checkedOff("pc-a")
		no := false
		pc.behind = &no
		w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "16-aaaaaaa"})
		act := w.waitFor(8*time.Minute, "asked", func(r *Run) bool { return r.Approval == "needed" })
		if host(act, "pc-a").To != "-" {
			t.Fatalf("to: %q", host(act, "pc-a").To)
		}
		if _, err := w.confirm("Ann"); err != nil {
			t.Fatal(err)
		}
		done := w.finish()
		has(t, host(done, "pc-a"), StOff, "already on the newest release (16-aaaaaaa), and switched off again.")
		if got := w.callsFor("pc-a"); got != "wake arm go shutdown" || pc.on {
			t.Fatalf("requests: %q", got)
		}
	})
	t.Run("the panel knows no release", func(t *testing.T) {
		w := newWorld(t)
		pc := w.checkedOff("pc-a")
		pc.behind = nil
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		w.waitFor(8*time.Minute, "asked", func(r *Run) bool { return r.Approval == "needed" })
		if _, err := w.confirm("Ann"); err != nil {
			t.Fatal(err)
		}
		done := w.finish()
		has(t, host(done, "pc-a"), StOff, "not updated (the panel did not know the newest signed release when the run started, so the confirmation names no version), and switched off again.")
		if got := w.callsFor("pc-a"); got != "wake arm go shutdown" || pc.on {
			t.Fatalf("requests: %q", got)
		}
	})
}
