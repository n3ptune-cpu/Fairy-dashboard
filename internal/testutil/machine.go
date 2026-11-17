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

	// Runs is set for a machine whose checker takes runs (NewRunMachine): Fairy-agent's real run watch.
	Runs *approval.RunWatch
	// PowerPending says whether a power action is pending on the machine (nil: none), and OnSelfOff is told
	// when the run watch schedules the self-switch-off (the countdown in seconds). Both are the test's.
	PowerPending func() bool
	OnSelfOff    func(delay time.Duration)

	// Plugs is set for a machine whose agent reads plugs with sockets and switches them (Fairy-agent v0.10.0):
	// POST /plug/switch and the challenge for socket_set answer as the agent does, with the agent's real gate.
	Plugs *MachinePlugs

	// Uninstall: the machine's config says uninstall.enabled (Fairy-agent 0.11.0): POST /uninstall answers as the
	// agent does (agent/uninstall.go), with the agent's real gate, once. Uninstalled: it was started.
	Uninstall   bool
	Uninstalled bool

	mu       sync.Mutex
	passed   []Sent // control requests the gate let through, in order
	asked    []Sent // every request the panel sent (also refused ones), with its body
	now      func() time.Time
	uptime   time.Duration
	uptimeFn func() time.Duration
	boot     string
	runs     bool
}

// Sent is one request the panel sent to a machine.
type Sent struct {
	Path string
	Body map[string]any
	// Approved: it carried an approval the gate accepted.
	Approved bool
	// Wait is the wait the panel asked for with this one request (poller.WithAgentWait; 0: the usual one).
	Wait time.Duration
}

// MachinePlugs are the sockets of a Machine and what its made-up Home Assistant does with them. The rules are
// the agent's (Fairy-agent, pkg/agent/plug.go), in its order: what the machine's config says about the socket,
// the tag, the passkey check looked at and not used up, the state, then the gate's last word, which uses the
// approval up, then the switch.
type MachinePlugs struct {
	// Off: switching is turned off in the machine's config (404 plug_switching_disabled).
	Off bool
	// Sockets by "<plug id>/<socket id>".
	Sockets map[string]*MachineSocket
	// HeldS: the wait after a refused token is on, with this many seconds left (502 ha_unreachable with
	// retry_after_s, for the challenge and for the switch).
	HeldS int
	// Answer, when set, is what the switch request is answered with after the approval was used up, in place
	// of a switch that works (a status of 0: it works). For Home Assistant's own failures.
	Answer func(b api.PlugSwitch) (int, jsonx.M)
	// Unconfirmed: the request is sent and the socket does not show the new state (200, confirmed false).
	Unconfirmed bool
}

// MachineSocket is one socket of a Machine.
type MachineSocket struct {
	Tag   string
	State string // what Home Assistant says: on, off, unavailable, unknown
	// Refuse is the agent's own refusal for this socket ("": none): not_allowed, feeds_self, no_token.
	Refuse string
}

// plugRefusals are the statuses of the agent's own refusals (Fairy-agent, plugRefusals).
var plugRefusals = map[string]int{api.CodeUnknownSocket: 404, api.CodeNotAllowed: 403, api.CodeFeedsSelf: 403, api.CodeSocketChanged: 409,
	api.CodeNoSwitchToken: 409, api.CodeSocketUnavailable: 409, api.CodeHAUnreachable: 502, api.CodePlugSwitchingDisabled: 404}

func plugNo(code string, retryS int) (bool, int, jsonx.M, func()) {
	out := jsonx.M{"error": "the machine's own sentence for " + code, "code": code}
	if retryS > 0 {
		out["retry_after_s"] = float64(retryS)
	}
	return true, plugRefusals[code], out, nil
}

