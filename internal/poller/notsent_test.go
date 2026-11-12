package poller

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// A control request to an agent fails in one of two ways, and the backend says which: it never left the panel
// (no connection could be had: NotSent), or it left, or may have, and no usable answer came back. "Update
// unpowered hosts" must not take the second for "nothing happened" (audit 5, r2-p2-03, -04, -05).
func TestAgentRequestSaysWhetherTheRequestLeft(t *testing.T) {
	taken := make(chan struct{}, 4)
	over := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		taken <- struct{}{}
		<-over // the agent got the request; its answer never comes
	}))
	defer hang.Close()
	defer close(over)
	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		taken <- struct{}{}
		c, _, _ := w.(http.Hijacker).Hijack() // the agent got the request and the connection breaks
		_ = c.Close()
	}))
	defer cut.Close()
	l, err := net.Listen("tcp", "127.0.0.1:0") // a port nobody listens on: the connection is refused
	if err != nil {
		t.Fatal(err)
	}
	refused := "http://" + l.Addr().String()
	_ = l.Close()

	for _, c := range []struct {
		name, url string
		left      bool // the agent got the request
	}{
		{"the answer never comes (timeout)", hang.URL, true},
		{"the connection breaks after the request", cut.URL, true},
		{"the connection is refused", refused, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := testutil.Config(t, []string{deadURL(t)}, c.url, deadURL(t))
			cfg.RequestTimeout = 0.1
			p, err := New(cfg, config.Secrets{}, nil) // the real HTTP backend
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			status, _, err := p.AgentRequest(ctx, mustAgent(t, p, "laptop"), "POST", "/update", map[string]any{"requested_by": "x"})
			if err == nil {
				t.Fatalf("the request must fail (status %d)", status)
			}
			var ns *NotSent
			if got := !errors.As(err, &ns); got != c.left {
				t.Fatalf("\"may have left\" = %v, want %v (%v)", got, c.left, err)
			}
			select {
			case <-taken:
				if !c.left {
					t.Fatal("the agent got a request that is reported as never sent")
				}
			default:
				if c.left {
					t.Fatal("the agent never got the request")
				}
			}
		})
	}
}
