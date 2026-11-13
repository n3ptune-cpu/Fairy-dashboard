// Package poller polls Proxmox, agents, power sources and UPSes in the background into an in-memory cache and
// builds the views the API serves from it (system list, summary, polling status, health file).
//
// Every source is its own scheduler job: per-source interval, per-request timeout below the interval, never
// overlapping itself, per-host overrides / pause / boost-while-viewing.
package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"net/url"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/normalise"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/power"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/scheduler"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/ups"
)

// Backend does the network I/O (replaced by the simulator in demo mode).
type Backend interface {
	FetchProxmox(ctx context.Context, pve config.Proxmox, timeout time.Duration) ([]jsonx.M, error)
	FetchNodeStatus(ctx context.Context, pve config.Proxmox, node string) (jsonx.M, error)
	FetchAgentMetrics(ctx context.Context, a config.Agent, timeout time.Duration) (jsonx.M, error)
	PVEPower(ctx context.Context, clusterID, path string, data url.Values) (any, error)
	FetchProcesses(ctx context.Context, a config.Agent, systemID string, limit int, sortBy string) (model.ProcessList, error)
	// AgentRequest: method GET uses the read token, POST the control token. Returns status and JSON body.
	AgentRequest(ctx context.Context, a config.Agent, method, path string, body any) (int, jsonx.M, error)
}

