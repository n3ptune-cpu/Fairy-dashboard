package poller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// A request to an agent waits request_timeout + 8 s for its answer. One request can ask for more
// (AgentRequestWithin, WithAgentWait): the switching of a plug's socket, which the agent works on for up to
// 18 s. Every other request waits as it always did, and a request never waits less than the usual time.
func TestAgentRequestWithItsOwnWait(t *testing.T) {
	var delay atomic.Int64
	var got atomic.Int32
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Add(1)
		select {
		case <-time.After(time.Duration(delay.Load())):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"confirmed": true}`))
	}))
	defer slow.Close()
	cfg := testutil.Config(t, []string{deadURL(t)}, slow.URL, deadURL(t))
	// the usual wait is request_timeout + 8 s: -7.7 makes it 0.3 s for this test (no file can say that)
	cfg.RequestTimeout = -7.7
	p, err := New(cfg, config.Secrets{}, nil) // the real HTTP backend
	if err != nil {
		t.Fatal(err)
	}
	a := mustAgent(t, p, "laptop")
	ctx := context.Background()

	// an answer that takes longer than the usual wait: lost for a usual request ...
	delay.Store(int64(900 * time.Millisecond))
	start := time.Now()
	if status, _, err := p.AgentRequest(ctx, a, "POST", "/plug/switch", map[string]any{}); err == nil {
		t.Fatalf("a request with the usual wait got the late answer (status %d)", status)
	}
	if took := time.Since(start); took < 250*time.Millisecond || took > 800*time.Millisecond {
		t.Errorf("the usual wait was %v, want about 0.3 s", took)
	}
	// ... and there for the one that asked for a longer wait
	status, data, err := p.AgentRequestWithin(ctx, a, "POST", "/plug/switch", map[string]any{}, 5*time.Second)
	if err != nil || status != 200 || data["confirmed"] != true {
		t.Fatalf("with its own wait: %d %v %v", status, data, err)
	}
	// its own wait ends too
	delay.Store(int64(5 * time.Second))
	start = time.Now()
	if _, _, err := p.AgentRequestWithin(ctx, a, "POST", "/plug/switch", map[string]any{}, 600*time.Millisecond); err == nil {
		t.Fatal("a request got an answer after its own wait was over")
	}
	if took := time.Since(start); took < 500*time.Millisecond || took > 2*time.Second {
		t.Errorf("its own wait was %v, want about 0.6 s", took)
	}
	// a wait that is shorter than the usual one does not shorten it
	delay.Store(int64(150 * time.Millisecond))
	if status, _, err := p.AgentRequestWithin(ctx, a, "POST", "/plug/switch", map[string]any{}, 10*time.Millisecond); err != nil || status != 200 {
		t.Fatalf("a shorter wait of its own cut the usual one: %d %v", status, err)
	}
	if got.Load() != 4 {
		t.Errorf("the agent got %d requests, want 4 (none is sent twice)", got.Load())
	}
	// the wait is the request's own: nothing is left in a context that did not ask
	if _, ok := AgentWait(ctx); ok {
		t.Error("a plain context has a wait")
	}
	if w, ok := AgentWait(WithAgentWait(ctx, 25*time.Second)); !ok || w != 25*time.Second {
		t.Errorf("the wait a context carries: %v %v", w, ok)
	}
	if _, ok := AgentWait(WithAgentWait(ctx, 0)); ok {
		t.Error("a wait of nothing counts as one")
	}
}
