package poller

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/scheduler"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

func mustAgent(t *testing.T, p *Poller, id string) config.Agent {
	t.Helper()
	a, ok := p.AgentByID(id)
	if !ok {
		t.Fatal("no agent " + id)
	}
	return a
}

// offNow is what the Updates page and a run ask of the poller's view: "off" is offline with nothing at all for
// wakerun.MinOff.
func offNow(p *Poller, id string, now time.Time) (bool, AgentInfo) {
	info, sys, _ := p.AgentSeen(id)
	o := wakerun.Observation{Online: info.Online, SeenAt: info.SeenAt, QuietSince: info.QuietSince, Status: sys.Status}
	if info.Sign != "" {
		o.OnBy = info.Sign
	}
	return wakerun.OffNow(o, now), info
}

// r1-p2-03 (audit 5), built from the auditor's proof. "Off" is 15 minutes in which the panel got nothing at
// all from the machine. The poller counted every failed poll that was not a refused connection as nothing at
// all: also an HTTP 401 or 500, an answer that is not JSON, a certificate it does not accept, and a
// connection the machine took and then did not answer in time. Each of those is sent by a machine that runs.
// After 15 minutes of them the panel offered the machine as "Off", ticked, and for the hanging agent the
// run's direct question said "timeout", which a run takes for "nothing answered": the wake went out.
//
// Now each of them is a sign of life: it starts the 15 minutes again like an answer, the machine shows as on,
// and the direct question never says "timeout" for it.
func TestAnAnswerFromARunningMachineIsNeverSilence(t *testing.T) {
	for _, mode := range []string{"401", "500", "junk", "hang", "tls", "closes"} {
		t.Run(mode, func(t *testing.T) {
			var hits atomic.Int32
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				switch mode {
				case "401":
					w.WriteHeader(401)
				case "500":
					w.WriteHeader(500)
				case "junk":
					_, _ = w.Write([]byte("<html>not an agent</html>"))
				case "hang": // the machine takes the connection and the request, and its agent does not answer in time
					select {
					case <-r.Context().Done():
					case <-time.After(3 * time.Second):
					}
				}
			})
			var url string
			switch mode {
			case "tls": // something that speaks TLS with a certificate the panel does not know
				srv := httptest.NewUnstartedServer(h)
				srv.TLS = &tls.Config{}
				srv.Config.ErrorLog = log.New(io.Discard, "", 0) // "bad certificate", once per poll
				srv.StartTLS()
				defer srv.Close()
				url = srv.URL
			case "closes": // takes the connection and closes it without a word
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer ln.Close()
				go func() {
					for {
						c, err := ln.Accept()
						if err != nil {
							return
						}
						hits.Add(1)
						c.Close()
					}
				}()
				url = "http://" + ln.Addr().String()
			default:
				srv := httptest.NewServer(h)
				defer srv.Close()
				defer srv.CloseClientConnections()
				url = srv.URL
			}
			cfg := testutil.Config(t, []string{deadURL(t)}, url, deadURL(t))
			cfg.RequestTimeout = 0.25
			cfg.Intervals.Agent = 60                  // a poll a minute, as this test polls
			p, err := New(cfg, config.Secrets{}, nil) // the real HTTP backend
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC)
			p.Clock = func() time.Time { return now }
			ctx := context.Background()
			for i := 0; i < 17; i++ { // seventeen minutes, a poll a minute
				if i > 0 {
					now = now.Add(time.Minute)
				}
				p.pollAgent(ctx, mustAgent(t, p, "laptop"), 0)
				off, info := offNow(p, "laptop", now)
				if off || !info.QuietSince.IsZero() || info.Sign == "" || info.Online {
					t.Fatalf("poll %d: off=%v, quiet since %v, sign %q", i+1, off, info.QuietSince, info.Sign)
				}
			}
			_, info := offNow(p, "laptop", now)
			probe := p.ProbeAgent(ctx, "laptop")
			t.Logf("the machine took %d requests; sign: %q; the direct question says %q", hits.Load(), info.Sign, probe)
			if probe == "" || probe == "timeout" || probe == "connection failed" {
				t.Fatalf("the direct question must not say that nothing answered: %q", probe)
			}
			if hits.Load() == 0 && mode != "tls" { // the handshake ends before a request
				t.Fatal("the stand-in was never asked")
			}
			wantSign := map[string]string{"401": "its address answered: http 401", "500": "its address answered: http 500",
				"junk": "its address answered: invalid response", "hang": "its address took the connection and gave no answer: timeout",
				"tls": "its address answered: tls verification failed", "closes": SignRefused}[mode]
			if info.Sign != wantSign || info.Refused != (mode == "closes") {
				t.Fatalf("sign %q, want %q (refused %v)", info.Sign, wantSign, info.Refused)
			}
			if mode == "hang" && !strings.HasPrefix(probe, ProbeReached) {
				t.Fatalf("a hanging agent: the direct question says %q", probe)
			}
			// and then the machine really goes: the silence, and the 15 minutes, begin with the first poll that
			// gets nothing at all
			p.Backend = silentBackend{p.Backend}
			now = now.Add(time.Minute)
			p.pollAgent(ctx, mustAgent(t, p, "laptop"), 0)
			start := now
			for i := 0; i < 14; i++ {
				now = now.Add(time.Minute)
				p.pollAgent(ctx, mustAgent(t, p, "laptop"), 0)
			}
			if off, info := offNow(p, "laptop", now); off || !info.QuietSince.Equal(start) || info.Sign != "" {
				t.Fatalf("14 minutes after the last sign of life: off=%v %+v", off, info)
			}
			now = now.Add(time.Minute)
			p.pollAgent(ctx, mustAgent(t, p, "laptop"), 0)
			if off, info := offNow(p, "laptop", now); !off {
				t.Fatalf("15 minutes of nothing at all after the last sign of life is off: %+v", info)
			}
		})
	}
}