// Poller owns the in-memory state of every system and source and the jobs that refresh it.
type Poller struct {
	Cfg     *config.Panel
	Secrets config.Secrets
	Clock   func() time.Time
	// Mono is a clock that only goes forward at the speed of time and does not move when the system's clock
	// is set. nil: the steady clock reading Go keeps in the values of Clock (time.Now), or, with a Clock that
	// has none (a test's), that Clock itself. How long an agent has given nothing at all is measured with it
	// (agents.go, quietRow), so a clock that is set forward cannot make silence.
	Mono      func() time.Duration
	Backend   Backend
	Power     *power.Manager
	UPS       *ups.Manager
	Admin     *adminstate.State
	Scheduler *scheduler.Scheduler
	StartedAt time.Time

	born  time.Time // for mono: it holds Go's steady clock reading
	rates *normalise.RateTracker
	// cfgMu guards agents, overrides and the Cfg.Agents / Cfg.Systems slices, which hosts added or removed in
	// the panel (Add host) change at run time. The slices are replaced, never changed in place. Read them only
	// through AgentByID / Override / Agents / SystemsConfig. Lock order: p.mu may be held while taking cfgMu,
	// never the other way round.
	cfgMu     sync.RWMutex
	overrides map[string]*config.SystemOverride
	agents    map[string]config.Agent
	clusters  map[string]config.Proxmox

	// idsMu guards clusterIDs and present (read by scheduler callbacks, which must never take mu).
	idsMu sync.Mutex
	// clustersMu guards clusters and Cfg.Proxmox (Proxmox nodes added / removed in the panel). Never held while
	// taking another lock.
	clustersMu sync.RWMutex
	present    map[string]bool
	// decided: agent id -> the id of the node the configuration gives it (nodeLinks), for linkOfLocked. Set by
	// relink; an agent that is not in it is not decided.
	decided atomic.Pointer[map[string]string]
	// linkMu makes relink's reading of the configuration and its keeping of the answer one step. Taken with no
	// other lock held; cfgMu and clustersMu are taken under it.
	linkMu sync.Mutex
	// relinkRead, when set, is called by relink between its reading and its keeping (tests).
	relinkRead func()

	mu            sync.Mutex
	systems       map[string]model.System
	sources       map[string]model.SourceStatus
	sourceMono    map[string]time.Duration // source name -> the steady clock at its last good answer (AnsweredSinceUp)
	sourceAsked   map[string]time.Duration // Proxmox source name -> the steady clock when the request left that brought its last good answer (ListAskedAgo)
	sourceOrder   []string
	agentPower    map[string]*model.ComponentPower
	pveRefs       map[string]pveRef
	nodeCPU       map[string]string
	nodeInflight  map[string]string // node system id -> its cluster, while its CPU model is being fetched
	limitMu       sync.Mutex
	pveOverLimit  map[string]bool // clusters whose last answer listed more than the panel takes (logged once)
	pending       map[string]pendingNote
	asleep        map[string]time.Time // system id -> when the panel's sleep takes effect (see NoteSleep)
	clusterIDs    map[string][]string
	fingerprints  map[string]uint64
	agentFailures map[string]int
	agentQuiet    map[string]quietRow             // agent id -> the row of failed polls it is in (see quietGap)
	quietRows     uint64                          // rows of silent polls counted so far (quietRow.row)
	helperSeen    map[string]map[string]time.Time // agent id -> helper reading group -> last seen while the helper ran ("" = last running poll)
	agentInfo     map[string]AgentInfo
	unencodable   map[string]bool // systems shown as error entries, already logged (see encodeSystems)
	version       int
	snap          []model.System
	snapVer       int
	snapJSON      map[string][]byte
	snapJSONVer   int
	stateVersion  int
	healthAt      time.Time
	// agent id -> the version of its last good answer, kept over a restart (lastversion.go)
	lastVersion map[string]string
	// agent id -> when its version was last written to state.json, and whether a newer one waits to be written
	versionSavedAt map[string]time.Time
	versionUnsaved map[string]bool
	// system id -> a run saw this machine go off after its shutdown: its row is "offline", not "stale", until it
	// answers again (knownoff.go)
	knownOff map[string]bool
	// agent id -> the time of its last good answer, kept over a restart, and what state.json holds of it
	// (lastseen.go). seenWriteAt: when the times were last written; seenDrop: entries to take out of the file.
	lastSeen    map[string]time.Time
	seenSaved   map[string]time.Time
	seenWriteAt time.Time
	seenDrop    []string
	seenErr     string
	// seenWrite is held for one write of the times, from the copy to the file (flushSeen). Taken before mu,
	// never while mu is held.
	seenWrite sync.Mutex
	// agent id -> how often an agent was added in the panel under this id since the panel started (AgentEntry)
	agentAdded map[string]int
	// mach is the record of the machines that check approvals themselves (machines.go)
	mach machineStore
	// machWrite is held for one write of machines.json, from the copy of the records to the file
	// (flushMachines). Lock order: it is taken before mu, never while mu is held, and nothing but mu and the
	// file's own lock is taken under it; it is never held while the panel waits for a machine.
	machWrite sync.Mutex
	// machEmit is held while the events about that record are sent to OnMachine (emitMachines), so that they
	// arrive in the order they were decided. Lock order: taken without mu and without machWrite held; mu is
	// taken under it only to take the queue. OnMachine runs under it and must not come back into a call
	// that sends events (PinMachine, AcceptMachine, AddAgent, RemoveAgent, a poll).
	machEmit sync.Mutex
	// OnMachine, when set, hears what happens to that record: an id that came on record at first sight, a host
	// whose binding became an alarm or is in order again. It is called outside the poller's locks.
	OnMachine func(MachineEvent)
}

