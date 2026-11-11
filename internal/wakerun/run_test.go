package wakerun

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func has(t *testing.T, h *Host, state string, parts ...string) {
	t.Helper()
	if h.State != state {
		t.Errorf("%s: state %q, want %q (%s)", h.ID, h.State, state, h.Text)
	}
	for _, p := range parts {
		if !strings.Contains(h.Text, p) {
			t.Errorf("%s: the sentence lacks %q: %s", h.ID, p, h.Text)
		}
	}
}

// The whole way for one machine that is off: wake, wait for the agent, update, wait for the result, wait a
// minute, the delayed shutdown, wait until it is gone. Three requests, in that order, and nothing else.
func TestOneMachineWokenUpdatedAndSwitchedOffAgain(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	started := w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
	if started.State != RunRunning || started.ListHash != ListHash([]string{"pc-a"}, nil, false) || started.By != "Ann" {
		t.Fatalf("the started run: %+v", started)
	}
	r := w.finish()
	h := host(r, "pc-a")
	has(t, h, StOff, "Woken, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.")
	if got := strings.Join(w.calls, ", "); got != "wake pc-a, update pc-a, shutdown pc-a 120" {
		t.Fatalf("requests: %s", got)
	}
	if got := w.steps("pc-a"); got != "wake:ok update:ok shutdown:ok" {
		t.Fatalf("audit steps: %s", got)
	}
	if pc.on || pc.version != "17-bbbbbbb" || r.State != RunDone || h.On != OnNo || h.Update != UpdOK {
		t.Fatalf("the machine: on=%v %s; run %s; host on=%s update=%s", pc.on, pc.version, r.State, h.On, h.Update)
	}
	// the order in time: online after the wake, the update's result before the pause, the pause of a minute,
	// the countdown of two minutes
	if !h.OnlineAt.After(*h.WakeSentAt) || !h.UpdateSentAt.After(*h.OnlineAt) {
		t.Fatalf("times: wake %v online %v update %v", h.WakeSentAt, h.OnlineAt, h.UpdateSentAt)
	}
	var settleAt, shutAt time.Time
	for _, l := range h.Log {
		if strings.Contains(l.Text, "before the shutdown") {
			settleAt = l.T
		}
		if strings.HasPrefix(l.Text, "shutting down at") {
			shutAt = l.T
		}
	}
	if d := shutAt.Sub(settleAt); d < 60*time.Second || d > 63*time.Second {
		t.Fatalf("the pause between the update's result and the shutdown request was %s, want about a minute", d)
	}
	if d := h.ShutdownAt.Sub(shutAt); d != 120*time.Second {
		t.Fatalf("the countdown is %s, want 2 minutes", d)
	}
	if r.Summary != "PC-A: "+h.Text {
		t.Fatalf("summary: %q", r.Summary)
	}
}

// Three machines that are off and one that is on and was ticked for an update: the three are woken, updated
// and switched off; the fourth is updated and never asked to shut down.
func TestThreeOffAndOneOn(t *testing.T) {
	w := newWorld(t)
	for _, id := range []string{"pc-a", "pc-b", "pve"} {
		w.off(id)
	}
	w.ms["pc-b"].updMode = "current" // its agent finds nothing newer
	on := w.running("win-test")
	r := w.start(StartRequest{Hosts: []string{"pc-a", "pc-b", "pve"}, UpdateOnly: []string{"win-test"}})
	if host(r, "win-test").Mode != ModeUpdateOnly || host(r, "pc-a").StartStatus != "offline" || host(r, "win-test").StartStatus != "online" {
		t.Fatalf("the list: %s", describe(r))
	}
	r = w.finish()
	has(t, host(r, "pc-a"), StOff, "updated from 16-aaaaaaa to 17-bbbbbbb")
	has(t, host(r, "pc-b"), StOff, "already on the newest release (16-aaaaaaa)")
	has(t, host(r, "pve"), StOff)
	has(t, host(r, "win-test"), StStayedOn, "It was on and stays on. It was updated to 17-bbbbbbb.")
	if got := w.callsFor("win-test"); got != "update" {
		t.Fatalf("the machine that was on got: %q", got)
	}
	if !on.on || on.shutdowns != 0 || on.wakes != 0 {
		t.Fatal("the machine that was on must be on and untouched by wake and shutdown")
	}
	for _, id := range []string{"pc-a", "pc-b", "pve"} {
		if got := w.callsFor(id); got != "wake update shutdown" {
			t.Errorf("%s got: %q", id, got)
		}
		if w.ms[id].on {
			t.Errorf("%s is still on", id)
		}
	}
	if n := strings.Count(r.Summary, "\n"); n != 3 {
		t.Fatalf("the summary has %d lines, want one per machine:\n%s", n+1, r.Summary)
	}
}

// A machine that never comes online: the wake is repeated once a minute, the run gives up after
// online_timeout_s, says so, and sends it nothing else. If it appears while the run still works on another
// machine, that is added: it is on then, and the run does not touch it.
func TestMachineThatNeverComesOnline(t *testing.T) {
	w := newWorld(t)
	dead := w.off("pc-dead")
	dead.wakeBoots = false
	w.off("pc-a").bootTakes = 200 * time.Second
	w.start(StartRequest{Hosts: []string{"pc-dead", "pc-a"}})
	w.advance(302 * time.Second)
	a, _ := w.m.Snapshot()
	has(t, host(a, "pc-dead"), StGaveUp, "It did not come online within 5 min after the wake", "It may still be starting, or be on without its network connection (Tailscale)",
		"if it did switch on, it is ON", "Fairy has not switched it off and will not")
	if first := host(a, "pc-dead").Log[0].Text; first != "wake sent: waiting for it to come online, up to 5 min (the run goes on as soon as its agent answers)" {
		t.Fatalf("the wait is not named: %s", first)
	}
	if got := w.callsFor("pc-dead"); got != "wake wake wake wake wake" {
		t.Fatalf("wakes for the dead machine: %q", got)
	}
	// someone presses its power button; the run is still busy with pc-a
	dead.on, dead.bootedAt, dead.agentReadyAt = true, w.now, w.now.Add(20*time.Second)
	r := w.finish()
	h := host(r, "pc-dead")
	has(t, h, StGaveUp, "It came online later", "it is ON, and Fairy did not touch it")
	if got := w.callsFor("pc-dead"); got != "wake wake wake wake wake" || dead.shutdowns != 0 || !dead.on || h.On != OnYes {
		t.Fatalf("after it appeared: %q shutdowns %d on %v (%s)", got, dead.shutdowns, dead.on, h.On)
	}
	has(t, host(r, "pc-a"), StOff)
	late := 0
	for _, e := range w.events {
		if e.Kind == EvLate {
			late++
		}
	}
	if late != 1 {
		t.Fatalf("%d \"came online late\" events", late)
	}
}

