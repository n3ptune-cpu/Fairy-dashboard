package poller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Second review of the fixes for audit 5, round 2 (note N3), the reviewer's test with what it must show: what
// the real HTTP backend gives for a control request that is answered with an error status. With the agent's own
// JSON: the status and the body, no error. With a body that is not the agent's (a proxy's error page): the
// status and an error that is not NotSent. powerctl reads the status and takes both for a refusal; it took the
// second for "no answer: the request may have arrived".
func TestAControlRequestAnsweredWithAnErrorStatus(t *testing.T) {
	page := func(status int) *httptest.Server { // something in front of the agent (a reverse proxy) answers for it
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(status)
			_, _ = w.Write([]byte("<html><body><h1>" + http.StatusText(status) + "</h1>nginx</body></html>"))
		}))
	}
	own := func(status int, body string) *httptest.Server { // the agent itself
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
	}
	for _, c := range []struct {
		name    string
		srv     *httptest.Server
		status  int
		wantErr bool
	}{
		{"a proxy's HTML 502 page (the agent behind it is down)", page(502), 502, true},
		{"a proxy's HTML 504 page", page(504), 504, true},
		{"an HTML 404 page (another web server at the agent's address)", page(404), 404, true},
		{"an HTML 403 page", page(403), 403, true},
		{"the agent's own JSON 500", own(500, `{"error":"no"}`), 500, false},
		{"the agent's own JSON 404", own(404, `{"error":"no"}`), 404, false},
		{"an error status without a body", own(503, ``), 503, false},
		{"200 with a body that cannot be read: the request may have been carried out", own(200, `<html>ok</html>`), 200, true},
	} {
		cfg := testutil.Config(t, []string{deadURL(t)}, c.srv.URL, deadURL(t))
		cfg.RequestTimeout = 0.5
		p, err := New(cfg, config.Secrets{}, nil) // the real HTTP backend
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		status, _, err := p.AgentRequest(ctx, mustAgent(t, p, "laptop"), "POST", "/wol", map[string]any{"mac": "00:00:5e:00:53:01"})
		cancel()
		var ns *NotSent
		if status != c.status || (err != nil) != c.wantErr || errors.As(err, &ns) {
			t.Errorf("%s: status %d, error %v (never left: %v); want status %d, an error: %v", c.name, status, err, errors.As(err, &ns), c.status, c.wantErr)
		}
		c.srv.Close()
	}
}
