package wakerun

import (
	"strings"
	"testing"
	"time"
)

// "Switched off again" for a machine that is ON (random run 3904 of the machines that check approvals; the
// watch is the same for every machine, and has been since before the check). The run asked for the shutdown
// and saw its countdown. The countdown did not switch the machine off: it was called off on the machine itself
// while the panel could not reach it, or the shutdown did not happen. After the countdown's time the machine
// answered, for minutes, with no shutdown pending. Then the link dropped (within the five minutes the run
// still watches), two polls after the due time got nothing, and that silence was read as the machine going
// off by the run's countdown.
//
// A machine whose agent has answered after the countdown's time with nothing pending is up after its
// countdown. Its silence later is a machine that is out of reach, or one that somebody or something else
// switched off: the run says when it last answered and that it does not know.
func TestAMachineSeenUpAfterItsCountdownIsNotOffByItsSilenceLater(t *testing.T) {
	type at struct{ from, to time.Duration } // an outage, counted from the countdown's end (to 0: for good)
	for name, c := range map[string]struct {
		checked   bool
		ignore    bool          // the shutdown does not happen
		calledOff time.Duration // the countdown is called off on the machine itself then (from its end; 0: not)
		down      []at
		stop      time.Duration // the kill switch stops the run then (0: not)
		want      string
	}{
		"called off on the machine while it was out of reach, as in run 3904": {
			calledOff: -18 * time.Second, down: []at{{-30 * time.Second, 61 * time.Second}, {194 * time.Second, 0}},
			want: " after its countdown's time, with no shutdown pending: the shutdown was called off on the machine, or did not happen. It does not answer now: not known whether it is on."},
		"the same for a machine that checks approvals": {checked: true,
			calledOff: -18 * time.Second, down: []at{{-30 * time.Second, 61 * time.Second}, {194 * time.Second, 0}},
			want: " after its countdown's time, with no shutdown pending: the shutdown was called off on the machine, or did not happen. It does not answer now: not known whether it is on."},
		"the shutdown did not happen, and two minutes later the link drops": {ignore: true, down: []at{{120 * time.Second, 0}},
			want: " after its countdown's time, with no shutdown pending: the shutdown was called off on the machine, or did not happen. It does not answer now: not known whether it is on."},
		"the same, and the run is stopped before it has looked twice": {ignore: true, down: []at{{120 * time.Second, 0}}, stop: 125 * time.Second,
			want: " Asked to switch off, not confirmed: it does not answer now."},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			pc := w.off("pc-a")
			if c.checked {
				pc = w.checkedOff("pc-a")
			}
			if c.ignore {
				pc.shutMode = "ignore"
			}
			var due time.Time
			confirmed := false
			w.afterTick = func() {
				if a, _ := w.m.Snapshot(); a != nil && a.Approval == "needed" && !confirmed {
					confirmed = true
					if _, err := w.confirm("Ann"); err != nil {
						t.Fatal(err)
					}
				}
				if pc.pending != nil && due.IsZero() {
					due = pc.pending.At
				}
				if due.IsZero() {
					return
				}
				since := w.now.Sub(due)
				if c.calledOff != 0 && since >= c.calledOff && pc.pending != nil {
					pc.pending = nil
					w.runCancelled(pc)
				}
				pc.unreachable = false
				for _, d := range c.down {
					if since >= d.from && (d.to == 0 || since < d.to) {
						pc.unreachable = true
					}
				}
				if c.stop != 0 && since >= c.stop {
					w.blocked = "control is disabled by an administrator"
				}
			}
			w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
			r := over(t, w)
			h := host(r, "pc-a")
			if !pc.on {
				t.Fatal("the model: the machine went off")
			}
			if h.On == OnNo || h.State == StOff {
				t.Fatalf("REPORTED OFF, AND IT IS ON: [%s] power %q: %s", h.State, h.On, h.Text)
			}
			if h.State != StLeftOn || h.On != OnUnknown || !strings.HasSuffix(h.Text, c.want) {
				t.Fatalf("[%s] power %q: %s\nwant ...%s", h.State, h.On, h.Text, c.want)
			}
			if c.stop == 0 && !strings.Contains(h.Text, "and asked to switch off. Its agent still answered at ") {
				t.Fatalf("the report does not say when it last answered: %s", h.Text)
			}
		})
	}
}