// The relay refuses or is gone: the machine was not woken, and the report says why in the relay's words.
func TestWakeThatCannotBeSent(t *testing.T) {
	w := newWorld(t)
	w.off("pc-a").relayDown = "relay nas is offline"
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	r := w.finish()
	has(t, host(r, "pc-a"), StGaveUp, "The wake could not be sent: relay nas is offline", "It was not woken.")
	if got := strings.Join(w.calls, ","); got != "wake pc-a" || w.steps("pc-a") != "wake:relay nas is offline" || host(r, "pc-a").On != OnUntouched {
		t.Fatalf("requests %q steps %q", got, w.steps("pc-a"))
	}
}

// A machine of the wake list that is on when the run starts is never shut down. Without the tick "update it
// if it is on" nothing is sent to it at all; with it, it is updated and left on.
func TestMachineThatIsOnWhenTheRunStarts(t *testing.T) {
	for _, updateIfOn := range []bool{false, true} {
		w := newWorld(t)
		pc := w.running("pc-a")
		w.start(StartRequest{Hosts: []string{"pc-a"}, UpdateIfOn: updateIfOn})
		r := w.finish()
		h := host(r, "pc-a")
		if !updateIfOn {
			has(t, h, StSkipped, "It was already on", "Nothing was sent to it and it stays on.")
			if len(w.calls) != 0 {
				t.Fatalf("requests to a machine that was on and not ticked for an update: %v", w.calls)
			}
		} else {
			has(t, h, StStayedOn, "It was already on and stays on. It was updated to 17-bbbbbbb.")
			if got := strings.Join(w.calls, ","); got != "update pc-a" {
				t.Fatalf("requests: %q", got)
			}
		}
		if !pc.on || pc.shutdowns != 0 || !h.AlreadyOn || h.On != OnYes {
			t.Fatalf("update_if_on=%v: on=%v shutdowns=%d", updateIfOn, pc.on, pc.shutdowns)
		}
	}
}

// The machine was off when the admin chose it and when the run started, and someone switches it on before the
// run gets to it. Its agent is not up yet, so the direct question gets no answer and a wake goes out, which
// changes nothing. When the agent answers, it reports an uptime longer than the time since the wake: the run
// did not wake it, and never switches it off.
func TestMachineSwitchedOnBySomeoneBeforeTheWake(t *testing.T) {
	for _, updateIfOn := range []bool{false, true} {
		w := newWorld(t)
		w.off("pc-first").bootTakes = 90 * time.Second
		pc := w.off("pc-a")
		w.start(StartRequest{Hosts: []string{"pc-first", "pc-a"}, UpdateIfOn: updateIfOn})
		// between the start and the run's first step: a person at the machine
		pc.on, pc.bootedAt, pc.agentReadyAt = true, w.now, w.now.Add(45*time.Second)
		r := w.finish()
		h := host(r, "pc-a")
		if h.WakeSentAt == nil || !h.ProbedOff || pc.wakes != 1 {
			t.Fatalf("the run did send its wake (the machine did not answer yet): %+v", h)
		}
		if !h.AlreadyOn || pc.shutdowns != 0 || !pc.on || h.On != OnYes {
			t.Fatalf("update_if_on=%v: already_on=%v shutdowns=%d on=%v: %s", updateIfOn, h.AlreadyOn, pc.shutdowns, pc.on, h.Text)
		}
		if updateIfOn {
			has(t, h, StStayedOn, "It was already on and stays on.", "updated to 17-bbbbbbb")
		} else {
			has(t, h, StStayedOn, "It was already on (it has been up for", "longer than the", "It was not updated and stays on.")
			if got := w.callsFor("pc-a"); got != "wake" {
				t.Fatalf("requests: %q", got)
			}
		}
		has(t, host(r, "pc-first"), StOff)
	}
}

