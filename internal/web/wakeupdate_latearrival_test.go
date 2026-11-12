package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// Audit 6, r4-p2-02, with the real poller, powerctl and run manager (the auditor's proof under a normal name,
// with what must hold now). The run's shutdown request gets no answer in the panel's time and reaches the agent
// some seconds AFTER the panel stopped waiting; the polls in between are answered and show no countdown. The
// run wrote "the request did not arrive. It is ON." after two of them, and the machine went off unwatched.

// lateFarm runs "Update unpowered hosts" for pc-a with a shutdown request whose answer is lost, and which the
// farm's agent gets late after the panel stopped waiting (late < 0: never). It returns the machine's result and
// lets the farm go on until well after the countdown of a request that did arrive.
func lateFarm(t *testing.T, late time.Duration) (*wakerun.Host, *farm) {
	t.Helper()
	e := setup(t)
	f := newFarm(t, e)
	f.offline()
	f.optIn("pc-a")
	lost := false
	e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
		if agent == "pc-a" && path == "/power" && !lost {
			lost = true
			return 0, nil, context.DeadlineExceeded // the panel waited its 12 s and gave up
		}
		return 0, nil, nil
	}
	if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"}); err != nil {
		t.Fatal(err)
	}
	var arrives time.Time
	var final *wakerun.Host
	for i := 0; i < 3600; i++ {
		a, hist := e.s.WakeRuns.Snapshot()
		if a == nil && len(hist) > 0 && final == nil {
			final = hostOf(hist[0], "pc-a")
		}
		f.tick()
		e.b.Mu.Lock()
		if lost && arrives.IsZero() { // the farm must not act on the request yet: it is still on its way
			f.seen = len(e.b.Requests)
			arrives = f.now.Add(late)
		}
		if late >= 0 && !arrives.IsZero() && f.now.Equal(arrives) { // now it reaches the agent: its countdown of 120 s starts
			f.pending["pc-a"] = f.now.Add(120 * time.Second)
		}
		e.b.Mu.Unlock()
		if final != nil && !arrives.IsZero() && f.now.After(arrives.Add(130*time.Second)) {
			break
		}
	}
	if final == nil {
		t.Fatalf("%v late: the run did not end", late)
	}
	return final, f
}

func TestWakeUpdateAShutdownRequestThatArrivesAfterThePanelStoppedWaiting(t *testing.T) {
	for _, late := range []time.Duration{time.Second, 3 * time.Second, 5 * time.Second, 20 * time.Second, 90 * time.Second} {
		h, f := lateFarm(t, late)
		if h.State != wakerun.StOff || h.On != wakerun.OnNo || f.on["pc-a"] || h.Text != "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again." {
			t.Errorf("the request reaches the agent %v after the panel stopped waiting: [%s] power %q, the machine on: %v: %s", late, h.State, h.On, f.on["pc-a"], h.Text)
		}
		if strings.Contains(h.Text, "did not arrive") {
			t.Errorf("%v late: the report says that the request did not arrive: %s", late, h.Text)
		}
	}
}

// The request never arrives: no "did not arrive", no "It is ON" as the last word. The run watches until five
// minutes after the countdown would have ended, and the machine's power goes into the record as "not known".
func TestWakeUpdateAShutdownRequestWithoutAnAnswerThatNeverShows(t *testing.T) {
	h, f := lateFarm(t, -1)
	want := "The shutdown request got no answer, and its agent has shown no countdown since (7 min). The request may still arrive: the machine then switches off 2 min later " +
		"unless someone cancels that on its page. It is on now, and Fairy no longer watches it. It was updated to 17-bbbbbbb."
	if h.State != wakerun.StLeftOn || h.On != wakerun.OnUnknown || !f.on["pc-a"] || h.Text != want {
		t.Errorf("[%s] power %q, the machine on: %v: %s", h.State, h.On, f.on["pc-a"], h.Text)
	}
}
