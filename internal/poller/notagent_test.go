package poller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// r2-b1-01 (bug run of audit 5), the auditor's steps: something at an agent's address answers every request
// with 200 and JSON that is not an agent's ({"a":"b"}). The host was "online" with empty readings, without an
// error, and never went offline, so nobody was told. Now such an answer is "invalid response", exactly like an
// answer that is not JSON: stale while the last real answer is fresh, then offline, with the error.
//
// For "Update unpowered hosts" the machine is still on (something at its address answered): the poll is a
// sign of life, never the silence of a machine that is off, however long it lasts, and the run's direct
// question does not say "timeout" or "connection failed".
func TestA200AnswerThatIsNotAnAgentsIsNotOnline(t *testing.T) {
	for _, foreign := range []string{`{"a":"b"}`, `{}`, `[1,2]`, `null`, `"ok"`, ``,
		`{"agent_version":"16-a791c55"}`,                     // no platform
		`{"platform":{"system":"Linux"}}`,                    // no version
		`{"agent_version":16,"platform":{"system":"Linux"}}`, // a version that is not a text
		`{"agent_version":"16-a791c55","platform":"Linux"}`,  // a platform that is not an object
	} {
		t.Run(foreign, func(t *testing.T) {
			var body atomic.Value
			real, _ := json.Marshal(testutil.AgentMetrics())
			body.Store(real)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body.Load().([]byte))
			}))
			defer srv.Close()
			cfg := testutil.Config(t, []string{deadURL(t)}, srv.URL, deadURL(t))
			cfg.Intervals.Agent = 60
			p, err := New(cfg, config.Secrets{}, nil) // the real HTTP backend
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 11, 12, 3, 0, 0, 0, time.UTC)
			p.Clock = func() time.Time { return now }
			ctx := context.Background()
			agent := mustAgent(t, p, "laptop")

			p.pollAgent(ctx, agent, 0)
			if lap := snap(p)["laptop"]; lap.Status != "online" || lap.Error != nil || lap.Cores == nil {
				t.Fatalf("the real answer: %s %v", lap.Status, lap.Error)
			}
			if got := p.ProbeAgent(ctx, "laptop"); got != "" {
				t.Fatalf("the direct question of a real agent: %q", got)
			}

			body.Store([]byte(foreign))
			for i := 1; i <= 17; i++ { // seventeen minutes, a poll a minute
				now = now.Add(time.Minute)
				p.pollAgent(ctx, agent, 0)
				lap := snap(p)["laptop"]
				want := "offline" // offline_after (60 s in the fixture and by default) is over at the first poll, a minute later
				if lap.Status != want || lap.Error == nil || *lap.Error != "invalid response" {
					t.Fatalf("poll %d: %s, error %v; want %s with \"invalid response\"", i, lap.Status, lap.Error, want)
				}
				off, info := offNow(p, "laptop", now)
				if off || info.Online || !info.QuietSince.IsZero() || info.Sign != "its address answered: invalid response" || info.Refused {
					t.Fatalf("poll %d: for a run the machine must be on: off=%v online=%v quiet since %v sign %q", i, off, info.Online, info.QuietSince, info.Sign)
				}
			}
			if got := p.ProbeAgent(ctx, "laptop"); got != "invalid response" {
				t.Fatalf("the direct question: %q, want \"invalid response\"", got)
			}

			body.Store(real) // the agent is back: online again, the error is gone
			now = now.Add(time.Minute)
			p.pollAgent(ctx, agent, 0)
			if lap := snap(p)["laptop"]; lap.Status != "online" || lap.Error != nil {
				t.Fatalf("the agent is back: %s %v", lap.Status, lap.Error)
			}
		})
	}
}

// A foreign answer within offline_after of the last real one is "stale" (the readings of the last real answer
// stay on the page with the error), as for every other failed poll.
func TestAForeignAnswerRightAfterARealOneIsStale(t *testing.T) {
	var foreign atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if foreign.Load() {
			_, _ = w.Write([]byte(`{"a":"b"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(testutil.AgentMetrics())
	}))
	defer srv.Close()
	cfg := testutil.Config(t, []string{deadURL(t)}, srv.URL, deadURL(t))
	p, err := New(cfg, config.Secrets{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p.pollAgent(ctx, mustAgent(t, p, "laptop"), 0)
	foreign.Store(true)
	p.pollAgent(ctx, mustAgent(t, p, "laptop"), 0)
	lap := snap(p)["laptop"]
	if lap.Status != "stale" || lap.Error == nil || *lap.Error != "invalid response" || lap.Cores == nil || *lap.Cores != 8 {
		t.Fatalf("%s %v %v", lap.Status, lap.Error, lap.Cores)
	}
}
