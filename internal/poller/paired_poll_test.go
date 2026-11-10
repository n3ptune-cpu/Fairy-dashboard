package poller

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// N5, kept by decision: the Tailscale check of Add host covers pairing, not the polls after it. A paired host
// whose name leads outside the Tailscale ranges is still polled, with its read token, like every agent in
// config.yaml (hosts that exist keep working whatever network they are on). This test holds the behaviour and
// the guide together: if the polls ever get a check, the guide's paragraph has to change with it.
func TestPollsAfterPairingAreNotCheckedAndTheGuideSaysSo(t *testing.T) {
	var got atomic.Value
	lan := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(testutil.AgentMetrics())
	}))
	defer lan.Close()
	cfg := testutil.Config(t, []string{deadURL(t)}, deadURL(t), deadURL(t))
	p, err := New(cfg, config.Secrets{SessionSecret: "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(lan.URL, "http://"))
	// a host as Add host stores it (by name); the name leads to 127.0.0.1, which is not a Tailscale address
	tok := strings.Repeat("r", 32)
	if err := p.AddAgent(config.Agent{ID: "box", Name: "box", URL: "http://localhost:" + port, Managed: true, Token: tok}, nil); err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	if sent, _ := got.Load().(string); sent != "Bearer "+tok || snap(p)["box"].Status != "online" {
		t.Fatalf("the paired host was not polled as before: sent %d bytes, shown as %s", len(sent), snap(p)["box"].Status)
	}
	guide, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(guide)), " ")
	for _, want := range []string{
		"The check covers pairing and nothing after it.",
		"those requests are not checked against the Tailscale ranges",
		"the read token goes with every poll and the control token with every action",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("README.md no longer says: %q", want)
		}
	}
}
