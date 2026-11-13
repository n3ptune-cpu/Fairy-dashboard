package powerctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// The panel's rule for an answer with an error status (only the agent's own JSON error is a refusal; 502 and
// 504 are "no answer" whatever the body) and the approval protocol meet in agentCall: a machine that checks a
// passkey approval itself answers a shutdown, a restart, a sleep and an update with a status and a code of its
// own (403 approval_required, 403 approval_refused, 503 approval_unavailable, 404 approval_disabled). Each of
// them is the agent's own "no", with its code and its status, and never "no answer of the agent": the page
// asks for the passkey on approval_required, says in words what the other three mean, and a run reads the
// code. With the real HTTP backend, and with the bodies the agent writes (its "error" text and its "code";
// approval_refused can carry when to ask again).
func TestTheAgentsApprovalAnswersAreItsOwnNo(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   map[string]any
		page   int // the status of the error the page gets
	}{
		{"an approval is required, and the request carried none", 403,
			map[string]any{"error": "this needs a passkey approval (Pocket ID) and the request carried none", "code": api.CodeApprovalRequired}, 400},
		{"the approval was not accepted", 403,
			map[string]any{"error": "approval refused (expired): the approval is older than the machine accepts", "code": api.CodeApprovalRefused}, 400},
		{"the approval was not accepted, and the machine says when to ask again", 403,
			map[string]any{"error": "approval refused (too many): try again in 12 s", "code": api.CodeApprovalRefused, "retry_after_s": 12}, 400},
		{"the machine's trust file cannot be used", 503,
			map[string]any{"error": "not run: the trust file cannot be read", "code": api.CodeApprovalUnavailable}, apierr.Upstream},
		{"nothing on the machine checks approvals", 404,
			map[string]any{"error": "approval is disabled on this agent", "code": api.CodeApprovalDisabled}, 409},
	} {
		raw, _ := json.Marshal(c.body)
		var status int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(raw)
		}))
		p, a := realPoller(t, srv.URL)
		for path, body := range map[string]map[string]any{
			"/power":  {"action": "shutdown", "delay_s": 60, "requested_by": "Ann"},
			"/update": {"requested_by": "Ann"},
		} {
			// the agent's own status: its refusal, with the code and the status
			status = c.status
			_, err := agentCall(context.Background(), p, a, path, body)
			var no *AgentRefusal
			var lost *Unanswered
			var ae *apierr.Error
			if !errors.As(err, &no) || errors.As(err, &lost) {
				t.Errorf("%s, %s: %T %v; want the agent's own no (an AgentRefusal), never \"no answer\"", c.name, path, err, err)
				continue
			}
			if no.Code != c.body["code"] || no.Status != c.status {
				t.Errorf("%s, %s: code %q status %d, want %q and %d", c.name, path, no.Code, no.Status, c.body["code"], c.status)
			}
			if !errors.As(err, &ae) || ae.Status != c.page || said(err) != c.body["error"] {
				t.Errorf("%s, %s: the page gets %v, want %d with the machine's own words", c.name, path, err, c.page)
			}
			// the same body behind the two statuses a gateway gives after it passed the request on: not the
			// agent's answer (the agent never answers these requests with 502 or 504), so "no answer"
			// (the words are fix8's since the rebase onto it: "no answer (http 504): it may have arrived"; they
			// were "agent unreachable: http 504, and not an answer of the agent")
			for _, gw := range []int{502, 504} {
				status = gw
				_, err := agentCall(context.Background(), p, a, path, body)
				if !errors.As(err, &lost) || errors.As(err, &no) || said(err) != fmt.Sprintf("no answer (http %d): it may have arrived", gw) {
					t.Errorf("%s, %s, with the status %d: %T %v; want \"no answer of the agent\"", c.name, path, gw, err, err)
				}
			}
		}
		srv.Close()
	}
}

// The same through the function the Power menu and the Update button use: the machine's "approval_required"
// to a shutdown is not hidden by the rule that a shutdown's "yes" needs the agent's countdown, with and
// without an approval in the request.
func TestAShutdownsApprovalRefusalComesThroughExecute(t *testing.T) {
	p, b := testPoller(t)
	var laptop model.System
	for _, s := range p.Snapshot() {
		if s.ID == "laptop" {
			laptop = s
		}
	}
	req := Request{Action: "shutdown", Confirm: &laptop.Name}
	if err := req.Validate(); err != nil {
		t.Fatal(err)
	}
	a, _ := p.AgentByID("laptop")
	for _, code := range []string{api.CodeApprovalRequired, api.CodeApprovalRefused} {
		b.Reply = func(string, string) (int, jsonx.M, error) {
			return 403, jsonx.M{"error": "the machine's words", "code": code}, nil
		}
		refused := func(what string, err error) {
			var no *AgentRefusal
			var lost *Unanswered
			if !errors.As(err, &no) || errors.As(err, &lost) || no.Code != code || no.Status != 403 || said(err) != "the machine's words" {
				t.Errorf("%s, %s: %T %v; want the agent's own no with its code", code, what, err, err)
			}
		}
		for name, ap := range map[string]*api.Approval{"a shutdown without an approval": nil, "a shutdown with an approval": {}} {
			_, err := ExecuteApproved(context.Background(), p, laptop, req, "Ann", ap)
			refused(name, err)
		}
		_, err := UpdateAgentApproved(context.Background(), p, a, "Ann", nil)
		refused("an update", err)
	}
}