// known is the agent's socketKnown: the socket, or the code of the refusal.
func (p *MachinePlugs) known(source, socket, tag string) (*MachineSocket, string) {
	s := p.Sockets[source+"/"+socket]
	switch {
	case p.Off:
		return nil, api.CodePlugSwitchingDisabled
	case s == nil:
		return nil, api.CodeUnknownSocket
	case s.Refuse == api.CodeFeedsSelf || s.Refuse == api.CodeNotAllowed:
		return s, s.Refuse
	case tag != s.Tag:
		return s, api.CodeSocketChanged
	case s.Refuse != "":
		return s, s.Refuse
	}
	return s, ""
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

// NewRunMachine is NewMachine for a machine whose checker takes the ticket and the confirmation of a run
// (Fairy-agent v0.9.0: "runs" in its approval block), through the agent's real gate and run watch. Its uptime
// is ten seconds until the test says otherwise (SetUptime): a machine that has just started.
func NewRunMachine(t testing.TB, id string) *Machine {
	t.Helper()
	m := &Machine{ID: id, TrustPath: filepath.Join(t.TempDir(), approval.TrustFileName), now: time.Now, uptime: 10 * time.Second, boot: "boot-1", runs: true}
	m.Restart()
	return m
}

// Restart is the machine's agent starting anew (a process does not outlive a boot): a new gate and a new run
// watch on the same files. With SetBoot before it, it is a new boot.
func (m *Machine) Restart() {
	gate := approval.NewGate(approval.Options{Files: approval.Files{Path: m.TrustPath}, EnforcedBy: api.EnforcedByAgent, Runs: m.runs, MinDelayS: 10,
		Now: func() time.Time { m.mu.Lock(); defer m.mu.Unlock(); return m.now() },
		Uptime: func() (time.Duration, error) {
			m.mu.Lock()
			fn, up := m.uptimeFn, m.uptime
			m.mu.Unlock()
			if fn != nil {
				return fn(), nil
			}
			return up, nil
		},
		BootID: func() (string, error) { m.mu.Lock(); defer m.mu.Unlock(); return m.boot, nil }})
	watch := approval.NewRunWatch(gate, approval.RunHooks{
		Pending: func() bool { return m.PowerPending != nil && m.PowerPending() },
		Shutdown: func(delay time.Duration, _, _ string) error {
			if m.OnSelfOff != nil {
				m.OnSelfOff(delay)
			}
			return nil
		},
		Cancel: func() error { return nil }})
	m.mu.Lock()
	m.Gate, m.Runs = gate, watch
	m.mu.Unlock()
}

// SetUptime makes the machine's uptime what fn says, and SetBoot names its boot.
func (m *Machine) SetUptime(fn func() time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.uptimeFn = fn
}

func (m *Machine) SetBoot(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.boot = id
}

// SetNow sets the machine's clock alone.
func (m *Machine) SetNow(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
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
	m.mu.Lock()
	gate := m.Gate
	m.mu.Unlock()
	raw, _ := json.Marshal(gate.StatusNow())
	out := jsonx.M{}
	_ = json.Unmarshal(raw, &out)
	return out
}

// request answers a POST the panel sends: handled reports whether the machine answered (false: the backend's
// scripted answer follows; applied, when not nil, is then told whether that answer was 200, as the agent tells
// its gate that an approved fan change was applied).
func (m *Machine) request(path string, body any) (handled bool, status int, data jsonx.M, applied func()) {
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
	refusal := func(r *approval.Refusal) (bool, int, jsonx.M, func()) {
		out := jsonx.M{"error": r.Msg, "code": r.Code}
		if r.Reason != "" {
			out["reason"] = r.Reason
		}
		if r.RetryAfterS > 0 {
			out["retry_after_s"] = float64(r.RetryAfterS)
		}
		return true, r.Status, out, nil
	}
	switch path {
	case api.PathApprovalChallenge:
		var req api.ApprovalRequest
		if !strict(&req) {
			return true, 400, jsonx.M{"error": "not a request that can be approved"}, nil
		}
		if req.Verb == api.ApproveSocketSet {
			if _, err := req.Canonical(); err != nil {
				return true, 400, jsonx.M{"error": "not a request that can be approved: " + err.Error()}, nil
			}
			if m.Plugs == nil { // an agent that reads no plugs
				return plugNo(api.CodePlugSwitchingDisabled, 0)
			}
			if _, code := m.Plugs.known(*req.Source, *req.Socket, *req.Tag); code != "" {
				return plugNo(code, 0)
			}
			if st := m.Gate.StatusNow().State; st == api.ApprovalRequired && m.Plugs.HeldS > 0 {
				return plugNo(api.CodeHAUnreachable, m.Plugs.HeldS)
			}
		}
		ch, ref := m.Gate.Challenge(req)
		if ref != nil {
			return refusal(ref)
		}
		out := jsonx.M{}
		b, _ := json.Marshal(ch)
		_ = json.Unmarshal(b, &out)
		return true, 200, out, nil
	case api.PathApprovalTrust:
		var in api.TrustInit
		if !strict(&in) {
			return true, 400, jsonx.M{"error": "invalid", "code": api.CodeTrustInvalid}, nil
		}
		st, ref := m.Gate.Init(in)
		if ref != nil {
			return refusal(ref)
		}
		out := jsonx.M{}
		b, _ := json.Marshal(st)
		_ = json.Unmarshal(b, &out)
		return true, 200, out, nil
	}
	if path == api.PathPlugSwitch {
		var in api.PlugSwitch
		if !strict(&in) {
			return true, 400, jsonx.M{"error": "not a switch request"}, nil
		}
		req := in.Request()
		if _, err := req.Canonical(); err != nil {
			return true, 400, jsonx.M{"error": "not a switch request: " + err.Error()}, nil
		}
		if m.Plugs == nil || m.Plugs.Off {
			return plugNo(api.CodePlugSwitchingDisabled, 0)
		}
		sock, code := m.Plugs.known(in.Source, in.Socket, in.Tag)
		if code != "" {
			return plugNo(code, 0)
		}
		if ref := m.Gate.Look(req, in.Approval); ref != nil { // looked at in full, nothing used up
			return refusal(ref)
		}
		if m.Plugs.HeldS > 0 {
			return plugNo(api.CodeHAUnreachable, m.Plugs.HeldS)
		}
		was := sock.State
		if was != api.SocketStateOn && was != api.SocketStateOff {
			return plugNo(api.CodeSocketUnavailable, 0) // the approval is whole
		}
		if _, ref := m.Gate.Admit(req, in.Approval); ref != nil { // the approval is used up here
			return refusal(ref)
		}
		rec.Approved = true
		m.mu.Lock()
		m.passed = append(m.passed, rec)
		m.mu.Unlock()
		if m.Plugs.Answer != nil {
			if st, out := m.Plugs.Answer(in); st != 0 {
				return true, st, out, nil
			}
		}
		state := in.To
		if m.Plugs.Unconfirmed {
			state = was
		} else {
			sock.State = in.To
		}
		return true, 200, jsonx.M{"source": in.Source, "socket": in.Socket, "to": in.To, "was": was, "state": state,
			"confirmed": state == in.To, "took_ms": float64(840)}, nil
	}
	if path == api.PathUninstall {
		// the agent's order: strict body, no trust is approval_off, the approval looked at, once, then used up
		var in api.UninstallRequest
		if !strict(&in) {
			return true, 400, jsonx.M{"error": "not an uninstall request"}, nil
		}
		if !m.Uninstall {
			return true, 404, jsonx.M{"error": "that is disabled on this agent", "code": api.CodeUninstallDisabled}, nil
		}
		req := api.ApprovalRequest{Verb: api.ApproveUninstall}
		if ref := m.Gate.Look(req, in.Approval); ref != nil {
			return refusal(ref)
		}
		m.mu.Lock()
		done := m.Uninstalled
		m.mu.Unlock()
		if done {
			return true, 409, jsonx.M{"error": "an uninstall was already started on this machine", "code": api.CodeUninstallBusy}, nil
		}
		if _, ref := m.Gate.Admit(req, in.Approval); ref != nil {
			return refusal(ref)
		}
		rec.Approved = true
		m.mu.Lock()
		m.Uninstalled = true
		m.passed = append(m.passed, rec)
		m.mu.Unlock()
		return true, 202, jsonx.M{"state": api.UninstallStarted}, nil
	}
	if m.Runs != nil && (path == api.PathRunArm || path == api.PathRunGo) {
		answer := func(ans api.RunAnswer, ref *approval.Refusal) (bool, int, jsonx.M, func()) {
			if ref != nil {
				return refusal(ref)
			}
			out := jsonx.M{}
			b, _ := json.Marshal(ans)
			_ = json.Unmarshal(b, &out)
			rec.Approved = true
			return true, 200, out, nil
		}
		if path == api.PathRunArm {
			var in api.RunArm
			if !strict(&in) || in.Approval == nil {
				return true, 400, jsonx.M{"error": "a run ticket is an approval: the body needs \"approval\""}, nil
			}
			return answer(m.Runs.Arm(in.Approval))
		}
		var in api.RunGo
		if !strict(&in) || in.ShutdownDelayS == nil || in.WithinS == nil {
			return true, 400, jsonx.M{"error": "not a run confirmation"}, nil
		}
		return answer(m.Runs.Go(in.Request(), in.Approval))
	}
	if path == api.PathRunArm || path == api.PathRunGo {
		// a checker that keeps no runs (an agent as v0.8.0 was has no such route at all): the real agent's answer
		ref := approval.NoRuns()
		return refusal(ref)
	}
	var b struct {
		Run      string        `json:"run"`
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
	case path == "/power" && b.Action == "cancel" && m.Runs != nil:
		// as the agent: a cancel ends the run this machine is in, then stops what is pending
		if _, err := m.Runs.Cancel("a cancel of the pending power action (POST /power)"); err != nil {
			return true, 500, jsonx.M{"error": "the end of the run could not be written down; cancel again"}, nil
		}
		return false, 0, nil, nil
	default:
		return false, 0, nil, nil // cancel, wake, ...: nothing to check
	}
	if b.Run != "" && b.Approval == nil && m.Runs != nil && (path == "/update" || b.Action == "shutdown") {
		// the one update and the one shutdown of a confirmed run: looked at, then used up, as the agent does
		if _, ref := m.Gate.RunCovers(req, b.Run); ref != nil {
			return refusal(ref)
		}
		if _, ref := m.Gate.AdmitRun(req, b.Run); ref != nil {
			return refusal(ref)
		}
		return false, 0, nil, nil
	}
	// the agent's two questions: when the request arrives (nothing is used up), and when it is carried out
	if ref := m.Gate.Precheck(req, b.Approval != nil); ref != nil {
		return refusal(ref)
	}
	pass, ref := m.Gate.Admit(req, b.Approval)
	if ref != nil {
		return refusal(ref)
	}
	rec.Approved = b.Approval != nil
	return false, 0, nil, pass.Applied
}

// GatedBackend is a Backend whose agents can be Machines.
type GatedBackend struct {
	*Backend
	// WaitOf, when set, reads the wait one request asked for from its context (poller.AgentWait; this package
	// cannot name the poller, whose own tests use it). It is recorded with the request (Sent.Wait).
	WaitOf   func(ctx context.Context) (time.Duration, bool)
	mu       sync.Mutex
	machines map[string]*Machine
	lies     map[string]func(path string) (int, jsonx.M)
	relays   map[string]func(path string, body any) (int, jsonx.M)
	fails    map[string]error
	stale    map[string]jsonx.M
	live     map[string]func() jsonx.M
}

// waitOf reads the wait a request asked for out of its context, when the test said how (WaitOf).
func (g *GatedBackend) waitOf(ctx context.Context) (time.Duration, bool) {
	if g.WaitOf == nil {
		return 0, false
	}
	return g.WaitOf(ctx)
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

// AddRuns is Add for a machine whose checker takes runs (NewRunMachine).
func (g *GatedBackend) AddRuns(t testing.TB, id string) *Machine {
	m := NewRunMachine(t, id)
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

// Relay is Lie for a machine that needs to see what it was sent: fn gets the request's body too, so it can
// hand the request on to another machine (Forward) and answer with that machine's answer. For a host that
// answers in the name of another one.
func (g *GatedBackend) Relay(id string, fn func(path string, body any) (int, jsonx.M)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.relays == nil {
		g.relays = map[string]func(path string, body any) (int, jsonx.M){}
	}
	if fn == nil {
		delete(g.relays, id)
		return
	}
	g.relays[id] = fn
}

// Forward sends a POST to the machine id as if the panel had sent it there (through its gate), for Relay.
func (g *GatedBackend) Forward(id, path string, body any) (int, jsonx.M) {
	status, data, err := g.AgentRequest(context.Background(), config.Agent{ID: id}, http.MethodPost, path, body)
	if err != nil {
		return 502, jsonx.M{"error": err.Error()}
	}
	return status, data
}

// Stale makes the agent id's /metrics carry this approval block instead of the machine's real one (the answer of
// a poll from before the machine changed); nil: the real one again; an empty block: no block at all (the agent
// was put back to a release from before the check).
func (g *GatedBackend) Stale(id string, block jsonx.M) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stale == nil {
		g.stale = map[string]jsonx.M{}
	}
	if block == nil {
		delete(g.stale, id)
	} else {
		g.stale[id] = block
	}
}

// Live makes the agent id's /metrics carry the approval block fn returns at every poll (nil: no block), whether
// or not the id is a Machine: for an agent whose check is somebody else's (a real agent behind Relay). A nil fn
// ends it.
func (g *GatedBackend) Live(id string, fn func() jsonx.M) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.live == nil {
		g.live = map[string]func() jsonx.M{}
	}
	if fn == nil {
		delete(g.live, id)
		return
	}
	g.live[id] = fn
}

// Fail makes every POST to the agent id fail with err (a machine that does not answer control requests).
func (g *GatedBackend) Fail(id string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fails == nil {
		g.fails = map[string]error{}
	}
	g.fails[id] = err
}

// FetchAgentMetrics adds a machine's approval block to the scripted answer.
func (g *GatedBackend) FetchAgentMetrics(ctx context.Context, a config.Agent, timeout time.Duration) (jsonx.M, error) {
	out, err := g.Backend.FetchAgentMetrics(ctx, a, timeout)
	g.mu.Lock()
	m, old, live := g.machines[a.ID], g.stale[a.ID], g.live[a.ID]
	g.mu.Unlock()
	if live != nil && err == nil {
		if block := live(); block != nil {
			out["approval"] = block
		} else {
			delete(out, "approval")
		}
		return out, err
	}
	switch {
	case err != nil || m == nil:
	case old != nil && len(old) == 0:
		delete(out, "approval") // Stale(id, jsonx.M{}): no block at all, as an agent from before the check answers
	case old != nil:
		out["approval"] = old
	default:
		out["approval"] = m.Block()
	}
	return out, err
}

// AgentRequest sends a machine's control requests through its gate first.
func (g *GatedBackend) AgentRequest(ctx context.Context, a config.Agent, method, path string, body any) (int, jsonx.M, error) {
	g.mu.Lock()
	m, lie, fail, relay := g.machines[a.ID], g.lies[a.ID], g.fails[a.ID], g.relays[a.ID]
	g.mu.Unlock()
	if fail != nil && method == http.MethodPost {
		return 0, nil, fail
	}
	if relay != nil && method == http.MethodPost {
		if status, data := relay(path, body); status != 0 {
			return status, data, nil
		}
	}
	if lie != nil && method == http.MethodPost {
		if status, data := lie(path); status != 0 {
			return status, data, nil
		}
	}
	if m != nil && method == http.MethodPost {
		handled, status, data, applied := m.request(path, body)
		if wait, ok := g.waitOf(ctx); ok { // the wait the panel asked for with this one request
			m.mu.Lock()
			if n := len(m.asked); n > 0 {
				m.asked[n-1].Wait = wait
			}
			m.mu.Unlock()
		}
		if handled {
			return status, data, nil
		}
		status, data, err := g.Backend.AgentRequest(ctx, a, method, path, body)
		if err == nil && status == 200 && applied != nil {
			applied() // the fan window opens only after an approved fan change that was applied
		}
		return status, data, err
	}
	return g.Backend.AgentRequest(ctx, a, method, path, body)
}
