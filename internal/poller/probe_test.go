package poller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// ProbeAgent asks the agent itself, now, with the real HTTP backend: it tells "answered", "the machine refused
// the connection" and "something answered that is not usable" apart, and leaves the poller's own view as it was. AgentSeen
// hands out the agent's entry and the time of the poll it is from together.
func TestProbeAgentAsksTheAgentItself(t *testing.T) {
	mode := atomic.Value{}
	mode.Store("ok")
	var hits atomic.Int32
	laptop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch mode.Load() {
		case "401":
			w.WriteHeader(401)
		case "junk":
			_, _ = w.Write([]byte("<html>not an agent</html>"))
		default:
			_, _ = w.Write([]byte(`{"hostname":"laptop","uptime_s":42,"agent_version":"16-abcdef0"}`))
		}
	}))
	defer laptop.Close()
	cfg := testutil.Config(t, []string{deadURL(t)}, laptop.URL, deadURL(t))
	p, err := New(cfg, config.Secrets{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if got := p.ProbeAgent(ctx, "laptop"); got != "" {
		t.Fatalf("an agent that answers: %q", got)
	}
	// Nothing listens at tower's port, but the machine with that address runs (it is this one) and refuses
	// the connection: that is not "nothing answered".
	if got := p.ProbeAgent(ctx, "tower"); got != ProbeRefused {
		t.Fatalf("a machine that is on and where nothing listens: %q, want %q", got, ProbeRefused)
	}
	if got := p.ProbeAgent(ctx, "nobody"); got == "" {
		t.Fatal("an agent the panel does not know cannot be on")
	}
	// the probe did not teach the poller anything
	if _, _, known := p.AgentSeen("laptop"); known {
		t.Fatal("a probe must not change the poller's view")
	}
	p.PollOnce(ctx)
	info, sys, known := p.AgentSeen("laptop")
	if !known || !info.Online || sys.UptimeS == nil || *sys.UptimeS != 42 || sys.LastSeen == nil || !sys.LastSeen.Equal(info.SeenAt) {
		t.Fatalf("after a poll: %+v uptime %v", info, sys.UptimeS)
	}
	if info, sys, known := p.AgentSeen("tower"); !known || info.Online || sys.Status != "offline" {
		t.Fatalf("an agent that never answered: %+v %s %v", info, sys.Status, known)
	}
	// something answers that is not the agent: neither "on" nor "off"
	for m, want := range map[string]string{"401": "http 401", "junk": "invalid response"} {
		mode.Store(m)
		if got := p.ProbeAgent(ctx, "laptop"); got != want {
			t.Errorf("%s: %q, want %q", m, got, want)
		}
	}
	// the poller still says what its last poll said: the probe is how a caller learns about now
	if info, _, _ := p.AgentSeen("laptop"); !info.Online {
		t.Fatal("the poller's view changed without a poll")
	}
	laptop.Close()
	if got := p.ProbeAgent(ctx, "laptop"); got != ProbeRefused {
		t.Fatalf("the agent is gone, its machine is not: %q, want %q", got, ProbeRefused)
	}
}

// What a failed request says about the machine. Refused, reset, closed without an answer: the machine sent
// that, so it is on. No answer at all (a timeout, no route to it, a name that does not resolve) says nothing.
func TestProbeTellsARefusalFromNoAnswer(t *testing.T) {
	dial := func(inner error) error {
		return &url.Error{Op: "Get", URL: "http://pc", Err: &net.OpError{Op: "dial", Net: "tcp", Err: inner}}
	}
	sys := func(e syscall.Errno) error { return os.NewSyscallError("connect", e) }
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"refused":                 {dial(sys(syscall.ECONNREFUSED)), ProbeRefused},
		"reset":                   {&url.Error{Op: "Get", URL: "http://pc", Err: &net.OpError{Op: "read", Net: "tcp", Err: sys(syscall.ECONNRESET)}}, ProbeRefused},
		"closed before an answer": {&url.Error{Op: "Get", URL: "http://pc", Err: io.EOF}, ProbeRefused},
		"closed in the answer":    {fmt.Errorf("reading: %w", io.ErrUnexpectedEOF), ProbeRefused},
		"no route to the host":    {dial(sys(syscall.EHOSTUNREACH)), "connection failed"},
		"network unreachable":     {dial(sys(syscall.ENETUNREACH)), "connection failed"},
		"the name is unknown":     {dial(&net.DNSError{Err: "no such host", Name: "pc", IsNotFound: true}), "connection failed"},
		"timeout":                 {dial(os.ErrDeadlineExceeded), "timeout"},
		"the deadline":            {fmt.Errorf("get: %w", context.DeadlineExceeded), "timeout"},
		"the test backend's off":  {devices.Errf("connection failed"), "connection failed"},
		"http 401":                {&devices.HTTPStatusError{Code: 401}, "http 401"},
	} {
		b := testutil.NewBackend()
		b.Metrics["laptop"] = func() (jsonx.M, error) { return nil, c.err }
		p, err := New(testutil.Config(t, []string{"http://pve.invalid"}, "http://laptop.invalid", "http://tower.invalid"), config.Secrets{}, b)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.ProbeAgent(context.Background(), "laptop"); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
		if c.want == ProbeRefused != refused(c.err) {
			t.Errorf("%s: refused() = %v", name, refused(c.err))
		}
	}
	if refused(nil) || refused(errors.New("connection refused")) {
		t.Error("only the error's kind counts, never its text")
	}
}

