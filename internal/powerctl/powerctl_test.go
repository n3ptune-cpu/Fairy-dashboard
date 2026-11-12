package powerctl

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

func testPoller(t *testing.T) (*poller.Poller, *testutil.Backend) {
	t.Helper()
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	b := testutil.NewBackend()
	p, err := poller.New(cfg, config.Secrets{}, b)
	if err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	return p, b
}

// An agent's refusal keeps the code the agent gave, next to the error the page shows. The page's error is what
// it was before (status and text); a caller that must tell refusals apart reads the code.
func TestAgentRefusalKeepsTheAgentsCode(t *testing.T) {
	p, b := testPoller(t)
	a, _ := p.AgentByID("laptop")
	for _, c := range []struct {
		status      int
		body        jsonx.M
		wantStatus  int
		wantCode    string
		wantInText  string
		wantRefusal bool
	}{
		{403, jsonx.M{"error": "approval required: confirm this action with Pocket ID", "code": "approval_required"}, 400, "approval_required", "approval required", true},
		{429, jsonx.M{"error": "too many control requests; try again in a minute"}, 400, "", "too many control requests", true},
		{503, jsonx.M{"error": "helper stopped", "code": "helper_stopped"}, 409, "helper_stopped", "elevated helper is stopped", true},
		{404, jsonx.M{"error": "self update is disabled on this agent", "code": "self_update_disabled"}, 409, "self_update_disabled", "disabled", true},
		{500, jsonx.M{"error": "boom", "code": "Not A Code <b>"}, apierr.Upstream, "", "boom", true}, // not a code: not repeated
		{401, jsonx.M{"error": "unauthorized"}, apierr.Upstream, "", "control token", false},
	} {
		b.Reply = func(string, string) (int, jsonx.M, error) { return c.status, c.body, nil }
		_, err := UpdateAgent(context.Background(), p, a, "Ann")
		var ae *apierr.Error
		if !errors.As(err, &ae) || ae.Status != c.wantStatus || !strings.Contains(ae.Detail.(string), c.wantInText) {
			t.Errorf("agent %d: the page's error is %v", c.status, err)
			continue
		}
		var ref *AgentRefusal
		if errors.As(err, &ref) != c.wantRefusal {
			t.Errorf("agent %d: refusal %v, want %v", c.status, ref != nil, c.wantRefusal)
		} else if ref != nil && (ref.Code != c.wantCode || ref.Status != c.status) {
			t.Errorf("agent %d: code %q status %d", c.status, ref.Code, ref.Status)
		}
	}
}

// UpdateAgent is the Update button's request for a caller without a request of its own: the same one request
// to the agent, refused while control is disabled and for an agent without self_update in the panel's config.
func TestUpdateAgent(t *testing.T) {
	p, b := testPoller(t)
	a, _ := p.AgentByID("laptop")
	data, err := UpdateAgent(context.Background(), p, a, "Ann")
	if err != nil || data["state"] != "downloading" {
		t.Fatal(data, err)
	}
	if len(b.Requests) != 1 || b.Requests[0] != "laptop POST /update map[requested_by:Ann]" {
		t.Fatal("the request:", b.Requests)
	}
	tower, _ := p.AgentByID("tower")
	if _, err := UpdateAgent(context.Background(), p, tower, "Ann"); err == nil || len(b.Requests) != 1 {
		t.Fatal("an agent without self_update must not be asked", err, b.Requests)
	}
	if err := p.Admin.SetControlDisabled(true, "test", "kill switch"); err != nil {
		t.Fatal(err)
	}
	var ae *apierr.Error
	if _, err := UpdateAgent(context.Background(), p, a, "Ann"); !errors.As(err, &ae) || ae.Status != 409 || len(b.Requests) != 1 {
		t.Fatal("with control disabled nothing is sent", err, b.Requests)
	}
}

// A control request that got no usable answer is told apart from one that never left the panel and from an
// agent's own "no": the wake / update / shut down run must know that what it asked for may be happening
// (audit 5, r2-p2-03, -04, -05). The page's error is the same in every case.
func TestAnUnansweredRequestIsNotARefusal(t *testing.T) {
	p, b := testPoller(t)
	a, _ := p.AgentByID("laptop")
	for _, c := range []struct {
		name       string
		reply      func() (int, jsonx.M, error)
		unanswered bool
	}{
		{"timeout", func() (int, jsonx.M, error) { return 0, nil, context.DeadlineExceeded }, true},
		{"an answer that cannot be read", func() (int, jsonx.M, error) { return 200, nil, devices.Errf("invalid response") }, true},
		{"the connection broke", func() (int, jsonx.M, error) { return 0, nil, io.ErrUnexpectedEOF }, true},
		{"never sent: no connection", func() (int, jsonx.M, error) {
			return 0, nil, &poller.NotSent{Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}}
		}, false},
		{"the agent says no", func() (int, jsonx.M, error) { return 409, jsonx.M{"error": "busy"}, nil }, false},
		{"the agent fails", func() (int, jsonx.M, error) { return 500, jsonx.M{"error": "boom"}, nil }, false},
	} {
		b.Reply = func(string, string) (int, jsonx.M, error) { return c.reply() }
		_, err := UpdateAgent(context.Background(), p, a, "Ann")
		var ae *apierr.Error
		if !errors.As(err, &ae) {
			t.Errorf("%s: the page's error is %v", c.name, err)
			continue
		}
		var lost *Unanswered
		if errors.As(err, &lost) != c.unanswered {
			t.Errorf("%s: unanswered %v, want %v (%v)", c.name, lost != nil, c.unanswered, err)
		}
		if c.unanswered && (ae.Status != 502 || !strings.HasPrefix(ae.Detail.(string), "agent unreachable: ")) {
			t.Errorf("%s: the page's error changed: %d %v", c.name, ae.Status, ae.Detail)
		}
	}
}
