package poller

import (
	"context"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// r2-b1-14 (bug run of audit 5): a host whose agent is stopped was "OFFLINE" with "Power on" as the thing to
// press on its own page, while the Updates page called the same machine "ON" (it refused the connection, so
// it runs). The host's entry now carries what the Updates page goes by: machine_on, the sign of life of the
// latest poll. It is set only while the host is stale or offline, and only when the machine itself took part
// in that poll; a machine that gives nothing at all has none, and neither has one that answers.
func TestAHostWhoseMachineAnswersWithoutItsAgentSaysSo(t *testing.T) {
	b := testutil.NewBackend()
	var answer func() (jsonx.M, error)
	b.Metrics["laptop"] = func() (jsonx.M, error) { return answer() }
	cfg := testutil.Config(t, []string{"http://pve.invalid"}, "http://laptop.invalid", "http://tower.invalid")
	cfg.Intervals.Agent = 60
	p, err := New(cfg, config.Secrets{}, b)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 12, 3, 0, 0, 0, time.UTC)
	p.Clock = func() time.Time { return now }
	poll := func() (status, on string) {
		p.pollAgent(context.Background(), mustAgent(t, p, "laptop"), 0)
		s := snap(p)["laptop"]
		return s.Status, s.MachineOn
	}
	ok := func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
	refuse := func() (jsonx.M, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	}
	silent := func() (jsonx.M, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}
	}

	answer = ok
	if st, on := poll(); st != "online" || on != "" {
		t.Fatalf("an agent that answers: %s, machine_on %q", st, on)
	}
	// its agent is stopped: the machine refuses the connection. Stale at first, then offline; on both times.
	answer = refuse
	now = now.Add(10 * time.Second)
	if st, on := poll(); st != "stale" || on != SignRefused {
		t.Fatalf("refused, 10 s after the last answer: %s, machine_on %q", st, on)
	}
	now = now.Add(2 * time.Minute)
	if st, on := poll(); st != "offline" || on != SignRefused {
		t.Fatalf("refused for two minutes: %s, machine_on %q", st, on)
	}
	// something else answers in its place
	answer = func() (jsonx.M, error) { return nil, &devices.HTTPStatusError{Code: 401} }
	now = now.Add(time.Minute)
	if st, on := poll(); st != "offline" || on != "its address answered: http 401" {
		t.Fatalf("http 401: %s, machine_on %q", st, on)
	}
	// the machine is switched off: nothing at all comes back. Not "on" any more, at the first such poll.
	answer = silent
	now = now.Add(time.Minute)
	if st, on := poll(); st != "offline" || on != "" {
		t.Fatalf("nothing at all: %s, machine_on %q", st, on)
	}
	// and back
	answer = ok
	now = now.Add(time.Minute)
	if st, on := poll(); st != "online" || on != "" {
		t.Fatalf("the agent is back: %s, machine_on %q", st, on)
	}
	// a host that was never seen and gives nothing at all (tower in the fixture) has none
	p.pollAgent(context.Background(), mustAgent(t, p, "tower"), 0)
	if s := snap(p)["tower"]; s.Status != "offline" || s.MachineOn != "" {
		t.Fatalf("a host that never answered: %s, machine_on %q", s.Status, s.MachineOn)
	}
}
