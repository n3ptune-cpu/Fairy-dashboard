package wakerun

import (
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Random runs with machines that check passkey approvals themselves, next to machines that do not: links that
// drop, answers that are lost (to the ticket, to the confirmation, to a cancel), machines that refuse, that are
// started again, whose helper stops, on which a guest starts; an admin who confirms at some moment or never,
// cancels the run either way, stops everything, or keeps one machine on at the machine itself; and the
// panel's clock set forward or back. The world's judge is on throughout (RULE BROKEN: a ticket to a machine the
// run did not wake, a second ticket, an update or a shutdown to a machine with the check without a
// confirmation that machine accepted, a second confirmation). On top of it, half an hour after each run:
//
//  1. a machine reported as off is off, and went off by a countdown;
//  2. a machine with the check reported as ON that its own timer switched off has a sentence that says it may;
//  3. a machine with the check that accepted no confirmation runs the version it had (nothing updated it);
//  4. after "Cancel run" with the machines left on, nothing but cancels is sent to any machine;
//  5. after such a cancel with a working link, no machine with the check goes off;
//  6. every machine ends with a sentence, and the run ends.
//
// 300 runs with every test run; FAIRY_RANDOM_RUNS and FAIRY_RANDOM_FROM as for the other random runs.
func TestRandomRunsWithMachinesThatCheckApprovals(t *testing.T) {
	runs := 300
	if n, err := strconv.Atoi(os.Getenv("FAIRY_RANDOM_RUNS")); err == nil && n > 0 {
		runs = n
	}
	if testing.Short() {
		runs = 60
	}
	bad := 0
	seen := map[string]int{} // how the machines with the check ended, over all runs (logged: what the runs covered)
	defer func() { t.Logf("%d runs; machines with the check: %s", runs, sorted(seen)) }()
	for seed := randomFrom(); seed < randomFrom()+runs && bad < 8; seed++ {
		rng := rand.New(rand.NewSource(int64(seed)*104729 + 7))
		w, ahead := jumpWorld(t)
		secs := func(lo, hi int) time.Duration { return time.Duration(lo+rng.Intn(hi-lo+1)) * time.Second }
		pick := func(xs ...string) string { return xs[rng.Intn(len(xs))] }
		type later struct {
			at time.Duration
			do func()
		}
		var plan []later
		clean := map[string]bool{} // nothing stands between the panel and this machine, and nothing happens on it
		n := 1 + rng.Intn(3)
		req := StartRequest{UpdateIfOn: rng.Intn(2) == 0, Latest: "17-bbbbbbb"}
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("m%d", i)
			mc := w.off(id)
			clean[id] = true
			if rng.Intn(3) != 0 {
				mc.chk.on, mc.chk.used = true, map[string]bool{}
			}
			mc.bootTakes = secs(9, 200)
			mc.post = time.Duration(rng.Int63n(int64(mc.bootTakes)))
			mc.updMode = pick("ok", "ok", "ok", "current", "fail", "noverdict", "rollback")
			mc.updTakes = secs(5, 120)
			mc.shutMode = pick("ok", "ok", "ok", "ok", "hang", "busy")
			if rng.Intn(3) == 0 {
				mc.info.ShutdownDelay = secs(10, 600)
			}
			dirty := func() { clean[id] = false }
			if mc.chk.on {
				switch rng.Intn(10) {
				case 0:
					mc.chk.armLost, mc.chk.armLate = pick("arrives", "never", "late"), secs(1, 500)
					dirty()
				case 1:
					mc.chk.armSays = pick("clock_back", "not_fresh", "started_before", "before_record", "already_armed", "run_used", "power_pending", "limits", "other_run", "clock")
				case 2:
					mc.chk.goLost = pick("arrives", "never")
					dirty()
				case 3:
					mc.chk.goSays = pick("too_late", "run_over", "other_run", "limits", "already_confirmed", "twice")
					if mc.chk.goSays == "twice" { // the confirmation reaches it twice: confirmed, and the panel is told "already_confirmed"
						mc.chk.goSays, mc.chk.goTwice = "", true
					}
				case 4:
					mc.cancelLost = pick("arrives", "never", "always", "always arrives")
					dirty()
				case 5:
					mc.chk.cancelSays = "the pending action is stopped, but the end of the run could not be written down; cancel again"
					dirty()
				}
				if rng.Intn(8) == 0 { // the helper stops, and is started again or not
					from := secs(30, 1300)
					plan = append(plan, later{from, func() { mc.chk.helperStopped = true }})
					if rng.Intn(2) == 0 {
						plan = append(plan, later{from + secs(5, 600), func() { mc.chk.helperStopped = false }})
					}
					dirty()
				}
				if rng.Intn(10) == 0 { // the machine is started again
					plan = append(plan, later{secs(60, 1300), func() {
						if mc.on {
							mc.bootedAt, mc.agentReadyAt, mc.pending, mc.updating = w.now, w.now.Add(secs(5, 60)), nil, false
						}
					}})
					dirty()
				}
				if rng.Intn(10) == 0 { // its checker says nothing about runs for a while
					from := secs(30, 1300)
					plan = append(plan, later{from, func() { mc.chk.noRuns = true }}, later{from + secs(1, 120), func() { mc.chk.noRuns = false }})
					dirty()
				}
				if rng.Intn(8) == 0 { // somebody keeps it on at the machine itself
					plan = append(plan, later{secs(60, 1300), func() {
						if mc.on {
							mc.pending = nil
							w.runCancelled(mc)
						}
					}})
					dirty()
				}
			}
			for k := rng.Intn(4); k > 1; k-- { // the link drops and comes back
				from, dur := secs(0, 1300), secs(1, 120)
				plan = append(plan, later{from, func() { mc.unreachable = true }}, later{from + dur, func() { mc.unreachable = false }})
				dirty()
			}
			if rng.Intn(7) == 0 {
				plan = append(plan, later{secs(0, 1300), func() { mc.guests = 1 }})
				dirty()
			}
			if rng.Intn(14) == 0 {
				plan = append(plan, later{secs(0, 1300), func() { mc.paused = true }})
				dirty()
			}
			if rng.Intn(14) == 0 {
				plan = append(plan, later{secs(0, 1300), func() { mc.info.Entry++ }})
				dirty()
			}
			req.Hosts = append(req.Hosts, id)
		}
		r := w.start(req)
		cancelAt, cancelCalls, leaveOn := time.Duration(-1), 0, false
		switch rng.Intn(8) {
		case 0, 1:
			off := rng.Intn(2) == 0
			plan = append(plan, later{secs(0, 1500), func() {
				if _, err := w.m.CancelRun(r.ID, "Bob", off); err == nil && cancelAt < 0 {
					cancelAt, cancelCalls, leaveOn = w.mono, len(w.calls), !off
				}
			}})
		case 2:
			plan = append(plan, later{secs(0, 1500), func() { w.blocked = "control is disabled by an administrator" }})
		}
		if rng.Intn(3) != 0 { // the admin confirms, some time after being asked (or tries to, too late)
			wait := secs(1, 700)
			var asked time.Duration
			confirmed := false
			plan = append(plan, later{0, nil})
			defer func() {}()
			w.afterTick = func() {
				a, _ := w.m.Snapshot()
				if a == nil || confirmed {
					return
				}
				if a.Approval == "needed" && asked == 0 {
					asked = w.mono
				}
				if asked != 0 && w.mono-asked >= wait {
					confirmed = true
					_, _ = w.confirm("Ann")
				}
			}
		}
		if rng.Intn(6) == 0 { // the panel's clock is set
			jump := pick("fwd", "back")
			plan = append(plan, later{secs(30, 1300), func() {
				if jump == "fwd" {
					*ahead = secs(60, 7200)
				} else {
					*ahead = -secs(60, 7200)
				}
			}})
		}
		startAt := w.mono
		step := func() {
			for j := range plan {
				if plan[j].do != nil && w.mono-startAt >= plan[j].at {
					plan[j].do()
					plan[j].do = nil
				}
			}
			w.tick()
		}
		ended := false
		for i := 0; i < 3*3600; i++ {
			if a, _ := w.m.Snapshot(); a == nil {
				ended = true
				break
			}
			step()
		}
		if !ended {
			a, _ := w.m.Snapshot()
			bad++
			t.Errorf("seed %d: the run did not end in three hours: %s", seed, describe(a))
			continue
		}
		_, hist := w.m.Snapshot()
		done := hist[0]
		w.afterTick = nil
		before := t.Failed()
		w.wellReported(done)
		if t.Failed() && !before {
			bad++
			t.Logf("seed %d: (the report above)", seed)
		}
		endCalls := len(w.calls)
		for i := 0; i < 1800; i++ {
			step()
		}
		if len(w.calls) != endCalls {
			bad++
			t.Errorf("seed %d: requests after the run was over: %v", seed, w.calls[endCalls:])
		}
		if cancelAt >= 0 && leaveOn { // 4.
			for _, c := range w.calls[cancelCalls:] {
				if !strings.HasPrefix(c, "cancel ") {
					bad++
					t.Errorf("seed %d: after \"Cancel run\" (machines left on) the run sent %q", seed, c)
				}
			}
		}
		for _, h := range done.Hosts {
			mc := w.ms[h.ID]
			if mc.chk.on {
				seen[h.State+"/"+h.On+"/arm:"+h.Arm+"/confirm:"+h.Confirm]++
			}
			fail := func(format string, args ...any) {
				bad++
				t.Errorf("seed %d: %s: %s\n  [%s] on=%q %s\n  requests: %s\n  machine: on=%v went off=%v checked=%v arm lost %q says %q, go lost %q says %q, cancel lost %q says %q\n  history: %s",
					seed, h.ID, fmt.Sprintf(format, args...), h.State, h.On, h.Text, w.callsFor(h.ID), mc.on, mc.wentOff, mc.chk.on, mc.chk.armLost, mc.chk.armSays,
					mc.chk.goLost, mc.chk.goSays, mc.cancelLost, mc.chk.cancelSays, logOf(h))
			}
			switch {
			case h.On == OnNo && (mc.on || !mc.wentOff): // 1.
				fail("reported OFF, and half an hour later it is on=%v (went off by a countdown: %v)", mc.on, mc.wentOff)
			case mc.chk.on && h.On == OnYes && !mc.on && mc.wentOff && !saysItMayGo(h.Text) && !strings.Contains(h.Text, "switches off"): // 2.
				fail("reported ON with no word that it may go, and it went off")
			}
			if mc.chk.on && len(mc.chk.goTaken) == 0 && !mc.chk.startApproved && mc.version != "16-aaaaaaa" { // 3.
				fail("it accepted no confirmation and runs %s", mc.version)
			}
			if mc.chk.on && cancelAt >= 0 && leaveOn && clean[h.ID] && mc.wentOff && mc.offAt.Sub(t0) > cancelAt+30*time.Second { // 5.
				fail("it went off after \"Cancel run\" with the machines left on, with a working link")
			}
			if mc.chk.on && (mc.chk.arms > 1 || mc.chk.gos > 1) {
				fail("%d tickets and %d confirmations were sent to it", mc.chk.arms, mc.chk.gos)
			}
		}
		if w.tk.confirms > 1 {
			bad++
			t.Errorf("seed %d: the run was confirmed %d times", seed, w.tk.confirms)
		}
	}
}
