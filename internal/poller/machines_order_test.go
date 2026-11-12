package poller

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// machines.json never goes back to an older record.
//
// Every answer of an agent ends in flushMachines, and the agents are asked at the same time. flushMachines took
// its copy of the changed records under p.mu and wrote it after the lock was released, and nothing kept two
// such writes in the order of their copies. What one host shows changes another host's record too (its alarm),
// so two answers have the same record in their copies, an older and a newer one. When the newer copy was
// written first, the older one was written over it and stayed: the newer write had already taken the record
// off the list of what is still to write. Memory was right, the file was not, until the record changed again;
// after a restart the older record counted (seen as an alarm that is written to the audit log a second time).
//
// Here eight hosts are asked at the same time, poll after poll, as the scheduler asks them. Two answers of a
// poll change the laptop's record: its own answer the state, the tower's answer the alarm (the tower shows the
// laptop's machine id in every second answer, which makes the laptop a duplicate). The six others change their
// own record only; their writes are what the two wait behind. After every poll the file must say what memory
// says, with nothing left to write.
//
// Without the lock in flushMachines it failed in 200 of 200 runs on four cores, at poll 12 in the middle and at
// poll 102 at the latest (with 60 polls: 194 of 200). It is a race: how often it shows depends on the machine.
func TestMachinesFileNeverGoesBackToAnOlderRecord(t *testing.T) {
	const (
		polls = 120
		id    = "bGFwdG9wLW1hY2hpbmUtaWQtMDE"
	)
	others := []string{"nas", "desk", "htpc", "box", "pi", "nuc"}
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	for _, more := range others {
		a := cfg.Agents[1]
		a.ID, a.URL = more, "http://"+more
		cfg.Agents = append(cfg.Agents, a)
	}
	b := testutil.NewBackend()
	// what a host shows in its answers, in turn (the backend asks one host at a time, under its lock)
	shows := func(host string, states ...string) {
		n := 0
		b.Metrics[host] = func() (jsonx.M, error) {
			m := testutil.AgentMetrics()
			switch state := states[n%len(states)]; state {
			case "":
			case api.ApprovalRequired:
				m["approval"] = jsonx.M{"state": state, "proto": float64(api.ApprovalProto), "machine_id": id}
			default:
				m["approval"] = jsonx.M{"state": state}
			}
			n++
			return m, nil
		}
	}
	shows("laptop", api.ApprovalRequired, api.ApprovalBroken)
	shows("tower", api.ApprovalRequired, "")
	for _, more := range others {
		shows(more, api.ApprovalBroken, api.ApprovalUnknown)
	}
	p, err := New(cfg, config.Secrets{}, b)
	if err != nil {
		t.Fatal(err)
	}
	// A clock of whole seconds that never shows the same time twice: the file keeps seconds, and every change of
	// a record then has its own time in it. (An hour ahead: the start is over, alarms are announced.)
	var tick atomic.Int64
	start := time.Now().Add(time.Hour).Truncate(time.Second)
	p.Clock = func() time.Time { return start.Add(time.Duration(tick.Add(1)) * time.Second) }

	ctx := context.Background()
	if err := p.PinMachine("laptop", id, approval.PinTurnOn, "nora"); err != nil {
		t.Fatal(err)
	}
	hosts := p.Agents()
	for _, a := range hosts {
		p.pollAgent(ctx, a, 0)
	}
	if bl, bt := p.BindingOf("laptop"), p.BindingOf("tower"); bl.Status != approval.BindDuplicate || bl.Record.MachineID != id || bt.Record.MachineID != "" || !bt.Record.Unaccepted {
		t.Fatalf("before the first poll: laptop %+v, tower %+v", bl, bt)
	}

	same := func(x, y approval.Record) bool {
		if !x.At.Equal(y.At) || !x.StateAt.Equal(y.StateAt) {
			return false
		}
		x.At, x.StateAt, y.At, y.StateAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
		return x == y
	}
	alarms := map[string]bool{}
	for poll := 1; poll <= polls; poll++ {
		var wg sync.WaitGroup
		for _, a := range hosts {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p.pollAgent(ctx, a, 0)
			}()
		}
		wg.Wait()

		p.mu.Lock()
		memory, unwritten := map[string]approval.Record{}, len(p.mach.dirty)+len(p.mach.drop)
		for k, r := range p.mach.records {
			memory[k] = r
		}
		p.mu.Unlock()
		file, err := p.Admin.Machines()
		if err != nil {
			t.Fatal(err)
		}
		if unwritten != 0 || len(file) != len(memory) || len(memory) != len(hosts) {
			t.Fatalf("poll %d: %d records still to write, %d in the file, %d in memory:\nfile:   %+v\nmemory: %+v", poll, unwritten, len(file), len(memory), file, memory)
		}
		for k, r := range memory {
			if !same(file[k], r) {
				t.Fatalf("poll %d: machines.json has another record of %s than memory (an older one was written last):\nfile:   %+v\nmemory: %+v", poll, k, file[k], r)
			}
		}
		alarms[memory["laptop"].Alarm] = true
	}
	// the records did change the way the test means them to (without that it would prove nothing)
	if !alarms[approval.BindDuplicate] || !alarms[""] {
		t.Fatalf("the laptop's alarm never changed: %v", alarms)
	}
	// still one file, replaced in one step, for the panel's user only
	path := adminstate.MachinesPath(cfg.DataDir)
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("machines.json: %v %v", st, err)
	}
	if left, _ := filepath.Glob(filepath.Join(cfg.DataDir, ".machines.json.*")); len(left) != 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
}
