package poller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// r2-p2-11 (audit 5, round 2): an agent that reports another valid version at every poll made the panel
// rewrite state.json at every poll (87 times in 90 s for the auditor). An agent's version is now written at
// most once a minute; what the panel shows is always the latest, and the latest is written when the panel
// stops.
func TestAnAgentsVersionIsWrittenAtMostOnceAMinute(t *testing.T) {
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	state := filepath.Join(cfg.DataDir, "state.json")
	ctx := context.Background()
	b := testutil.NewBackend()
	n := 0
	b.Metrics["laptop"] = func() (jsonx.M, error) { // another valid version at every poll
		n++
		m := testutil.AgentMetrics()
		m["agent_version"] = fmt.Sprintf("%d-%07x", 16+n%2, n)
		return m, nil
	}
	p, err := New(cfg, config.Secrets{}, b)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 12, 3, 0, 0, 0, time.UTC)
	p.Clock = func() time.Time { return now }
	writes, last := 0, os.FileInfo(nil)
	count := func() {
		st, err := os.Stat(state)
		if err != nil {
			return
		}
		if last == nil || !os.SameFile(last, st) { // every write puts a new file in the old one's place
			writes++
		}
		last = st
	}
	for i := 0; i < 90; i++ { // ninety seconds, a poll a second
		p.PollOnce(ctx)
		count()
		if got, want := p.LastAgentVersion("laptop"), fmt.Sprintf("%d-%07x", 16+n%2, n); got != want {
			t.Fatalf("poll %d: the panel knows %q, the agent said %q", i, got, want)
		}
		now = now.Add(time.Second)
	}
	if writes != 2 { // at the first answer, and once more a minute later
		t.Fatalf("state.json was written %d times in 90 s, want 2", writes)
	}
	// the last value is not in the file yet; it is when the poller stops
	final := p.LastAgentVersion("laptop")
	if raw, _ := os.ReadFile(state); strings.Contains(string(raw), `"version": "`+final+`"`) {
		t.Fatalf("setup: the last version is already written: %s", raw)
	}
	p.FlushVersions()
	raw, err := os.ReadFile(state)
	if err != nil || !strings.Contains(string(raw), `"version": "`+final+`"`) {
		t.Fatalf("after the poller stopped, state.json lacks %q: %s (%v)", final, raw, err)
	}
	// and a second flush writes nothing
	count()
	p.FlushVersions()
	before := writes
	count()
	if writes != before {
		t.Fatal("a flush with nothing waiting wrote the file")
	}
	// the next start reads the last value
	p2, err := New(cfg, config.Secrets{}, testutil.NewBackend())
	if err != nil || p2.LastAgentVersion("laptop") != final {
		t.Fatalf("after a restart: %q, want %q (%v)", p2.LastAgentVersion("laptop"), final, err)
	}
}

// What an admin sees is not slowed down: an agent's first version is written at once, an update more than a
// minute later at once, and one that follows sooner with the agent's first answer after the minute.
func TestAnAgentsVersionAfterAnUpdateIsWritten(t *testing.T) {
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	state := filepath.Join(cfg.DataDir, "state.json")
	ctx := context.Background()
	b := testutil.NewBackend()
	b.Metrics["laptop"] = versioned("16-a791c55")
	p, err := New(cfg, config.Secrets{}, b)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 12, 3, 0, 0, 0, time.UTC)
	p.Clock = func() time.Time { return now }
	has := func(v string) bool {
		raw, _ := os.ReadFile(state)
		return strings.Contains(string(raw), `"version": "`+v+`"`)
	}
	set := func(v string) {
		b.Mu.Lock()
		b.Metrics["laptop"] = versioned(v)
		b.Mu.Unlock()
	}
	p.PollOnce(ctx)
	if !has("16-a791c55") {
		t.Fatal("the first version is not written at once")
	}
	now = now.Add(5 * time.Minute)
	set("17-bbbbbbb")
	p.PollOnce(ctx)
	if !has("17-bbbbbbb") {
		t.Fatal("an update five minutes later is not written at once")
	}
	now = now.Add(20 * time.Second)
	set("18-ccccccc")
	p.PollOnce(ctx)
	if has("18-ccccccc") || p.LastAgentVersion("laptop") != "18-ccccccc" {
		t.Fatalf("a second change 20 s later: written %v, known %q", has("18-ccccccc"), p.LastAgentVersion("laptop"))
	}
	now = now.Add(41 * time.Second) // the same version, the minute is over
	p.PollOnce(ctx)
	if !has("18-ccccccc") {
		t.Fatal("the version that waited is not written with the first answer after the minute")
	}
	// a clock that is set back does not hold a write up
	now = now.Add(-time.Hour)
	set("19-ddddddd")
	p.PollOnce(ctx)
	if !has("19-ddddddd") {
		t.Fatal("a version after the clock was set back is not written")
	}
}
