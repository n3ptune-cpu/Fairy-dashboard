//go:build unix

package poller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
	"golang.org/x/sys/unix"
)

// What OnMachine hears arrives in the order it was decided, whichever goroutine sends it.
//
// The flake of agentcontract's TestBindingALaunderedTurnOnEndsAsOneIDOnTwoHosts (once in 220 runs, and once
// in 530 here): the audit log read "pc-a: duplicate, pc-b: duplicate, pc-b: vanished" while pc-b's standing
// alarm was "duplicate". Two answers of one poll are two goroutines. The one with pc-b's answer decided
// "vanished" (pc-b shows no check, and the panel had not yet heard who holds its id), the one with pc-a's
// answer decided "duplicate" for both a moment later, and each sent its own events after it had released the
// poller's lock: the second one first. The log and the Notifications list ended with an alarm that had been
// replaced; the same order of sending could leave "cleared" under an alarm that stands.
//
// Here the order is forced, not left to the scheduler: tower's answer brings a new agent version, which is
// written to state.json between the decision and the sending, and the test holds that file's lock (as
// `fairy admin` does while it writes). So tower's goroutine has decided and waits; laptop's answer is taken
// meanwhile, start to end; then the lock is let go.
func TestMachineEventsArriveInTheOrderTheyWereDecided(t *testing.T) {
	const id = "b3JkZXItb2YtZXZlbnRzLTAx"
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	b := testutil.NewBackend()
	answer := func(agent, version string, block jsonx.M) {
		b.Mu.Lock()
		defer b.Mu.Unlock()
		b.Metrics[agent] = func() (jsonx.M, error) {
			m := testutil.AgentMetrics()
			m["agent_version"] = version
			if block != nil {
				m["approval"] = block
			}
			return m, nil
		}
	}
	off := jsonx.M{"state": api.ApprovalOff, "proto": float64(api.ApprovalProto)}
	answer("laptop", "", off) // no version yet: the first one an agent reports is written at once (lastversion.go)
	answer("tower", "", off)
	p, err := New(cfg, config.Secrets{}, b)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []string
	p.OnMachine = func(e MachineEvent) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, fmt.Sprintf("%s %s %s", e.AgentID, e.Kind, e.Binding.Status))
	}
	p.MachinesLoaded()
	p.PollOnce(context.Background()) // both are heard once: changes of alarms are announced from here on
	// A Turn on for tower that tower handed on to laptop (the contract test's case): the id is on record for
	// tower, laptop holds it and shows it, tower shows no check.
	if err := p.PinMachine("tower", id, approval.PinTurnOn, "nora"); err != nil {
		t.Fatal(err)
	}
	answer("laptop", "", jsonx.M{"state": api.ApprovalRequired, "proto": float64(api.ApprovalProto), "machine_id": id})
	answer("tower", "42-bbbbbbb", off) // its first version: state.json is written after the decision, before the sending
	laptop, _ := p.AgentByID("laptop")
	tower, _ := p.AgentByID("tower")

	lock, err := os.OpenFile(filepath.Join(cfg.DataDir, "state.json.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); p.pollAgent(context.Background(), tower, 0) }()
	decided := func() bool { // tower's answer is taken, and "vanished" is decided for it
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.mach.last["tower"] == approval.BindVanished
	}
	for end := time.Now().Add(10 * time.Second); !decided(); time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
			t.Fatal("tower's answer never made its alarm \"vanished\"")
		}
	}
	select {
	case <-done:
		t.Fatal("the fixture: tower's poll did not wait for state.json")
	case <-time.After(20 * time.Millisecond):
	}
	p.pollAgent(context.Background(), laptop, 0) // decides "duplicate" for both, and sends
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	<-done

	mu.Lock()
	defer mu.Unlock()
	want := "tower alarm vanished | laptop alarm duplicate | tower alarm duplicate"
	if strings.Join(got, " | ") != want {
		t.Fatalf("the events arrived as\n  %s\nand were decided as\n  %s", strings.Join(got, " | "), want)
	}
	// the last word about each host is what holds for it
	for _, a := range []string{"laptop", "tower"} {
		if st := p.BindingOf(a).Status; st != approval.BindDuplicate {
			t.Errorf("%s: %s", a, st)
		}
	}
}
