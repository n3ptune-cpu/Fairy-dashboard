package poller

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// AgentInfo.SeenAgo (review of fix7r2, R2-01): how long ago the agent's last answer came, by the steady clock.
// SeenAt is a time of day and moves with every setting of the panel's clock; SeenAgo does not. It is what a
// caller compares with a moment of its own (the run: "from after my shutdown request").
func TestSeenAgoIsMeasuredWithTheSteadyClock(t *testing.T) {
	b := testutil.NewBackend()
	up := true
	b.Metrics["laptop"] = func() (jsonx.M, error) {
		if !up {
			return nil, devices.Errf("connection failed")
		}
		return testutil.AgentMetrics(), nil
	}
	cfg := testutil.Config(t, []string{"http://pve.invalid"}, "http://laptop.invalid", "http://tower.invalid")
	p, err := New(cfg, config.Secrets{}, b)
	if err != nil {
		t.Fatal(err)
	}
	var ck sync.Mutex
	steady, skew := time.Duration(0), time.Duration(0)
	t0 := time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC)
	p.Clock = func() time.Time {
		ck.Lock()
		defer ck.Unlock()
		return t0.Add(steady + skew)
	}
	p.Mono = func() time.Duration {
		ck.Lock()
		defer ck.Unlock()
		return steady
	}
	pass := func(d, set time.Duration) {
		ck.Lock()
		steady, skew = steady+d, skew+set
		ck.Unlock()
	}
	ctx := context.Background()
	if i, ok := p.AgentInfo("laptop"); ok || i.SeenAgo != 0 {
		t.Fatalf("never polled: %+v", i)
	}
	p.PollOnce(ctx)
	seen := func() AgentInfo {
		i, _, _ := p.AgentSeen("laptop")
		return i
	}
	if i := seen(); !i.Online || i.SeenAgo != 0 || !i.SeenAt.Equal(t0) {
		t.Fatalf("just answered: %+v", i)
	}
	// time passes without a poll, and the time of day is set: back an hour, forward two, back two seconds
	for _, set := range []time.Duration{-time.Hour, 2 * time.Hour, -2 * time.Second} {
		before := seen().SeenAgo
		pass(3*time.Second, set)
		if i := seen(); i.SeenAgo != before+3*time.Second || !i.SeenAt.Equal(t0) {
			t.Fatalf("3 s later, the time of day set by %v: answered %v ago (before: %v), at %v", set, i.SeenAgo, before, i.SeenAt)
		}
		if i, _ := p.AgentInfo("laptop"); i.SeenAgo != before+3*time.Second {
			t.Fatalf("AgentInfo: %v", i.SeenAgo)
		}
	}
	// a poll that gets nothing keeps the last answer and its age
	b.Mu.Lock()
	up = false
	b.Mu.Unlock()
	pass(5*time.Second, 0)
	p.PollOnce(ctx)
	if i := seen(); i.Online || i.SeenAgo != 14*time.Second {
		t.Fatalf("after a failed poll: %+v", i)
	}
	// the next answer: now, whatever the time of day says
	b.Mu.Lock()
	up = true
	b.Mu.Unlock()
	pass(5*time.Second, -time.Hour)
	p.PollOnce(ctx)
	// (19 s have passed; the time of day is 2 s behind that after the four settings)
	if i := seen(); !i.Online || i.SeenAgo != 0 || !i.SeenAt.Equal(t0.Add(17*time.Second)) {
		t.Fatalf("answered again, with the time of day an hour back: %+v", i)
	}
}
