package web

import (
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// Audit 5, round 3 (r3-p2-03; second review of fix6, N1), with the real poller, powerctl and run: a reverse
// proxy stands in front of the agent. A request of the run goes through it to the agent, the agent acts on it,
// and the agent's answer does not come back in the proxy's time: the proxy answers 504 Gateway Timeout with
// its own page (502 when the connection to the agent broke after the request, 503 when it gave up on it).
// Since d728730 every status of 400 or more was "a refusal: nothing happened". The farm acts on every request
// it was sent, like the agent behind the proxy does. The first two are the auditor's proofs (TestR3P2...).

// errorPage is what the real HTTP backend gives for an error status with a body that is not JSON
// (poller/errorpage_test.go).
func errorPage(status int) (int, jsonx.M, error) {
	return status, nil, devices.Errf("invalid response")
}

// The shutdown: "Left ON: the shutdown was not accepted (agent unreachable: http 504, and not an answer of
// the agent)", power on; three minutes later the machine was off.
func TestWakeUpdateAGatewayErrorForTheShutdownIsNotARefusal(t *testing.T) {
	for _, status := range []int{504, 502, 503, 404} {
		e, f := lostFarm(t, "")
		e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
			if agent == "pc-a" && path == "/power" {
				return errorPage(status)
			}
			return 0, nil, nil
		}
		if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"}); err != nil {
			t.Fatal(err)
		}
		h := hostOf(f.finish(), "pc-a")
		for i := 0; i < 180; i++ { // the countdown the agent took runs out
			f.tick()
		}
		if h.On == wakerun.OnYes && !f.on["pc-a"] {
			t.Errorf("%d: WRONG REPORT: the run says %q (%s) and the machine went off by the run's own shutdown request: %s", status, h.On, h.State, h.Text)
		}
		// the request arrived: its agent showed the countdown, and the machine went: "off", like after a timeout
		if h.State != wakerun.StOff || h.On != wakerun.OnNo || !h.ShutdownUnsure || f.on["pc-a"] {
			t.Errorf("%d: [%s] power %q, shutdown unconfirmed %v, machine on %v: %s", status, h.State, h.On, h.ShutdownUnsure, f.on["pc-a"], h.Text)
		}
		if got := strings.Join(f.requests(), ", "); got != "nas /wol, pc-a /update, pc-a /power" { // never sent again
			t.Errorf("%d: requests: %s", status, got)
		}
	}
}

// The wake: the relay behind the proxy sent the packet, the proxy answers 504. The run said "The wake could
// not be sent ... It was not woken.", power "untouched", and ended; the machine started and stayed on with
// nobody told. Now it is waited for, and left ON and reported: a wake request without an answer is not a wake
// the run knows of (mayShutDown).
func TestWakeUpdateAGatewayErrorForTheWakeIsNotNothingSent(t *testing.T) {
	for _, status := range []int{504, 502, 503, 403} {
		e, f := lostFarm(t, "")
		e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
			if agent == "nas" && path == "/wol" {
				return errorPage(status)
			}
			return 0, nil, nil
		}
		if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"}); err != nil {
			t.Fatal(err)
		}
		h := hostOf(f.finish(), "pc-a")
		for i := 0; i < 120; i++ {
			f.tick()
		}
		if h.On == wakerun.OnUntouched && f.on["pc-a"] {
			t.Errorf("%d: WRONG REPORT: the run says %q (%s: %s) and the machine was started by the run's wake packet and is ON", status, h.On, h.State, h.Text)
		}
		if h.State != wakerun.StLeftOn || h.On != wakerun.OnYes || !h.WakeUnsure || !f.on["pc-a"] ||
			!strings.Contains(h.Text, "Left ON: the run's wake request for it got no answer, so Fairy cannot tell that the run woke it.") {
			t.Errorf("%d: [%s] power %q, wake unconfirmed %v, machine on %v: %s", status, h.State, h.On, h.WakeUnsure, f.on["pc-a"], h.Text)
		}
		for _, r := range f.requests() {
			if r == "pc-a /power" {
				t.Errorf("%d: a shutdown was asked for after a wake that only got an error page: %s", status, strings.Join(f.requests(), ", "))
			}
		}
	}
}

// The cancel (second review of fix6, N1): "Cancel run" with "leave on" during the countdown, and the proxy
// answers the cancel with 504. With the refusal of d728730 the run did not know that the cancel may have
// arrived: when the machine then went out of reach, the last look said "It is off (not answering)" for a
// machine whose countdown was called off. A cancel without an answer is never "off".
func TestWakeUpdateAGatewayErrorForTheCancelIsNotOff(t *testing.T) {
	e, f := lostFarm(t, "")
	cancelSent := false
	e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
		if agent == "pc-a" && path == "/power" && cancelSent {
			return errorPage(504)
		}
		return 0, nil, nil
	}
	r, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3600; i++ {
		f.tick()
		if a, _ := e.s.WakeRuns.Snapshot(); a != nil && hostOf(a, "pc-a").State == wakerun.StShutting {
			break
		}
	}
	for i := 0; i < 10; i++ {
		f.tick()
	}
	e.b.Mu.Lock()
	cancelSent = true
	e.b.Metrics["pc-a"] = func() (jsonx.M, error) { return nil, devices.Errf("connection failed") } // and the link goes
	e.b.Mu.Unlock()
	if _, err := e.s.WakeRuns.CancelRun(r.ID, "Alice", false); err != nil {
		t.Fatal(err)
	}
	h := hostOf(f.finish(), "pc-a")
	for i := 0; i < 180; i++ {
		f.tick()
	}
	if !f.on["pc-a"] {
		t.Fatalf("the farm took the cancel, and the machine went off all the same: %s", strings.Join(f.requests(), ", "))
	}
	if h.On == wakerun.OnNo || strings.Contains(h.Text, "It is off") {
		t.Errorf("REPORTED OFF after a cancel that arrived (the machine is on): %s", h.Text)
	}
	if h.State != wakerun.StLeftOn || h.On != wakerun.OnUnknown ||
		!strings.Contains(h.Text, "the request to call its shutdown countdown (due at ") || !strings.Contains(h.Text, "off got no answer (agent unreachable: http 504, and not an answer of the agent): it may have arrived.") {
		t.Errorf("[%s] power %q: %s", h.State, h.On, h.Text)
	}
}
