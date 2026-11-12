package poller

import (
	"context"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// AgentInfo.Silent: the row of polls that got nothing at all, poll by poll, for a caller that must know that
// silence was seen by polls made after a moment of its own (audit 5, r3-p2-04: the run's "off" after a
// countdown). Every poll of the row is counted, the latest one is placed in time (began, ended), and anything
// that is not silence ends the row: an answer, a refused connection, a time the panel did not watch.
func TestSilentPollsAreCountedPollByPoll(t *testing.T) {
	b := testutil.NewBackend()
	mode := "down"
	// The clock of the test: the polls of the other machines read it from their own goroutines, so it is kept
	// under a lock (the race detector saw the plain variable).
	var ck sync.Mutex
	var took time.Duration // how long a poll takes
	now := time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC)
	pass := func(d time.Duration) {
		ck.Lock()
		now = now.Add(d)
		ck.Unlock()
	}
	takes := func(d time.Duration) {
		ck.Lock()
		took = d
		ck.Unlock()
	}
	b.Metrics["laptop"] = func() (jsonx.M, error) {
		ck.Lock()
		now = now.Add(took)
		ck.Unlock()
		switch mode {
		case "down":
			return nil, devices.Errf("connection failed")
		case "refused":
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
		}
		return testutil.AgentMetrics(), nil
	}
	cfg := testutil.Config(t, []string{"http://pve.invalid"}, "http://laptop.invalid", "http://tower.invalid")
	p, err := New(cfg, config.Secrets{}, b)
	if err != nil {
		t.Fatal(err)
	}
	p.Clock = func() time.Time {
		ck.Lock()
		defer ck.Unlock()
		return now
	}
	ctx := context.Background()
	silent := func() SilentPolls {
		i, _, _ := p.AgentSeen("laptop")
		if j, _ := p.AgentInfo("laptop"); j.Silent != i.Silent {
			t.Fatalf("AgentInfo and AgentSeen disagree: %+v %+v", j.Silent, i.Silent)
		}
		return i.Silent
	}
	set := func(m string) {
		b.Mu.Lock()
		mode = m
		b.Mu.Unlock()
	}
	if s := silent(); s != (SilentPolls{}) {
		t.Fatalf("never polled: %+v", s)
	}
	takes(3 * time.Second)
	p.PollOnce(ctx) // began at 0 s, ended at 3 s
	pass(2 * time.Second)
	first := silent()
	if first.N != 1 || first.Row == 0 || first.BeganAgo != 5*time.Second || first.EndedAgo != 2*time.Second {
		t.Fatalf("after one silent poll that took 3 s, looked at 2 s later: %+v", first)
	}
	p.PollOnce(ctx) // began at 5 s, ended at 8 s
	if s := silent(); s.N != 2 || s.Row != first.Row || s.BeganAgo != 3*time.Second || s.EndedAgo != 0 {
		t.Fatalf("after the second: %+v", s)
	}
	// a refused connection is a sign of life: no row
	set("refused")
	p.PollOnce(ctx)
	if s := silent(); s != (SilentPolls{}) {
		t.Fatalf("after a refused poll: %+v", s)
	}
	// silence again: another row, counted from one
	set("down")
	p.PollOnce(ctx)
	second := silent()
	if second.N != 1 || second.Row == first.Row || second.Row == 0 {
		t.Fatalf("the first silent poll after a refusal: %+v (the row before: %d)", second, first.Row)
	}
	// an answer ends it
	set("up")
	p.PollOnce(ctx)
	if s := silent(); s != (SilentPolls{}) {
		t.Fatalf("after an answer: %+v", s)
	}
	set("down")
	p.PollOnce(ctx)
	p.PollOnce(ctx)
	third := silent()
	if third.N != 2 || third.Row == second.Row {
		t.Fatalf("two silent polls after an answer: %+v", third)
	}
	// the panel stopped asking (polling paused): the row is not given out any more, and the next poll begins one
	takes(0)
	pass(p.quietGap(cfg.Intervals.Agent) + time.Second)
	if s := silent(); s != (SilentPolls{}) {
		t.Fatalf("after a time without polls: %+v", s)
	}
	p.PollOnce(ctx)
	if s := silent(); s.N != 1 || s.Row == third.Row {
		t.Fatalf("the first poll after a time without polls: %+v (the row before: %d)", s, third.Row)
	}
}
