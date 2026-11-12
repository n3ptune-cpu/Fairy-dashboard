package agentcontract_test

// What the panel asks of a 200 answer to /metrics before it takes it as an agent's ("agent_version" as a text
// and "platform" as an object; bug run of audit 5, r2-b1-01) must hold for the barest agent there is: the
// real fairy-agent handler with nothing switched on, host details hidden, and no version (a build made by hand
// says "dev"; here even that is left out). If the agent ever stops sending one of the two, this fails here
// and not on somebody's Overview.

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/agenttest"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
)

func TestTheBarestRealAgentIsTakenAsAnAgent(t *testing.T) {
	for _, tc := range []struct{ name, yaml, version string }{
		{"nothing switched on, no version", "listen: 127.0.0.1:9101\n", ""},
		{"host details hidden", "listen: 127.0.0.1:9101\nhide_host_details: true\n", "dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acfg, err := agent.Parse([]byte(tc.yaml), filepath.Join(t.TempDir(), "config.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			a := agent.New(acfg, agent.Tokens{Read: readTok}, agent.Deps{Collector: agenttest.NewCollector(), Version: tc.version})
			srv := httptest.NewServer(a.Handler())
			t.Cleanup(srv.Close)
			p, err := poller.New(panelConfig(t, srv.URL), config.Secrets{SessionSecret: strings.Repeat("s", 48)}, nil)
			if err != nil {
				t.Fatal(err)
			}
			p.PollOnce(context.Background())
			for _, s := range p.Snapshot() {
				if s.ID == "srv-tower" {
					if s.Status != "online" || s.Error != nil {
						t.Fatalf("a real agent was turned away: %s %v", s.Status, s.Error)
					}
					return
				}
			}
			t.Fatal("no entry for the agent")
		})
	}
}
