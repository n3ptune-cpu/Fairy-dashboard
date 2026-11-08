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
	if len(l.hits) > 10000 { // bound memory under a spray of source addresses
		l.hits = map[string][]time.Time{}
	}
	return true
}