// QuietSince: since when the panel has asked an agent poll after poll without an answer. An answer ends the
// row. So does a time in which the panel did not ask: after it the row starts again, because nobody knows
// what the machine did meanwhile.
func TestQuietSinceIsAnUnbrokenRowOfFailedPolls(t *testing.T) {
	b := testutil.NewBackend()
	down := true
	b.Metrics["laptop"] = func() (jsonx.M, error) {
		if down {
			return nil, devices.Errf("connection failed")
		}
		return testutil.AgentMetrics(), nil
	}
	p, err := New(testutil.Config(t, []string{"http://pve.invalid"}, "http://laptop.invalid", "http://tower.invalid"), config.Secrets{}, b)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC)
	p.Clock = func() time.Time { return now }
	ctx := context.Background()
	quiet := func() time.Time {
		i, _, _ := p.AgentSeen("laptop")
		return i.QuietSince
	}
	set := func(v bool) {
		b.Mu.Lock()
		down = v
		b.Mu.Unlock()
	}
	if !quiet().IsZero() {
		t.Fatal("an agent that was never polled is not quiet: nothing is known about it")
	}
	p.PollOnce(ctx)
	start := now
	if !quiet().Equal(start) {
		t.Fatalf("after the first failed poll: %v, want %v", quiet(), start)
	}
	for i := 0; i < 20; i++ { // twenty minutes, a poll a minute
		now = now.Add(time.Minute)
		p.PollOnce(ctx)
	}
	if !quiet().Equal(start) {
		t.Fatalf("a row of failed polls keeps its beginning: %v, want %v", quiet(), start)
	}
	if i, ok := p.AgentInfo("laptop"); ok || !i.QuietSince.Equal(start) || i.Refused {
		t.Fatalf("AgentInfo: %+v %v", i, ok)
	}
	// a poll the machine itself refuses: it runs, its agent does not listen. The row goes on.
	b.Mu.Lock()
	b.Metrics["laptop"] = func() (jsonx.M, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	}
	b.Mu.Unlock()
	now = now.Add(time.Minute)
	p.PollOnce(ctx)
	if i, _, _ := p.AgentSeen("laptop"); !i.Refused || !i.QuietSince.Equal(start) || i.Online {
		t.Fatalf("a refused poll: %+v", i)
	}
	b.Mu.Lock()
	b.Metrics["laptop"] = func() (jsonx.M, error) {
		if down {
			return nil, devices.Errf("connection failed")
		}
		return testutil.AgentMetrics(), nil
	}
	b.Mu.Unlock()
	now = now.Add(time.Minute)
	p.PollOnce(ctx)
	if i, _, _ := p.AgentSeen("laptop"); i.Refused || !i.QuietSince.Equal(start) {
		t.Fatalf("the next poll got no answer at all: %+v", i)
	}
	// the slowest poll interval is inside the gap
	now = now.Add(QuietGap)
	p.PollOnce(ctx)
	if !quiet().Equal(start) {
		t.Fatalf("a poll after %v is still the same row: %v", QuietGap, quiet())
	}
	// the panel did not ask for longer than that (polling paused, the host asleep, the clock set forward)
	now = now.Add(QuietGap + time.Second)
	if !quiet().IsZero() {
		t.Fatal("no poll for longer than the gap: the panel is not watching, so it must not say when the quiet began")
	}
	p.PollOnce(ctx)
	if !quiet().Equal(now) {
		t.Fatalf("after a gap the row starts again: %v, want %v", quiet(), now)
	}
	// the clock set back
	now = now.Add(-time.Hour)
	if !quiet().IsZero() {
		t.Fatal("a clock that went back: not watching")
	}
	p.PollOnce(ctx)
	if !quiet().Equal(now) {
		t.Fatalf("after the clock went back the row starts again: %v, want %v", quiet(), now)
	}
	// an answer ends the row; the next failure starts a new one
	set(false)
	now = now.Add(time.Minute)
	p.PollOnce(ctx)
	if i, _, _ := p.AgentSeen("laptop"); !i.Online || !i.QuietSince.IsZero() {
		t.Fatalf("an agent that answers is not quiet: %+v", i)
	}
	set(true)
	now = now.Add(time.Minute)
	p.PollOnce(ctx)
	if !quiet().Equal(now) {
		t.Fatalf("a new row: %v, want %v", quiet(), now)
	}
}
