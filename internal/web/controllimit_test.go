package web

import (
	"fmt"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// r2-b1-10 (bug run of audit 5): the limit on control requests (6 a minute by default) is met by ordinary use,
// and its refusal said "too many control requests; slow down": not for how long, and the audit log had no line
// for it. The refusal now names the seconds until the next request is taken, in its text and in Retry-After,
// and is written to the audit log like every other refusal.
func TestTheControlLimitSaysHowLongToWait(t *testing.T) {
	e := setup(t)
	c := e.cookie(controller(), 0)
	hdr := []string{"Cookie", c, "X-Requested-With", "fairy"}
	limit := e.s.Cfg.ControlRatePerMin
	var r resp
	n := 0
	for ; n < 40; n++ {
		if r = e.do("POST", "/api/systems/laptop/fan/mode", map[string]any{"mode": "full"}, hdr...); r.code == 429 {
			break
		}
	}
	if r.code != 429 || n != limit {
		t.Fatalf("request %d was answered %d; the limit is %d a minute", n+1, r.code, limit)
	}
	m := regexp.MustCompile(`^too many control requests; try again in (\d+) s$`).FindStringSubmatch(fmt.Sprint(r.body["detail"]))
	if m == nil {
		t.Fatalf("the refusal does not say how long to wait: %s", r.raw)
	}
	secs, _ := strconv.Atoi(m[1])
	if secs < 55 || secs > 60 || r.hdr.Get("Retry-After") != m[1] {
		t.Fatalf("try again in %d s, Retry-After %q: six requests just now leave the first one 55 to 60 s of its minute", secs, r.hdr.Get("Retry-After"))
	}
	// the refusal is in the audit log, as a refusal (and so throttled like the others)
	var line map[string]any
	for _, en := range e.auditEntries("", "auth.denied") {
		if en["outcome"] == "429: too many control requests" {
			line = en
		}
	}
	if line == nil || line["target"] != "/api/systems/laptop/fan/mode" {
		t.Fatalf("no audit line for the refused request: %v", e.auditEntries("", "auth.denied"))
	}
	// the request after it is refused too, and tells the same or a shorter time
	r2 := e.do("POST", "/api/systems/laptop/fan/mode", map[string]any{"mode": "full"}, hdr...)
	if again, _ := strconv.Atoi(r2.hdr.Get("Retry-After")); r2.code != 429 || again < 1 || again > secs {
		t.Fatalf("the next request: %d, Retry-After %q", r2.code, r2.hdr.Get("Retry-After"))
	}
}

// wait is the time until the oldest hit that still counts is a minute old; it records nothing.
func TestRateLimiterWait(t *testing.T) {
	l := newRateLimiter(3)
	if l.wait("u") != 0 {
		t.Fatal("an unused limiter makes nobody wait")
	}
	now := time.Now()
	l.hits["u"] = []time.Time{now.Add(-90 * time.Second), now.Add(-50 * time.Second), now.Add(-20 * time.Second)}
	if w := l.wait("u"); w != 0 { // one of the three is older than a minute: there is room
		t.Fatalf("two hits in the last minute of three allowed: wait %d", w)
	}
	l.hits["u"] = append(l.hits["u"], now.Add(-5*time.Second))
	if w := l.wait("u"); w < 10 || w > 11 { // the hit of 50 s ago leaves in 10 s
		t.Fatalf("wait %d s, want 10 or 11", w)
	}
	if l.allow("u") || len(l.hits["u"]) != 3 {
		t.Fatalf("the limiter must still refuse, and wait must not have recorded a hit: %v", l.hits["u"])
	}
	if l.wait("other") != 0 {
		t.Fatal("another user does not wait")
	}
}