// The panel shows a machine as offline, but it is on: the last polls failed (the network between them). Asked
// directly it answers, or it does not and answers later with an uptime of days. Either way it is not a
// machine this run woke.
func TestMachineThatOnlyLookedOff(t *testing.T) {
	t.Run("the direct question is answered", func(t *testing.T) {
		w := newWorld(t)
		pc := w.running("pc-a")
		pc.unreachable = true
		w.advance(90 * time.Second) // the panel now shows it as offline
		if o := w.Observe("pc-a"); o.Online || o.Status != "offline" {
			t.Fatalf("the panel's view: %+v", o)
		}
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		pc.unreachable = false // reachable again, before the poller has noticed
		pc.obs = Observation{Status: "offline"}
		w.m.Tick(context.Background())
		a, hist := w.m.Snapshot()
		if a != nil || len(hist) != 1 {
			t.Fatal("the run should be over after its first step")
		}
		has(t, host(hist[0], "pc-a"), StSkipped, "It was already on (its agent answered when the run asked it directly, before any wake)")
		if len(w.calls) != 0 || pc.shutdowns != 0 {
			t.Fatalf("requests: %v", w.calls)
		}
	})
	t.Run("it answers only after the wake", func(t *testing.T) {
		w := newWorld(t)
		pc := w.running("pc-a")
		pc.unreachable = true
		w.advance(90 * time.Second)
		w.start(StartRequest{Hosts: []string{"pc-a"}, UpdateIfOn: true})
		w.advance(30 * time.Second)
		pc.unreachable = false
		r := w.finish()
		h := host(r, "pc-a")
		has(t, h, StStayedOn, "It was already on and stays on.")
		if !h.ProbedOff || h.WakeSentAt == nil || !h.AlreadyOn || pc.shutdowns != 0 || !pc.on {
			t.Fatalf("%+v shutdowns %d", h, pc.shutdowns)
		}
		if got := w.callsFor("pc-a"); got != "wake update" {
			t.Fatalf("requests: %q", got)
		}
		found := false
		for _, l := range h.Log {
			found = found || strings.Contains(l.Text, "it has been up for 3 days, longer than the")
		}
		if !found {
			t.Fatalf("the reason is not in its history: %+v", h.Log)
		}
	})
	t.Run("stale, not offline", func(t *testing.T) {
		w := newWorld(t)
		pc := w.running("pc-a")
		pc.unreachable = true
		w.advance(20 * time.Second) // no answer for 20 s: the panel shows "stale", not "offline"
		w.start(StartRequest{Hosts: []string{"pc-a"}, UpdateIfOn: true})
		r := w.finish()
		has(t, host(r, "pc-a"), StSkipped, "The panel did not show it as off, only as not answering (stale when the run started", "Nothing was sent to it.")
		if len(w.calls) != 0 {
			t.Fatalf("requests: %v", w.calls)
		}
	})
	t.Run("something else answers at its address", func(t *testing.T) {
		w := newWorld(t)
		w.off("pc-a").probeSays = "http 401"
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		r := w.finish()
		has(t, host(r, "pc-a"), StSkipped, "could not tell that it is off", "http 401", "Nothing was sent to it.")
		if len(w.calls) != 0 {
			t.Fatalf("requests: %v", w.calls)
		}
	})
	t.Run("polling is paused", func(t *testing.T) {
		w := newWorld(t)
		w.off("pc-a").paused = true
		w.poll()
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		r := w.finish()
		has(t, host(r, "pc-a"), StSkipped, "Polling is paused for it")
		if len(w.calls) != 0 {
			t.Fatalf("requests: %v", w.calls)
		}
	})
}

// An agent that reports no uptime: the run cannot tell that the machine started after the wake. It updates
// it and leaves it on, with that reason.
func TestMachineWithoutUptimeIsLeftOn(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	pc.noUptime = true
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	r := w.finish()
	has(t, host(r, "pc-a"), StLeftOn, "Left ON: its agent reports no uptime, so Fairy cannot tell that it started after the wake.", "It was updated to 17-bbbbbbb.")
	if got := w.callsFor("pc-a"); got != "wake update" || !pc.on {
		t.Fatalf("requests: %q on=%v", got, pc.on)
	}
}

// A machine that is on and was ticked "update only" is not woken when it has gone off meanwhile.
func TestUpdateOnlyMachineThatIsOffIsNotWoken(t *testing.T) {
	w := newWorld(t)
	pc := w.running("pc-a")
	w.start(StartRequest{UpdateOnly: []string{"pc-a"}})
	pc.on = false
	w.poll()
	r := w.finish()
	has(t, host(r, "pc-a"), StSkipped, "its agent does not answer now", "it was not woken")
	if len(w.calls) != 0 || pc.on {
		t.Fatalf("requests: %v", w.calls)
	}
}

// What the update can do, for a machine the run woke. Whatever happens to the update, the machine was off
// before and is switched off again, and the failure is in the report; only an update that never ends keeps it on.
func TestUpdateOutcomesOfAWokenMachine(t *testing.T) {
	for _, c := range []struct {
		mode    string
		prep    func(mc *machine)
		state   string
		update  string
		calls   string
		wantOn  bool
		version string
		parts   []string
	}{
		{mode: "ok", state: StOff, update: UpdOK, calls: "wake update shutdown", version: "17-bbbbbbb", parts: []string{"updated from 16-aaaaaaa to 17-bbbbbbb"}},
		{mode: "noverdict", state: StOff, update: UpdOK, calls: "wake update shutdown", version: "17-bbbbbbb", parts: []string{"updated from 16-aaaaaaa to 17-bbbbbbb"}},
		{mode: "current", state: StOff, update: UpdCurrent, calls: "wake update shutdown", version: "16-aaaaaaa", parts: []string{"already on the newest release"}},
		{mode: "ok", prep: func(mc *machine) { no := false; mc.behind = &no }, state: StOff, update: UpdCurrent, calls: "wake shutdown", version: "16-aaaaaaa",
			parts: []string{"already on the newest release (16-aaaaaaa)"}}, // the panel already knows: no request
		{mode: "fail", state: StOff, update: UpdFailed, calls: "wake update shutdown", version: "16-aaaaaaa",
			parts: []string{"NOT updated (downloading the release: connection reset)", "switched off again"}},
		{mode: "rollback", state: StOff, update: UpdFailed, calls: "wake update shutdown", version: "16-aaaaaaa", parts: []string{"NOT updated (", "switched off again"}},
		{mode: "refuse", state: StOff, update: UpdFailed, calls: "wake update shutdown", version: "16-aaaaaaa",
			parts: []string{"NOT updated (an update was installed on this machine less than 10 minutes ago"}},
		{mode: "disabled", state: StOff, update: UpdFailed, calls: "wake shutdown", version: "16-aaaaaaa", parts: []string{"NOT updated (self-update is not enabled on its agent)"}},
		{mode: "busy", state: StOff, update: UpdOK, calls: "wake update update shutdown", version: "17-bbbbbbb"}, // the agent's limit: asked again
		{mode: "approval", state: StOff, update: UpdApproval, calls: "wake update shutdown", version: "16-aaaaaaa",
			parts: []string{"NOT updated (its agent asks for an approval (Pocket ID) before an update, and this run has none for it)"}},
		{mode: "stuck", state: StLeftOn, update: UpdStuck, calls: "wake update", wantOn: true, version: "16-aaaaaaa",
			parts: []string{"Its update did not finish (no result 15 min after it was asked for)", "left it ON", "not cut off"}},
	} {
		t.Run(c.mode+"/"+c.calls, func(t *testing.T) {
			w := newWorld(t)
			pc := w.off("pc-a")
			pc.updMode = c.mode
			if c.prep != nil {
				c.prep(pc)
			}
			w.start(StartRequest{Hosts: []string{"pc-a"}})
			r := w.finish()
			h := host(r, "pc-a")
			has(t, h, c.state, c.parts...)
			if got := w.callsFor("pc-a"); got != c.calls || h.Update != c.update || pc.on != c.wantOn || pc.version != c.version {
				t.Fatalf("requests %q update %q on %v version %s: %s", got, h.Update, pc.on, pc.version, h.Text)
			}
		})
	}
}

