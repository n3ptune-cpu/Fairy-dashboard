package wakerun

import (
	"math"
	"strings"
	"testing"
	"time"
)

// r2-p2-08 (audit 5, round 2): an uptime between about 9.3e9 and 1e10 seconds (the panel keeps what an agent
// reports up to 1e10) overflowed "uptime x one second" as a time.Duration and came out negative, which is
// "shorter than the time since the wake" in all three comparisons: a machine that was on all along and out of
// reach, with a broken or hostile agent, looked freshly started and was switched off. An uptime is now bounded
// before any arithmetic: negative, or longer than fifty years, is "no usable uptime", and such a machine is
// never switched off.

// provenWith is the whole proof for a machine that answered 50 s after the wake and reported this uptime.
func provenWith(uptime int64) (*Host, HostInfo, Observation) {
	wake := t0.Add(10 * time.Second)
	online := wake.Add(50 * time.Second)
	q := int64(MinOff / time.Second)
	return &Host{ID: "pc-a", Mode: ModeWake, StartStatus: "offline", QuietForS: &q, ProbedOff: true, WakeSentAt: &wake, Wakes: 1, OnlineAt: &online, BootUptimeS: &uptime,
			wakeMono: 7 * time.Hour, onlineMono: 7*time.Hour + 51*time.Second},
		HostInfo{ID: "pc-a", OptedIn: true}, Observation{Online: true, Status: "online"}
}

func allowedWith(uptime int64) (bool, string) {
	h, info, o := provenWith(uptime)
	return mayShutDown(h, info, o, func() (int, bool, string) { return 0, true, "" })
}

func TestUptimeIsBoundedBeforeItIsUsed(t *testing.T) {
	for _, c := range []struct {
		uptime int64
		ok     bool
		why    string
	}{
		{0, true, ""},
		{1, true, ""},
		{49, true, ""},
		{50, false, "running for longer than since the wake"}, // exactly as long as since the wake
		{51, false, "running for longer than since the wake"},
		{3 * 86400, false, "running for longer than since the wake"},
		{maxUptimeS, false, "running for longer than since the wake"},
		{maxUptimeS + 1, false, "an uptime that cannot be right"},
		{9_223_372_036, false, "an uptime that cannot be right"},                // the last one that still fits a Duration
		{9_223_372_037, false, "an uptime that cannot be right"},                // the first one that overflows
		{9_500_000_000, false, "an uptime that cannot be right (9500000000 s)"}, // the auditor's
		{10_000_000_000, false, "an uptime that cannot be right"},
		{18_446_744_074, false, "an uptime that cannot be right"}, // wraps around to a small positive Duration
		{math.MaxInt64, false, "an uptime that cannot be right"},
		{-1, false, "an uptime that cannot be right (-1 s)"},
		{math.MinInt64, false, "an uptime that cannot be right"},
	} {
		ok, why := allowedWith(c.uptime)
		if ok != c.ok || !strings.Contains(why, c.why) {
			t.Errorf("uptime %d s: allowed=%v %q, want %v %q", c.uptime, ok, why, c.ok, c.why)
		}
		if usable := uptimeUsable(c.uptime); usable != (c.uptime >= 0 && c.uptime <= maxUptimeS) {
			t.Errorf("uptimeUsable(%d) = %v", c.uptime, usable)
		}
	}
	// every usable uptime fits a Duration with room to spare
	if d := time.Duration(maxUptimeS) * time.Second; d <= 0 || d.Hours() < 50*365*24-1 {
		t.Fatalf("maxUptimeS as a Duration: %v", d)
	}
}

// Whatever number an agent reports: the run may switch the machine off only when the number is an uptime
// shorter than the 50 s since the wake.
func FuzzUptimeNeverLooksFreshWhenItIsNot(f *testing.F) {
	for _, s := range []int64{0, 49, 50, -1, maxUptimeS, maxUptimeS + 1, 9_223_372_036, 9_223_372_037, 9_500_000_000, 18_446_744_074, math.MaxInt64, math.MinInt64} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, uptime int64) {
		if ok, why := allowedWith(uptime); ok != (uptime >= 0 && uptime < 50) {
			t.Fatalf("uptime %d s: allowed=%v (%s)", uptime, ok, why)
		}
	})
}

// The auditor's case with the test world: a machine that was on all along and out of the panel's reach comes
// back during the run's wait, and its agent reports an uptime of 9.5e9 s (301 years).
func TestAnAbsurdUptimeIsNotAFreshStart(t *testing.T) {
	for _, uptime := range []int64{9_500_000_000, -7, maxUptimeS + 1, 18_446_744_074} {
		for _, updateIfOn := range []bool{false, true} {
			w := newWorld(t)
			pc := w.running("pc-a")
			pc.unreachable = true
			w.silentFor(pc, 20*time.Minute)
			pc.uptimeSays = &uptime
			w.start(StartRequest{Hosts: []string{"pc-a"}, UpdateIfOn: updateIfOn})
			w.advance(30 * time.Second)
			pc.unreachable = false        // back in reach
			h := host(w.finish(), "pc-a") // the world's judge fails a shutdown request for a machine that was on
			if !pc.on || pc.shutdowns != 0 || h.On != OnYes || h.State == StOff {
				t.Fatalf("uptime %d: on=%v shutdowns=%d %s/%s: %s", uptime, pc.on, pc.shutdowns, h.State, h.On, h.Text)
			}
			if h.BootUptimeS == nil || *h.BootUptimeS != uptime || !logHas(h, "its agent reports an uptime that cannot be right") {
				t.Fatalf("uptime %d: the record: %v %v", uptime, h.BootUptimeS, h.Log)
			}
			if !strings.Contains(h.Text, "an uptime that cannot be right") {
				t.Fatalf("uptime %d: the report does not say why it was left on: %s", uptime, h.Text)
			}
		}
	}
}
