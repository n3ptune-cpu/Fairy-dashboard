package web

import (
	"sync"
	"time"
)

// Unauthenticated clients can make the panel write audit entries (a rejected API token, a failed or
// rate-limited sign-in). So that nobody can flood the audit log, such entries are throttled: per client IP
// the first anonPerIP per minute are written, and in all at most anonTotal per minute; the rest are counted
// and written as one "audit.suppressed" summary entry per minute.
const (
	anonPerIP = 10
	anonTotal = 300
	anonKeys  = 10000
)

type auditThrottle struct {
	mu         sync.Mutex
	window     time.Time      // start of the current minute
	perIP      map[string]int // entries written this minute, by IP
	total      int            // entries written this minute
	suppressed int            // entries dropped this minute
	fromIPs    map[string]struct{}
}

func newAuditThrottle() *auditThrottle {
	return &auditThrottle{window: time.Now(), perIP: map[string]int{}, fromIPs: map[string]struct{}{}}
}

// allow reports whether an anonymous entry from ip may be written now. When a minute has passed it also
// returns the previous minute's summary (dropped entries and distinct IPs) to write first.
func (t *auditThrottle) allow(ip string, now time.Time) (ok bool, dropped, ips int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	dropped, ips = t.rollLocked(now)
	if t.perIP[ip] >= anonPerIP || t.total >= anonTotal {
		t.suppressed++
		if len(t.fromIPs) < anonKeys {
			t.fromIPs[ip] = struct{}{}
		}
		return false, dropped, ips
	}
	if len(t.perIP) < anonKeys {
		t.perIP[ip]++
	} else {
		t.total = anonTotal // too many addresses: everything else this minute is only counted
	}
	t.total++
	return true, dropped, ips
}

// flush returns the summary of a finished minute (if any), for the periodic writer.
func (t *auditThrottle) flush(now time.Time) (dropped, ips int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rollLocked(now)
}

func (t *auditThrottle) rollLocked(now time.Time) (dropped, ips int) {
	if now.Sub(t.window) < time.Minute {
		return 0, 0
	}
	dropped, ips = t.suppressed, len(t.fromIPs)
	t.window, t.total, t.suppressed = now, 0, 0
	clear(t.perIP)
	clear(t.fromIPs)
	return dropped, ips
}
