package powerctl

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// Second review of fix7a (R7A-08): what still counted as the agent's own word.
//
// 502 and 504 are what a gateway answers after it passed the request on: the agent may have taken it. Any JSON
// object with a non-empty "error" at such a status was the agent's refusal all the same, and a gateway that
// answers in JSON ({"error":"gateway timeout"}) was "Left ON: the shutdown was not accepted" for a machine
// that then went off. 502 and 504 are "no answer of the agent" whatever the body. The agent itself answers a
// power request with 504 in one case, and means just that: its helper was sent the request and did not answer
// in time (Fairy-agent, r4-a1-01). Its other answers to a power request, an update and a wake are 400, 404,
// 409, 429, 500, 503, and the approval statuses.
func TestAGatewayStatusIsNoAnswerWhateverTheBody(t *testing.T) {
	for _, status := range []int{502, 504} {
		for name, body := range map[string]string{
			"a gateway that answers in JSON":             `{"error":"gateway timeout"}`,
			"with a code that looks like an agent's":     `{"error":"upstream timed out","code":"upstream_timeout"}`,
			"with what the agent says when it is busy":   `{"error":"too many control requests; try again in a minute"}`,
			"with what the agent's BMC relay would say":  `{"error":"the BMC did not answer","code":"bmc_unreachable"}`,
			"with a body that is not the agent's at all": `{"message":"Bad Gateway","status":502}`,
		} {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(body))
			}))
			p, a := realPoller(t, srv.URL)
			for _, c := range []struct {
				path string
				body map[string]any
			}{
				{"/power", map[string]any{"action": "shutdown", "delay_s": 120}},
				{"/power", map[string]any{"action": "cancel", "delay_s": 60}},
				{"/update", map[string]any{"requested_by": "x"}},
				{"/wol", map[string]any{"mac": "00:00:5e:00:53:01"}},
			} {
				_, err := agentCall(context.Background(), p, a, c.path, c.body)
				var lost *Unanswered
				var no *AgentRefusal
				if err == nil || errors.As(err, &no) || !errors.As(err, &lost) || said(err) != "agent unreachable: http "+itoa(status)+", and not an answer of the agent" {
					t.Errorf("%d %s, POST %s %v: %T %q; want \"no answer of the agent\", never a refusal", status, name, c.path, c.body["action"], err, said(err))
				}
			}
			srv.Close()
		}
	}
}

// What the agent and its relays really answer stays what it was: its own "no", with its own words. The one
// route on which the agent itself answers 502 and 504 is its BMC relay (POST /bmc/power: the BMC it asked
// refused, or did not answer), with a code of its own (bmc_...): that stays the agent's answer, on that route
// and with such a code only.
func TestTheAgentsAndTheRelaysOwnAnswersStay(t *testing.T) {
	for _, c := range []struct {
		name   string
		path   string
		status int
		body   string
		agents bool
		text   string
	}{
		{"the relay: Wake-on-LAN is off", "/wol", 404, `{"error":"wol is not enabled on this agent","code":"wol_disabled"}`, true, "wol is not enabled on this agent"},
		{"the relay: its rate limit", "/wol", 429, `{"error":"too many control requests; try again in a minute"}`, true, "too many control requests; try again in a minute"},
		{"the relay: the packet could not be sent", "/wol", 500, `{"error":"could not send the magic packet"}`, true, "could not send the magic packet"},
		{"the relay: a MAC it does not take", "/wol", 400, `{"error":"mac must be six hex pairs"}`, true, "mac must be six hex pairs"},
		{"the agent: a shutdown is pending", "/power", 409, `{"error":"a shutdown is already pending; cancel it first"}`, true, "a shutdown is already pending; cancel it first"},
		{"the agent: it could not schedule", "/power", 500, `{"error":"could not schedule the shutdown: exit status 1"}`, true, "could not schedule the shutdown: exit status 1"},
		{"the agent: its helper is stopped", "/power", 503, `{"error":"helper stopped","code":"helper_stopped"}`, true, HelperStoppedWhy},
		{"the agent: no update right now", "/update", 503, `{"error":"an update is running","code":"self_update_unavailable"}`, true, "an update is running"},
		{"the BMC relay: no such target", "/bmc/power", 404, `{"error":"no bmc_relay target \"x\" on this agent","code":"unknown_target"}`, true, `no bmc_relay target "x" on this agent`},
		{"the BMC relay: the BMC did not answer", "/bmc/power", 504, `{"error":"the BMC at 10.0.0.9 did not answer within 15 s (is the relay cabled to it?)","code":"bmc_unreachable"}`, true,
			"the BMC at 10.0.0.9 did not answer within 15 s (is the relay cabled to it?)"},
		{"the BMC relay: the BMC refused the login", "/bmc/power", 502, `{"error":"the BMC at https://10.0.0.9 refused the login (401)","code":"bmc_refused"}`, true,
			"the BMC at https://10.0.0.9 refused the login (401)"},
		// a gateway in front of the relay is a gateway there too
		{"a gateway's 504 in front of the BMC relay", "/bmc/power", 504, `{"error":"gateway timeout"}`, false, "agent unreachable: http 504, and not an answer of the agent"},
		{"a gateway's 502 with another code", "/bmc/power", 502, `{"error":"bad gateway","code":"bad_gateway"}`, false, "agent unreachable: http 502, and not an answer of the agent"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		}))
		p, a := realPoller(t, srv.URL)
		_, err := agentCall(context.Background(), p, a, c.path, map[string]any{})
		srv.Close()
		var lost *Unanswered
		var no *AgentRefusal
		if err == nil || errors.As(err, &no) != c.agents || errors.As(err, &lost) == c.agents || said(err) != c.text {
			t.Errorf("%s: %T %q; want the agent's own no: %v, %q", c.name, err, said(err), c.agents, c.text)
		}
	}
}