// New builds the poller; backend nil = the real HTTP backend.
func New(cfg *config.Panel, secrets config.Secrets, backend Backend) (*Poller, error) {
	p := &Poller{Cfg: cfg, Secrets: secrets, Clock: time.Now, born: time.Now(), rates: normalise.NewRateTracker(),
		overrides: map[string]*config.SystemOverride{}, agents: map[string]config.Agent{},
		clusters: map[string]config.Proxmox{}, systems: map[string]model.System{},
		sources: map[string]model.SourceStatus{}, sourceMono: map[string]time.Duration{}, sourceAsked: map[string]time.Duration{}, agentPower: map[string]*model.ComponentPower{},
		pveRefs: map[string]pveRef{}, nodeCPU: map[string]string{}, nodeInflight: map[string]string{}, pveOverLimit: map[string]bool{},
		pending: map[string]pendingNote{}, asleep: map[string]time.Time{}, clusterIDs: map[string][]string{}, fingerprints: map[string]uint64{},
		agentFailures: map[string]int{}, agentQuiet: map[string]quietRow{}, knownOff: map[string]bool{}, agentInfo: map[string]AgentInfo{}, helperSeen: map[string]map[string]time.Time{}, snapVer: -1, snapJSONVer: -1,
		stateVersion: -1, StartedAt: time.Now().UTC(), present: map[string]bool{}}
	for i := range cfg.Systems {
		p.overrides[cfg.Systems[i].ID] = &cfg.Systems[i]
	}
	for _, a := range cfg.Agents {
		p.agents[a.ID] = a
	}
	for _, c := range cfg.Proxmox {
		p.clusters[c.ID] = c
		p.clusterIDs[c.ID] = []string{}
	}
	p.relink()
	if backend == nil {
		hb, err := newHTTPBackend(p)
		if err != nil {
			return nil, err
		}
		backend = hb
	}
	p.Backend = backend
	iv := cfg.Intervals
	timeout := time.Duration(cfg.RequestTimeout * float64(time.Second))
	var err error
	p.Power, err = power.NewManager(cfg.PowerSources, iv.History, timeout, cfg.OfflineAfter, cfg.PowerHistoryS,
		func(aid string) *model.ComponentPower {
			p.mu.Lock()
			defer p.mu.Unlock()
			return p.agentPower[aid]
		}, scheduler.Monotonic)
	if err != nil {
		return nil, err
	}
	p.Power.ShownID = p.ShownID // a link written with either id of a machine lands on the row that is there (nodeid.go)
	p.UPS, err = ups.NewManager(cfg.UPS, iv.History, timeout, cfg.OfflineAfter, cfg.PowerHistoryS,
		filepath.Join(cfg.DataDir, "ups-events.jsonl"))
	if err != nil {
		return nil, err
	}
	p.Admin = adminstate.New(cfg.DataDir)
	p.loadLastVersions()
	p.loadLastSeen()
	p.loadMachines()
	p.Scheduler = scheduler.New(filepath.Join(cfg.DataDir, "state.json"), cfg.Boost(), nil)
	p.buildJobs()
	for _, w := range p.NodeLinkWarnings() { // an agent's node that the configuration does not decide: said once (nodeid.go)
		slog.Warn("config: " + w)
	}
	for _, l := range p.NodeLinksInferred() { // a node that was worked out from an id_prefix: never in silence
		slog.Info("config: " + l)
	}
	for _, l := range p.NodeLinksUndecided() { // every other agent whose node is not decided, and where the rows put it
		slog.Info("config: " + l)
	}
	return p, nil
}

func (p *Poller) buildJobs() {
	cfg, iv, sch := p.Cfg, p.Cfg.Intervals, p.Scheduler
	tc := cfg.RequestTimeout
	for _, c := range cfg.Proxmox {
		sch.Add(p.proxmoxJob(c))
	}
	for _, a := range cfg.Agents {
		sch.Add(p.agentJob(a))
	}
	for _, r := range p.Power.Runners { // phase 1: `agent` power sources read the latest agent metrics
		r := r
		sch.Add(&scheduler.Job{Key: "power:" + r.ID(), DefaultS: r.Interval, FloorS: r.Source.FloorInterval(),
			TimeoutCap: tc, Phase: 1,
			Fn: func(ctx context.Context, t time.Duration) float64 { r.Poll(ctx, t); return 0 },
			Systems: func() []string {
				if s := r.SystemOf(); s != "" {
					return []string{s}
				}
				return nil
			}})
	}
	for _, u := range p.UPS.Sources {
		u := u
		sch.Add(&scheduler.Job{Key: "ups:" + u.ID, DefaultS: iv.UPS, TimeoutCap: tc, Phase: 1,
			Fn: func(ctx context.Context, t time.Duration) float64 { p.UPS.PollSource(ctx, u, t); return 0 }})
	}
	sch.Add(&scheduler.Job{Key: "history", DefaultS: iv.History, Phase: 2,
		Fn: func(context.Context, time.Duration) float64 { p.Power.SampleHistory(); p.UPS.SampleHistory(); return 0 }})
}

