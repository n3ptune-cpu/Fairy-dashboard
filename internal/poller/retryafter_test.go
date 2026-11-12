package poller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// An agent's 429 says when to ask again in its Retry-After header (its request budgets: 60; too_many_challenges:
// 1 to 90, the same number as "retry_after_s" in the body). The panel hands the header's number on with the
// answer; a header that is no such number is not used, and no other answer gets the member.
func TestRetryAfterOfAnAgents429IsHandedOn(t *testing.T) {
	var status int
	var header, body string
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if header != "" {
			w.Header().Set("Retry-After", header)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer agent.Close()
	cfg := testutil.Config(t, []string{deadURL(t)}, agent.URL, deadURL(t))
	p, err := New(cfg, config.Secrets{SessionSecret: "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		status       int
		header, body string
		want         any
	}{
		{429, "60", `{"error":"too many approval challenge requests; try again in a minute"}`, 60.0},
		{429, "37", `{"code":"too_many_challenges","error":"... Try again in 37 s","retry_after_s":37}`, 37.0},
		{429, "", `{"code":"too_many_challenges","retry_after_s":12}`, 12.0}, // no header: the body's own member stays
		{429, "soon", `{"error":"x"}`, nil},
		{429, "0", `{"error":"x"}`, nil},
		{429, "99999", `{"error":"x"}`, nil},
		{409, "60", `{"error":"x"}`, nil},
		{200, "60", `{"ok":true}`, nil},
	} {
		status, header, body = c.status, c.header, c.body
		st, data, err := p.AgentRequest(context.Background(), cfg.Agents[0], "POST", "/approval/challenge", map[string]any{"verb": "apply_update"})
		if err != nil || st != c.status || data["retry_after_s"] != c.want {
			t.Errorf("%d with Retry-After %q and %s: status %d, retry_after_s %v (want %v), %v", c.status, c.header, c.body, st, data["retry_after_s"], c.want, err)
		}
	}
}
