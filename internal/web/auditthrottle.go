package web

import (
	"sort"
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

// Signed-in users and API tokens can make the panel write entries too: every request the panel refuses
// (wrong role, missing CSRF header, kill switch) is one "auth.denied" line. Left alone, one account could push
// the earlier history out of the rotated log in minutes. So these are throttled per subject: the first
// deniedPerSub per minute are written, the rest are counted and written as one "audit.suppressed" entry per
// subject and minute, which names the subject. Entries about things a user did (control actions, changes to
// users and sites, sign-ins) are never throttled.
const (
	deniedPerSub = 10
	deniedKeys   = 1000 // subjects tracked per minute; any further ones share one budget (subject "")
)

// deniedCount is one subject's refused requests in the current minute.
type deniedCount struct {
	actor   string // how the audit log names the subject (for the summary)
	written int
	dropped int
}

// deniedSummary is what one subject's summary entry says about a finished minute.
type deniedSummary struct {
	Sub     string // "" = the subjects beyond deniedKeys together
	Actor   string
	Dropped int
}

type auditThrottle struct {
	mu         sync.Mutex
	window     time.Time      // start of the current minute
	perIP      map[string]int // entries written this minute, by IP
	total      int            // entries written this minute
	suppressed int            // entries dropped this minute
	fromIPs    map[string]struct{}

	denied     map[string]*deniedCount // refused requests of signed-in users this minute, by subject
	deniedDone []deniedSummary         // summaries of finished minutes that are not written yet
}

func newAuditThrottle() *auditThrottle {
	return &auditThrottle{window: time.Now(), perIP: map[string]int{}, fromIPs: map[string]struct{}{},
		denied: map[string]*deniedCount{}}
}

// allowDenied reports whether an "auth.denied" entry about the signed-in subject sub may be written now. Like
// allow it also returns the anonymous summary of a minute that just ended; the per-subject summaries of that
// minute wait in takeDenied.
func (t *auditThrottle) allowDenied(sub, actor string, now time.Time) (ok bool, dropped, ips int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	dropped, ips = t.rollLocked(now)
	d := t.denied[sub]
	if d == nil {
		if len(t.denied) >= deniedKeys { // too many subjects this minute: the rest share one budget
			sub, actor = "", "other accounts"
			d = t.denied[sub]
		}
		if d == nil {
			d = &deniedCount{actor: actor}
			t.denied[sub] = d
		}
	}
	if d.written >= deniedPerSub {
		d.dropped++
		return false, dropped, ips
	}
	d.written++
	return true, dropped, ips
}

// takeDenied returns the per-subject summaries of finished minutes, each once.
func (t *auditThrottle) takeDenied() []deniedSummary {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := t.deniedDone
	t.deniedDone = nil
	return out
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
	first := len(t.deniedDone)
	for sub, d := range t.denied {
		if d.dropped > 0 {
			t.deniedDone = append(t.deniedDone, deniedSummary{Sub: sub, Actor: d.actor, Dropped: d.dropped})
		}
	}
	done := t.deniedDone[first:]
	sort.Slice(done, func(i, j int) bool { return done[i].Sub < done[j].Sub })
	clear(t.denied)
	return dropped, ips
}
