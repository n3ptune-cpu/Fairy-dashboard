package powerctl

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
)

// Audit 5, round 3 (r3-p2-08): what answers at the agent's address sends a redirect with no body for a POST (a
// proxy in front of the agent that sends everything to its sign-in page or to https; Go's own http.Redirect
// writes no body for a POST). The panel does not follow it, read "status 302, empty body, no error", and knew
// only the statuses from 400 up: the request counted as ACCEPTED, and the agent never saw it. The auditor's
// proof (TestR3P2ARedirectIsTakenForTheAgentsYes) with what it must show now: "no answer of the agent".
func TestARedirectIsNotTheAgentsYes(t *testing.T) {
	var posts, followed atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed.Add(1) // where the redirect points: the panel must never come here
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sent":true}`))
	}))
	defer target.Close()
	for _, status := range []int{http.StatusFound, http.StatusMovedPermanently, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				posts.Add(1)
				http.Redirect(w, r, target.URL+"/?rd="+r.URL.Path, status) // no body for a POST
			}
		}))
		p, a := realPoller(t, front.URL)
		for _, c := range []struct {
			path string
			body map[string]any
		}{
			{"/power", map[string]any{"action": "cancel", "delay_s": 60}},
			{"/power", map[string]any{"action": "shutdown", "delay_s": 120}},
			{"/wol", map[string]any{"mac": "00:00:5e:00:53:01"}},
			{"/update", map[string]any{"requested_by": "x"}},
		} {
			data, err := agentCall(context.Background(), p, a, c.path, c.body)
			var lost *Unanswered
			var no *AgentRefusal
			if err == nil {
				t.Errorf("%d, POST %s %v: a redirect that never reached the agent counts as accepted (%v)", status, c.path, c.body["action"], data)
			} else if !errors.As(err, &lost) || errors.As(err, &no) || said(err) != "agent unreachable: http "+itoa(status)+", and not an answer of the agent" {
				t.Errorf("%d, POST %s: %T %v, want \"no answer of the agent\"", status, c.path, err, err)
			}
		}
		front.Close()
	}
	if followed.Load() != 0 || posts.Load() != 20 {
		t.Fatalf("the panel followed %d redirect(s) (%d requests sent, want 20 and none followed)", followed.Load(), posts.Load())
	}
}

func itoa(n int) string {
	return string([]byte{byte('0' + n/100), byte('0' + n/10%10), byte('0' + n%10)})
}

// A "yes" is a good status WITH the agent's own answer. The agent answers every control request it accepts
// with a JSON object that is not empty ({"action":...,"pending":...}, {"sent":true,...}, {"state":...}); a
// 200 without a body, or with "{}", comes from something else at its address.
func TestAYesIsAGoodStatusWithTheAgentsAnswer(t *testing.T) {
	p, b := testPoller(t)
	a, _ := p.AgentByID("laptop")
	for _, c := range []struct {
		name   string
		status int
		body   jsonx.M
		yes    bool
	}{
		{"the agent's answer to a power request", 200, jsonx.M{"action": "cancel", "pending": nil}, true},
		{"the agent's answer to an update request", 202, jsonx.M{"state": "downloading", "current": "41-aaaaaaa"}, true},
		{"200 without a body", 200, jsonx.M{}, false},
		{"200 without a body (nothing decoded)", 200, nil, false},
		{"204", 204, jsonx.M{}, false},
		{"a redirect with a body of its own", 302, jsonx.M{"location": "https://login.example.invalid/"}, false},
		{"a redirect that carries what looks like the agent's answer", 307, jsonx.M{"action": "cancel", "pending": nil}, false},
		{"101", 101, jsonx.M{"sent": true}, false},
	} {
		b.Reply = func(string, string) (int, jsonx.M, error) { return c.status, c.body, nil }
		data, err := agentCall(context.Background(), p, a, "/power", map[string]any{"action": "cancel"})
		var lost *Unanswered
		if c.yes != (err == nil) || (!c.yes && !errors.As(err, &lost)) || (c.yes && len(data) == 0) {
			t.Errorf("%s: data %v, error %T %v; want a yes: %v", c.name, data, err, err, c.yes)
		}
	}
}
