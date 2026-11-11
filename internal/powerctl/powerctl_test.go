package powerctl

import (
	"context"
	"errors"
	"strings"
	"testing"

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
