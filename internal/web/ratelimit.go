package web

import (
	"sync"
	"time"
)

type rateLimiter struct {
	perMin int
	mu     sync.Mutex
	hits   map[string][]time.Time
}

func newRateLimiter(perMin int) *rateLimiter {
	return &rateLimiter{perMin: max(perMin, 1), hits: map[string][]time.Time{}}
}

// allow records a hit and reports whether it is within the per-minute budget.
func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	q := l.hits[key]
	i := 0
	for i < len(q) && now.Sub(q[i]) > time.Minute {
		i++
	}
	q = q[i:]
	if len(q) >= l.perMin {
		l.hits[key] = q
		return false
	}
	l.hits[key] = append(q, now)
	if len(l.hits) > maxKeys { // bound memory under a spray of source addresses
		for k, q := range l.hits { // drop keys without a hit in the last minute first
			if len(q) == 0 || now.Sub(q[len(q)-1]) > time.Minute {
				delete(l.hits, k)
			}
		}
		if len(l.hits) > maxKeys {
			l.hits = map[string][]time.Time{key: l.hits[key]}
		}
	}
	return true
}

// wait is how many whole seconds from now the next hit of key will be within the budget (0: now): the time until
// the oldest hit that still counts is a minute old. It records nothing.
func (l *rateLimiter) wait(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	var live []time.Time
	for _, t := range l.hits[key] {
		if now.Sub(t) <= time.Minute {
			live = append(live, t)
		}
	}
	if len(live) < l.perMin {
		return 0
	}
	// hits are in the order they were made: the one that has to leave is the perMin-th from the end
	left := time.Minute - now.Sub(live[len(live)-l.perMin])
	return int(left/time.Second) + 1
}

// free reports whether a hit would be within the budget right now, without recording one.
func (l *rateLimiter) free(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now, n := time.Now(), 0
	for _, t := range l.hits[key] {
		if now.Sub(t) <= time.Minute {
			n++
		}
	}
	return n < l.perMin
}

// maxKeys bounds a limiter's memory.
const maxKeys = 10000
