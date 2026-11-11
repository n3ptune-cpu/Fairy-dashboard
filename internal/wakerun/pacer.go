package wakerun

import "time"

// An agent accepts a limited number of control requests a minute for everything together (its
// control_rate_per_min, 12 unless its owner changed it): wake packets it relays, updates, power actions, fans.
// A run that wakes several machines through one relay would use that up at once, and the relay would refuse
// the rest of the run and the admin's own clicks. So the run counts what it sends to each agent and stays
// under a limit of its own (wake_update.agent_requests_per_min, 8): a request that does not fit waits for a
// later step.

const paceWindow = time.Minute + 2*time.Second // the agent counts a sliding minute; two seconds for the way there

type pacer struct {
	limit int
	sent  map[string][]time.Time // agent id -> when the run's requests of the last minute were sent
}

func newPacer(limit int) *pacer {
	if limit < 1 {
		limit = 1
	}
	return &pacer{limit: limit, sent: map[string][]time.Time{}}
}

// used drops what is older than the window and returns how many requests are left in it.
func (p *pacer) used(agent string, now time.Time) int {
	l := p.sent[agent]
	i := 0
	for i < len(l) && now.Sub(l[i]) >= paceWindow {
		i++
	}
	l = l[i:]
	if len(l) == 0 {
		delete(p.sent, agent)
	} else {
		p.sent[agent] = l
	}
	return len(l)
}

// fits reports whether n more requests to agent stay under the limit now. A single action of more requests
// than the whole limit (a machine with many network cards and a small limit) fits when nothing else was sent
// in the window.
func (p *pacer) fits(agent string, n int, now time.Time) bool {
	u := p.used(agent, now)
	return u+n <= p.limit || u == 0
}

// take counts n requests to agent if they fit, and reports whether they did.
func (p *pacer) take(agent string, n int, now time.Time) bool {
	if !p.fits(agent, n, now) {
		return false
	}
	for i := 0; i < n; i++ {
		p.sent[agent] = append(p.sent[agent], now)
	}
	return true
}