// proxmoxJob polls one Proxmox cluster (or standalone node).
func (p *Poller) proxmoxJob(c config.Proxmox) *scheduler.Job {
	return &scheduler.Job{Key: "proxmox:" + c.ID, DefaultS: p.Cfg.Intervals.Proxmox, TimeoutCap: p.Cfg.RequestTimeout,
		Fn: func(ctx context.Context, t time.Duration) float64 { p.pollProxmox(ctx, c, t); return 0 },
		Systems: func() []string {
			p.idsMu.Lock()
			defer p.idsMu.Unlock()
			return slices.Clone(p.clusterIDs[c.ID])
		}}
}

// agentJob polls one agent. The job is filed under the row the agent's readings are shown in: the row of
// the node (or guest) it is linked to, when that row is this agent's, else the agent's own.
//
// The row of a node is one agent's (AgentFor: the first agent of the configuration whose link is that node),
// and a second agent that names the same node keeps a row of its own (buildSnapshot). Its job was still filed
// under the node's id: its own row had no poll job, so its interval could not be set and "Pause" on it showed
// "paused" while the agent was polled as before, and pausing the node's row paused both agents (second review
// of build 14, R6). (Systems is asked by the scheduler and must not take p.mu: AgentFor does not.)
func (p *Poller) agentJob(a config.Agent) *scheduler.Job {
	return &scheduler.Job{Key: "agent:" + a.ID, DefaultS: p.Cfg.Intervals.Agent, TimeoutCap: p.Cfg.RequestTimeout,
		Fn: func(ctx context.Context, t time.Duration) float64 { return p.pollAgent(ctx, a, t) },
		Systems: func() []string {
			if l := p.LinkOf(a); l != "" {
				if first, ok := p.AgentFor(l); !ok || first.ID == a.ID {
					return []string{l}
				}
			}
			return []string{a.ID}
		}}
}

func (p *Poller) now() (time.Time, float64) {
	t := p.Clock().UTC()
	return t, float64(t.UnixNano()) / 1e9
}

// mono is the steady clock (see Poller.Mono). Clock's value is used as it comes (not through UTC(), which
// drops the steady reading): with time.Now both it and born hold one, and their difference is steady time.
func (p *Poller) mono() time.Duration {
	if p.Mono != nil {
		return p.Mono()
	}
	return p.Clock().Sub(p.born)
}

func (p *Poller) degrade(s model.System, now time.Time, err string) model.System {
	// (knownOff: a machine a run saw go off is not "stale" for a minute first, see knownoff.go)
	fresh := s.LastSeen != nil && now.Sub(*s.LastSeen).Seconds() < p.Cfg.OfflineAfter && !p.knownOff[s.ID]
	if fresh {
		s.Status = "stale"
	} else {
		s.Status = "offline"
		s.CPUPercent = nil
	}
	s.Error = model.P(textsafe.ErrorText(err)) // short and clean, whatever failed (T-06)
	s.NetRxBps, s.NetTxBps = nil, nil
	// A shutdown / reboot the agent had scheduled is over once the host stops answering after its time (or
	// once it is offline): no more "shutdown in 0:00" and no Cancel for a machine that is already off.
	if s.PowerPending != nil && (s.Status == "offline" || !now.Before(s.PowerPending.At)) {
		s.PowerPending = nil
	}
	return s
}

