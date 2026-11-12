package powerctl

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// deadURL is an address nothing listens at.
func deadURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return "http://" + ln.Addr().String()
}

// said is the text of the error the page gets.
func said(err error) string {
	var ae *apierr.Error
	if errors.As(err, &ae) {
		s, _ := ae.Detail.(string)
		return s
	}
	return ""
}

// realPoller is a poller with the real HTTP backend whose agent "tower" (it has a control token) is at url.
func realPoller(t *testing.T, url string) (*poller.Poller, config.Agent) {
	t.Helper()
	cfg := testutil.Config(t, []string{deadURL(t)}, deadURL(t), url)
	cfg.RequestTimeout = 2
	p, err := poller.New(cfg, config.Secrets{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := p.AgentByID("tower")
	if !ok {
		t.Fatal("no agent")
	}
	return p, a
}

// Audit 5, round 3 (r3-p2-03), with the real HTTP backend and a real reverse proxy (Go's
// httputil.ReverseProxy with a time limit for the upstream's answer, as nginx's proxy_read_timeout or
// HAProxy's "timeout server"): the request for the shutdown REACHES the agent, which starts its countdown; the
// agent's answer is slow; the proxy answers 504 with its own page. That was an AgentRefusal ("nothing
// happened"); it is "no answer of the agent". The auditor's proof (TestR3P2AProxyErrorStatusAfterTheAgentTook-
// TheRequest), one level up: what powerctl makes of it.
func TestAProxysErrorStatusIsNotTheAgentsNo(t *testing.T) {
	var countdowns atomic.Int64
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/power" {
			countdowns.Add(1)                  // the agent schedules the shutdown ...
			time.Sleep(900 * time.Millisecond) // ... and its answer is slow (a helper that takes its time)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"action":"shutdown","pending":{"action":"shutdown","at":1900000000,"delay_s":120}}`))
	}))
	defer agent.Close()
	up, _ := url.Parse(agent.URL)
	rp := httputil.NewSingleHostReverseProxy(up)
	rp.Transport = &http.Transport{ResponseHeaderTimeout: 200 * time.Millisecond}
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) { // nginx's page for proxy_read_timeout
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusGatewayTimeout)
		_, _ = w.Write([]byte("<html><body><h1>504 Gateway Time-out</h1>nginx</body></html>"))
	}
	proxy := httptest.NewServer(rp)
	defer proxy.Close()
	p, a := realPoller(t, proxy.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := agentCall(ctx, p, a, "/power", map[string]any{"action": "shutdown", "delay_s": 120})
	var lost *Unanswered
	var no *AgentRefusal
	if countdowns.Load() != 1 || !errors.As(err, &lost) || errors.As(err, &no) ||
		said(err) != "agent unreachable: http 504, and not an answer of the agent" {
		t.Fatalf("the agent started %d countdown(s), and powerctl says: %T %v (want an Unanswered, never an AgentRefusal)", countdowns.Load(), err, err)
	}
}

// What answers at the agent's address, by status and body, with the real HTTP backend: the agent's own error
// body is its "no"; every other answer with an error status is "no answer of the agent".
func TestAnErrorStatusIsARefusalOnlyWithTheAgentsOwnBody(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		ctype  string
		body   string
		agents bool   // the agent's own "no"
		text   string // of the error the page gets
	}{
		{"the agent: no such target", 404, "application/json", `{"error":"that is disabled on this agent","code":"power_disabled"}`, true, "that is disabled on this agent"},
		{"the agent: forbidden", 403, "application/json", `{"error":"approval required","code":"approval_required"}`, true, "approval required"},
		{"the agent: already pending", 409, "application/json", `{"error":"a shutdown is already pending; cancel it first"}`, true, "a shutdown is already pending; cancel it first"},
		{"the agent: locked", 423, "application/json", `{"error":"locked"}`, true, "locked"},
		{"the agent: its rate limit", 429, "application/json", `{"error":"too many control requests"}`, true, "too many control requests"},
		{"the agent: it failed", 500, "application/json", `{"error":"could not schedule the shutdown: exit status 1"}`, true, "could not schedule the shutdown: exit status 1"},
		{"the agent: helper stopped", 503, "application/json", `{"error":"helper stopped","code":"helper_stopped"}`, true, HelperStoppedWhy},
		{"a proxy's 502 page", 502, "text/html", "<html><h1>502 Bad Gateway</h1></html>", false, "agent unreachable: http 502, and not an answer of the agent"},
		{"a proxy's 503 page", 503, "text/html", "<html><h1>503 Service Unavailable</h1></html>", false, "agent unreachable: http 503, and not an answer of the agent"},
		{"a proxy's 504 page", 504, "text/html", "<html><h1>504 Gateway Time-out</h1></html>", false, "agent unreachable: http 504, and not an answer of the agent"},
		{"a proxy's 504 without a body", 504, "", "", false, "agent unreachable: http 504, and not an answer of the agent"},
		{"a proxy's 502 as JSON of its own", 502, "application/json", `{"message":"Bad Gateway","status":502}`, false, "agent unreachable: http 502, and not an answer of the agent"},
		{"another server's 404 page", 404, "text/html", "<html>Not Found</html>", false, "agent unreachable: http 404, and not an answer of the agent"},
		{"a sign-in proxy's 401", 401, "text/plain", "Unauthorized", false, "agent unreachable: http 401, and not an answer of the agent"},
		{"a 429 that is not the agent's", 429, "text/plain", "slow down", false, "agent unreachable: http 429, and not an answer of the agent"},
		{"an error with \"error\" that is not a text", 500, "application/json", `{"error":{"code":500}}`, false, "agent unreachable: http 500, and not an answer of the agent"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c.ctype != "" {
				w.Header().Set("Content-Type", c.ctype)
			}
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		}))
		p, a := realPoller(t, srv.URL)
		_, err := agentCall(context.Background(), p, a, "/power", map[string]any{"action": "shutdown", "delay_s": 120})
		srv.Close()
		var lost *Unanswered
		var no *AgentRefusal
		if err == nil || errors.As(err, &no) != c.agents || errors.As(err, &lost) == c.agents || said(err) != c.text {
			t.Errorf("%s: %T %q; want the agent's own no: %v, %q", c.name, err, err, c.agents, c.text)
		}
		if c.agents && no.Status != c.status {
			t.Errorf("%s: the refusal's status is %d", c.name, no.Status)
		}
	}
	// the agent's own 401 keeps its own text (and is no AgentRefusal: nothing reads a code from it)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()
	p, a := realPoller(t, srv.URL)
	_, err := agentCall(context.Background(), p, a, "/update", map[string]any{})
	var lost *Unanswered
	if err == nil || errors.As(err, &lost) || said(err) != "agent refused the control token (check control_token_env)" {
		t.Errorf("the agent's own 401: %T %v", err, err)
	}
}
