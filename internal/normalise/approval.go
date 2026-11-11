package normalise

import (
	"regexp"
	"slices"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
)

// The "approval" block of an agent's /metrics (Fairy-agent api.ApprovalStatus; release item 1.1):
//
//	"approval": {"state": "off"|"required"|"broken"|"unknown", "enforced_by": "helper"|"agent", "proto": 1,
//	             "issuer": "...", "client_id": "...", "keys": [{"kid", "alg", "thumbprint"}], "users": 2,
//	             "fan_window_s": 30, "boot_shutdown_s": 0, "error": "...", "agent_checks": [...], "agent_error": "..."}
//
// Absent: an agent from before the feature (or a Windows agent with nothing that checks). Like every block of
// an agent's answer it is read defensively: an unknown state reads as "unknown" (which the panel treats like
// "the machine must be asked"), texts are bounded and cleaned, numbers are kept inside what the agent can
// send, and anything that is not what it should be is dropped. The panel never decides from this block that
// an approval is NOT needed for a machine that says it is: it only decides whether to ask the machine.

var (
	approvalStates  = map[string]bool{api.ApprovalOff: true, api.ApprovalRequired: true, api.ApprovalBroken: true, api.ApprovalUnknown: true}
	approvalVerbs   = []string{api.ApproveFanSet, api.ApproveFanMode, api.ApprovePower, api.ApproveApplyUpdate}
	approvalIDRe    = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)
	approvalKidRe   = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{1,128}$`)
	approvalThumbRe = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	approvalAlgRe   = regexp.MustCompile(`^[A-Za-z0-9]{1,16}$`)
)

// agentApproval reads the optional `approval` block; nil when the agent sends none.
func agentApproval(m jsonx.M) *model.Approval {
	b := jsonx.Obj(m, "approval")
	if b == nil {
		return nil
	}
	out := &model.Approval{State: api.ApprovalUnknown, Keys: []model.ApprovalKey{}, AgentChecks: []string{}}
	if st := jsonx.S(b, "state"); approvalStates[st] {
		out.State = st
	}
	if by := jsonx.S(b, "enforced_by"); by == api.EnforcedByHelper || by == api.EnforcedByAgent {
		out.EnforcedBy = by
	}
	if n := jsonx.IntP(b, "proto"); n != nil && *n >= 0 && *n <= 1000 {
		out.Proto = *n
	}
	out.Issuer = textsafe.Clip(jsonx.S(b, "issuer"), 255)
	if id := jsonx.S(b, "client_id"); approvalIDRe.MatchString(id) {
		out.ClientID = id
	}
	for _, k := range jsonx.Objs(b["keys"]) {
		if len(out.Keys) == api.MaxTrustKeys {
			break
		}
		kid, alg, tp := jsonx.S(k, "kid"), jsonx.S(k, "alg"), jsonx.S(k, "thumbprint")
		if approvalKidRe.MatchString(kid) && approvalAlgRe.MatchString(alg) && approvalThumbRe.MatchString(tp) {
			out.Keys = append(out.Keys, model.ApprovalKey{Kid: kid, Alg: alg, Thumbprint: tp})
		}
	}
	if n := jsonx.IntP(b, "users"); n != nil && *n >= 0 && *n <= api.MaxTrustUsers {
		out.Users = *n
	}
	out.FanWindowS, out.BootShutdownS = boundedInt(b, "fan_window_s", 60), boundedInt(b, "boot_shutdown_s", 900)
	out.Error = textsafe.Clip(jsonx.S(b, "error"), 300)
	for _, v := range jsonx.List(b, "agent_checks") {
		if s, ok := v.(string); ok && slices.Contains(approvalVerbs, s) && !slices.Contains(out.AgentChecks, s) {
			out.AgentChecks = append(out.AgentChecks, s)
		}
	}
	out.AgentError = textsafe.Clip(jsonx.S(b, "agent_error"), 300)
	return out
}

// boundedInt is m[k] as a whole number from 0 to max, or nil.
func boundedInt(m jsonx.M, k string, max int) *int {
	n := jsonx.IntP(m, k)
	if n == nil || *n < 0 || *n > max {
		return nil
	}
	return n
}
