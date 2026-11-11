package poller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// ProbeAgent asks the agent itself, now, with the real HTTP backend: it tells "answered", "nothing answered"
// and "something answered that is not usable" apart, and leaves the poller's own view as it was. AgentSeen
// hands out the agent's entry and the time of the poll it is from together.
func TestProbeAgentAsksTheAgentItself(t *testing.T) {
	mode := atomic.Value{}
	mode.Store("ok")
	var hits atomic.Int32
	laptop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch mode.Load() {
		case "401":
			w.WriteHeader(401)
		case "junk":
			_, _ = w.Write([]byte("<html>not an agent</html>"))
		default:
			_, _ = w.Write([]byte(`{"hostname":"laptop","uptime_s":42,"agent_version":"16-abcdef0"}`))
		}
	}))
	defer laptop.Close()
	cfg := testutil.Config(t, []string{deadURL(t)}, laptop.URL, deadURL(t))
	p, err := New(cfg, config.Secrets{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if got := p.ProbeAgent(ctx, "laptop"); got != "" {
		t.Fatalf("an agent that answers: %q", got)
	}
	if got := p.ProbeAgent(ctx, "tower"); got != "connection failed" && got != "timeout" {
		t.Fatalf("nothing listens: %q", got)
	}
	if got := p.ProbeAgent(ctx, "nobody"); got == "" {
		t.Fatal("an agent the panel does not know cannot be on")
	}
	// the probe did not teach the poller anything
	if _, _, known := p.AgentSeen("laptop"); known {
		t.Fatal("a probe must not change the poller's view")
	}
	p.PollOnce(ctx)
	info, sys, known := p.AgentSeen("laptop")
	if !known || !info.Online || sys.UptimeS == nil || *sys.UptimeS != 42 || sys.LastSeen == nil || !sys.LastSeen.Equal(info.SeenAt) {
		t.Fatalf("after a poll: %+v uptime %v", info, sys.UptimeS)
	}
	if info, sys, known := p.AgentSeen("tower"); !known || info.Online || sys.Status != "offline" {
		t.Fatalf("an agent that never answered: %+v %s %v", info, sys.Status, known)
	}
	// something answers that is not the agent: neither "on" nor "off"
	for m, want := range map[string]string{"401": "http 401", "junk": "invalid response"} {
		mode.Store(m)
		if got := p.ProbeAgent(ctx, "laptop"); got != want {
			t.Errorf("%s: %q, want %q", m, got, want)
		}
	}
	// the poller still says what its last poll said: the probe is how a caller learns about now
	if info, _, _ := p.AgentSeen("laptop"); !info.Online {
		t.Fatal("the poller's view changed without a poll")
	}
	laptop.Close()
	if got := p.ProbeAgent(ctx, "laptop"); got != "connection failed" && got != "timeout" {
		t.Fatalf("the agent is gone: %q", got)
	}
}
