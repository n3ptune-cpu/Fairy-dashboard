package wakerun

import (
	"strings"
	"testing"
	"time"
)

// Build 14: after how long without an answer a machine counts as off is a setting (wake_update.off_after_s,
// default 900), and it and the countdown of the shutdown can be set for one machine (the wake_update: block
// of its systems: entry). A run takes the machine's own values wherever it took the panel's.

// off7 is a machine that is off and has given nothing for seven minutes.
func off7(w *world, id string) *machine {
	mc := w.off(id)
	w.silentFor(mc, 7*time.Minute)
	return mc
}

// Silent for seven minutes: a machine that counts as off after six is off, and is woken, updated and switched
// off again; one with the default is "not sure" and is sent nothing; and so is one with six minutes that has
// been silent for five, with its own number in the sentence.
func TestAMachineCountsAsOffAfterItsOwnTime(t *testing.T) {
	w := newWorld(t)
	six := off7(w, "pc-six")
	six.info.OffAfter = 6 * time.Minute
	off7(w, "pc-default")
	five := w.off("pc-five")
	five.info.OffAfter = 6 * time.Minute
	w.silentFor(five, 5*time.Minute)
	w.start(StartRequest{Hosts: []string{"pc-six", "pc-default", "pc-five"}})
	r := w.finish()
	has(t, host(r, "pc-six"), StOff, "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.")
	has(t, host(r, "pc-default"), StSkipped, "Not sure that it is off, so nothing was sent to it: Fairy takes a machine for off only after 15 min without an answer (it may be restarting, or be on and out of reach).")
	has(t, host(r, "pc-five"), StSkipped, "Not sure that it is off, so nothing was sent to it: Fairy takes a machine for off only after 6 min without an answer (it may be restarting, or be on and out of reach).")
	if got := strings.Join(w.calls, ", "); got != "wake pc-six, update pc-six, shutdown pc-six 120" {
		t.Fatalf("requests: %s", got)
	}
	// the report has the machine's own time where it is not the panel's, and nothing where it is
	if h := host(r, "pc-six"); h.OffAfterS == nil || *h.OffAfterS != 360 || h.ShutdownDelayS != nil {
		t.Errorf("pc-six: off_after_s %v, shutdown_delay_s %v", h.OffAfterS, h.ShutdownDelayS)
	}
	if h := host(r, "pc-default"); h.OffAfterS != nil || h.ShutdownDelayS != nil {
		t.Errorf("pc-default has timings of its own: %v %v", h.OffAfterS, h.ShutdownDelayS)
	}
	if r.Settings.OffAfterS != 900 || r.Settings.OffAfter != MinOff {
		t.Errorf("the run's settings: %+v", r.Settings)
	}
}

// The panel's own setting: with off_after_s 360 every machine that has none of its own counts as off after six
// minutes; one that has its own, longer one does not.
func TestThePanelsOffAfterIsASetting(t *testing.T) {
	w := newWorld(t)
	w.m.Settings = w.m.Settings.WithOffAfter(360)
	off7(w, "pc-a")
	long := off7(w, "pc-long")
	long.info.OffAfter = 30 * time.Minute
	same := off7(w, "pc-same")
	same.info.OffAfter = 6 * time.Minute // its own, and the panel's value: nothing to say in the report
	w.start(StartRequest{Hosts: []string{"pc-a", "pc-long", "pc-same"}})
	r := w.finish()
	has(t, host(r, "pc-a"), StOff, "switched off again.")
	has(t, host(r, "pc-same"), StOff, "switched off again.")
	has(t, host(r, "pc-long"), StSkipped, "Fairy takes a machine for off only after 30 min without an answer")
	if h := host(r, "pc-long"); h.OffAfterS == nil || *h.OffAfterS != 1800 {
		t.Errorf("pc-long: %v", h.OffAfterS)
	}
	if host(r, "pc-a").OffAfterS != nil || host(r, "pc-same").OffAfterS != nil || r.Settings.OffAfterS != 360 {
		t.Errorf("the report: %v %v, settings %+v", host(r, "pc-a").OffAfterS, host(r, "pc-same").OffAfterS, r.Settings)
	}
	// a setting of 0 or less is no setting
	if s := NewSettings(300, 60, 120, 8).WithOffAfter(0); s.OffAfter != MinOff || s.OffAfterS != 900 {
		t.Errorf("WithOffAfter(0): %+v", s)
	}
	// "off" for the page, with the same time
	o := w.Observe("pc-long")
	if !OffNowAfter(o, w.now, 6*time.Minute) || OffNowAfter(o, w.now, 30*time.Minute) || OffNow(o, w.now) != OffNowAfter(o, w.now, 0) {
		t.Errorf("OffNowAfter for a machine that has given nothing for %s", QuietFor(o, w.now))
	}
}

