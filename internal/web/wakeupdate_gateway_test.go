package web

import (
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// Second review of fix7a (R7A-08), with the real poller, powerctl and run. The farm acts on every request it
// was sent, like the agent behind a gateway does.

// A gateway in front of the agent that answers in JSON. The run's shutdown request goes through it, the agent
// starts its countdown, the gateway answers 504 {"error":"gateway timeout"} (or 502). A JSON object with an
// "error" was the agent's own refusal at every status: "Left ON: the shutdown was not accepted (gateway
// timeout).", power on, and the machine went off two minutes later.
func TestWakeUpdateAGatewaysJSONErrorForTheShutdownIsNotARefusal(t *testing.T) {
	for _, status := range []int{504, 502} {
		e, f := lostFarm(t, "")
		e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
			if agent == "pc-a" && path == "/power" {
				return status, jsonx.M{"error": "gateway timeout"}, nil
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
		if strings.Contains(h.Text, "was not accepted") {
			t.Errorf("%d: \"refused\" for a request that arrived: %s", status, h.Text)
		}
		// the request arrived: its agent showed the countdown, and the machine went: "off", like after a timeout
		if h.State != wakerun.StOff || h.On != wakerun.OnNo || !h.ShutdownUnsure || f.on["pc-a"] {
			t.Errorf("%d: [%s] power %q, shutdown unconfirmed %v, machine on %v: %s", status, h.State, h.On, h.ShutdownUnsure, f.on["pc-a"], h.Text)
		}
		if got := strings.Join(f.requests(), ", "); got != "nas /wol, pc-a /update, pc-a /power" { // never sent again
			t.Errorf("%d: requests: %s", status, got)
		}
		log := ""
		for _, l := range h.Log {
			log += l.Text + "\n"
		}
		if want := "the shutdown request got no answer (http " + map[int]string{504: "504", 502: "502"}[status] + " in place of an answer): it may have arrived."; !strings.Contains(log, want) {
			t.Errorf("%d: the history lacks %q:\n%s", status, want, log)
		}
	}
}

// The same gateway in front of the relay, for the wake: "The wake could not be sent: gateway timeout. It was
// not woken.", and the machine started. And the relay's own refusal, which stays one: nothing was sent.
func TestWakeUpdateAGatewaysJSONErrorForTheWakeIsNotNothingSent(t *testing.T) {
	e, f := lostFarm(t, "")
	e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
		if agent == "nas" && path == "/wol" {
			return 504, jsonx.M{"error": "gateway timeout"}, nil
		}
		return 0, nil, nil
	}
	if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"}); err != nil {
		t.Fatal(err)
	}
	h := hostOf(f.finish(), "pc-a")
	if h.On == wakerun.OnUntouched && f.on["pc-a"] {
		t.Errorf("WRONG REPORT: the run says %q (%s: %s) and the machine was started by the run's wake packet and is ON", h.On, h.State, h.Text)
	}
	if h.State != wakerun.StLeftOn || h.On != wakerun.OnYes || !h.WakeUnsure || !f.on["pc-a"] ||
		!strings.Contains(h.Text, "Left ON: the run's wake request for it got no answer, so Fairy cannot tell that the run woke it.") {
		t.Errorf("[%s] power %q, wake unconfirmed %v, machine on %v: %s", h.State, h.On, h.WakeUnsure, f.on["pc-a"], h.Text)
	}
	// the relay's own "no" (Wake-on-LAN is off on it): a refusal, as ever
	e, f = lostFarm(t, "")
	f.wakes = false // the relay sends nothing
	e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
		if agent == "nas" && path == "/wol" {
			return 404, jsonx.M{"error": "wol is not enabled on this agent", "code": "wol_disabled"}, nil
		}
		return 0, nil, nil
	}
	if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"}); err != nil {
		t.Fatal(err)
	}
	h = hostOf(f.finish(), "pc-a")
	if h.State != wakerun.StGaveUp || h.On != wakerun.OnUntouched || h.WakeUnsure || h.Text != "The wake could not be sent: wol is not enabled on this agent. It was not woken." {
		t.Errorf("the relay's own refusal: [%s] power %q, wake unconfirmed %v: %s", h.State, h.On, h.WakeUnsure, h.Text)
	}
}

// A good status for the shutdown without the agent's countdown in the answer (something else answers at the
// agent's address). It counted as accepted: "shutting down at ...", and silence after that time was "switched
// off again" for a machine that never got a shutdown. It is "no answer": the run watches, sees no countdown,
// and says so, with the machine's power "not known". (It said "the request did not arrive. It is ON." after
// two answers without a countdown: a request can arrive later than that, audit 6, r4-p2-02.)
func TestWakeUpdateAYesForTheShutdownWithoutACountdownIsNotAccepted(t *testing.T) {
	e, f := lostFarm(t, "")
	e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
		if agent == "pc-a" && path == "/power" {
			req := e.b.Requests[len(e.b.Requests)-1]
			e.b.Requests[len(e.b.Requests)-1] = strings.Replace(req, "action:shutdown", "action:lost", 1) // it never reached the agent
			return 200, jsonx.M{"ok": true}, nil
		}
		return 0, nil, nil
	}
	if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"}); err != nil {
		t.Fatal(err)
	}
	h := hostOf(f.finish(), "pc-a")
	want := "The shutdown request got no answer, and its agent has shown no countdown since (7 min). The request may still arrive: the machine then switches off 2 min later " +
		"unless someone cancels that on its page. It is on now, and Fairy no longer watches it. It was updated to 17-bbbbbbb."
	if h.State != wakerun.StLeftOn || h.On != wakerun.OnUnknown || !h.ShutdownUnsure || !f.on["pc-a"] || h.Text != want {
		t.Errorf("[%s] power %q, shutdown unconfirmed %v, machine on %v: %s", h.State, h.On, h.ShutdownUnsure, f.on["pc-a"], h.Text)
	}
	log := ""
	for _, l := range h.Log {
		log += l.Text + "\n"
	}
	if strings.Contains(log, "shutting down at") ||
		!strings.Contains(log, "the shutdown request got no answer (agent unreachable: its answer to the shutdown has no countdown in it, so it is not an answer of the agent): it may have arrived.") {
		t.Errorf("the history:\n%s", log)
	}
}
