package poller

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// One sender at a time (machEmit). The queue alone does not keep the order: a goroutine takes the events that
// wait and then calls OnMachine for each, and OnMachine takes its time (it writes the audit log and the
// Notifications list). Without the lock a second goroutine takes what was decided LATER and delivers it while
// the first is still inside OnMachine with what was decided earlier: "cleared" is in the log before the alarm
// it clears. The review of build 14 took the lock out and no test noticed, because nothing made the first
// delivery take time.
//
// Here the first delivery waits inside OnMachine. While it waits, a later decision's goroutine must deliver
// nothing: its event arrives when the first delivery is over, after it.
func TestALaterMachineEventWaitsForTheDeliveryOfAnEarlierOne(t *testing.T) {
	p, err := New(testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower"), config.Secrets{}, testutil.NewBackend())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []string
	inside, release := make(chan struct{}), make(chan struct{})
	p.OnMachine = func(e MachineEvent) {
		mu.Lock()
		got = append(got, e.Kind+" "+e.AgentID)
		first := len(got) == 1
		mu.Unlock()
		if first {
			close(inside)
			<-release
		}
	}
	delivered := func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(got, ", ")
	}
	decide := func(kind, agent string) { // a decision: made and queued under the poller's lock
		p.mu.Lock()
		p.queueMachinesLocked([]MachineEvent{{AgentID: agent, Kind: kind}})
		p.mu.Unlock()
	}
	var wg sync.WaitGroup
	decide("alarm", "laptop")
	wg.Add(1)
	go func() { defer wg.Done(); p.machinesChanged() }()
	<-inside // the first delivery is inside OnMachine and stays there
	decide("clear", "laptop")
	second := make(chan struct{})
	wg.Add(1)
	go func() { defer wg.Done(); p.machinesChanged(); close(second) }()
	select {
	case <-second:
		t.Fatalf("the later decision's goroutine came back while the earlier delivery was still going on; delivered: %s", delivered())
	case <-time.After(200 * time.Millisecond):
	}
	if d := delivered(); d != "alarm laptop" {
		t.Fatalf("delivered while the first delivery was still going on: %s", d)
	}
	close(release)
	wg.Wait()
	if d := delivered(); d != "alarm laptop, clear laptop" {
		t.Fatalf("delivered: %s", d)
	}
}
