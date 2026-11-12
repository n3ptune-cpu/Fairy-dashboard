package normalise

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
)

func block(t *testing.T, st api.ApprovalStatus) jsonx.M {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"approval": st})
	if err != nil {
		t.Fatal(err)
	}
	m := jsonx.M{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// The agent's own type, written as the agent writes it, comes out with every field the page shows.
func TestApprovalBlockOfTheAgent(t *testing.T) {
	if agentApproval(jsonx.M{"hostname": "old-agent"}) != nil {
		t.Fatal("an agent without the block has no approval state")
	}
	w, b := 30, 0
	thumb := strings.Repeat("A", 43)
	got := agentApproval(block(t, api.ApprovalStatus{State: api.ApprovalRequired, EnforcedBy: api.EnforcedByHelper, Proto: api.ApprovalProto,
		Issuer: "https://id.example.org", ClientID: "3f6c0c0e-0000-4000-8000-000000000000",
		Keys: []api.ApprovalKey{{Kid: "abc123", Alg: "RS256", Thumbprint: thumb}}, Users: 2, FanWindowS: &w, BootShutdownS: &b,
		AgentChecks: []string{api.ApproveFanSet, api.ApproveFanMode}}))
	if got == nil || got.State != "required" || got.EnforcedBy != "helper" || got.Proto != api.ApprovalProto || got.Issuer != "https://id.example.org" ||
		got.ClientID != "3f6c0c0e-0000-4000-8000-000000000000" || len(got.Keys) != 1 || got.Keys[0].Thumbprint != thumb || got.Users != 2 ||
		got.FanWindowS == nil || *got.FanWindowS != 30 || got.BootShutdownS == nil || *got.BootShutdownS != 0 || len(got.AgentChecks) != 2 {
		t.Fatalf("%+v", got)
	}
	off := agentApproval(block(t, api.ApprovalStatus{State: api.ApprovalOff, EnforcedBy: api.EnforcedByAgent, Proto: 1}))
	if off.State != "off" || off.Keys == nil || len(off.Keys) != 0 || off.AgentChecks == nil || off.FanWindowS != nil {
		t.Fatalf("off: %+v", off)
	}
	broken := agentApproval(block(t, api.ApprovalStatus{State: api.ApprovalBroken, EnforcedBy: api.EnforcedByAgent, Proto: 1, Error: "the trust file is not valid JSON"}))
	if broken.State != "broken" || broken.Error != "the trust file is not valid JSON" {
		t.Fatalf("broken: %+v", broken)
	}
}

// Whatever answers in the agent's place cannot put anything odd on the page. A state the panel does not know
// reads as "unknown", which is never treated as "nothing is checked".
func TestApprovalBlockIsReadDefensively(t *testing.T) {
	long := strings.Repeat("x", 5000)
	m := jsonx.M{"approval": jsonx.M{"state": "definitely-off", "enforced_by": "the panel", "proto": 99999.0,
		"issuer": long + "\x1b[31m", "client_id": "has space", "users": 1e9, "fan_window_s": 61.0, "boot_shutdown_s": -5.0,
		"error": long, "agent_error": "line\nbreak\u202e", "agent_checks": []any{"fan_set", "fan_set", "format_disk", 7.0},
		"keys": []any{
			jsonx.M{"kid": "ok", "alg": "RS256", "thumbprint": strings.Repeat("B", 43)},
			jsonx.M{"kid": "short", "alg": "RS256", "thumbprint": "tooshort"},
			jsonx.M{"kid": "<script>", "alg": "RS256", "thumbprint": strings.Repeat("B", 43)},
			"not an object",
			jsonx.M{"kid": "k3", "alg": "RS256", "thumbprint": strings.Repeat("C", 43)},
			jsonx.M{"kid": "k4", "alg": "RS256", "thumbprint": strings.Repeat("D", 43)},
			jsonx.M{"kid": "k5", "alg": "RS256", "thumbprint": strings.Repeat("E", 43)},
			jsonx.M{"kid": "k6", "alg": "RS256", "thumbprint": strings.Repeat("F", 43)},
		}}}
	got := agentApproval(m)
	if got.State != api.ApprovalUnknown || got.EnforcedBy != "" || got.Proto != 0 || got.ClientID != "" || got.Users != 0 ||
		got.FanWindowS != nil || got.BootShutdownS != nil {
		t.Fatalf("%+v", got)
	}
	if len(got.Issuer) > 260 || len(got.Error) > 310 || strings.ContainsAny(got.Issuer+got.Error+got.AgentError, "\x1b\n\u202e") {
		t.Fatalf("texts are not bounded and cleaned: %d %d %q", len(got.Issuer), len(got.Error), got.AgentError)
	}
	if len(got.Keys) != api.MaxTrustKeys || got.Keys[0].Kid != "ok" || got.Keys[1].Kid != "k3" {
		t.Fatalf("keys: %+v", got.Keys)
	}
	if len(got.AgentChecks) != 1 || got.AgentChecks[0] != "fan_set" {
		t.Fatalf("agent_checks: %v", got.AgentChecks)
	}
	for _, v := range []any{"required", 7.0, []any{}, nil} {
		if a := agentApproval(jsonx.M{"approval": v}); a != nil {
			t.Errorf("approval: %v is not a block, got %+v", v, a)
		}
	}
	if a := agentApproval(jsonx.M{"approval": jsonx.M{}}); a == nil || a.State != api.ApprovalUnknown {
		t.Errorf("an empty block is a block in an unknown state: %+v", a)
	}
}
