package web

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
)

// slowAgent is the test backend with an agent that takes its time over the two read requests the panel passes
// through for a viewer (the process list, the fan status): they hold until release is closed. It counts how
// many of them are at the agent at the same moment, per agent. Everything else answers at once.
type slowAgent struct {
	poller.Backend
	release chan struct{}
	mu      sync.Mutex
	now     map[string]int
	peak    map[string]int
	control atomic.Int64 // POST requests that reached the agent
}

func (b *slowAgent) hold(ctx context.Context, agent string) {
	b.mu.Lock()
	b.now[agent]++
	b.peak[agent] = max(b.peak[agent], b.now[agent])
	b.mu.Unlock()
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	b.mu.Lock()
	b.now[agent]--
	b.mu.Unlock()
}

func (b *slowAgent) FetchProcesses(ctx context.Context, a config.Agent, sid string, n int, s string) (model.ProcessList, error) {
	b.hold(ctx, a.ID)
	return b.Backend.FetchProcesses(ctx, a, sid, n, s)
}

func (b *slowAgent) AgentRequest(ctx context.Context, a config.Agent, method, path string, body any) (int, jsonx.M, error) {
	if method == http.MethodGet {
		b.hold(ctx, a.ID)
	} else {
		b.control.Add(1)
	}
	return b.Backend.AgentRequest(ctx, a, method, path, body)
}

func (b *slowAgent) atAgent(agent string) (now, peak int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.now[agent], b.peak[agent]
}