// silentBackend answers every poll of an agent like the address of a machine without power: the attempt to
// connect gets nothing and times out.
type silentBackend struct{ Backend }

func (silentBackend) FetchAgentMetrics(context.Context, config.Agent, time.Duration) (map[string]any, error) {
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}
}

// What counts as a sign of life, and what does not, by the error alone.
func TestLifeSign(t *testing.T) {
	dialTimeout := &net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}
	for name, c := range map[string]struct {
		err  error
		sign string
	}{
		"no error":                         {nil, ""},
		"no answer to the connect":         {dialTimeout, ""},
		"a timeout by its word":            {devices.Errf("timeout"), ""},
		"connection failed by its word":    {devices.Errf("connection failed"), ""},
		"no route":                         {&net.OpError{Op: "dial", Net: "tcp", Err: &net.AddrError{Err: "no route to host"}}, ""},
		"a name that does not resolve":     {&net.DNSError{Err: "no such host", Name: "pc.invalid", IsNotFound: true}, ""},
		"a status":                         {&devices.HTTPStatusError{Code: 503}, "its address answered: http 503"},
		"not JSON":                         {devices.Errf("invalid response"), "its address answered: invalid response"},
		"connected, then a timeout":        {&reachedError{err: dialTimeout}, "its address took the connection and gave no answer: timeout"},
		"first byte, then a timeout":       {&reachedError{err: dialTimeout, answered: true}, "its address answered: timeout"},
		"connected, then the status":       {&reachedError{err: &devices.HTTPStatusError{Code: 401}, answered: true}, "its address answered: http 401"},
		"an old connection, then nothing":  {context.DeadlineExceeded, ""},
		"the panel's own url is not valid": {devices.Errf("invalid url"), ""},
	} {
		if got := lifeSign(c.err); got != c.sign {
			t.Errorf("%s: %q, want %q", name, got, c.sign)
		}
	}
}

