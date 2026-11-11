package poller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

func versioned(v string) func() (jsonx.M, error) {
	return func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["agent_version"] = v
		return m, nil
	}
}

// The version an agent reported is known again after a restart of the panel while the host is off; it is
// written when it changes and not on every poll; it is not handed out as the agent's present state.
func TestLastAgentVersionSurvivesARestart(t *testing.T) {
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	state := filepath.Join(cfg.DataDir, "state.json")
	ctx := context.Background()
	b := testutil.NewBackend()
	b.Metrics["laptop"] = versioned("16-a791c55")
	p, err := New(cfg, config.Secrets{}, b)
	if err != nil {
		t.Fatal(err)
	}
	if v := p.LastAgentVersion("laptop"); v != "" {
		t.Fatal("before any poll:", v)
	}
	p.PollOnce(ctx)
	if v := p.LastAgentVersion("laptop"); v != "16-a791c55" || p.LastAgentVersion("tower") != "" {
		t.Fatal("after a poll:", v, p.LastAgentVersion("tower"))
	}
	first, err := os.ReadFile(state)
	if err != nil || !strings.Contains(string(first), `"version": "16-a791c55"`) {
		t.Fatalf("state.json after the first good poll: %s (%v)", first, err)
	}
	written, _ := os.Stat(state)
	for range 3 { // the same version again: nothing is written (a write puts a new file in the old one's place)
		p.PollOnce(ctx)
	}
	if st, err := os.Stat(state); err != nil || !os.SameFile(written, st) {
		t.Fatal("state.json was rewritten without a new version", err)
	}
	if written.Mode().Perm() != 0o600 {
		t.Fatal("mode", written.Mode().Perm())
	}

	// the panel starts again; the laptop is off now, the tower was never on
	off := testutil.NewBackend()
	off.Metrics["laptop"] = off.Metrics["tower"]
	p2, err := New(cfg, config.Secrets{}, off)
	if err != nil {
		t.Fatal(err)
	}
	p2.PollOnce(ctx)
	if v := p2.LastAgentVersion("laptop"); v != "16-a791c55" || p2.LastAgentVersion("tower") != "" {
		t.Fatalf("after the restart: laptop %q, tower %q", v, p2.LastAgentVersion("tower"))
	}
	// it is not the agent's present state: what decides on that still knows nothing of the laptop
	if info, seen := p2.AgentInfo("laptop"); seen || info.Online || info.Version != "" {
		t.Fatalf("AgentInfo after the restart: %+v seen %v", info, seen)
	}
	if info, _, _ := p2.AgentSeen("laptop"); info.Online || info.Version != "" {
		t.Fatalf("AgentSeen after the restart: %+v", info)
	}

	// it comes back with another version (it was updated by hand meanwhile): that one is kept from now on
	off.Mu.Lock()
	off.Metrics["laptop"] = versioned("17-bbbbbbb")
	off.Mu.Unlock()
	p2.PollOnce(ctx)
	if info, seen := p2.AgentInfo("laptop"); !seen || !info.Online || info.Version != "17-bbbbbbb" || p2.LastAgentVersion("laptop") != "17-bbbbbbb" {
		t.Fatalf("back on: %+v", info)
	}
	if raw, _ := os.ReadFile(state); !strings.Contains(string(raw), `"version": "17-bbbbbbb"`) || strings.Contains(string(raw), "16-a791c55") {
		t.Fatalf("state.json after the new version: %s", raw)
	}
	// an answer without a usable version does not wipe the last one
	off.Mu.Lock()
	off.Metrics["laptop"] = versioned("not a version\x1b[31m")
	off.Mu.Unlock()
	p2.PollOnce(ctx)
	if info, _ := p2.AgentInfo("laptop"); info.Version != "" || p2.LastAgentVersion("laptop") != "17-bbbbbbb" {
		t.Fatalf("an unusable version: info %q, last %q", info.Version, p2.LastAgentVersion("laptop"))
	}
}

// What is in state.json is only used when it is a version the panel would take from an agent; and removing a
// host forgets its version, so a host added later under the same id starts without one.
func TestLastAgentVersionIsCheckedAndForgotten(t *testing.T) {
	cfg := testutil.Config(t, []string{deadURL(t)}, deadURL(t), deadURL(t))
	state := filepath.Join(cfg.DataDir, "state.json")
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := `{"poll_overrides": {"laptop": 5}, "agent_versions": {"laptop": {"version": "<b>16</b>"}, "tower": {"version": "16-a791c55"},
	  "srv-winpc": {"version": "15-0123abc"}, "gone": {"version": "14-0000000"}}}`
	if err := os.WriteFile(state, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := New(cfg, config.Secrets{}, testutil.NewBackend())
	if err != nil {
		t.Fatal(err)
	}
	if p.LastAgentVersion("laptop") != "" || p.LastAgentVersion("tower") != "16-a791c55" || p.LastAgentVersion("srv-winpc") != "15-0123abc" {
		t.Fatalf("read at the start: %q %q %q", p.LastAgentVersion("laptop"), p.LastAgentVersion("tower"), p.LastAgentVersion("srv-winpc"))
	}
	a := config.Agent{ID: "srv-winpc", Name: "SRV-WINPC", URL: deadURL(t), Managed: true, Token: "managed-read-token-xxxxxxxxxxxxxxxx"}
	if err := p.AddAgent(a, nil); err != nil {
		t.Fatal(err)
	}
	p.RemoveAgent("srv-winpc")
	if v := p.LastAgentVersion("srv-winpc"); v != "" {
		t.Fatal("after removing the host:", v)
	}
	d, err := statefile.Read(state)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := d["agent_versions"].(map[string]any)
	if _, still := m["srv-winpc"]; still || m["tower"] == nil || m["gone"] == nil || d["poll_overrides"] == nil {
		t.Fatalf("state.json after removing the host: %v", d)
	}
}
