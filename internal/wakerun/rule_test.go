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

// mayShutDown says yes for a machine with the whole proof, and no as soon as any one part of it is missing,
// with the reason for the report.
func TestMayShutDownNeedsEveryPartOfTheProof(t *testing.T) {
	wake := t0.Add(10 * time.Second)
	online := wake.Add(50 * time.Second)
	up := int64(30) // it reported 30 s of uptime 50 s after the wake: it started after the wake
	proven := func() (*Host, HostInfo, Observation) {
		w, o, u, q := wake, online, up, int64(MinOff/time.Second)
		return &Host{ID: "pc-a", Mode: ModeWake, StartStatus: "offline", QuietForS: &q, ProbedOff: true, WakeSentAt: &w, Wakes: 1, OnlineAt: &o, BootUptimeS: &u,
				wakeMono: 7 * time.Hour, onlineMono: 7*time.Hour + 51*time.Second}, // the steady clock: read a moment after the answer
			HostInfo{ID: "pc-a", OptedIn: true}, Observation{Online: true, Status: "online"}
	}
	noGuests := func() (int, bool, string) { return 0, true, "" }
	h, info, o := proven()
	if ok, why := mayShutDown(h, info, o, noGuests); !ok || why != "" {
		t.Fatalf("the whole proof: %v %q", ok, why)
	}
	early, late, zero := wake.Add(-time.Second), wake, int64(0)
	long, exact, negative := int64(3*86400), int64(50), int64(-5)
	almost, minute, none := int64(MinOff/time.Second)-1, int64(90), int64(0)
	for name, c := range map[string]struct {
		change func(h *Host, info *HostInfo, o *Observation)
		guests func() (int, bool, string)
		why    string
	}{
		"chosen as a machine that is on":        {change: func(h *Host, _ *HostInfo, _ *Observation) { h.Mode = ModeUpdateOnly }, why: "chosen as a machine that is on"},
		"an unknown mode":                       {change: func(h *Host, _ *HostInfo, _ *Observation) { h.Mode = "" }, why: "chosen as a machine that is on"},
		"marked as already on":                  {change: func(h *Host, _ *HostInfo, _ *Observation) { h.AlreadyOn = true }, why: "already on before this run did anything"},
		"online at the start":                   {change: func(h *Host, _ *HostInfo, _ *Observation) { h.StartStatus = "online" }, why: "did not show it as off when the run started"},
		"stale at the start":                    {change: func(h *Host, _ *HostInfo, _ *Observation) { h.StartStatus = "stale" }, why: "did not show it as off"},
		"never polled at the start":             {change: func(h *Host, _ *HostInfo, _ *Observation) { h.StartStatus = "" }, why: "did not show it as off"},
		"quiet for a second less than it takes": {change: func(h *Host, _ *HostInfo, _ *Observation) { h.QuietForS = &almost }, why: "not sure that it was off before the run: it had not gone 15 min without an answer"},
		"quiet for 90 s: it may be restarting":  {change: func(h *Host, _ *HostInfo, _ *Observation) { h.QuietForS = &minute }, why: "not sure that it was off before the run"},
		"quiet for no time at all":              {change: func(h *Host, _ *HostInfo, _ *Observation) { h.QuietForS = &none }, why: "not sure that it was off before the run"},
		"how long it was quiet is not known":    {change: func(h *Host, _ *HostInfo, _ *Observation) { h.QuietForS = nil }, why: "not sure that it was off before the run"},
		"not asked directly before the wake":    {change: func(h *Host, _ *HostInfo, _ *Observation) { h.ProbedOff = false }, why: "did not confirm that it was off right before the wake"},
		"no wake by this run":                   {change: func(h *Host, _ *HostInfo, _ *Observation) { h.WakeSentAt = nil }, why: "sent no wake for it"},
		"no wake accepted":                      {change: func(h *Host, _ *HostInfo, _ *Observation) { h.Wakes = 0 }, why: "sent no wake for it"},
		"never seen online":                     {change: func(h *Host, _ *HostInfo, _ *Observation) { h.OnlineAt = nil }, why: "not seen coming online after this run's wake"},
		"seen online before the wake":           {change: func(h *Host, _ *HostInfo, _ *Observation) { h.OnlineAt = &early }, why: "not seen coming online after this run's wake"},
		"seen online at the moment of the wake": {change: func(h *Host, _ *HostInfo, _ *Observation) { h.OnlineAt = &late }, why: "not seen coming online after this run's wake"},
		"no uptime reported":                    {change: func(h *Host, _ *HostInfo, _ *Observation) { h.BootUptimeS = nil }, why: "reports no uptime"},
		"up for three days":                     {change: func(h *Host, _ *HostInfo, _ *Observation) { h.BootUptimeS = &long }, why: "running for longer than since the wake: it was already on"},
		"up exactly as long as since the wake":  {change: func(h *Host, _ *HostInfo, _ *Observation) { h.BootUptimeS = &exact }, why: "running for longer than since the wake"},
		// the two times of day say 50 s since the wake, the steady clock says less: the clock was set forward
		"up longer than the steady clock says":  {change: func(h *Host, _ *HostInfo, _ *Observation) { h.onlineMono = h.wakeMono + 10*time.Second }, why: "running for longer than since the wake: it was already on"},
		"up exactly as long as it says":         {change: func(h *Host, _ *HostInfo, _ *Observation) { h.onlineMono = h.wakeMono + 30*time.Second }, why: "running for longer than since the wake"},
		"no steady clock reading of the answer": {change: func(h *Host, _ *HostInfo, _ *Observation) { h.onlineMono = 0 }, why: "running for longer than since the wake"},
		"no steady clock reading at all":        {change: func(h *Host, _ *HostInfo, _ *Observation) { h.wakeMono, h.onlineMono = 0, 0 }, why: "running for longer than since the wake"},
		"a negative uptime":                     {change: func(h *Host, _ *HostInfo, _ *Observation) { h.BootUptimeS = &negative }, why: "its agent reports an uptime that cannot be right (-5 s)"},
		"no longer switched on for it":          {change: func(_ *Host, i *HostInfo, _ *Observation) { i.OptedIn = false }, why: "no longer switched on for"},
		"its agent does not answer":             {change: func(_ *Host, _ *HostInfo, o *Observation) { o.Online = false }, why: "its agent is not answering right now"},
		"a power action is pending":             {change: func(_ *Host, _ *HostInfo, o *Observation) { o.Pending = &Pending{Action: "reboot", At: t0} }, why: "another power action is already pending on it (reboot at 03:00:00 UTC)"},
		"its agent downloads an update":         {change: func(_ *Host, _ *HostInfo, o *Observation) { o.UpdateState = "downloading" }, why: "its agent says that an update is being installed on it right now (downloading)"},
		"its agent applies an update":           {change: func(_ *Host, _ *HostInfo, o *Observation) { o.UpdateState = "applying" }, why: "its agent says that an update is being installed on it right now (applying)"},
		"a guest runs":                          {guests: func() (int, bool, string) { return 1, true, "win11" }, why: "1 guest is running on it (win11)"},
		"guests run":                            {guests: func() (int, bool, string) { return 4, true, "" }, why: "4 guests are running on it"},
		"guests unknown":                        {guests: func() (int, bool, string) { return 0, false, "no Proxmox API for it" }, why: "cannot see whether guests run on it (no Proxmox API for it)"},
	} {
		h, info, o := proven()
		if c.change != nil {
			c.change(h, &info, &o)
		}
		g := noGuests
		if c.guests != nil {
			g = c.guests
		}
		ok, why := mayShutDown(h, info, o, g)
		if ok || !strings.Contains(why, c.why) {
			t.Errorf("%s: allowed=%v, reason %q (want %q)", name, ok, why, c.why)
		}
	}
	// A wake request that left and got no answer is not a wake the run knows of: with the whole rest of the
	// proof, the answer is no (second review, P4) ...
	h, info, o = proven()
	h.Wakes, h.WakeUnsure = 0, true
	if ok, why := mayShutDown(h, info, o, noGuests); ok || why != "the run's wake request for it got no answer, so Fairy cannot tell that the run woke it" {
		t.Errorf("a wake whose answer was lost, and the whole rest of the proof: allowed=%v, reason %q", ok, why)
	}
	// ... and yes once a later wake request for it was accepted, when the machine started after THAT one
	// (second review of fix6, F1). The fixture: the first request left 60 s before the accepted one; the machine
	// answered 50 s after the accepted one with 30 s of uptime, as in the proof above.
	lateProven := func() (*Host, HostInfo, Observation) {
		h, info, o := proven()
		first, accepted := wake.Add(-60*time.Second), wake
		h.Wakes, h.WakeUnsure, h.WakeSentAt, h.WakeOKAt = 1, true, &first, &accepted
		h.wakeMono, h.wakeOKMono = 7*time.Hour-60*time.Second, 7*time.Hour
		return h, info, o
	}
	h, info, o = lateProven()
	if ok, why := mayShutDown(h, info, o, noGuests); !ok {
		t.Errorf("a first wake whose answer was lost, a later one accepted, and the whole rest of the proof: %q", why)
	}
	// Every measurement is taken from the accepted wake, not from the request that got no answer: each of
	// these is "started after the wake" against the lost request (110 s before the answer) and not against the
	// accepted one.
	const lateWhy = "the run's first wake request for it got no answer, and it was already starting when a later one was accepted, so Fairy cannot tell that the run woke it"
	up70, up50, up5 := int64(70), int64(50), int64(5)
	for name, change := range map[string]func(h *Host){
		"no time of an accepted wake is known":               func(h *Host) { h.WakeOKAt = nil },
		"online before the accepted wake":                    func(h *Host) { h.OnlineAt = &early },
		"online at the moment of the accepted wake":          func(h *Host) { h.OnlineAt = &late },
		"up for longer than since the accepted wake":         func(h *Host) { h.BootUptimeS = &up70 },
		"up exactly as long as since the accepted wake":      func(h *Host) { h.BootUptimeS = &up50 },
		"up longer than the steady clock says since it":      func(h *Host) { h.onlineMono = h.wakeOKMono + 20*time.Second },
		"up exactly as long as the steady clock says":        func(h *Host) { h.onlineMono = h.wakeOKMono + 30*time.Second },
		"the steady clock says the answer came before it":    func(h *Host) { h.onlineMono = h.wakeOKMono - time.Second },
		"answered 6 s after the accepted wake, up for 5 s":   func(h *Host) { h.BootUptimeS, h.onlineMono = &up5, h.wakeOKMono+6*time.Second },
		"answered 7.9 s after the accepted wake, up for 5 s": func(h *Host) { h.BootUptimeS, h.onlineMono = &up5, h.wakeOKMono+7900*time.Millisecond },
	} {
		h, info, o = lateProven()
		change(h)
		if ok, why := mayShutDown(h, info, o, noGuests); ok || why != lateWhy {
			t.Errorf("a first wake whose answer was lost, a later one accepted, %s: allowed=%v, reason %q", name, ok, why)
		}
	}
	// 8 s after the accepted wake with a fresh uptime is no longer "too soon"
	h, info, o = lateProven()
	h.BootUptimeS, h.onlineMono = &up5, h.wakeOKMono+8*time.Second
	if ok, why := mayShutDown(h, info, o, noGuests); !ok {
		t.Errorf("answered 8 s after the accepted wake, up for 5 s: %q", why)
	}
	// and a machine whose first wake request was accepted is not measured this way at all
	h, info, o = proven()
	h.WakeOKAt, h.wakeOKMono = nil, 0
	if ok, why := mayShutDown(h, info, o, noGuests); !ok {
		t.Errorf("a first wake that was accepted has no second time: %q", why)
	}
	// an uptime of 0 s is a machine that just started: the proof holds
	h, info, o = proven()
	h.BootUptimeS = &zero
	if ok, _ := mayShutDown(h, info, o, noGuests); !ok {
		t.Error("an uptime of 0 s after the wake")
	}
	// the guests are only asked about when everything else holds (the question can be slow, and its answer
	// must never be what turns a machine that was on into one that may be switched off)
	h, info, o = proven()
	h.AlreadyOn = true
	asked := false
	mayShutDown(h, info, o, func() (int, bool, string) { asked = true; return 0, true, "" })
	if asked {
		t.Error("guests were asked about for a machine that is refused anyway")
	}
}

