package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/scheduler"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// Second review of fix7a (R7A-01, R7A-02), with the real poller and powerctl.
//
// The run's shutdown is accepted and its countdown is seen. The link to the machine drops before the countdown
// ends and stays down, and the shutdown does not happen on the machine (called off at its keyboard, or the
// system did not go down). The run is right as long as nothing else happens: the machine was out of reach
// before its countdown ended, so its silence afterwards proves nothing: "not confirmed that it is off".
//
// But the poller starts a NEW row of silent polls whenever the panel was not watching for a while: the time of
// day was set (back by any amount, forward by more than half a minute), the panel's host was suspended (the
// time of day jumps, the steady clock stands still), polling was paused and taken up again. The run took the
// new row for a new silence and forgot that the machine had been out of reach before, although it never
// answered in between: two polls and 20 s later it wrote "switched off again". The machine was on. The same
// when the run's look came late and never saw the first poll of the row (R7A-02).

// cutFarm is the farm with a time of day that a test can set apart from the steady clock.
func cutFarm(t *testing.T) (*env, *farm, *time.Duration) {
	t.Helper()
	e := setup(t)
	f := newFarm(t, e)
	skew := new(time.Duration)
	began := f.now
	p := e.s.Poller
	p.Clock = func() time.Time { return f.now.Add(*skew) }
	p.Mono = func() time.Duration { return f.now.Sub(began) }
	e.s.WakeRuns.Now = p.Clock
	e.s.WakeRuns.Mono = p.Mono
	f.offline()
	f.optIn("pc-a", "pc-b")
	e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
		if path == "/power" {
			return 200, jsonx.M{"action": "shutdown", "pending": jsonx.M{"action": "shutdown", "at": float64(f.now.Add(120 * time.Second).Unix()), "delay_s": 120.0, "requested_by": "x"}}, nil
		}
		return 0, nil, nil
	}
	return e, f, skew
}

const (
	cutNotConfirmed = "It was already out of reach before its countdown ended, and it does not answer now: Fairy cannot tell a machine that went off " +
		"from a connection that dropped. Not confirmed that it is off."
	cutOff = "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again."
)