// stamp: updated_at = last time a displayed value actually changed (Proxmox refreshes only every ~10 s).
// Caller holds p.mu.
func (p *Poller) stamp(s model.System, now time.Time) model.System {
	c := s
	c.LastSeen, c.UpdatedAt, c.Error, c.PollIntervalS, c.PollMode, c.Boosted = nil, nil, nil, nil, "", false
	b, _ := json.Marshal(c)
	h := fnv.New64a()
	h.Write(b)
	fp := h.Sum64()
	if old, ok := p.systems[s.ID]; ok && p.fingerprints[s.ID] == fp && old.UpdatedAt != nil {
		s.UpdatedAt = old.UpdatedAt
		return s
	}
	p.fingerprints[s.ID] = fp
	s.UpdatedAt = model.P(now)
	return s
}

// caller holds p.mu
func (p *Poller) source(name string, ok bool, now time.Time, latency *time.Duration, err *string) {
	prev, had := p.sources[name]
	if err != nil { // health.json, the Add host dialog and `fairy admin health` show it: short and clean (T-06)
		if c := textsafe.ErrorText(*err); c != *err {
			err = &c
		}
	}
	st := model.SourceStatus{Name: name, OK: ok, LastError: err}
	if latency != nil {
		st.LatencyMS = model.P(math.Round(latency.Seconds()*10000) / 10)
	}
	if ok {
		st.LastSuccess = model.P(now)
		p.sourceMono[name] = p.mono() // the same moment as LastSuccess, on the steady clock
	} else if had {
		st.LastSuccess = prev.LastSuccess
	}
	if !had {
		p.sourceOrder = append(p.sourceOrder, name)
	}
	p.sources[name] = st
}

// ClusterCfg returns the configured Proxmox cluster with this id.
func (p *Poller) ClusterCfg(id string) (config.Proxmox, bool) {
	p.clustersMu.RLock()
	defer p.clustersMu.RUnlock()
	c, ok := p.clusters[id]
	return c, ok
}

// Clusters is every configured Proxmox cluster: config.yaml's, then the nodes added in the panel. Do not
// modify it.
func (p *Poller) Clusters() []config.Proxmox {
	p.clustersMu.RLock()
	defer p.clustersMu.RUnlock()
	return p.Cfg.Proxmox
}

// ProxmoxSetter is implemented by backends that keep per-cluster clients (the HTTP backend).
type ProxmoxSetter interface {
	SetProxmox(c config.Proxmox) error
	ForgetProxmox(id string)
}

// AddProxmox starts polling a Proxmox node added in the panel (Add host), without a restart. The caller has
// validated it (managed.Check).
func (p *Poller) AddProxmox(c config.Proxmox) error {
	p.clustersMu.Lock()
	if _, dup := p.clusters[c.ID]; dup {
		p.clustersMu.Unlock()
		return fmt.Errorf("proxmox %s is already configured", c.ID)
	}
	if ps, ok := p.Backend.(ProxmoxSetter); ok {
		if err := ps.SetProxmox(c); err != nil {
			p.clustersMu.Unlock()
			return err
		}
	}
	p.clusters[c.ID] = c
	p.Cfg.Proxmox = append(slices.Clone(p.Cfg.Proxmox), c)
	p.clustersMu.Unlock()
	p.relink()
	p.idsMu.Lock()
	p.clusterIDs[c.ID] = []string{}
	p.idsMu.Unlock()
	p.Scheduler.Add(p.proxmoxJob(c))
	p.Scheduler.Kick("proxmox:" + c.ID)
	return nil
}

