package testutil

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
)

// Machine is an agent that checks approvals itself, for tests of the panel: the control requests the panel
// sends to it go through Fairy-agent's real gate (pkg/approval: challenges, the trust file, the offline check
// of the token), and what gets past the gate is recorded. Everything else of an agent is the scripted Backend.
type Machine struct {
	ID   string
	Gate *approval.Gate
	// TrustPath is the machine's trust file.
	TrustPath string

	mu     sync.Mutex
	passed []Sent // control requests the gate let through, in order
	asked  []Sent // every request the panel sent (also refused ones), with its body
	now    func() time.Time
	uptime time.Duration
}

// Sent is one request the panel sent to a machine.
type Sent struct {
	Path string
	Body map[string]any
	// Approved: it carried an approval the gate accepted.
	Approved bool
}

// NewMachine is a machine with no trust file (approval "off"), an uptime of one hour and the test's clock.
func NewMachine(t testing.TB, id string) *Machine {
	t.Helper()
	m := &Machine{ID: id, TrustPath: filepath.Join(t.TempDir(), approval.TrustFileName), now: time.Now, uptime: time.Hour}
	m.Gate = approval.NewGate(approval.Options{Files: approval.Files{Path: m.TrustPath}, EnforcedBy: api.EnforcedByAgent,
		Now:    func() time.Time { m.mu.Lock(); defer m.mu.Unlock(); return m.now() },
		Uptime: func() (time.Duration, error) { m.mu.Lock(); defer m.mu.Unlock(); return m.uptime, nil }})
	return m
}

// SetClock sets the machine's clock and uptime.
func (m *Machine) SetClock(now func() time.Time, uptime time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now, m.uptime = now, uptime
}

// Passed are the control requests the gate let through.
func (m *Machine) Passed() []Sent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Sent(nil), m.passed...)
}

// Asked are all requests the panel sent, with their bodies.
func (m *Machine) Asked() []Sent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Sent(nil), m.asked...)
}

// Block is the machine's "approval" block for /metrics.
func (m *Machine) Block() jsonx.M {
	raw, _ := json.Marshal(m.Gate.StatusNow())
	out := jsonx.M{}
	_ = json.Unmarshal(raw, &out)
	return out
}

// request answers a POST the panel sends: handled reports whether the machine answered (false: the backend's
// scripted answer follows).
func (m *Machine) request(path string, body any) (handled bool, status int, data jsonx.M) {
	raw, _ := json.Marshal(body)
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	rec := Sent{Path: path, Body: fields}
	defer func() {
		m.mu.Lock()
		m.asked = append(m.asked, rec)
		if !handled && path != api.PathApprovalChallenge && path != api.PathApprovalTrust {
			m.passed = append(m.passed, rec)
		}
		m.mu.Unlock()
	}()
	strict := func(v any) bool {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		return dec.Decode(v) == nil
	}
	refusal := func(r *approval.Refusal) (bool, int, jsonx.M) {
		return true, r.Status, jsonx.M{"error": r.Msg, "code": r.Code}
	}
	switch path {
	case api.PathApprovalChallenge:
		var req api.ApprovalRequest
		if !strict(&req) {
			return true, 400, jsonx.M{"error": "not a request that can be approved"}
		}
		ch, ref := m.Gate.Challenge(req)
		if ref != nil {
			return refusal(ref)
		}
		out := jsonx.M{}
		b, _ := json.Marshal(ch)
		_ = json.Unmarshal(b, &out)
		return true, 200, out
	case api.PathApprovalTrust:
		var in api.TrustInit
		if !strict(&in) {
			return true, 400, jsonx.M{"error": "invalid", "code": api.CodeTrustInvalid}
		}
		st, ref := m.Gate.Init(in)
		if ref != nil {
			return refusal(ref)
		}
		out := jsonx.M{}
		b, _ := json.Marshal(st)
		_ = json.Unmarshal(b, &out)
		return true, 200, out
	}
	var b struct {
		Action   string        `json:"action"`
		DelayS   *int          `json:"delay_s"`
		Mode     *string       `json:"mode"`
		Zone     *int          `json:"zone"`
		Percent  *int          `json:"percent"`
		Approval *api.Approval `json:"approval"`
	}
	_ = json.Unmarshal(raw, &b)
	var req api.ApprovalRequest
	switch {
	case path == "/power" && (b.Action == "shutdown" || b.Action == "reboot" || b.Action == "sleep"):
		delay := 60
		if b.DelayS != nil {
			delay = *b.DelayS
		}
		req = api.ApprovalRequest{Verb: api.ApprovePower, Action: &b.Action, DelayS: &delay}
	case path == "/fan/mode":
		req = api.ApprovalRequest{Verb: api.ApproveFanMode, Mode: b.Mode}
	case path == "/fan/duty":
		req = api.ApprovalRequest{Verb: api.ApproveFanSet, Zone: b.Zone, Percent: b.Percent}
	case path == "/update":
		req = api.ApprovalRequest{Verb: api.ApproveApplyUpdate}
	default:
		return false, 0, nil // cancel, wake, ...: nothing to check
	}
	if ref := m.Gate.Check(req, b.Approval); ref != nil {
		return refusal(ref)
	}
	rec.Approved = b.Approval != nil
	return false, 0, nil
}

// GatedBackend is a Backend whose agents can be Machines.
type GatedBackend struct {
	*Backend
	mu       sync.Mutex
	machines map[string]*Machine
	lies     map[string]func(path string) (int, jsonx.M)
}

// NewGatedBackend wraps b.
func NewGatedBackend(b *Backend) *GatedBackend {
	return &GatedBackend{Backend: b, machines: map[string]*Machine{}}
}

// Add makes the agent id a Machine: its /metrics gets the machine's approval block, and its control requests
// go through the machine's gate.
func (g *GatedBackend) Add(t testing.TB, id string) *Machine {
	m := NewMachine(t, id)
	g.mu.Lock()
	g.machines[id] = m
	g.mu.Unlock()
	return m
}

// Lie makes the agent id answer a POST with what fn returns instead of what its machine would (a status of 0:
// the machine answers as usual). For a machine that is not honest.
func (g *GatedBackend) Lie(id string, fn func(path string) (int, jsonx.M)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lies == nil {
		g.lies = map[string]func(path string) (int, jsonx.M){}
	}
	g.lies[id] = fn
}

// FetchAgentMetrics adds a machine's approval block to the scripted answer.
func (g *GatedBackend) FetchAgentMetrics(ctx context.Context, a config.Agent, timeout time.Duration) (jsonx.M, error) {
	out, err := g.Backend.FetchAgentMetrics(ctx, a, timeout)
	g.mu.Lock()
	m := g.machines[a.ID]
	g.mu.Unlock()
	if err == nil && m != nil {
		out["approval"] = m.Block()
	}
	return out, err
}

// AgentRequest sends a machine's control requests through its gate first.
func (g *GatedBackend) AgentRequest(ctx context.Context, a config.Agent, method, path string, body any) (int, jsonx.M, error) {
	g.mu.Lock()
	m, lie := g.machines[a.ID], g.lies[a.ID]
	g.mu.Unlock()
	if lie != nil && method == http.MethodPost {
		if status, data := lie(path); status != 0 {
			return status, data, nil
		}
	}
	if m != nil && method == http.MethodPost {
		if handled, status, data := m.request(path, body); handled {
			return status, data, nil
		}
	}
	return g.Backend.AgentRequest(ctx, a, method, path, body)
}