func TestWakeUpdateOutOfReachBeforeTheEndIsNotForgotten(t *testing.T) {
	for _, c := range []struct {
		name string
		// drop: the link to the machine goes down this long before the countdown's end, and stays down (0: it stays up)
		drop time.Duration
		// goesOff: the shutdown happens on the machine at the end of its countdown
		goesOff bool
		// set: the panel's time of day is set by this much, 8 s after the countdown's end (the steady clock goes on)
		set time.Duration
		// pause: polling of the machine is paused from 5 s before the end to 5 s after it
		pause bool
		// lateLook: the run does not look at the machine from 6 s before the end to 4 s after it (its step waits
		// for another machine), while the poller polls it twice: once before the end, once after it
		lateLook bool
		want     string
	}{
		{name: "control: the link drops 30 s before the end, nothing else", drop: 30 * time.Second, want: cutNotConfirmed},
		{name: "the time of day is set back two seconds", drop: 30 * time.Second, set: -2 * time.Second, want: cutNotConfirmed},
		{name: "the time of day is set back one hour", drop: 30 * time.Second, set: -time.Hour, want: cutNotConfirmed},
		{name: "the time of day is set forward one minute", drop: 30 * time.Second, set: time.Minute, want: cutNotConfirmed},
		{name: "the panel's host was suspended for an hour (the time of day jumps, the steady clock does not)", drop: 30 * time.Second, set: time.Hour, want: cutNotConfirmed},
		{name: "polling is paused around the end and taken up again", drop: 30 * time.Second, pause: true, want: cutNotConfirmed},
		{name: "the run's look comes late and misses the poll before the end", drop: 5 * time.Second, lateLook: true, want: cutNotConfirmed},
		{name: "the look comes late, and the time of day is set back after it", drop: 5 * time.Second, lateLook: true, set: -2 * time.Second, want: cutNotConfirmed},
		// and what must stay: a machine that goes off at the end of an undisturbed countdown is seen going off
		{name: "it goes off, nothing else", goesOff: true, want: cutOff},
		{name: "it goes off, and the time of day is set back two seconds after it", goesOff: true, set: -2 * time.Second, want: cutOff},
		{name: "it goes off, and the time of day is set forward one minute after it", goesOff: true, set: time.Minute, want: cutOff},
		{name: "it goes off while polling is paused around the end", goesOff: true, pause: true, want: cutOff},
		{name: "it goes off, and the run's look comes late", goesOff: true, lateLook: true, want: cutOff},
	} {
		e, f, skew := cutFarm(t)
		var due time.Time
		linkDown := false
		e.b.Mu.Lock()
		e.b.Metrics["pc-a"] = func() (jsonx.M, error) {
			if at, ok := f.pending["pc-a"]; ok {
				due = at
			}
			if linkDown || (c.drop > 0 && !due.IsZero() && !f.now.Before(due.Add(-c.drop))) {
				linkDown = true
				return nil, devices.Errf("connection failed") // the link is down, and stays down
			}
			return f.metrics("pc-a")
		}
		e.b.Mu.Unlock()
		if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"}); err != nil {
			t.Fatal(c.name, err)
		}
		var run *wakerun.Run
		set, paused, late := false, false, false
		for i := 0; i < 3600 && run == nil; i++ {
			if c.lateLook && !late && !due.IsZero() && !f.now.Before(due.Add(-7*time.Second)) {
				// the poller goes on (a poll 5 s before the end and one 3 s after it); the run does not look
				late = true
				for _, at := range []time.Duration{-5 * time.Second, 3 * time.Second} {
					f.now = due.Add(at)
					e.b.Mu.Lock()
					if !c.goesOff {
						f.on["pc-a"] = true
					} else if at > 0 {
						f.on["pc-a"] = false
						delete(f.pending, "pc-a")
					}
					e.b.Mu.Unlock()
					e.s.Poller.PollOnce(context.Background())
				}
			}
			f.tick()
			e.b.Mu.Lock()
			if !c.goesOff && !due.IsZero() && !f.now.Before(due) {
				f.on["pc-a"] = true // the shutdown did not happen: the machine stays up
			}
			e.b.Mu.Unlock()
			if c.pause && !due.IsZero() {
				want := !f.now.Before(due.Add(-5*time.Second)) && f.now.Before(due.Add(5*time.Second))
				if want != paused {
					paused = want
					var o *scheduler.Override
					if want {
						o = &scheduler.Override{Paused: true}
					}
					if err := e.s.Poller.SetPoll("pc-a", o); err != nil {
						t.Fatal(c.name, err)
					}
				}
			}
			if c.set != 0 && !set && !due.IsZero() && !f.now.Before(due.Add(8*time.Second)) {
				set, *skew = true, c.set
			}
			if a, hist := e.s.WakeRuns.Snapshot(); a == nil && len(hist) > 0 {
				run = hist[0]
			}
		}
		if run == nil {
			t.Fatal(c.name, "the run did not end")
		}
		h := hostOf(run, "pc-a")
		// the link comes back: is the machine on?
		linkDown = false
		e.b.Mu.Lock()
		on := f.on["pc-a"]
		e.b.Metrics["pc-a"] = func() (jsonx.M, error) { return f.metrics("pc-a") }
		e.b.Mu.Unlock()
		for i := 0; i < 60; i++ {
			f.tick()
			e.b.Mu.Lock()
			f.on["pc-a"] = on
			e.b.Mu.Unlock()
		}
		status := ""
		for _, x := range e.s.Poller.Snapshot() {
			if x.ID == "pc-a" {
				status = x.Status
			}
		}
		if h.On == wakerun.OnNo && status == "online" {
			t.Errorf("%s: REPORTED OFF, AND IT IS ON: %s", c.name, h.Text)
		}
		if !strings.Contains(h.Text, c.want) || (c.want == cutOff) != (h.State == wakerun.StOff && h.On == wakerun.OnNo) ||
			(c.want == cutNotConfirmed && (h.State != wakerun.StLeftOn || h.On != wakerun.OnUnknown)) {
			t.Errorf("%s: [%s] power %q, the machine is %q afterwards: %s", c.name, h.State, h.On, status, h.Text)
		}
	}
}