// A run with two machines whose countdowns are not the same: each is asked for its own, each is watched for
// its own (the one with 30 s is "switched off again" long before the other's two minutes are over), and the
// run's own time limit is made with the longest of them.
func TestTwoMachinesWithDifferentCountdowns(t *testing.T) {
	w := newWorld(t)
	short := w.off("pc-short")
	short.info.ShutdownDelay = 30 * time.Second
	w.off("pc-long")
	w.start(StartRequest{Hosts: []string{"pc-short", "pc-long"}})
	r := w.finish()
	var asked []string
	for _, c := range w.calls {
		if strings.HasPrefix(c, "shutdown ") {
			asked = append(asked, c)
		}
	}
	if got := strings.Join(asked, ", "); got != "shutdown pc-short 30, shutdown pc-long 120" && got != "shutdown pc-long 120, shutdown pc-short 30" {
		t.Fatalf("shutdown requests: %s", got)
	}
	a, b := host(r, "pc-short"), host(r, "pc-long")
	has(t, a, StOff, "switched off again.")
	has(t, b, StOff, "switched off again.")
	if a.ShutdownDelayS == nil || *a.ShutdownDelayS != 30 || b.ShutdownDelayS != nil || r.Settings.ShutdownDelayS != 120 {
		t.Fatalf("the report: pc-short %v, pc-long %v, settings %d", a.ShutdownDelayS, b.ShutdownDelayS, r.Settings.ShutdownDelayS)
	}
	// each countdown, from the request to the time the machine was told: 30 s and 120 s
	countdown := func(h *Host) time.Duration {
		for _, l := range h.Log {
			if strings.HasPrefix(l.Text, "shutting down at") {
				return h.ShutdownAt.Sub(l.T)
			}
		}
		t.Fatalf("%s: no shutdown in its steps: %+v", h.ID, h.Log)
		return 0
	}
	if countdown(a) != 30*time.Second || countdown(b) != 120*time.Second {
		t.Fatalf("the countdowns: %s and %s", countdown(a), countdown(b))
	}
	// both were woken at the same time and take the same time to start and update: the short one is off about
	// a minute and a half before the long one
	if d := b.Since.Sub(a.Since); d < 80*time.Second || d > 100*time.Second {
		t.Fatalf("pc-short ended %s before pc-long, want about 90 s", d)
	}
	if short.on || w.ms["pc-long"].on {
		t.Fatal("a machine is still on")
	}
}

// A request for a shutdown that got no answer: the sentence about what may still happen names the machine's
// own countdown, not the panel's.
func TestALostShutdownNamesTheMachinesOwnCountdown(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	pc.info.ShutdownDelay = 45 * time.Second
	pc.shutLost = "never"
	w.start(StartRequest{Hosts: []string{"pc-a"}})
	h := host(w.finish(), "pc-a")
	if h.State != StLeftOn || !strings.Contains(h.Text, "The request may still arrive: the machine then switches off 45 s later") || strings.Contains(h.Text, "2 min later") {
		t.Fatalf("[%s] %s", h.State, h.Text)
	}
}

// The run's own time limit has room for the longest countdown of its machines.
func TestTheRunsLimitHasRoomForTheLongestCountdown(t *testing.T) {
	w := newWorld(t)
	s := NewSettings(300, 60, 120, 8)
	plain := &Run{Settings: s, Hosts: []*Host{{ID: "a"}, {ID: "b"}}}
	one := &Run{Settings: s, Hosts: []*Host{{ID: "a"}, {ID: "b", shutDelay: time.Hour}}}
	shorter := &Run{Settings: s, Hosts: []*Host{{ID: "a", shutDelay: 10 * time.Second}, {ID: "b"}}}
	if d := w.m.maxRun(one) - w.m.maxRun(plain); d != time.Hour-2*time.Minute {
		t.Errorf("a machine with a countdown of an hour makes the limit %s longer, want 58 minutes", d)
	}
	if w.m.maxRun(shorter) != w.m.maxRun(plain) {
		t.Errorf("a machine with a shorter countdown changes the limit: %s / %s", w.m.maxRun(shorter), w.m.maxRun(plain))
	}
}

// The proof for a shutdown (mayShutDown) asks for the machine's own time: a machine that had given nothing
// for seven minutes when the run started was off for a machine that counts as off after six, and was not for
// one with the default.
func TestTheProofOfOffAtTheStartUsesTheMachinesOwnTime(t *testing.T) {
	quiet := int64(7 * 60)
	h := &Host{QuietForS: &quiet}
	if offAtStart(h) {
		t.Error("seven minutes, the default: off at the start")
	}
	h.offAfter = 6 * time.Minute
	if !offAtStart(h) {
		t.Error("seven minutes, its own six: not off at the start")
	}
	h.offAfter = 8 * time.Minute
	if offAtStart(h) {
		t.Error("seven minutes, its own eight: off at the start")
	}
	if ok, why := mayShutDown(&Host{Mode: ModeWake, StartStatus: "offline", QuietForS: &quiet, offAfter: 8 * time.Minute}, HostInfo{}, Observation{}, nil); ok ||
		why != "Fairy is not sure that it was off before the run: it had not gone 8 min without an answer" {
		t.Errorf("mayShutDown: %v %q", ok, why)
	}
}
