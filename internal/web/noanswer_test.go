package web

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/updates"
)

// The panel's own requests that change something on a machine, outside the power path: a fan mode, a fan duty,
// the single "Update", "Update all", the helper's stop. Each of them read every error status as the agent's
// own "no" (424 with its words, or "agent error"), also 502 and 504, which a gateway between panel and agent
// gives AFTER it passed the request on, and which the agent itself now gives for a request it handed to its
// helper and got no answer to (Fairy-agent, r4-a1-01). And a redirect was "ok": "started". Now they are what
// they are on the power path (powerctl.NoAnswer): no answer of the agent, the request may have arrived.

// writes are the requests, each in a panel of its own. call sends one and gives the answer and its audit action.
var writes = []struct {
	name   string
	prep   func(e *env)
	call   func(e *env, hdr []string) resp
	action string
}{
	{"fan mode", nil, func(e *env, hdr []string) resp {
		return e.do("POST", "/api/systems/tower/fan/mode", map[string]any{"mode": "full"}, hdr...)
	}, "fan.mode"},
	{"fan duty", nil, func(e *env, hdr []string) resp {
		return e.do("POST", "/api/systems/tower/fan/duty", map[string]any{"zone": 0, "percent": 60}, hdr...)
	}, "fan.duty"},
	{"Update", selfUpdating, func(e *env, hdr []string) resp {
		return e.do("POST", "/api/admin/updates/laptop", map[string]any{}, hdr...)
	}, "update.agent"},
	{"Update all", selfUpdating, func(e *env, hdr []string) resp {
		r := e.do("POST", "/api/admin/updates", map[string]any{}, hdr...)
		if r.code != 200 {
			return r
		}
		for _, x := range r.body["results"].([]any) { // the row of the one agent that was asked: its words, as a status and a detail
			row := x.(map[string]any)
			if row["id"] != "laptop" {
				continue
			}
			if row["ok"] == true {
				return resp{code: 200, body: row}
			}
			var code int
			var detail string
			if _, err := fmt.Sscanf(fmt.Sprint(row["error"]), "error %d:", &code); err == nil {
				_, detail, _ = strings.Cut(fmt.Sprint(row["error"]), ": ")
			}
			return resp{code: code, body: map[string]any{"detail": detail}}
		}
		return resp{}
	}, "update.agent"},
	{"helper stop", func(e *env) { e.helperPoll("laptop", "running", helperAll...) }, func(e *env, hdr []string) resp {
		return e.do("POST", "/api/systems/laptop/helper/stop", map[string]any{"confirm": "laptop"}, hdr...)
	}, "helper_stop"},
}

// selfUpdating makes the laptop's agent one that can be updated: an older version than the latest release.
func selfUpdating(e *env) {
	e.s.Updates = updates.Static{Tag: "build-42", Version: "42-bbbbbbb", Build: model.P(42), Verified: true}
	e.b.Mu.Lock()
	e.b.Metrics["laptop"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["agent_version"] = "41-aaaaaaa"
		m["features"].(jsonx.M)["self_update"] = true
		m["self_update"] = jsonx.M{"state": "idle", "since": 1.0}
		return m, nil
	}
	e.b.Mu.Unlock()
	e.s.Poller.PollOnce(context.Background())
}

// write sends one of the requests to a new panel whose agents answer every POST with the given answer.
func write(t *testing.T, i int, st int, body jsonx.M, err error) (resp, string) {
	t.Helper()
	w := writes[i]
	e := setup(t)
	e.s.control = newRateLimiter(100)
	if w.prep != nil {
		w.prep(e)
	}
	e.b.Reply = func(_, path string) (int, jsonx.M, error) {
		if path == "/fan/status" || path == "/metrics" {
			return 0, nil, nil
		}
		return st, body, err
	}
	r := w.call(e, []string{"Cookie", e.cookie(controller(), 0), "X-Requested-With", "fairy"})
	outcome := ""
	for _, a := range e.auditEntries("", w.action) {
		outcome = fmt.Sprint(a["outcome"])
	}
	return r, outcome
}

const (
	helperSilent = "the helper was sent the fan mode and did not answer in time: the fans may have been changed all the same: look at the fan status"
	bmcSaidNo    = "BMC refused the fan mode: ipmitool: timeout"
)

