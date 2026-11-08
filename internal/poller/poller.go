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
	"math"
	"net/url"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/normalise"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/power"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/scheduler"
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
	Cfg       *config.Panel
	Secrets   config.Secrets
	Clock     func() time.Time
	Backend   Backend
	Power     *power.Manager
	UPS       *ups.Manager
	Admin     *adminstate.State
	Scheduler *scheduler.Scheduler
	StartedAt time.Time

	rates *normalise.RateTracker
	// cfgMu guards agents, overrides and the Cfg.Agents / Cfg.Systems slices, which hosts added or removed in
	// the panel (Add host) change at run time. The slices are replaced, never changed in place.
	cfgMu     sync.RWMutex
	overrides map[string]*config.SystemOverride
	agents    map[string]config.Agent
	clusters  map[string]config.Proxmox

	// idsMu guards clusterIDs and present (read by scheduler callbacks, which must never take mu).
	idsMu   sync.Mutex
	present map[string]bool

	mu            sync.Mutex
	systems       map[string]model.System
	sources       map[string]model.SourceStatus
	sourceOrder   []string
	agentPower    map[string]*model.ComponentPower
	pveRefs       map[string]pveRef
	nodeCPU       map[string]string
	nodeInflight  map[string]bool
	pending       map[string]pendingNote
	clusterIDs    map[string][]string
	fingerprints  map[string]uint64
	agentFailures map[string]int
	agentInfo     map[string]AgentInfo
	version       int
	snap          []model.System
	snapVer       int
	snapJSON      map[string][]byte
	snapJSONVer   int
	stateVersion  int
	healthAt      time.Time
}

// New builds the poller; backend nil = the real HTTP backend.
func New(cfg *config.Panel, secrets config.Secrets, backend Backend) (*Poller, error) {
	p := &Poller{Cfg: cfg, Secrets: secrets, Clock: time.Now, rates: normalise.NewRateTracker(),
		overrides: map[string]*config.SystemOverride{}, agents: map[string]config.Agent{},
		clusters: map[string]config.Proxmox{}, systems: map[string]model.System{},
		sources: map[string]model.SourceStatus{}, agentPower: map[string]*model.ComponentPower{},
		pveRefs: map[string]pveRef{}, nodeCPU: map[string]string{}, nodeInflight: map[string]bool{},
		pending: map[string]pendingNote{}, clusterIDs: map[string][]string{}, fingerprints: map[string]uint64{},
		agentFailures: map[string]int{}, agentInfo: map[string]AgentInfo{}, snapVer: -1, snapJSONVer: -1,
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
	p.UPS, err = ups.NewManager(cfg.UPS, iv.History, timeout, cfg.OfflineAfter, cfg.PowerHistoryS,
		filepath.Join(cfg.DataDir, "ups-events.jsonl"))
	if err != nil {
		return nil, err
	}
	p.Admin = adminstate.New(cfg.DataDir)
	p.Scheduler = scheduler.New(filepath.Join(cfg.DataDir, "state.json"), cfg.Boost(), nil)
	p.buildJobs()
	return p, nil
}

func (p *Poller) buildJobs() {
	cfg, iv, sch := p.Cfg, p.Cfg.Intervals, p.Scheduler
	tc := cfg.RequestTimeout
	for _, c := range cfg.Proxmox {
		c := c
		sch.Add(&scheduler.Job{Key: "proxmox:" + c.ID, DefaultS: iv.Proxmox, TimeoutCap: tc,
			Fn: func(ctx context.Context, t time.Duration) float64 { p.pollProxmox(ctx, c, t); return 0 },
			Systems: func() []string {
				p.idsMu.Lock()
				defer p.idsMu.Unlock()
				return slices.Clone(p.clusterIDs[c.ID])
			}})
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

// agentJob polls one agent.
func (p *Poller) agentJob(a config.Agent) *scheduler.Job {
	return &scheduler.Job{Key: "agent:" + a.ID, DefaultS: p.Cfg.Intervals.Agent, TimeoutCap: p.Cfg.RequestTimeout,
		Fn: func(ctx context.Context, t time.Duration) float64 { return p.pollAgent(ctx, a, t) },
		Systems: func() []string {
			if l := p.LinkOf(a); l != "" {
				return []string{l}
			}
			return []string{a.ID}
		}}
}

func (p *Poller) now() (time.Time, float64) {
	t := p.Clock().UTC()
	return t, float64(t.UnixNano()) / 1e9
}

func (p *Poller) degrade(s model.System, now time.Time, err string) model.System {
	fresh := s.LastSeen != nil && now.Sub(*s.LastSeen).Seconds() < p.Cfg.OfflineAfter
	if fresh {
		s.Status = "stale"
	} else {
		s.Status = "offline"
		s.CPUPercent = nil
	}
	s.Error = model.P(err)
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
	st := model.SourceStatus{Name: name, OK: ok, LastError: err}
	if latency != nil {
		st.LatencyMS = model.P(math.Round(latency.Seconds()*10000) / 10)
	}
	if ok {
		st.LastSuccess = model.P(now)
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
	c, ok := p.clusters[id]
	return c, ok
}

// PVERef returns the Proxmox cluster, node, kind and vmid behind a system id (ok=false until seen).
func (p *Poller) PVERef(id string) (cluster, node, kind string, vmid *int, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.pveRefs[id]
	return r.Cluster, r.Node, r.Kind, r.VMID, ok
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
	p.Cfg.Agents = append(slices.Clone(p.Cfg.Agents), a)
	if o != nil {
		oo := *o
		p.Cfg.Systems = append(slices.Clone(p.Cfg.Systems), oo)
		p.overrides[oo.ID] = &oo
	}
	p.cfgMu.Unlock()
	p.Scheduler.Add(p.agentJob(a))
	p.Scheduler.Kick("agent:" + a.ID)
	return nil
}

// RemoveAgent stops polling an agent added in the panel and forgets what it reported.
func (p *Poller) RemoveAgent(id string) {
	p.cfgMu.Lock()
	a, ok := p.agents[id]
	delete(p.agents, id)
	p.Cfg.Agents = slices.DeleteFunc(slices.Clone(p.Cfg.Agents), func(x config.Agent) bool { return x.ID == id })
	shown := id
	if ok {
		if l := p.linkOfLocked(a); l != "" {
			shown = l
		}
	}
	if o := p.overrides[shown]; o != nil && ok && a.Managed {
		delete(p.overrides, shown)
		p.Cfg.Systems = slices.DeleteFunc(slices.Clone(p.Cfg.Systems), func(x config.SystemOverride) bool { return x.ID == shown })
	}
	p.cfgMu.Unlock()
	p.Scheduler.Remove("agent:" + id)
	if ts, ok := p.Backend.(TokenSetter); ok {
		ts.ForgetAgent(id)
	}
	p.mu.Lock()
	delete(p.systems, id)
	delete(p.agentInfo, id)
	delete(p.agentFailures, id)
	delete(p.agentPower, id)
	delete(p.sources, "agent:"+id)
	p.sourceOrder = slices.DeleteFunc(p.sourceOrder, func(x string) bool { return x == "agent:"+id })
	p.syncPresent()
	p.snapVer, p.snapJSONVer = -1, -1
	p.version++
	p.mu.Unlock()
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
			p.Scheduler.Wait()
			p.Power.Close()
			return
		case <-t.C:
		}
	}
}