// T-20: GET /api/systems/{id}/processes and /fan are one request from the panel to the agent each, for
// anyone who may see the host (a viewer, a read token). The agent of this release admits 32 connections of
// the panel and lets 128 wait; a few hundred such requests in flight filled both, and the panel's own polls
// and an admin's "cancel shutdown" were reset. Now the panel passes at most maxAgentReadsPerUser of them
// through for one user and maxAgentReadsPerAgent to one agent at a time, and answers the rest 429 at once: a
// control request always finds room at the agent.
func TestReadRequestsPassedToAnAgentAreLimited(t *testing.T) {
	e := setup(t)
	sb := &slowAgent{Backend: e.s.Poller.Backend, release: make(chan struct{}), now: map[string]int{}, peak: map[string]int{}}
	e.s.Poller.Backend = sb
	var once sync.Once
	letGo := func() { once.Do(func() { close(sb.release) }) }
	t.Cleanup(letGo) // also when the test fails half-way: the held requests end before the server is closed
	e.s.control = newRateLimiter(1000)
	admin := e.cookie(controller(), 0)
	raw, _, _ := e.s.Tokens.Create("reader", "system", "read", "cli:test", 0)
	tok := []string{"Authorization", "Bearer " + raw}

	var wg sync.WaitGroup
	var ok, limited, other atomic.Int64
	flood := func(n int, path string, hdr ...string) {
		for range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := e.do("GET", path, nil, hdr...)
				switch {
				case r.code == 200:
					ok.Add(1)
				case r.code == 429 && r.hdr.Get("Retry-After") != "":
					limited.Add(1)
				default:
					other.Add(1)
					t.Errorf("GET %s: %d %s", path, r.code, r.raw)
				}
			}()
		}
	}
	settle := func(want int64) {
		t.Helper()
		for i := 0; i < 200 && limited.Load() < want; i++ {
			time.Sleep(10 * time.Millisecond)
		}
		if got := limited.Load(); got != want {
			t.Fatalf("%d requests were answered 429, want %d", got, want)
		}
	}

	// one user (here: one read token): maxAgentReadsPerUser at the agent, the rest refused at once
	flood(60, "/api/systems/laptop/processes", tok...)
	settle(60 - maxAgentReadsPerUser)
	if now, peak := sb.atAgent("laptop"); now != maxAgentReadsPerUser || peak != maxAgentReadsPerUser {
		t.Fatalf("one user's flood: %d requests at the agent (peak %d), want %d", now, peak, maxAgentReadsPerUser)
	}
	// more users on the same host: the agent never sees more than maxAgentReadsPerAgent
	more := 0
	for i := range 6 {
		raw, _, _ := e.s.Tokens.Create("reader"+string(rune('a'+i)), "system", "read", "cli:test", 0)
		flood(20, "/api/systems/laptop/processes", "Authorization", "Bearer "+raw)
		more += 20
	}
	settle(int64(60 + more - maxAgentReadsPerAgent))
	if now, peak := sb.atAgent("laptop"); now != maxAgentReadsPerAgent || peak != maxAgentReadsPerAgent {
		t.Fatalf("seven users' flood: %d requests at the agent (peak %d), want %d", now, peak, maxAgentReadsPerAgent)
	}
	// the fan status of another host is counted for that host: the first user is at their own limit, another is not
	if r := e.do("GET", "/api/systems/tower/fan", nil, tok...); r.code != 429 {
		t.Fatalf("a user at the limit asks for another host's fan status: %d", r.code)
	}
	flood(1, "/api/systems/tower/fan", "Cookie", admin)
	for i := 0; i < 200; i++ {
		if now, _ := sb.atAgent("tower"); now == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if now, _ := sb.atAgent("tower"); now != 1 {
		t.Fatalf("the admin's fan status request is not at the agent (%d there)", now)
	}
	// An admin is not held back by the limit of a host that others keep busy. (Found in review: two viewers at
	// their own limit used up the host's 8, and then every Processes and Fans request for that host was
	// answered 429, the admin's too, so the Fans tab lost its mode buttons.) The admin's own limit still
	// holds: one request is at tower, so maxAgentReadsPerUser-1 more are passed on and the rest refused.
	flood(maxAgentReadsPerUser+2, "/api/systems/laptop/processes", "Cookie", admin)
	settle(int64(60 + more - maxAgentReadsPerAgent + 3))
	if now, peak := sb.atAgent("laptop"); now != maxAgentReadsPerAgent+maxAgentReadsPerUser-1 || peak != now {
		t.Fatalf("with the admin's requests: %d at the agent (peak %d), want %d", now, peak, maxAgentReadsPerAgent+maxAgentReadsPerUser-1)
	}
	// and what an admin has there takes no room from the others: the host's limit counts them alone
	if r := e.do("GET", "/api/systems/laptop/processes", nil, tok...); r.code != 429 {
		t.Fatalf("a user at the limit, with the host at its limit: %d", r.code)
	}

	// while all of that is in flight, the admin's control request goes straight through
	before := sb.control.Load()
	ctl := []string{"Cookie", admin, "X-Requested-With", "fairy"}
	r := e.do("POST", "/api/systems/laptop/power", map[string]any{"action": "shutdown", "delay_s": 120, "confirm": "laptop"}, ctl...)
	if r.code != 200 || sb.control.Load() != before+1 {
		t.Fatalf("the admin's shutdown during the flood: %d %s (reached the agent: %v)", r.code, r.raw, sb.control.Load() != before)
	}
	r = e.do("POST", "/api/systems/laptop/power", map[string]any{"action": "cancel"}, ctl...)
	if r.code != 200 || sb.control.Load() != before+2 {
		t.Fatalf("the admin's cancel during the flood: %d %s (reached the agent: %v)", r.code, r.raw, sb.control.Load() != before+1)
	}
	// and the page itself is not limited
	if r := e.do("GET", "/api/systems/laptop", nil, tok...); r.code != 200 {
		t.Fatal("GET /api/systems/laptop:", r.code)
	}

	letGo()
	wg.Wait()
	if got := ok.Load(); got != int64(maxAgentReadsPerAgent+maxAgentReadsPerUser) || other.Load() != 0 {
		t.Fatalf("%d requests were answered 200, want %d", got, maxAgentReadsPerAgent+maxAgentReadsPerUser)
	}
	// the slots are free again
	if r := e.do("GET", "/api/systems/laptop/processes", nil, tok...); r.code != 200 {
		t.Fatalf("after the flood: %d %s", r.code, r.raw)
	}
	if msg := string(e.do("GET", "/api/systems/laptop/processes", nil, tok...).raw); strings.Contains(msg, "too many") {
		t.Fatal(msg)
	}
}