// The same for a machine that was on: it ends with the update's result and is left on.
func TestUpdateOutcomesOfAMachineThatWasOn(t *testing.T) {
	for mode, part := range map[string]string{
		"ok":       "It was updated to 17-bbbbbbb.",
		"current":  "It already ran the newest release.",
		"fail":     "Its update did not work: downloading the release: connection reset.",
		"approval": "Its update did not work: its agent asks for an approval (Pocket ID) before an update",
		"stuck":    "Its update did not work: no result 15 min after it was asked for.",
	} {
		w := newWorld(t)
		pc := w.running("pc-a")
		pc.updMode = mode
		w.start(StartRequest{UpdateOnly: []string{"pc-a"}})
		r := w.finish()
		has(t, host(r, "pc-a"), StStayedOn, "It was on and stays on.", part)
		if got := w.callsFor("pc-a"); got != "update" || !pc.on {
			t.Fatalf("%s: requests %q on %v", mode, got, pc.on)
		}
	}
}

// An update that someone else started is watched, not asked for a second time.
func TestUpdateAlreadyRunning(t *testing.T) {
	w := newWorld(t)
	pc := w.running("pc-a")
	pc.updating, pc.updDoneAt, pc.updState, pc.updMsg = true, w.now.Add(40*time.Second), "downloading", "downloading 17-bbbbbbb"
	w.poll()
	w.start(StartRequest{UpdateOnly: []string{"pc-a"}})
	r := w.finish()
	has(t, host(r, "pc-a"), StStayedOn, "It was updated to 17-bbbbbbb.")
	if len(w.calls) != 0 {
		t.Fatalf("requests: %v", w.calls)
	}
}

// What the shutdown request can meet, for a machine the run woke and updated.
func TestShutdownOutcomes(t *testing.T) {
	for _, c := range []struct {
		name   string
		prep   func(w *world, mc *machine)
		during func(w *world, mc *machine) // once, when the countdown runs
		state  string
		calls  string
		wantOn bool
		parts  []string
	}{
		{name: "works", state: StOff, calls: "wake update shutdown"},
		{name: "the agent's limit, then works", prep: func(_ *world, mc *machine) { mc.shutMode = "busy" }, state: StOff, calls: "wake update shutdown shutdown"},
		{name: "the helper is stopped", prep: func(_ *world, mc *machine) { mc.shutMode = "refuse" }, state: StLeftOn, calls: "wake update shutdown", wantOn: true,
			parts: []string{"Left ON: the shutdown was not accepted (the elevated helper is stopped: start the helper first).", "It was updated to 17-bbbbbbb."}},
		{name: "the agent wants an approval", prep: func(_ *world, mc *machine) { mc.shutMode = "approval" }, state: StLeftOn, calls: "wake update shutdown", wantOn: true,
			parts: []string{"Left ON: its agent asks for an approval (Pocket ID) before a shutdown, and this run has none for it."}},
		{name: "accepted, but it stays on", prep: func(_ *world, mc *machine) { mc.shutMode = "ignore" }, state: StLeftOn, calls: "wake update shutdown", wantOn: true,
			parts: []string{"It is still on 5 min after its shutdown was due: the shutdown did not work. It is ON."}},
		{name: "someone cancels the countdown", during: func(_ *world, mc *machine) { mc.pending = nil }, state: StLeftOn, calls: "wake update shutdown", wantOn: true,
			parts: []string{"Its shutdown was cancelled, in the panel or on the machine, so it is ON."}},
		{name: "someone cancels the countdown within a second", during: func(_ *world, mc *machine) { mc.pending = nil }, state: StLeftOn, calls: "wake update shutdown", wantOn: true,
			parts: []string{"Its shutdown was cancelled, in the panel or on the machine, so it is ON."}},
		{name: "an answer from before the shutdown arrives after it", prep: func(_ *world, mc *machine) { mc.staleOnce = true }, state: StOff, calls: "wake update shutdown"},
		{name: "one guest runs on the node", prep: func(_ *world, mc *machine) { mc.guests, mc.guestNames = 1, "win11" }, state: StLeftOn, calls: "wake update", wantOn: true,
			parts: []string{"Left ON: 1 guest is running on it (win11)."}},
		{name: "three guests run on the node", prep: func(_ *world, mc *machine) { mc.guests = 3 }, state: StLeftOn, calls: "wake update", wantOn: true,
			parts: []string{"Left ON: 3 guests are running on it."}},
		{name: "the panel cannot see the node's guests", prep: func(_ *world, mc *machine) {
			mc.guestsWhy = "the Proxmox API of pve has not answered since it came online"
		},
			state: StLeftOn, calls: "wake update", wantOn: true,
			parts: []string{"Left ON: Fairy cannot see whether guests run on it (the Proxmox API of pve has not answered since it came online)."}},
		{name: "another power action is pending", prep: func(w *world, mc *machine) { mc.updTakes = 20 * time.Second },
			during: nil, state: StLeftOn, calls: "wake update", wantOn: true, parts: []string{"Left ON: another power action is already pending on it (reboot at"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			pc := w.off("pc-a")
			if c.prep != nil {
				c.prep(w, pc)
			}
			w.start(StartRequest{Hosts: []string{"pc-a"}})
			did := false
			for i := 0; i < 3*3600; i++ {
				a, _ := w.m.Snapshot()
				if a == nil {
					break
				}
				h := host(a, "pc-a")
				if c.name == "another power action is pending" && h.State == StSettling && pc.pending == nil {
					pc.pending = &Pending{Action: "reboot", At: w.now.Add(time.Hour)} // an admin's own, scheduled on the machine's page
				}
				wait := 20 * time.Second
				if strings.Contains(c.name, "within a second") { // before any poll has shown the countdown
					wait = -time.Second
				}
				if c.during != nil && !did && h.State == StShutting && w.now.Sub(h.Since) > wait {
					did = true
					c.during(w, pc)
				}
				w.tick()
			}
			r := w.finish()
			h := host(r, "pc-a")
			has(t, h, c.state, c.parts...)
			if got := w.callsFor("pc-a"); got != c.calls || pc.on != c.wantOn {
				t.Fatalf("requests %q on %v: %s", got, pc.on, h.Text)
			}
		})
	}
}