// For a shutdown, the agent's "yes" is a good status with its "pending" block: the countdown it started. The
// agent always sends it ({"action":"shutdown","pending":{...}}). Any other non-empty JSON object with a good
// status counted as "accepted" too: something that is not the agent, at its address, and no countdown anywhere.
// Such an answer is "no answer of the agent" for a shutdown: the run then watches the machine and never says
// "off" unless it saw the countdown.
func TestAShutdownsYesNeedsTheAgentsCountdown(t *testing.T) {
	p, b := testPoller(t)
	var laptop model.System
	for _, s := range p.Snapshot() {
		if s.ID == "laptop" {
			laptop = s
		}
	}
	good := jsonx.M{"action": "shutdown", "at": 1.9e9, "delay_s": 120.0, "requested_by": "Ann"}
	ask := func(action string) Request {
		r := Request{Action: action, Confirm: &laptop.Name}
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, c := range []struct {
		name string
		body jsonx.M
		yes  bool
	}{
		{"the agent's answer", jsonx.M{"action": "shutdown", "pending": good}, true},
		{"a good status with another body", jsonx.M{"ok": true}, false},
		{"the action without a countdown", jsonx.M{"action": "shutdown"}, false},
		{"\"pending\": null", jsonx.M{"action": "shutdown", "pending": nil}, false},
		{"\"pending\": true", jsonx.M{"action": "shutdown", "pending": true}, false},
		{"a countdown without a time", jsonx.M{"action": "shutdown", "pending": jsonx.M{"action": "shutdown", "delay_s": 120.0}}, false},
		{"a countdown of another action", jsonx.M{"action": "shutdown", "pending": jsonx.M{"action": "reboot", "at": 1.9e9, "delay_s": 120.0}}, false},
		{"a sign-in page's JSON", jsonx.M{"login": "required", "url": "https://login.example.invalid/"}, false},
	} {
		b.Reply = func(_, path string) (int, jsonx.M, error) {
			if path == "/power" {
				return 200, c.body, nil
			}
			return 0, nil, nil
		}
		res, err := Execute(context.Background(), p, laptop, ask("shutdown"), "Ann")
		var lost *Unanswered
		var no *AgentRefusal
		switch {
		case c.yes && (err != nil || res.Pending == nil || res.Pending.Action != "shutdown" || !strings.HasPrefix(res.Message, "shutdown in ")):
			t.Errorf("%s: %+v %v; want accepted, with the countdown", c.name, res, err)
		case !c.yes && (err == nil || !errors.As(err, &lost) || errors.As(err, &no) ||
			said(err) != "agent unreachable: its answer to the shutdown has no countdown in it, so it is not an answer of the agent"):
			t.Errorf("%s: %+v, %T %q; want \"no answer of the agent\"", c.name, res, err, said(err))
		}
		p.NotePending("laptop", nil)
	}
	// only the shutdown: a cancel is answered without a countdown, as ever
	b.Reply = func(_, path string) (int, jsonx.M, error) {
		if path == "/power" {
			return 200, jsonx.M{"action": "cancel", "pending": nil}, nil
		}
		return 0, nil, nil
	}
	laptop.PowerPending = &model.PowerPending{Action: "shutdown"}
	if res, err := Execute(context.Background(), p, laptop, ask("cancel"), "Ann"); err != nil || res.Message != "cancelled" {
		t.Errorf("a cancel: %+v %v", res, err)
	}
}