// RemoveProxmox stops polling a Proxmox node added in the panel and forgets its node and guests.
func (p *Poller) RemoveProxmox(id string) {
	p.clustersMu.Lock()
	c, ok := p.clusters[id]
	if !ok || !c.Managed {
		p.clustersMu.Unlock()
		return
	}
	delete(p.clusters, id)
	p.Cfg.Proxmox = slices.DeleteFunc(slices.Clone(p.Cfg.Proxmox), func(x config.Proxmox) bool { return x.ID == id })
	p.clustersMu.Unlock()
	p.relink()
	p.Scheduler.Remove("proxmox:" + id)
	if ps, ok := p.Backend.(ProxmoxSetter); ok {
		ps.ForgetProxmox(id)
	}
	p.idsMu.Lock()
	delete(p.clusterIDs, id)
	p.idsMu.Unlock()
	p.limitMu.Lock()
	delete(p.pveOverLimit, id)
	p.limitMu.Unlock()
	p.mu.Lock()
	var gone []string
	for sid, s := range p.systems {
		if s.Source == "proxmox" && model.Deref(s.ClusterID) == id {
			delete(p.systems, sid)
			gone = append(gone, sid)
		}
	}
	p.forgetSystemsLocked(gone)
	for ref, r := range p.pveRefs {
		if r.Cluster == id {
			delete(p.pveRefs, ref)
		}
	}
	delete(p.sources, "proxmox:"+id)
	delete(p.sourceMono, "proxmox:"+id)
	delete(p.sourceAsked, "proxmox:"+id)
	p.sourceOrder = slices.DeleteFunc(p.sourceOrder, func(x string) bool { return x == "proxmox:"+id })
	p.syncPresent()
	p.snapVer, p.snapJSONVer = -1, -1
	p.version++
	p.mu.Unlock()
}

// PVERef returns the Proxmox cluster, node, kind and vmid behind a system id (ok=false until seen).
func (p *Poller) PVERef(id string) (cluster, node, kind string, vmid *int, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.pveRefs[id]
	return r.Cluster, r.Node, r.Kind, r.VMID, ok
}

// GuestState is a guest's state in the Proxmox API's latest answer, word for word, and whether the panel has
// one for it at all. Only "stopped" means that the guest does not run: a paused or suspended guest still holds
// its memory, and "unknown" (or any other word) says nothing.
func (p *Poller) GuestState(id string) (state string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.pveRefs[id]
	return r.State, ok && r.Kind != "node"
}

// Override returns the `systems:` entry for a system id, or nil.
func (p *Poller) Override(id string) *config.SystemOverride {
	p.cfgMu.RLock()
	defer p.cfgMu.RUnlock()
	return p.overrides[id]
}

// Agents is every configured agent: config.yaml's, then the ones added in the panel. Do not modify it.
func (p *Poller) Agents() []config.Agent {
	p.cfgMu.RLock()
	defer p.cfgMu.RUnlock()
	return p.Cfg.Agents
}

// SystemsConfig is every systems: entry (config.yaml's and the ones of hosts added in the panel). Do not modify it.
func (p *Poller) SystemsConfig() []config.SystemOverride {
	p.cfgMu.RLock()
	defer p.cfgMu.RUnlock()
	return p.Cfg.Systems
}

// ConfigSnapshot is a copy of the configuration as it is now: config.yaml plus the hosts and Proxmox nodes
// added in the panel, read under the locks that guard the three lists Add host / Re-pair / Remove replace
// (agents, systems, proxmox). The lists themselves are shared (they are replaced, never changed in place): do
// not modify them.
func (p *Poller) ConfigSnapshot() *config.Panel {
	p.cfgMu.RLock()
	defer p.cfgMu.RUnlock()
	p.clustersMu.RLock()
	defer p.clustersMu.RUnlock()
	c := *p.Cfg
	return &c
}

// TokenSetter is a Backend that learns tokens of agents added at run time (the real HTTP backend).
type TokenSetter interface {
	SetAgentTokens(a config.Agent) error
	ForgetAgent(id string)
}