// Cancelling a run. By default nothing new is sent, a countdown the run started is called off, and every
// machine the run woke stays on and is named as ON.
func TestCancelLeavesWokenMachinesOn(t *testing.T) {
	w := newWorld(t)
	w.off("pc-count").bootTakes = 20 * time.Second // furthest: its countdown runs when the run is cancelled
	w.off("pc-upd").bootTakes = 150 * time.Second  // its update runs
	w.off("pc-wake").wakeBoots = false             // woken, never answered
	for i := 0; i < 9; i++ {                       // more than the relay takes in a minute: the last ones still wait
		w.off(fmt.Sprintf("pc-q%d", i)).wakeBoots = false
	}
	on := w.running("win-test")
	on.updTakes = 20 * time.Minute
	ids := []string{"pc-count", "pc-upd", "pc-wake"}
	for i := 0; i < 9; i++ {
		ids = append(ids, fmt.Sprintf("pc-q%d", i))
	}
	r := w.start(StartRequest{Hosts: ids, UpdateOnly: []string{"win-test"}})
	for i := 0; i < 600; i++ {
		a, _ := w.m.Snapshot()
		if host(a, "pc-count").State == StShutting && host(a, "pc-upd").State == StUpdating && host(a, "pc-upd").UpdateSentAt != nil {
			break
		}
		w.tick()
	}
	a, _ := w.m.Snapshot()
	if host(a, "pc-count").State != StShutting || host(a, "pc-upd").State != StUpdating || host(a, "pc-wake").State != StWaking {
		t.Fatalf("not the moment the test wants: %s", describe(a))
	}
	before := len(w.calls)
	if _, err := w.m.CancelRun("not-the-run", "Bob", false); err == nil {
		t.Fatal("another run's id must not cancel this one")
	}
	if c, err := w.m.CancelRun(r.ID, "Bob", false); err != nil || c.Cancel == nil || c.Cancel.By != "Bob" || c.Cancel.SwitchOff {
		t.Fatal(c, err)
	}
	w.tick()
	done := w.finish()
	if done.State != RunCancelled {
		t.Fatalf("run state %s", done.State)
	}
	has(t, host(done, "pc-count"), StLeftOn, "The run was cancelled by Bob: its shutdown countdown was called off. The run woke it and it is ON.", "It was updated to 17-bbbbbbb.")
	has(t, host(done, "pc-upd"), StLeftOn, "The run was cancelled by Bob. The run woke it and it is ON.", "Its update was asked for and may still be running")
	has(t, host(done, "pc-wake"), StGaveUp, "The run was cancelled by Bob. A wake was sent to it: if it switches on, it stays ON.")
	has(t, host(done, "win-test"), StStayedOn, "The run was cancelled by Bob. It was already on and stays on.", "Its update was asked for and may still be running")
	queued := 0
	for _, h := range done.Hosts {
		if h.State == StSkipped {
			queued++
			has(t, h, StSkipped, "The run was cancelled by Bob. Nothing was sent to it.")
		}
	}
	if queued == 0 {
		t.Fatalf("no machine was still waiting for its turn: %s", describe(done))
	}
	if got := strings.Join(w.calls[before:], ","); got != "cancel pc-count" {
		t.Fatalf("after the cancel the run sent: %q (only the cancel of its own countdown is allowed)", got)
	}
	if !w.ms["pc-count"].on || !w.ms["pc-upd"].on || w.ms["pc-count"].pending != nil {
		t.Fatal("the woken machines must be on, with no countdown")
	}
	if w.steps("pc-count") != "wake:ok update:ok shutdown:ok cancel:ok" {
		t.Fatalf("audit steps: %s", w.steps("pc-count"))
	}
}

