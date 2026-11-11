package agentcontract_test

// A busy agent stays online. Fairy-agent v0.6.0 answers a /processes (or /fan/status) request over its cap with
// 503, code "busy" and Retry-After: 1 instead of holding one more of the panel's connections; /metrics has no
// such cap. The panel reads a host's state from /metrics alone, so a busy answer must cost one Processes
// request and nothing else: the host stays online, the next poll is answered, and when the agent has room
// again the Processes tab works again without anybody doing anything.
//
// The real agent handler with a process listing that waits until the test lets it go; the panel's real poller
// and web API in front of it.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/agenttest"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/collect"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/web"
)

// slowProcesses is agenttest's fake PC whose process listing waits for release.
type slowProcesses struct {
	*agenttest.Collector
	release chan struct{}
}

func (s slowProcesses) Processes(ctx context.Context, o collect.ProcOptions) ([]collect.Process, int, error) {
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
	return s.Collector.Processes(ctx, o)
}

func TestBusyAgentStaysOnline(t *testing.T) {
	acfg, err := agent.Parse([]byte("listen: 127.0.0.1:9101\n"), filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	slow := slowProcesses{agenttest.NewCollector(), make(chan struct{})}
	a := agent.New(acfg, agent.Tokens{Read: readTok}, agent.Deps{Collector: slow, Version: "busy"})
	agentSrv := httptest.NewServer(a.Handler())
	t.Cleanup(agentSrv.Close)

	cfg := panelConfig(t, agentSrv.URL)
	cfg.RequestTimeout = 30 // the held requests must outlast the test, not run into the panel's timeout
	p, err := poller.New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	p.Clock = func() time.Time { return now }
	state := func() model.System {
		t.Helper()
		for _, x := range p.Snapshot() {
			if x.ID == "srv-tower" {
				return x
			}
		}
		t.Fatal("srv-tower is not in the snapshot")
		return model.System{}
	}
	p.PollOnce(context.Background())
	if s := state(); s.Status != "online" {
		t.Fatalf("before: %+v", s)
	}
	srv := web.New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, p, nil, "test")
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	tok, _, err := srv.Tokens.Create("busy", "system", "read", "test", 0)
	if err != nil {
		t.Fatal(err)
	}
	ui := api{t, hs, tok}
	release := sync.OnceFunc(func() { close(slow.release) })
	t.Cleanup(release) // before the servers close: a failed test must not leave them waiting for held requests

	// Processes requests, one more at a time, until the agent refuses one: every one before it now holds one
	// of the agent's places and keeps it until the listing is let go. They are sent to the agent itself, with
	// its read token, as another reader of the same agent would: the panel alone no longer fills an agent, it
	// passes at most 8 such requests on at a time (T-20, internal/web/agentreads.go).
	results := make(chan int, 64)
	var wg sync.WaitGroup
	sent, refused := 0, 0
	for sent < cap(results) && refused == 0 {
		sent++
		wg.Go(func() {
			req, _ := http.NewRequest("GET", agentSrv.URL+"/processes?limit=25&sort=cpu", nil)
			req.Header.Set("Authorization", "Bearer "+readTok)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results <- 0
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			results <- resp.StatusCode
		})
		select {
		case c := <-results:
			if c != 503 {
				t.Fatalf("Processes request %d came back with %d while the listing waits", sent, c)
			}
			refused++
		case <-time.After(150 * time.Millisecond): // no answer: it waits at the agent
		}
	}
	if refused == 0 {
		t.Fatalf("the agent took %d Processes requests at once and refused none", sent)
	}

	// the agent's own answer over the cap, asked the way the panel asks (this pins the agent's side)
	req, _ := http.NewRequest("GET", agentSrv.URL+"/processes?limit=25&sort=cpu", nil)
	req.Header.Set("Authorization", "Bearer "+readTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") != "1" || !strings.Contains(string(raw), `"code":"`+agent.BusyCode+`"`) {
		t.Fatalf("a full agent must answer 503, Retry-After: 1 and code %q: got %d, Retry-After %q, %s",
			agent.BusyCode, resp.StatusCode, resp.Header.Get("Retry-After"), raw)
	}

	// the panel's answer to one more Processes tab: the agent answered and refused (424), said in words
	st, out := ui.do("GET", "/api/systems/srv-tower/processes?limit=25&sort=cpu", nil)
	if msg, _ := out["detail"].(string); st != 424 || !strings.Contains(msg, "agent error") || !strings.Contains(msg, "503") {
		t.Fatalf("one more Processes request while the agent is full: %d %v", st, out)
	}

	// polls while the agent is full: answered, the host is online, nothing says otherwise
	for i := range 3 {
		time.Sleep(260 * time.Millisecond) // the agent computes /metrics at most every 250 ms
		now = now.Add(2 * time.Second)
		p.PollOnce(context.Background())
		if s := state(); s.Status != "online" || s.Error != nil || s.LastSeen == nil || !s.LastSeen.Equal(now) || s.CPUPercent == nil {
			t.Fatalf("poll %d while the agent is full: status %q, error %v, last seen %v", i, s.Status, model.Deref(s.Error), s.LastSeen)
		}
	}
	for _, src := range p.Summary().Sources {
		if src.Name == "agent:srv-tower" && (!src.OK || src.LastError != nil) {
			t.Fatalf("the agent's source while it is full: %+v", src)
		}
	}
	if st, sys := ui.do("GET", "/api/systems/srv-tower", nil); st != 200 || sys["status"] != "online" {
		t.Fatal("the API's view of the host while its agent is full:", st, sys["status"])
	}

	// room again: the requests that waited are answered, and a new one works
	release()
	wg.Wait()
	close(results)
	answered := 0
	for c := range results {
		switch c {
		case 200:
			answered++
		case 503:
			refused++
		default:
			t.Fatal("a Processes request that waited ended with", c)
		}
	}
	if answered == 0 || answered+refused != sent {
		t.Fatalf("of %d Processes requests %d were answered and %d refused as busy", sent, answered, refused)
	}
	if st, pl := ui.do("GET", "/api/systems/srv-tower/processes?limit=3&sort=cpu", nil); st != 200 || pl["total_processes"] != 212.0 {
		t.Fatal("Processes after the agent has room again:", st, pl)
	}
	if s := state(); s.Status != "online" {
		t.Fatalf("after: %+v", s)
	}
}