// AddAgent starts polling an agent added in the panel (Add host), with its Wake-on-LAN systems: entry if any,
// without a restart. The caller has validated it (managed.Check).
func (p *Poller) AddAgent(a config.Agent, o *config.SystemOverride) error {
	p.cfgMu.Lock()
	if _, dup := p.agents[a.ID]; dup {
		p.cfgMu.Unlock()
		return fmt.Errorf("agent %s is already configured", a.ID)
	}
	if ts, ok := p.Backend.(TokenSetter); ok {
		if err := ts.SetAgentTokens(a); err != nil {
			p.cfgMu.Unlock()
			return err
		}
	}
	p.agents[a.ID] = a
	if p.agentAdded == nil {
		p.agentAdded = map[string]int{}
	}
	p.agentAdded[a.ID]++
	p.Cfg.Agents = append(slices.Clone(p.Cfg.Agents), a)
	if o != nil {
		// The entry stands in the place of one with its id that is there already: that can only be the
		// machine's own timings from config.yaml (managed.Check lets nothing else through), and they stay.
		oo := *o
		oo.ManagedBy = a.ID // this host's entry: the one its Remove takes out again (RemoveAgent)
		systems := slices.Clone(p.Cfg.Systems)
		if i := slices.IndexFunc(systems, func(x config.SystemOverride) bool { return x.ID == oo.ID }); i >= 0 {
			if oo.WakeUpdate == nil {
				oo.WakeUpdate = systems[i].WakeUpdate
			}
			systems[i] = oo
		} else {
			systems = append(systems, oo)
		}
		p.Cfg.Systems = systems
		p.overrides[oo.ID] = &oo
	}
	p.cfgMu.Unlock()
	p.relink()
	// A host that is added starts with nothing on record, whatever machines.json holds for its id: the record
	// of a host that had this id before (set aside at the start, or still counted when the file could not be
	// read then) is another machine's, and it is deleted here, before the new host is asked anything (audit 6,
	// r4-x1-03; review of passkey-k, RK-01). The new host is "not on record" until an admin accepts its id.
	p.mu.Lock()
	if rec, had := p.forgetMachineLocked(a.ID, true); had {
		p.queueMachinesLocked([]MachineEvent{{AgentID: a.ID, Kind: "gone", Binding: approval.Binding{Record: rec, Known: true}}})
		p.version++
	}
	seenGone := p.forgetSeenLocked(a.ID) // a host that is added was never seen, whatever state.json holds for its id
	p.mu.Unlock()
	p.machinesChanged()
	if seenGone {
		p.flushSeen()
	}
	p.Scheduler.Add(p.agentJob(a))
	p.Scheduler.Kick("agent:" + a.ID)
	return nil
}

// AgentEntry names the entry the panel has for an agent id right now: it changes every time an agent is added
// under that id (Add host, Re-pair), so whoever works with an agent over minutes (a wake / update run) can
// tell that the entry it started with was taken out and another one put in its place.
func (p *Poller) AgentEntry(id string) int {
	p.cfgMu.RLock()
	defer p.cfgMu.RUnlock()
	return p.agentAdded[id]
}