// Cancelling with "switch the woken machines off again": nothing new is started (no wake for a machine that
// still waits, no update that was not asked for yet), an update that runs is waited for, and every machine
// the run woke is switched off again. A machine that was on is left alone as always.
func TestCancelAndSwitchOffAgain(t *testing.T) {
	w := newWorld(t)
	w.off("pc-upd").bootTakes = 20 * time.Second // its update runs when the run is cancelled
	w.off("pc-on").bootTakes = 75 * time.Second  // just online, no update asked for yet
	w.off("pc-wake").bootTakes = 200 * time.Second
	for i := 0; i < 9; i++ {
		w.off(fmt.Sprintf("pc-q%d", i)).wakeBoots = false
	}
	on := w.running("win-test")
	ids := []string{"pc-upd", "pc-on", "pc-wake"}
	for i := 0; i < 9; i++ {
		ids = append(ids, fmt.Sprintf("pc-q%d", i))
	}
	r := w.start(StartRequest{Hosts: ids, UpdateOnly: []string{"win-test"}})
	for i := 0; i < 600; i++ {
		a, _ := w.m.Snapshot()
		if host(a, "pc-on").State == StOnline {
			break
		}
		w.tick()
	}
	a, _ := w.m.Snapshot()
	if host(a, "pc-upd").State != StUpdating && host(a, "pc-upd").State != StSettling || host(a, "pc-on").State != StOnline || host(a, "pc-wake").State != StWaking {
		t.Fatalf("not the moment the test wants: %s", describe(a))
	}
	if _, err := w.m.CancelRun(r.ID, "Bob", true); err != nil {
		t.Fatal(err)
	}
	before := len(w.calls)
	done := w.finish()
	for _, c := range w.calls[before:] { // after the cancel: shutdowns of woken machines, and nothing else
		if !strings.HasPrefix(c, "shutdown ") {
			t.Errorf("after the cancel the run sent %q", c)
		}
	}
	has(t, host(done, "pc-upd"), StOff, "updated from 16-aaaaaaa to 17-bbbbbbb", "switched off again")
	has(t, host(done, "pc-on"), StOff, "not updated (the run was cancelled before its update)", "switched off again")
	has(t, host(done, "pc-wake"), StOff, "not updated (the run was cancelled before its update)")
	for _, id := range []string{"pc-on", "pc-wake"} { // woken before the cancel; no update was ever asked for
		if got := w.callsFor(id); strings.Contains(got, "update") || !strings.HasSuffix(got, "wake shutdown") {
			t.Fatalf("%s got %q", id, got)
		}
	}
	for _, id := range []string{"pc-upd", "pc-on", "pc-wake"} {
		if w.ms[id].on {
			t.Errorf("%s is still on", id)
		}
	}
	if !on.on || on.shutdowns != 0 {
		t.Fatal("the machine that was on")
	}
	skipped := 0
	for _, h := range done.Hosts {
		if h.State == StSkipped {
			skipped++
		}
	}
	if skipped == 0 || done.State != RunCancelled || !done.Cancel.SwitchOff {
		t.Fatalf("%s", describe(done))
	}
}

// The kill switch goes on in the middle: nothing more is sent, not even a cancel, and every machine is
// reported as it is.
func TestControlDisabledDuringARun(t *testing.T) {
	w := newWorld(t)
	w.off("pc-count").bootTakes = 20 * time.Second
	w.off("pc-wake").wakeBoots = false
	w.start(StartRequest{Hosts: []string{"pc-count", "pc-wake"}})
	for i := 0; i < 600; i++ {
		a, _ := w.m.Snapshot()
		if host(a, "pc-count").State == StShutting {
			break
		}
		w.tick()
	}
	before := len(w.calls)
	w.blocked = "control is disabled by an administrator"
	r := w.finish()
	if r.State != RunStopped || r.Note != "stopped: control is disabled by an administrator" || len(w.calls) != before {
		t.Fatalf("run %s note %q, requests after the switch: %v", r.State, r.Note, w.calls[before:])
	}
	has(t, host(r, "pc-count"), StLeftOn, "the run stopped (control is disabled by an administrator) while its shutdown was counting down", "it switches off by itself unless someone cancels that")
	has(t, host(r, "pc-wake"), StGaveUp, "A wake was sent to it: if it switches on, it stays ON.")
	// and no run starts while it is on
	if _, err := w.m.Start(StartRequest{Hosts: []string{"pc-wake"}, By: "Ann"}); err == nil || !strings.Contains(err.Error(), "control is disabled") {
		t.Fatal("a run started with control disabled:", err)
	}
}

// The machine is switched off for these runs, or removed from the panel, while the run works on it: the run
// lets go of it where it is. A woken machine is then ON and is named so.
func TestMachineTakenOutDuringARun(t *testing.T) {
	for what, do := range map[string]func(w *world){
		"switched off for these runs": func(w *world) { w.ms["pc-a"].info.OptedIn = false },
		"is no longer in the panel":   func(w *world) { w.ms["gone"] = w.ms["pc-a"]; delete(w.ms, "pc-a") },
	} {
		w := newWorld(t)
		pc := w.off("pc-a")
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		for i := 0; i < 600; i++ {
			a, _ := w.m.Snapshot()
			if host(a, "pc-a").State == StUpdating {
				break
			}
			w.tick()
		}
		before := len(w.calls)
		do(w)
		w.m.Tick(context.Background())
		_, hist := w.m.Snapshot()
		if len(hist) != 1 {
			t.Fatalf("%s: the run should be over", what)
		}
		has(t, host(hist[0], "pc-a"), StLeftOn, "The run woke it and it is ON.")
		if !strings.Contains(strings.ToLower(host(hist[0], "pc-a").Text), strings.ToLower(what)[:10]) || len(w.calls) != before || !pc.on {
			t.Fatalf("%s: %s", what, host(hist[0], "pc-a").Text)
		}
	}
}