// The rule against whatever happens. Many runs over machines that are made up at random: off for long, on
// for days, on but unreachable (for a moment, or for longer than MinOff), restarting, on with an agent that
// does not listen, just gone quiet; a wake that works or does not; a person who switches a
// machine on at some moment before or during the run; an agent without uptime; updates and shutdowns that
// work, fail, hang or are refused; guests; a cancel of either kind, the kill switch, a machine taken out, all
// at random moments. The world is the judge (world.Shutdown): a shutdown request for a machine that was
// running when the run started, or when the run's first wake for it was accepted, fails the test. And every
// run must end with every machine reported.
func TestNoRunEverSwitchesOffAMachineItDidNotWake(t *testing.T) {
	runs := 400
	if n, err := strconv.Atoi(os.Getenv("FAIRY_RANDOM_RUNS")); err == nil && n > 0 { // as TestRandomRunsWithLinkFlaps
		runs = n
	}
	if testing.Short() {
		runs = 60
	}
	total, shut := 0, map[string]int{}
	for seed := 1; seed <= runs; seed++ {
		rng := rand.New(rand.NewSource(int64(seed)))
		lostRng := rand.New(rand.NewSource(int64(seed) + 1_000_000)) // its own, so that rng draws what it drew before
		// (build 14) the timings a machine can have of its own, and the panel's off_after: drawn from a third
		// source, for the same reason. Every third seed has the panel's own off_after too.
		timeRng := rand.New(rand.NewSource(int64(seed) + 2_000_000))
		w := newWorld(t)
		if seed%3 == 0 {
			w.m.Settings = w.m.Settings.WithOffAfter(300 + timeRng.Intn(3301))
		}
		pick := func(xs ...string) string { return xs[rng.Intn(len(xs))] }
		secs := func(lo, hi int) time.Duration { return time.Duration(lo+rng.Intn(hi-lo+1)) * time.Second }
		n := 1 + rng.Intn(5)
		var req StartRequest
		req.UpdateIfOn = rng.Intn(2) == 0
		type later struct {
			at time.Duration
			do func()
		}
		var plan []later
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("m%d", i)
			var mc *machine
			// its own off_after (300-3600 s) and its own countdown (10-3600 s, mostly short ones so that the runs
			// stay short), each for about every second machine
			var ownOff, ownDelay time.Duration
			if timeRng.Intn(2) == 0 {
				ownOff = time.Duration(300+timeRng.Intn(3301)) * time.Second
			}
			switch timeRng.Intn(6) {
			case 0, 1:
				ownDelay = time.Duration(10+timeRng.Intn(291)) * time.Second
			case 2:
				ownDelay = time.Duration(10+timeRng.Intn(3591)) * time.Second
			}
			// offAfter: after how long without a sign of life THIS machine counts as off
			offAfter := w.m.Settings.offAfter()
			if ownOff > 0 {
				offAfter = ownOff
			}
			switch pick("off", "off", "off", "off", "on", "unreachable", "unreachable for long", "restarting", "agent down", "quiet") {
			case "off":
				mc = w.off(id)
			case "on":
				mc = w.running(id)
				mc.bootedAt = w.now.Add(-secs(1, 400000))
			case "unreachable": // on, and the panel cannot reach it: it shows as stale or offline
				mc = w.running(id)
				mc.unreachable = true
				back := secs(0, 500)
				plan = append(plan, later{back, func() { mc.unreachable = false }})
			case "unreachable for long": // on, and out of reach for longer than MinOff: the panel takes it for off
				mc = w.running(id)
				mc.bootedAt = w.now.Add(-secs(7000, 400000))
				mc.unreachable = true
				w.silentFor(mc, secs(900, 5000))
				back := secs(0, 500)
				plan = append(plan, later{back, func() { mc.unreachable = false }})
			case "restarting": // on, restarting: no answer for less than MinOff, then it is back with a fresh uptime
				mc = w.running(id)
				mc.unreachable = true
				// (build 14) ... for less than the time after which THIS machine counts as off, with the two
				// minutes to spare the fixed 15 minutes had here (780 of 900 s). A restart that takes longer than
				// a machine's off_after is what a short off_after risks, and what `fairy admin config check` says.
				silent := secs(0, 780)
				if most := offAfter - 2*time.Minute; silent > most {
					silent = most
				}
				w.silentFor(mc, silent)
				back, up := secs(0, 500), secs(0, 120)
				plan = append(plan, later{back, func() { mc.unreachable, mc.bootedAt, mc.agentReadyAt = false, w.now.Add(-up), w.now }})
			case "agent down": // on, and its agent does not listen: the machine refuses the connection
				mc = w.running(id)
				mc.agentDown = true
				w.silentFor(mc, secs(0, 5000))
				back := secs(0, 500)
				plan = append(plan, later{back, func() { mc.agentDown = false }})
			case "quiet": // it went off a moment ago, or some minutes ago: the panel shows "stale" or "offline"
				mc = w.running(id)
				mc.on = false
				w.silentFor(mc, secs(0, 1500))
			}
			mc.info.OffAfter, mc.info.ShutdownDelay = ownOff, ownDelay
			mc.wakeBoots = rng.Intn(5) > 0
			mc.bootTakes = secs(5, 420)
			mc.post = time.Duration(rng.Int63n(int64(mc.bootTakes)))
			mc.noUptime = rng.Intn(10) == 0
			mc.updMode = pick("ok", "ok", "ok", "noverdict", "current", "fail", "rollback", "stuck", "approval", "refuse", "busy", "disabled")
			mc.updTakes = secs(5, 200)
			mc.shutMode = pick("ok", "ok", "ok", "approval", "refuse", "ignore", "busy")
			if rng.Intn(6) == 0 {
				mc.guests = rng.Intn(3)
				if rng.Intn(3) == 0 {
					mc.guestsWhy = "no Proxmox API for it"
				}
			}
			if rng.Intn(8) == 0 {
				mc.info.MACs = 1 + rng.Intn(4)
			}
			if rng.Intn(3) == 0 { // a person switches it on, some time around the run's wake
				at := secs(0, 500)
				ready := secs(1, 120)
				plan = append(plan, later{at, func() {
					if !mc.on && !mc.wentOff { // (not one the run has switched off and reported: the report was true when it was written)
						mc.on, mc.bootedAt, mc.agentReadyAt = true, w.now, w.now.Add(ready)
					}
				}})
			}
			if rng.Intn(12) == 0 {
				plan = append(plan, later{secs(0, 600), func() { mc.info.OptedIn = false }})
			}
			if lostRng.Intn(5) == 0 { // the answer to a request of the run is lost; the machine got the request, or did not
				switch lostRng.Intn(3) {
				case 0:
					mc.wakeLost = []string{"arrives", "never", "always"}[lostRng.Intn(3)]
				case 1:
					mc.updLost = []string{"arrives", "never"}[lostRng.Intn(2)]
				case 2:
					mc.shutLost = []string{"arrives", "never"}[lostRng.Intn(2)]
				}
			}
			if rng.Intn(4) == 0 {
				req.UpdateOnly = append(req.UpdateOnly, id)
			} else {
				req.Hosts = append(req.Hosts, id)
			}
		}
		w.advance(secs(0, 90)) // before the run: the panel's view settles, or not quite
		r := w.start(req)
		if rng.Intn(3) == 0 {
			off := rng.Intn(2) == 0
			plan = append(plan, later{secs(0, 900), func() { _, _ = w.m.CancelRun(r.ID, "Bob", off) }})
		}
		if rng.Intn(15) == 0 {
			plan = append(plan, later{secs(0, 900), func() { w.blocked = "control is disabled by an administrator" }})
		}
		startAt := w.now
		for i := 0; i < 5*3600; i++ {
			if a, _ := w.m.Snapshot(); a == nil {
				break
			}
			for j := range plan {
				if plan[j].do != nil && w.now.Sub(startAt) >= plan[j].at {
					plan[j].do()
					plan[j].do = nil
				}
			}
			w.tick()
		}
		done := w.finish()
		for _, h := range done.Hosts {
			total++
			shut[h.State]++
			mc := w.ms[h.ID]
			if h.State == StOff && (mc.onAtStart || !mc.wakeAccepted || *mc.onAtFirstWake || h.Mode != ModeWake) {
				t.Errorf("seed %d: %s ended as switched off, and the run did not wake it", seed, h.ID)
			}
			if mc.shutdowns > 0 && h.Mode != ModeWake {
				t.Errorf("seed %d: %s was chosen as on and got a shutdown request", seed, h.ID)
			}
		}
		if w.limited != 0 || w.maxInMin > 8 {
			t.Errorf("seed %d: an agent refused %d requests of the run; most in a minute %d", seed, w.limited, w.maxInMin)
		}
		if t.Failed() {
			t.Fatalf("seed %d: %s\nrequests: %v", seed, describe(done), w.calls)
		}
	}
	t.Logf("%d runs, %d machines; how they ended: %s", runs, total, sorted(shut))
	if shut[StOff] == 0 || shut[StLeftOn] == 0 || shut[StStayedOn] == 0 || shut[StGaveUp] == 0 || shut[StSkipped] == 0 {
		t.Fatalf("the random runs did not reach every end state: %s", sorted(shut))
	}
}