// RemoveAgent stops polling an agent added in the panel and forgets what it reported.
func (p *Poller) RemoveAgent(id string) {
	p.cfgMu.Lock()
	a, ok := p.agents[id]
	delete(p.agents, id)
	p.Cfg.Agents = slices.DeleteFunc(slices.Clone(p.Cfg.Agents), func(x config.Agent) bool { return x.ID == id })
	// The systems: entries this host's "Add host" made (its Wake-on-LAN, its BMC power-on: ManagedBy) go with
	// it, and no other entry. It used to be "the entry under the id the host is shown as": for a host that was
	// added without Wake-on-LAN and is the agent of a node of config.yaml, that is the node's own hand-written
	// entry, and Remove, which is also the first half of every Re-pair, took its wol: and everything else in
	// it out of the running panel until the next restart (second review of build 14, R4). A hand-written entry
	// is config.yaml's and stays as it is. So does the one part of a made entry that is config.yaml's: the
	// machine's own timings (AddAgent, managed.Check), which stay for a Re-pair.
	if ok && a.Managed {
		var kept []config.SystemOverride
		systems := slices.DeleteFunc(slices.Clone(p.Cfg.Systems), func(x config.SystemOverride) bool {
			if x.ManagedBy != id {
				return false
			}
			delete(p.overrides, x.ID)
			if x.WakeUpdate != nil {
				kept = append(kept, config.SystemOverride{ID: x.ID, WakeUpdate: x.WakeUpdate})
			}
			return true
		})
		for i := range kept {
			systems = append(systems, kept[i])
			p.overrides[kept[i].ID] = &kept[i]
		}
		p.Cfg.Systems = systems
	}
	p.cfgMu.Unlock()
	p.relink()
	p.Scheduler.Remove("agent:" + id)
	if ts, ok := p.Backend.(TokenSetter); ok {
		ts.ForgetAgent(id)
	}
	p.mu.Lock()
	delete(p.systems, id)
	delete(p.agentInfo, id)
	_, kept := p.lastVersion[id]
	delete(p.lastVersion, id)
	delete(p.versionSavedAt, id)
	delete(p.versionUnsaved, id)
	delete(p.agentFailures, id)
	delete(p.agentQuiet, id)
	delete(p.knownOff, id)
	delete(p.helperSeen, id)
	delete(p.agentPower, id)
	delete(p.sources, "agent:"+id)
	delete(p.sourceMono, "agent:"+id)
	p.sourceOrder = slices.DeleteFunc(p.sourceOrder, func(x string) bool { return x == "agent:"+id })
	p.forgetMachineLocked(id, false) // a host added later under the same id starts without a machine id on record
	seenGone := p.forgetSeenLocked(id)
	p.queueMachinesLocked(p.announceLocked())
	p.syncPresent()
	p.snapVer, p.snapJSONVer = -1, -1
	p.version++
	p.mu.Unlock()
	p.machinesChanged()
	if kept { // a host added later under the same id starts without a version
		p.saveVersion(id, "", time.Time{})
	}
	if seenGone { // and was never seen
		p.flushSeen()
	}
	p.Power.UpdateRemote(id, nil, false, nil, nil)
	p.UPS.UpdateFromAgent(id, nil, false, nil, nil)
}

// Bump records that the poller's data changed, so the cached snapshots are rebuilt on the next read.
func (p *Poller) Bump() {
	p.mu.Lock()
	p.version++
	p.mu.Unlock()
}

// PollOnce runs every source once in dependency order and waits (first poll, tests).
func (p *Poller) PollOnce(ctx context.Context) {
	p.Scheduler.RunAllOnce(ctx)
	p.Scheduler.TakeDirty()
	p.Bump()
}

// SyncAdminState picks up changes the admin CLI made to state.json (poll overrides, kill switch, host power).
func (p *Poller) SyncAdminState() {
	d, ver := p.Admin.StateData()
	p.mu.Lock()
	changed := ver != p.stateVersion
	p.stateVersion = ver
	p.mu.Unlock()
	if changed {
		p.Scheduler.Reload(d["poll_overrides"])
		p.mu.Lock()
		p.snapVer, p.snapJSONVer = -1, -1
		p.mu.Unlock()
		p.Bump()
	}
}

// Run polls until ctx ends.
func (p *Poller) Run(ctx context.Context) {
	p.PollOnce(ctx)
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		p.SyncAdminState()
		p.Scheduler.Tick(ctx)
		if time.Since(p.healthAt) > 5*time.Second {
			p.healthAt = time.Now()
			p.WriteHealth()
		}
		if p.Scheduler.TakeDirty() {
			p.Bump()
		}
		select {
		case <-ctx.Done():
			// A version that waited for its minute is written before the panel stops: first what waits now (the
			// stop waits for the poller only a few seconds, and a poll that hangs must not cost it), then what
			// the polls that were on their way still brought.
			p.FlushVersions()
			p.FlushSeen() // and when each machine was last seen (lastseen.go), the same two times
			p.Scheduler.Wait()
			p.FlushVersions()
			p.FlushSeen()
			p.Power.Close()
			return
		case <-t.C:
		}
	}
}
