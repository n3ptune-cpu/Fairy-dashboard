package scheduler

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
)

type clock struct {
	mu sync.Mutex
	t  float64
}

func (c *clock) now() float64  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(v float64) { c.mu.Lock(); c.t = v; c.mu.Unlock() }

func job(key string, def, floor float64, ids ...string) *Job {
	return &Job{Key: key, DefaultS: def, FloorS: floor, Systems: func() []string { return ids },
		Fn: func(context.Context, time.Duration) float64 { return 0 }}
}

func TestEffectiveIntervalsOverridesFloorsPause(t *testing.T) {
	c := &clock{t: 100}
	dir := t.TempDir()
	s := New(filepath.Join(dir, "state.json"), true, c.now)
	cluster := job("proxmox:a", 2, 1, "node-a", "qemu-1")
	plug := job("power:tuya", 5, 3, "llm-box")
	s.Add(cluster)
	s.Add(plug)
	if *s.Effective(cluster) != 2 || *s.Effective(plug) != 5 {
		t.Fatal("defaults")
	}
	_ = s.SetOverride("llm-box", &Override{Seconds: 1}) // below the Tuya floor -> clamped to 3
	if *s.Effective(plug) != 3 {
		t.Fatal(*s.Effective(plug))
	}
	d := s.Describe("llm-box")
	if !d[0].Clamped || d[0].FloorS != 3 {
		t.Fatalf("%+v", d)
	}
	_ = s.SetOverride("qemu-1", &Override{Seconds: 60}) // slowing one member never slows the shared job
	if *s.Effective(cluster) != 2 {
		t.Fatal("shared job slowed down")
	}
	_ = s.SetOverride("node-a", &Override{Paused: true})
	_ = s.SetOverride("qemu-1", &Override{Paused: true})
	if s.Effective(cluster) != nil {
		t.Fatal("all members paused -> job paused")
	}
	_ = s.SetOverride("qemu-1", &Override{Seconds: 999}) // clamped to 300
	if *s.Effective(cluster) != 300 {
		t.Fatal(*s.Effective(cluster))
	}
	// persisted for the CLI / restarts
	sd, _ := statefile.Read(filepath.Join(dir, "state.json"))
	po := sd["poll_overrides"].(map[string]any)
	if po["node-a"] != "paused" || po["qemu-1"] != 300.0 || po["llm-box"] != 1.0 {
		t.Fatalf("%v", po)
	}
	s2 := New(filepath.Join(dir, "state.json"), true, c.now)
	if !s2.IsPaused("node-a") {
		t.Fatal("reload")
	}
	_ = s.SetOverride("node-a", nil)
	if s.IsPaused("node-a") {
		t.Fatal("reset")
	}
}

func TestBoostWhileViewingWithGrace(t *testing.T) {
	c := &clock{t: 0}
	s := New("", true, c.now)
	j := job("agent:x", 5, 1, "x")
	s.Add(j)
	s.BoostAcquire("x")
	s.BoostAcquire("x")
	if *s.Effective(j) != 1 || !s.Boosted("x") {
		t.Fatal("boost")
	}
	s.BoostRelease("x")
	s.BoostRelease("x")
	c.set(29)
	if !s.Boosted("x") {
		t.Fatal("30 s grace")
	}
	c.set(31)
	if s.Boosted("x") || *s.Effective(j) != 5 {
		t.Fatal("grace over")
	}
	off := New("", false, c.now)
	off.Add(j)
	off.BoostAcquire("x")
	if off.Boosted("x") {
		t.Fatal("boost disabled")
	}
	// floors clamp boosts too
	bmc := job("power:redfish", 10, 10, "y")
	s.Add(bmc)
	s.BoostAcquire("y")
	if *s.Effective(bmc) != 10 {
		t.Fatal("floor")
	}
}

func TestNeverOverlapsAndHonoursBackoff(t *testing.T) {
	c := &clock{t: 0}
	s := New("", true, c.now)
	release := make(chan struct{})
	var runs atomic.Int32
	slow := &Job{Key: "slow", DefaultS: 1, Fn: func(context.Context, time.Duration) float64 {
		runs.Add(1)
		<-release
		return 0
	}}
	back := &Job{Key: "back", DefaultS: 1, Fn: func(context.Context, time.Duration) float64 { return 8 }}
	s.Add(slow)
	s.Add(back)
	ctx := context.Background()
	s.Tick(ctx)
	for i := 1; i <= 3; i++ {
		time.Sleep(20 * time.Millisecond)
		c.set(float64(i))
		s.Tick(ctx)
	}
	close(release)
	s.Wait()
	r, skipped := slow.Stats()
	if runs.Load() != 1 || r != 1 || skipped != 3 {
		t.Fatalf("runs %d skipped %d", runs.Load(), skipped)
	}
	br, _ := back.Stats()
	if br != 1 { // the backoff (8 s) pushed the next run past t=3
		t.Fatalf("backoff ignored: %d runs", br)
	}
	if TimeoutFor(slow, 1) != 900*time.Millisecond || TimeoutFor(slow, 0.5) != 800*time.Millisecond ||
		TimeoutFor(&Job{TimeoutCap: 4}, 10) != 4*time.Second {
		t.Fatal("timeouts")
	}
}

func TestReloadFromCLI(t *testing.T) {
	s := New("", true, (&clock{}).now)
	j := job("agent:x", 1, 1, "x")
	s.Add(j)
	if !s.Reload(map[string]any{"x": "paused", "bad": 0.2, "y": "nonsense"}) {
		t.Fatal("changed")
	}
	if !s.IsPaused("x") || len(s.Overrides()) != 1 {
		t.Fatalf("%v", s.Overrides())
	}
	if s.Reload(map[string]any{"x": "paused"}) {
		t.Fatal("no change")
	}
}