func TestAWriteWithoutAnAnswerOfTheAgentIsNotItsNo(t *testing.T) {
	for _, c := range []struct {
		name string
		st   int
		body jsonx.M
		want string
	}{
		{"504 of a gateway, an HTML page", 504, nil, "agent unreachable: http 504, and not an answer of the agent"},
		{"502 of a gateway, an HTML page", 502, nil, "agent unreachable: http 502, and not an answer of the agent"},
		{"504 of a gateway that answers in JSON", 504, jsonx.M{"error": "upstream request timeout"},
			"agent unreachable: http 504, and not an answer of the agent (the answer said: upstream request timeout)"},
		{"the agent's own 504: its helper got the request and did not answer", 504, jsonx.M{"error": helperSilent},
			"agent unreachable: http 504, and not an answer of the agent (the answer said: " + helperSilent + ")"},
		{"502 of a gateway that answers in JSON", 502, jsonx.M{"error": "bad gateway"}, "agent unreachable: http 502, and not an answer of the agent (the answer said: bad gateway)"},
		{"a redirect", 302, nil, "agent unreachable: http 302, and not an answer of the agent"},
		{"an error page that is not the agent's", 500, nil, "agent unreachable: http 500, and not an answer of the agent"},
		{"a login page of a proxy, 401", 401, nil, "agent unreachable: http 401, and not an answer of the agent"},
	} {
		for i, w := range writes {
			r, outcome := write(t, i, c.st, c.body, nil)
			if got := fmt.Sprint(r.body["detail"]); r.code != 502 || got != c.want {
				t.Errorf("%s, %s: %d %q, want 502 %q", w.name, c.name, r.code, got, c.want)
			}
			if want := "error 502: " + c.want; outcome != want {
				t.Errorf("%s, %s: the audit entry says %q, want %q", w.name, c.name, outcome, want)
			}
		}
	}
}

// The agent's own 502 on a fan route, with the reason of its BMC, stays what it was: its answer, in its words
// (424, never the panel's 502: a proxy in front of the panel puts its own page in place of a 502's body). On
// the other routes the agent has no such answer, and the same body is a gateway's. And its 504 is no answer on
// every route.
func TestTheAgentsOwn502OfAFanRouteStaysItsAnswer(t *testing.T) {
	for i, w := range writes {
		r, outcome := write(t, i, 502, jsonx.M{"error": bmcSaidNo}, nil)
		code, want := 502, "agent unreachable: http 502, and not an answer of the agent (the answer said: "+bmcSaidNo+")"
		if strings.HasPrefix(w.name, "fan ") {
			code, want = 424, bmcSaidNo
		}
		if got := fmt.Sprint(r.body["detail"]); r.code != code || got != want || outcome != fmt.Sprintf("error %d: %s", code, want) {
			t.Errorf("%s: %d %q, the audit entry %q; want %d %q", w.name, r.code, got, outcome, code, want)
		}
		r, _ = write(t, i, 504, jsonx.M{"error": bmcSaidNo}, nil)
		if got := fmt.Sprint(r.body["detail"]); r.code != 502 || !strings.HasPrefix(got, "agent unreachable: http 504, and not an answer of the agent") {
			t.Errorf("%s, 504: %d %q", w.name, r.code, got)
		}
	}
}

// A timeout and a broken connection were "agent unreachable" already (502), and stay so, in the same words.
func TestAWriteThatTimesOutIsAgentUnreachable(t *testing.T) {
	for i, w := range writes {
		r, outcome := write(t, i, 0, nil, context.DeadlineExceeded)
		if got := fmt.Sprint(r.body["detail"]); r.code != 502 || !strings.HasPrefix(got, "agent unreachable: ") || strings.Contains(got, "http") {
			t.Errorf("%s: %d %q", w.name, r.code, got)
		}
		if !strings.HasPrefix(outcome, "error 502: agent unreachable: ") {
			t.Errorf("%s: the audit entry says %q", w.name, outcome)
		}
	}
}

// The agent's own "no" stays its "no", in its own words and with the status the page had for it.
func TestAWriteTheAgentRefusedStaysARefusalInItsWords(t *testing.T) {
	for _, c := range []struct {
		st   int
		body jsonx.M
		want int
	}{
		{409, jsonx.M{"error": "fan mode is standard; set mode 'full' first (duty is ignored otherwise)"}, 409},
		{400, jsonx.M{"error": "percent must be an integer between 30 and 100"}, 400},
		{422, jsonx.M{"error": "BMC refused the fan mode heavyio: this board's BMC does not support it"}, 422},
		{429, jsonx.M{"error": "too many requests"}, 429},
		{500, jsonx.M{"error": "could not schedule it: access denied"}, 424},
		{503, jsonx.M{"error": "self-update is not available", "code": "self_update_unavailable"}, 424},
		{401, jsonx.M{"error": "unauthorized"}, 424},
	} {
		for i, w := range writes {
			r, outcome := write(t, i, c.st, c.body, nil)
			want := fmt.Sprint(c.body["error"])
			if c.st == 401 {
				want = "agent rejected the control token (check control_token_env)"
			}
			if got := fmt.Sprint(r.body["detail"]); r.code != c.want || got != want {
				t.Errorf("%s, %d: %d %q, want %d %q", w.name, c.st, r.code, got, c.want, want)
			}
			if want := fmt.Sprintf("error %d: %s", c.want, want); outcome != want {
				t.Errorf("%s, %d: the audit entry says %q, want %q", w.name, c.st, outcome, want)
			}
		}
	}
}

// And the agent's "yes" is as it was: a fan change it answered, an update it took.
func TestAWriteTheAgentTookIsOK(t *testing.T) {
	for i, w := range writes {
		r, outcome := write(t, i, 200, jsonx.M{"mode": "full", "state": "stopped"}, nil)
		if r.code != 200 || outcome != "ok" {
			t.Errorf("%s: %d %s, the audit entry %q", w.name, r.code, r.raw, outcome)
		}
	}
}
