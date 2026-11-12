package main

import (
	"testing"
	"time"
)

// Second review of fix7a (R7A-11 a): when the poller used up the whole wait at a stop, the wait for the
// notifications writer got a time of zero or less. A timer of no time has already fired, so Go picked at
// random between "it is written" and "time is up": the WARN "the notifications of the last seconds were not
// written" could be printed although they were. A writer that has finished is always seen as finished,
// whatever time is left.
func TestAWriterThatHasFinishedIsNeverReportedAsLate(t *testing.T) {
	closed := make(chan struct{})
	close(closed)
	for _, d := range []time.Duration{0, -time.Second, -time.Hour, time.Nanosecond} {
		for i := 0; i < 2000; i++ {
			if !waitStopped(closed, d) {
				t.Fatalf("a writer that has finished was reported as not finished (the time left: %v, try %d)", d, i)
			}
		}
	}
}

// The two waits of a stop: the poller and then the notifications writer, in one time for both. The writer
// always has its own part of it, also after a poller that never finishes.
func TestTheNotificationsWriterAlwaysHasItsTimeAtAStop(t *testing.T) {
	// (times with room for a busy machine: the writer is done 500 ms before its time is up)
	const total, least = 1500 * time.Millisecond, 600 * time.Millisecond
	never, closed := make(chan struct{}), make(chan struct{})
	close(closed)
	// a poll hangs for good; the writer finishes 100 ms after the poller's time is up
	late := make(chan struct{})
	go func() { time.Sleep(total - least + 100*time.Millisecond); close(late) }()
	start := time.Now()
	pollerOK, savedOK := waitWriters(never, late, total, least)
	if took := time.Since(start); pollerOK || !savedOK || took < total-least || took > total+5*time.Second {
		t.Errorf("a poller that hangs and a writer that needs 100 ms more: poller finished %v, writer finished %v, waited %v (the whole wait is %v)", pollerOK, savedOK, took, total)
	}
	// neither finishes: the whole time, not more, and both are said
	start = time.Now()
	pollerOK, savedOK = waitWriters(never, never, total, least)
	if took := time.Since(start); pollerOK || savedOK || took < total || took > total+5*time.Second {
		t.Errorf("nothing finishes: poller %v, writer %v, waited %v (the whole wait is %v)", pollerOK, savedOK, took, total)
	}
	// the usual stop: both have finished, no wait at all
	start = time.Now()
	if pollerOK, savedOK = waitWriters(closed, closed, total, least); !pollerOK || !savedOK || time.Since(start) > total/2 {
		t.Errorf("both finished: poller %v, writer %v, waited %v", pollerOK, savedOK, time.Since(start))
	}
	// a writer that has finished is seen as finished after a poller that used up its time, every time
	for i := 0; i < 20; i++ {
		if _, savedOK := waitWriters(never, closed, 20*time.Millisecond, 5*time.Millisecond); !savedOK {
			t.Fatalf("try %d: the writer had finished, and the stop says that its notifications were not written", i)
		}
	}
	if savedStopMin <= 0 || savedStopMin >= pollerStopWait/2 {
		t.Errorf("the writer's own part of the %v is %v", pollerStopWait, savedStopMin)
	}
}
