package web

import (
	"testing"
	"time"
)

// A run knows the countdown it started by its length (wakerun: callOff, otherShutdown): the agent says for
// every pending action with which delay it was asked for, and a shutdown of another length than the run's was
// asked for by somebody else and is not called off by the run. So what a run is told about a machine's pending
// action must carry that length (second review of build 14, R3; it carried the action and its time only).
func TestARunIsToldTheLengthOfAPendingCountdown(t *testing.T) {
	a := newApprovalEnv(t)
	at := time.Now().Add(8 * time.Second).Truncate(time.Second)
	a.pendingAt(at) // the fixture's shutdown: delay_s 10
	o := runOps{a.s}.Observe("laptop")
	if o.Pending == nil || o.Pending.Action != "shutdown" || !o.Pending.At.Equal(at) || o.Pending.DelayS != 10 {
		t.Fatalf("what the run is told is pending on laptop: %+v, want a shutdown at %s with a countdown of 10 s", o.Pending, at)
	}
}