// One run at a time, and what a start refuses. Nothing is kept or queued for a refused start.
func TestStartRules(t *testing.T) {
	w := newWorld(t)
	w.off("pc-a")
	w.off("pc-b")
	w.off("pc-no").info.OptedIn = false
	bad := func(req StartRequest, part string) {
		t.Helper()
		req.By = "Ann"
		r, err := w.m.Start(req)
		if err == nil || r != nil || !strings.Contains(err.Error(), part) {
			t.Fatalf("start %+v: %v (want %q)", req, err, part)
		}
		if a, hist := w.m.Snapshot(); a != nil || len(hist) != 0 {
			t.Fatal("a refused start left a run behind")
		}
	}
	bad(StartRequest{}, "no machine was chosen")
	bad(StartRequest{Hosts: []string{"pc-a", "pc-a"}}, "twice")
	bad(StartRequest{Hosts: []string{"pc-a"}, UpdateOnly: []string{"pc-a"}}, "twice")
	bad(StartRequest{Hosts: []string{"pc-a", "nobody"}}, "unknown machine: nobody")
	bad(StartRequest{Hosts: []string{"pc-no"}}, "PC-NO is not switched on for")
	var many []string
	for i := 0; i < MaxHosts+1; i++ {
		id := fmt.Sprintf("pc-%02d", i)
		w.off(id)
		many = append(many, id)
	}
	bad(StartRequest{Hosts: many}, "at most 16 machines")

	first := w.start(StartRequest{Hosts: []string{"pc-a"}})
	_, err := w.m.Start(StartRequest{Hosts: []string{"pc-b"}, By: "Bob"})
	se, ok := err.(*StartError)
	if !ok || !se.Busy || !strings.Contains(se.Msg, "a run started by Ann at 03:00 UTC is still going: one run at a time") {
		t.Fatalf("a second start while one runs: %v", err)
	}
	if a, _ := w.m.Snapshot(); a.ID != first.ID || len(a.Hosts) != 1 {
		t.Fatal("the running run changed")
	}
	w.finish()
	if got := w.callsFor("pc-b"); got != "" {
		t.Fatalf("the refused run's machine got %q", got)
	}
	// when the first is over, a new one starts
	second := w.start(StartRequest{Hosts: []string{"pc-b"}})
	if second.ID == first.ID {
		t.Fatal("ids repeat")
	}
	w.finish()
	if _, hist := w.m.Snapshot(); len(hist) != 2 || hist[0].ID != second.ID {
		t.Fatal("history: newest first")
	}
}

// ListHash names the confirmed list: the same machines and options give the same value in any order, anything
// else another one.
func TestListHash(t *testing.T) {
	base := ListHash([]string{"pc-a", "pc-b"}, []string{"win"}, false)
	if len(base) != 8 || base != ListHash([]string{"pc-b", "pc-a"}, []string{"win"}, false) {
		t.Fatal("order must not matter:", base)
	}
	for name, other := range map[string]string{
		"one more machine":   ListHash([]string{"pc-a", "pc-b", "pc-c"}, []string{"win"}, false),
		"one machine less":   ListHash([]string{"pc-a"}, []string{"win"}, false),
		"other list":         ListHash([]string{"pc-a", "pc-b", "win"}, nil, false),
		"swapped lists":      ListHash([]string{"win"}, []string{"pc-a", "pc-b"}, false),
		"the option":         ListHash([]string{"pc-a", "pc-b"}, []string{"win"}, true),
		"ids glued together": ListHash([]string{"pc-apc-b"}, []string{"win"}, false),
	} {
		if other == base {
			t.Errorf("%s: same hash", name)
		}
	}
}

// Pacing: twelve machines behind one relay, and three machines with two network cards each. An agent accepts
// 12 control requests a minute for everything; the run never sends one agent more than 8 in any minute, so no
// request of it is refused for that reason and 4 a minute stay free for the admin's own clicks.
func TestRunPacesItsRequestsPerAgent(t *testing.T) {
	t.Run("twelve machines, one relay", func(t *testing.T) {
		w := newWorld(t)
		var ids []string
		for i := 0; i < 12; i++ {
			id := fmt.Sprintf("pc-%02d", i)
			w.off(id).wakeBoots = i%2 == 0 // half of them never come online, so their wakes are repeated
			ids = append(ids, id)
		}
		w.start(StartRequest{Hosts: ids})
		r := w.finish()
		if w.limited != 0 || w.maxInMin > 8 {
			t.Fatalf("an agent refused %d requests; the most one agent got in a minute: %d (want at most 8)", w.limited, w.maxInMin)
		}
		off, gaveUp := 0, 0
		for _, h := range r.Hosts {
			switch h.State {
			case StOff:
				off++
			case StGaveUp:
				gaveUp++
			}
			if w.ms[h.ID].wakes < 1 {
				t.Errorf("%s was never sent a wake", h.ID)
			}
		}
		if off != 6 || gaveUp != 6 {
			t.Fatalf("off %d, gave up %d: %s", off, gaveUp, describe(r))
		}
		// the last machine had to wait for its turn: its wake went out in the second minute
		if d := host(r, "pc-11").WakeSentAt.Sub(r.StartedAt); d < 60*time.Second {
			t.Fatalf("the twelfth wake went out after %s: the first minute's share is 8", d)
		}
	})
	t.Run("three machines with two network cards", func(t *testing.T) {
		w := newWorld(t)
		for _, id := range []string{"pc-a", "pc-b", "pc-c"} {
			mc := w.off(id)
			mc.info.MACs = 2
			mc.wakeBoots = false
		}
		w.start(StartRequest{Hosts: []string{"pc-a", "pc-b", "pc-c"}})
		w.finish()
		if w.limited != 0 || w.maxInMin > 8 {
			t.Fatalf("refused %d, most in a minute %d", w.limited, w.maxInMin)
		}
	})
	t.Run("the relay is one of the machines that are on", func(t *testing.T) {
		// its own update counts with the wakes it relays
		w := newWorld(t)
		w.running("relay")
		var ids []string
		for i := 0; i < 8; i++ {
			id := fmt.Sprintf("pc-%02d", i)
			w.off(id)
			ids = append(ids, id)
		}
		w.start(StartRequest{Hosts: ids, UpdateOnly: []string{"relay"}})
		r := w.finish()
		if w.limited != 0 || w.maxInMin > 8 || host(r, "relay").State != StStayedOn {
			t.Fatalf("refused %d, most in a minute %d, relay %s", w.limited, w.maxInMin, host(r, "relay").State)
		}
	})
}