// r1-p2-04 (audit 5), built from the auditor's proof. The 15 minutes were measured with the time of day, and
// a pause of up to six minutes between two failed polls was kept in the row, for every agent, also one that
// is polled every second. Four failed polls, eight seconds of real time, and the clock set forward by just
// under six minutes three times made "no answer for 17 minutes 57 seconds": off.
//
// Now the silence is measured with the steady clock, which a clock that is set does not move, and a pause
// longer than the agent's own poll interval allows starts the row again.
func TestFifteenMinutesAreNeverMadeOfUnwatchedTime(t *testing.T) {
	type world struct {
		p    *Poller
		b    *testutil.Backend
		now  time.Time     // the time of day (can be set)
		mono time.Duration // the steady clock (cannot)
	}
	start := time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC)
	newWorld := func(t *testing.T, interval float64) *world {
		w := &world{b: testutil.NewBackend(), now: start}
		w.b.Metrics["laptop"] = func() (map[string]any, error) { return nil, devices.Errf("timeout") }
		cfg := testutil.Config(t, []string{"http://pve.invalid"}, "http://laptop.invalid", "http://tower.invalid")
		cfg.Intervals.Agent = interval
		p, err := New(cfg, config.Secrets{}, w.b)
		if err != nil {
			t.Fatal(err)
		}
		p.Clock = func() time.Time { return w.now }
		p.Mono = func() time.Duration { return w.mono }
		w.p = p
		return w
	}
	pass := func(w *world, d time.Duration) { w.now, w.mono = w.now.Add(d), w.mono+d } // time really passes
	poll := func(w *world) { w.p.pollAgent(context.Background(), mustAgent(t, w.p, "laptop"), 0) }
	quiet := func(w *world) time.Duration {
		info, sys, _ := w.p.AgentSeen("laptop")
		return wakerun.QuietFor(wakerun.Observation{QuietSince: info.QuietSince, Status: sys.Status}, w.now)
	}
	off := func(w *world) bool { o, _ := offNow(w.p, "laptop", w.now); return o }

	t.Run("the auditor's proof: three steps of the clock, eight seconds of polls", func(t *testing.T) {
		w := newWorld(t, 1)
		for i := 0; i < 4; i++ {
			pass(w, 2*time.Second)
			poll(w)
			if i < 3 {
				w.now = w.now.Add(6*time.Minute - 3*time.Second) // the clock is set forward; no time passes
			}
		}
		if off(w) || quiet(w) > 10*time.Second {
			t.Fatalf("watched for 8 s: quiet for %v, off=%v", quiet(w), off(w))
		}
	})
	t.Run("a step of the clock inside the gap adds no silence", func(t *testing.T) {
		w := newWorld(t, 60)
		poll(w)
		for i := 0; i < 9; i++ { // nine minutes of real silence, a poll a minute
			pass(w, time.Minute)
			poll(w)
		}
		if q := quiet(w); q != 9*time.Minute {
			t.Fatalf("nine minutes: %v", q)
		}
		for i := 0; i < 8; i++ { // eight steps of 70 s: each inside the gap for this interval (76.5 s)
			w.now = w.now.Add(70 * time.Second)
			pass(w, time.Second)
			poll(w)
		}
		if q := quiet(w); off(w) || q != 9*time.Minute+8*time.Second {
			t.Fatalf("the clock was set forward by 9 min 20 s in all, 8 s passed: quiet for %v, off=%v", q, off(w))
		}
		for i := 0; i < 6; i++ {
			pass(w, time.Minute)
			poll(w)
		}
		if q := quiet(w); !off(w) || q != 15*time.Minute+8*time.Second {
			t.Fatalf("fifteen real minutes of silence are off: quiet for %v, off=%v", q, off(w))
		}
	})
	t.Run("a step of the clock larger than the gap starts the count again", func(t *testing.T) {
		w := newWorld(t, 60)
		poll(w)
		for i := 0; i < 14; i++ {
			pass(w, time.Minute)
			poll(w)
		}
		w.now = w.now.Add(2 * time.Minute)
		if q := quiet(w); q != 0 {
			t.Fatalf("right after the step, before the next poll, the panel does not say how long: %v", q)
		}
		pass(w, time.Second)
		poll(w)
		if q := quiet(w); off(w) || q != 0 {
			t.Fatalf("after the step: quiet for %v, off=%v", q, off(w))
		}
	})
	t.Run("the clock set back", func(t *testing.T) {
		w := newWorld(t, 60)
		poll(w)
		for i := 0; i < 16; i++ {
			pass(w, time.Minute)
			poll(w)
		}
		if !off(w) {
			t.Fatal("sixteen minutes of silence are off")
		}
		w.now = w.now.Add(-time.Hour)
		pass(w, time.Second)
		poll(w)
		if q := quiet(w); off(w) || q != 0 {
			t.Fatalf("after the clock went back: quiet for %v, off=%v", q, off(w))
		}
	})
	t.Run("the panel's host slept: the steady clock stood still, the time of day did not", func(t *testing.T) {
		w := newWorld(t, 60)
		poll(w)
		for i := 0; i < 10; i++ {
			pass(w, time.Minute)
			poll(w)
		}
		w.now = w.now.Add(time.Hour) // an hour nobody watched; the machine may have been on in it
		for i := 0; i < 6; i++ {
			pass(w, time.Minute)
			poll(w)
		}
		if q := quiet(w); off(w) || q != 5*time.Minute {
			t.Fatalf("ten minutes, an hour asleep, six minutes: quiet for %v, off=%v", q, off(w))
		}
	})
	t.Run("a pause longer than the poll interval allows, at every interval", func(t *testing.T) {
		for _, interval := range []float64{1, 5, 60, 300} {
			w := newWorld(t, interval)
			gap := w.p.quietGap(interval)
			if max := time.Duration(interval)*time.Second + 45*time.Second; gap < time.Duration(interval)*time.Second || gap > max {
				t.Fatalf("every %v s: the gap is %v, want between the interval and %v", interval, gap, max)
			}
			poll(w)
			for w.mono < 14*time.Minute { // watched without a pause: one row
				pass(w, gap)
				poll(w)
			}
			if q := quiet(w); q < 14*time.Minute {
				t.Fatalf("every %v s: polls %v apart are one row: quiet for %v", interval, gap, q)
			}
			pass(w, gap+time.Second) // one pause too long (polling was paused, the panel hung)
			if q := quiet(w); q != 0 {
				t.Fatalf("every %v s: no poll for %v: the panel is not watching, quiet for %v", interval, gap+time.Second, q)
			}
			poll(w)
			pass(w, time.Minute)
			if off(w) {
				t.Fatalf("every %v s: off after an unwatched pause", interval)
			}
		}
	})
	t.Run("the interval changes between two polls", func(t *testing.T) {
		w := newWorld(t, 300)
		poll(w)
		pass(w, 5*time.Minute)
		poll(w)
		// someone opens the machine's page: it is polled every second from now on. The poll before that was up
		// to five minutes ago, as its interval then allowed: still one row.
		if err := w.p.Scheduler.SetOverride("laptop", &scheduler.Override{Seconds: 1}); err != nil {
			t.Fatal(err)
		}
		pass(w, 4*time.Minute)
		poll(w)
		if q := quiet(w); q != 9*time.Minute {
			t.Fatalf("after the interval became shorter: quiet for %v", q)
		}
		pass(w, 10*time.Second)
		poll(w)
		pass(w, 40*time.Second) // at one poll a second, 40 s without a poll is not watching
		if q := quiet(w); q != 0 {
			t.Fatalf("40 s without a poll at one poll a second: quiet for %v", q)
		}
	})
}

// With the real clock (no test clock at all) the silence is steady time: Go's own steady reading.
func TestTheSteadyClockIsGoSOwnByDefault(t *testing.T) {
	p, err := New(testutil.Config(t, []string{"http://pve.invalid"}, "http://laptop.invalid", "http://tower.invalid"), config.Secrets{}, testutil.NewBackend())
	if err != nil {
		t.Fatal(err)
	}
	a := p.mono()
	time.Sleep(20 * time.Millisecond)
	if d := p.mono() - a; d < 20*time.Millisecond || d > 5*time.Second {
		t.Fatalf("20 ms later the steady clock says %v", d)
	}
	wall := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	p.Clock = func() time.Time { return wall } // a test's clock has no steady reading: the steady clock follows it
	a = p.mono()
	wall = wall.Add(time.Minute)
	if d := p.mono() - a; d != time.Minute {
		t.Fatalf("with a test clock: %v", d)
	}
}