// What is right today stays: a machine whose countdown was seen and that stops answering at about its time,
// with no answer after it, went off.
func TestAMachineThatGoesSilentAtItsCountdownsEndIsOff(t *testing.T) {
	w := newWorld(t)
	pc := w.off("pc-a")
	w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
	r := over(t, w)
	if h := host(r, "pc-a"); h.State != StOff || h.On != OnNo || !strings.HasSuffix(h.Text, "and switched off again.") || pc.on {
		t.Fatalf("[%s] power %q: %s (the machine is on: %v)", h.State, h.On, h.Text, pc.on)
	}
}

// The same in the watch of a machine's own switch-off (nobody confirmed it; third look at cdd080f). The machine
// was started again while the panel could not reach it: its run is gone, and it stays on. One answer between
// two outages showed no run of this run's: one is not enough to say "not armed any more" (two are asked for),
// but it did take back "out of reach before the end". The silence after it, past the due time, was "It
// switched itself off again" for a machine that was on. Silence proves the machine's own switch-off only when
// the last answer the run read showed this run as armed or counting down.
func TestAnArmedMachineThatLastShowedNoRunIsNotOffByItsSilence(t *testing.T) {
	for name, c := range map[string]struct {
		restart bool   // the machine is started again during the first outage
		want    string // the end of the report
	}{
		"started again, one answer shows no run": {restart: true,
			want: "It was switching itself off; the last answer of its agent, at 03:19:28 UTC, showed no run on it, and it does not answer now: not confirmed that it is off."},
	} {
		t.Run(name, func(t *testing.T) {
			w, pc, _ := armedRun(t, nil)
			var t0 time.Time // the watch of its switch-off begins
			restarted := false
			w.afterTick = func() {
				a, _ := w.m.Snapshot()
				if a == nil {
					return
				}
				if t0.IsZero() {
					if stateOf(a, "pc-a") == StSelfOff {
						t0 = w.now
					}
					return
				}
				since := w.now.Sub(t0)
				if c.restart && since >= 10*time.Second && !restarted {
					restarted = true
					pc.bootedAt, pc.agentReadyAt, pc.pending, pc.updating = w.now, w.now.Add(5*time.Second), nil, false
				}
				pc.unreachable = since >= 5*time.Second && since != 150*time.Second // one answer, after the earliest end of its countdown
			}
			r := over(t, w)
			h := host(r, "pc-a")
			if !pc.on {
				t.Fatal("the model: the machine went off")
			}
			if h.On == OnNo || h.State == StOff {
				t.Fatalf("REPORTED OFF, AND IT IS ON: [%s] power %q: %s", h.State, h.On, h.Text)
			}
			if h.State != StLeftOn || h.On != OnUnknown || !strings.HasSuffix(h.Text, c.want) {
				t.Fatalf("[%s] power %q: %s\nwant ...%s", h.State, h.On, h.Text, c.want)
			}
		})
	}
}

// What is right today stays here too: the last answer showed the switch-off counting down, then nothing.
func TestAnArmedMachineThatGoesSilentWhileItCountsDownIsOff(t *testing.T) {
	w, pc, _ := armedRun(t, nil)
	r := over(t, w)
	if h := host(r, "pc-a"); h.State != StOff || h.On != OnNo || !strings.HasSuffix(h.Text, "It switched itself off again.") || pc.on {
		t.Fatalf("[%s] power %q: %s (the machine is on: %v)", h.State, h.On, h.Text, pc.on)
	}
}
