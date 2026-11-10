package enrollment

import (
	mrand "math/rand/v2"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
)

// How often the panel asks a machine to pair.
//
// An agent looks at enroll.RatePerMin (5) pairing requests a minute from one address, counted over the last
// 60 seconds (Fairy-agent pkg/agent/pairing.go; agents before pairing v2 count the same number for all
// callers together). Every further request is answered 429 until an earlier one is a minute old. A panel that
// asks faster than that spends the minute's requests in the first seconds and is then refused for the rest
// of the minute, which is exactly when the install command finishes: pairing came up to a minute late.
//
// The panel therefore stays under the budget by itself: one request less per minute than the agent allows,
// spread evenly, a little earlier or later each time so that two panels (or two tabs of one admin) do not
// fall into step. Even the shortest gap, five times over, is longer than the agent's minute, so the sixth
// request never meets five counted ones (pinned by TestPairPaceStaysInsideTheAgentsBudget). The price is that
// pairing follows the command by up to PairInterval plus the jitter instead of "at once or a minute late".
const (
	// PairInterval is the time between two pairing requests to one machine: 15 s for the agent's 5 a minute.
	PairInterval = time.Minute / (enroll.RatePerMin - 1)
	// pairJitterDiv: every gap is the interval plus or minus up to one part in pairJitterDiv (15 s: 13.1 s to
	// 16.9 s).
	pairJitterDiv = 8
	// pairBackoffMax: after answers that say the agent's budget is used up (429) the gap doubles, up to this
	// many intervals (15 s: 30 s, then 60 s, which is the agent's whole window).
	pairBackoffMax = 4
)

// jittered is d plus or minus up to d/pairJitterDiv.
func jittered(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	spread := d / pairJitterDiv
	return d - spread + mrand.N(2*spread+1)
}

// interval is the manager's base gap between pairing requests to one machine.
func (m *Manager) interval() time.Duration {
	if m.Interval > 0 {
		return m.Interval
	}
	return PairInterval
}

// turn asks for the next pairing request to an address: 0 means "send it now" (and the time of the one after
// it is set), anything else is how long to wait before asking again. The gap is kept per address, not per
// enrollment: a new code for the same machine right after a cancelled one, or a Re-pair right after Add
// host, continues the pace instead of starting a burst (the agent's count does not start again either). The
// first request to an address the panel has not asked lately goes out at once. Nothing is reserved ahead, so
// an enrollment that ends while it waits takes no turn away from the next one.
func (m *Manager) turn(url string) time.Duration {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.nextAsk == nil {
		m.nextAsk = map[string]time.Time{}
	}
	for k, t := range m.nextAsk { // addresses whose turn has come and nobody took it: nothing to remember
		if k != url && !t.After(now) {
			delete(m.nextAsk, k)
		}
	}
	if at := m.nextAsk[url]; at.After(now) {
		return at.Sub(now)
	}
	m.nextAsk[url] = now.Add(jittered(m.interval()))
	return 0
}

// holdOff moves the next request to an address further away after its agent answered 429 for the n-th time
// in a row, and returns that wait: twice the interval, then four times (the agent's whole window), each with
// the usual jitter. Something else is using the agent's budget, or the agent counts differently than this
// panel expects; asking at the normal pace would only keep the budget full.
func (m *Manager) holdOff(url string, n int) time.Duration {
	mult := time.Duration(pairBackoffMax)
	if n < 2 {
		mult = 2
	}
	d := jittered(mult * m.interval())
	at := time.Now().Add(d)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.nextAsk == nil {
		m.nextAsk = map[string]time.Time{}
	}
	if at.After(m.nextAsk[url]) {
		m.nextAsk[url] = at
	}
	return d
}