func TestPacer(t *testing.T) {
	p := newPacer(8)
	now := t0
	for i := 0; i < 8; i++ {
		if !p.take("relay", 1, now.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("request %d of the minute refused", i+1)
		}
	}
	if p.take("relay", 1, now.Add(30*time.Second)) || !p.take("other", 1, now.Add(30*time.Second)) {
		t.Fatal("the ninth to one agent must wait; another agent has its own count")
	}
	if p.take("relay", 1, now.Add(61*time.Second)) { // the first is 61 s old: the window is a minute and two seconds
		t.Fatal("too early")
	}
	if !p.take("relay", 1, now.Add(62*time.Second)) || p.take("relay", 1, now.Add(62*time.Second)) {
		t.Fatal("one place is free when the oldest request has left the window")
	}
	// more requests at once than the limit: only into an empty window
	small := newPacer(2)
	if !small.take("relay", 5, now) || small.take("relay", 1, now.Add(30*time.Second)) || !small.take("relay", 5, now.Add(63*time.Second)) {
		t.Fatal("a machine with more network cards than the limit")
	}
	if newPacer(0).limit != 1 {
		t.Fatal("a limit below one")
	}
}

// The hook for item 1.1. With an approver, the woken machines wait in "awaiting_approval" after they are
// online; the run asks once, when all of them are awake or given up on; the grant goes to their update and
// shutdown requests. A machine that was on needs none for being left alone, but its update waits too.
type approveAll struct{}

func (approveAll) Needed(HostInfo, Observation) bool { return true }

func TestApprovalAfterTheWake(t *testing.T) {
	w := newWorld(t)
	w.m.Approval = approveAll{}
	a := w.off("pc-a")
	b := w.off("pc-b")
	b.bootTakes = 100 * time.Second
	w.off("pc-dead").wakeBoots = false
	for _, mc := range []*machine{a, b} {
		mc.updMode, mc.shutMode = "approval", "approval" // their agents refuse without a grant
	}
	r := w.start(StartRequest{Hosts: []string{"pc-a", "pc-b", "pc-dead"}})
	asked := func() int {
		n := 0
		for _, e := range w.events {
			if e.Kind == EvApproval {
				n++
			}
		}
		return n
	}
	w.advance(200 * time.Second)
	act, _ := w.m.Snapshot()
	if host(act, "pc-a").State != StApproval || host(act, "pc-b").State != StApproval || asked() != 0 || act.Approval != "" {
		t.Fatalf("both awake, the third still waking: nobody is asked yet: %s (asked %d)", describe(act), asked())
	}
	if err := w.m.Approve("other", "grant"); err == nil {
		t.Fatal("another run's id")
	}
	w.advance(110 * time.Second) // the third is given up on
	act, _ = w.m.Snapshot()
	if asked() != 1 || act.Approval != "needed" {
		t.Fatalf("asked %d, approval %q", asked(), act.Approval)
	}
	for _, c := range w.calls { // nothing but wakes was sent before the approval
		if !strings.HasPrefix(c, "wake ") {
			t.Fatalf("before the approval the run sent %q", c)
		}
	}
	w.advance(60 * time.Second)
	if asked() != 1 {
		t.Fatal("asked again")
	}
	if err := w.m.Approve(r.ID, "grant"); err != nil {
		t.Fatal(err)
	}
	done := w.finish()
	has(t, host(done, "pc-a"), StOff, "updated from")
	has(t, host(done, "pc-b"), StOff, "updated from")
	has(t, host(done, "pc-dead"), StGaveUp)
	if done.Approval != "given" || a.on || b.on {
		t.Fatalf("approval %q", done.Approval)
	}

	// nobody approves: the woken machines are left on, and that is said
	w = newWorld(t)
	w.m.Approval = approveAll{}
	w.off("pc-a")
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	done = w.finish()
	has(t, host(done, "pc-a"), StLeftOn, "Nobody approved the update and the shutdown within 15 min. The run woke it and it is ON.")
	if got := w.callsFor("pc-a"); got != "wake" {
		t.Fatalf("requests: %q", got)
	}
	if err := w.m.Approve(done.ID, "grant"); err == nil {
		t.Fatal("a finished run takes no approval")
	}
}

// The page reads the run, and an admin cancels it, while the run works: Snapshot, CancelRun and Approve from
// other goroutines than the one that steps (run with -race).
func TestReadingAndCancellingWhileARunWorks(t *testing.T) {
	w := newWorld(t)
	w.m.Approval = approveAll{}
	for _, id := range []string{"pc-a", "pc-b", "pc-c"} {
		w.off(id)
	}
	r := w.start(StartRequest{Hosts: []string{"pc-a", "pc-b", "pc-c"}})
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			a, hist := w.m.Snapshot()
			for _, x := range append(hist, a) {
				if x != nil {
					_ = describe(x)
				}
			}
			_ = w.m.JournalError()
			if i%50 == 0 {
				_ = w.m.Approve(r.ID, "grant")
			}
			if i == 4000 {
				_, _ = w.m.CancelRun(r.ID, "Bob", true)
			}
		}
	}()
	end := w.finish()
	close(stop)
	<-done
	if end.ID != r.ID || len(end.Hosts) != 3 {
		t.Fatal("the run")
	}
}
