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
