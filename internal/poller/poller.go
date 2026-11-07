// Package poller: background polling of Proxmox + agents + power sources + UPSes into an in-memory cache.
//
// Every source is its own scheduler job: per-source interval, per-request timeout below the interval, never
// overlapping itself, per-host overrides / pause / boost-while-viewing.
package poller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nsxservices/fairy/internal/adminstate"
	"github.com/nsxservices/fairy/internal/apierr"
	"github.com/nsxservices/fairy/internal/config"
	"github.com/nsxservices/fairy/internal/devices"
	"github.com/nsxservices/fairy/internal/jsonx"
	"github.com/nsxservices/fairy/internal/model"
	"github.com/nsxservices/fairy/internal/normalise"
	"github.com/nsxservices/fairy/internal/power"
	"github.com/nsxservices/fairy/internal/scheduler"
	"github.com/nsxservices/fairy/internal/ups"
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

type pveRef struct {
	Cluster, Node, Kind string
	VMID                *int
}

type pendingNote struct {
	at      time.Time
	pending *model.PowerPending
}

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

	rates     *normalise.RateTracker
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
	version       int
	changed       chan struct{}
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
		agentFailures: map[string]int{}, changed: make(chan struct{}), snapVer: -1, snapJSONVer: -1,
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
		a := a
		sch.Add(&scheduler.Job{Key: "agent:" + a.ID, DefaultS: iv.Agent, TimeoutCap: tc,
			Fn: func(ctx context.Context, t time.Duration) float64 { return p.pollAgent(ctx, a, t) },
			Systems: func() []string {
				if l := p.LinkOf(a); l != "" {
					return []string{l}
				}
				return []string{a.ID}
			}})
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

// ------------------------------------------------------------- helpers
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

// ------------------------------------------------------------- Proxmox
func (p *Poller) pollProxmox(ctx context.Context, pve config.Proxmox, timeout time.Duration) {
	start := time.Now()
	src := "proxmox:" + pve.ID
	if timeout <= 0 {
		timeout = time.Duration(p.Cfg.RequestTimeout * float64(time.Second))
	}
	cctx, cancel := context.WithTimeout(ctx, timeout*time.Duration(len(pve.URLs))+500*time.Millisecond)
	data, err := p.Backend.FetchProxmox(cctx, pve, timeout)
	cancel()
	p.mu.Lock()
	defer p.bumpAfter()
	defer p.mu.Unlock()
	if err != nil {
		msg := normalise.Describe(err)
		now, _ := p.now()
		if prev, ok := p.sources[src]; !ok || prev.OK {
			slog.Warn("proxmox poll failed", "cluster", pve.ID, "error", msg)
		}
		for sid, s := range p.systems {
			if s.Source == "proxmox" && s.ClusterID != nil && *s.ClusterID == pve.ID && !p.Scheduler.IsPaused(sid) {
				p.systems[sid] = p.degrade(s, now, msg)
			}
		}
		p.source(src, false, now, nil, &msg)
		return
	}
	now, ts := p.now()
	var cluster *string
	if !pve.Standalone {
		cluster = model.P(pve.ClusterName)
	}
	prefix := pve.Prefix()
	list := normalise.NormaliseProxmox(data, now, ts, p.rates, normalise.ProxmoxOpts{
		IncludeTemplates: pve.IncludeTemplates, Prefix: prefix, ClusterID: model.P(pve.ID), Cluster: cluster,
		Group: pve.Group, Owner: pve.Owner})
	for _, r := range data {
		typ, node := jsonx.S(r, "type"), jsonx.S(r, "node")
		if node == "" {
			continue
		}
		if typ == "node" {
			p.pveRefs[prefix+"node-"+normalise.Slug(node)] = pveRef{pve.ID, node, "node", nil}
		} else if (typ == "qemu" || typ == "lxc") && jsonx.Num(r, "vmid") != nil {
			v := int(*jsonx.Num(r, "vmid"))
			p.pveRefs[fmt.Sprintf("%s%s-%d", prefix, typ, v)] = pveRef{pve.ID, node, typ, &v}
		}
	}
	fresh := map[string]model.System{}
	ids := make([]string, 0, len(list))
	for _, s := range list {
		s = normalise.ApplyOverride(s, p.overrides[s.ID])
		if old, ok := p.systems[s.ID]; ok && s.LastSeen == nil { // keep last_seen for guests whose node is down
			s.LastSeen = old.LastSeen
		}
		if s.Kind == "node" {
			if m, ok := p.nodeCPU[s.ID]; ok {
				s.CPUModel = model.P(m)
			} else if s.Status == "online" && !p.nodeInflight[s.ID] {
				p.nodeInflight[s.ID] = true
				go p.fetchNodeCPU(pve, s.ID)
			}
		}
		linked := false
		for _, a := range p.Cfg.Agents {
			if p.linkOfLocked(a) == s.ID {
				linked = true
			}
		}
		s.ProcessesAvailable = linked
		if old, ok := p.systems[s.ID]; ok && p.Scheduler.IsPaused(s.ID) {
			s = old // paused: keep the frozen values
		} else {
			s = p.stamp(s, now)
		}
		fresh[s.ID] = s
		ids = append(ids, s.ID)
	}
	for sid, v := range p.systems { // drop deleted guests
		if v.Source == "proxmox" && v.ClusterID != nil && *v.ClusterID == pve.ID {
			delete(p.systems, sid)
		}
	}
	for k, v := range fresh {
		p.systems[k] = v
	}
	p.idsMu.Lock()
	p.clusterIDs[pve.ID] = ids
	p.idsMu.Unlock()
	p.syncPresent()
	lat := time.Since(start)
	p.source(src, true, now, &lat, nil)
}

// fetchNodeCPU: CPU model from /nodes/{node}/status (cpuinfo.model). The PVE API exposes no DMI/board model;
// that comes from an agent running on the node. Fetched once per node, best effort.
func (p *Poller) fetchNodeCPU(pve config.Proxmox, sid string) {
	defer func() {
		p.mu.Lock()
		delete(p.nodeInflight, sid)
		p.mu.Unlock()
	}()
	p.mu.Lock()
	ref, ok := p.pveRefs[sid]
	p.mu.Unlock()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(p.Cfg.RequestTimeout*float64(time.Second)))
	defer cancel()
	st, err := p.Backend.FetchNodeStatus(ctx, pve, ref.Node)
	if err != nil {
		slog.Debug("node status", "system", sid, "error", normalise.Describe(err))
		return
	}
	if m := jsonx.S(jsonx.Obj(st, "cpuinfo"), "model"); m != "" {
		p.mu.Lock()
		p.nodeCPU[sid] = strings.TrimSpace(m)
		p.mu.Unlock()
	}
}

// PVEPower posts with the separate power token (never the read-only one).
func (p *Poller) PVEPower(ctx context.Context, clusterID, path string, data url.Values) (any, error) {
	return p.Backend.PVEPower(ctx, clusterID, path, data)
}

// ------------------------------------------------------------- lookups used by power control
func (p *Poller) ClusterCfg(id string) (config.Proxmox, bool) {
	c, ok := p.clusters[id]
	return c, ok
}

func (p *Poller) PVERef(id string) (cluster, node, kind string, vmid *int, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.pveRefs[id]
	return r.Cluster, r.Node, r.Kind, r.VMID, ok
}

func (p *Poller) Override(id string) *config.SystemOverride { return p.overrides[id] }

func (p *Poller) AgentByID(id string) (config.Agent, bool) {
	a, ok := p.agents[id]
	return a, ok
}

// RelaySystem: the system an agent reports as (itself, or the node it enriches).
func (p *Poller) RelaySystem(agentID string) *model.System {
	a, ok := p.agents[agentID]
	if !ok {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.systems[agentID]; ok {
		return &s
	}
	if s, ok := p.systems[p.linkOfLocked(a)]; ok {
		return &s
	}
	return nil
}

// NotePending shows a just-scheduled shutdown immediately (until the agent's own report catches up).
func (p *Poller) NotePending(id string, pending *model.PowerPending) {
	p.mu.Lock()
	p.pending[id] = pendingNote{time.Now(), pending}
	p.snapVer, p.snapJSONVer = -1, -1
	p.mu.Unlock()
	p.Bump()
}

// LinkOf: the Proxmox node (or guest) this agent runs on: links_to, or `node:` matched by node name.
func (p *Poller) LinkOf(a config.Agent) string { return p.linkOfLocked(a) }

// syncPresent refreshes the id set used by LinkOf. Caller holds p.mu.
func (p *Poller) syncPresent() {
	p.idsMu.Lock()
	defer p.idsMu.Unlock()
	clear(p.present)
	for k := range p.systems {
		p.present[k] = true
	}
}

// linkOfLocked only takes idsMu (safe with or without p.mu held).
func (p *Poller) linkOfLocked(a config.Agent) string {
	if a.LinksTo != "" {
		return a.LinksTo
	}
	if a.Node != "" {
		var ids []string
		for _, c := range p.Cfg.Proxmox {
			ids = append(ids, c.Prefix()+"node-"+normalise.Slug(a.Node))
		}
		p.idsMu.Lock()
		defer p.idsMu.Unlock()
		for _, i := range ids {
			if p.present[i] {
				return i
			}
		}
		if len(ids) > 0 {
			return ids[0]
		}
	}
	return ""
}

// ------------------------------------------------------------- agents
// pollAgent returns a back-off delay (s) while the agent is unreachable, so offline hosts aren't hit every second.
func (p *Poller) pollAgent(ctx context.Context, a config.Agent, timeout time.Duration) float64 {
	start := time.Now()
	name := "agent:" + a.ID
	if timeout <= 0 {
		timeout = time.Duration(p.Cfg.RequestTimeout * float64(time.Second))
	}
	cctx, cancel := context.WithTimeout(ctx, timeout+500*time.Millisecond)
	payload, err := p.Backend.FetchAgentMetrics(cctx, a, timeout)
	cancel()
	defer p.Bump()
	if err != nil {
		msg := normalise.Describe(err)
		p.mu.Lock()
		now, _ := p.now()
		if prev, ok := p.sources[name]; !ok || prev.OK {
			slog.Info("agent poll failed", "agent", a.ID, "error", msg)
		}
		if prev, ok := p.systems[a.ID]; ok {
			p.systems[a.ID] = p.degrade(prev, now, msg)
		} else {
			s := model.System{ID: a.ID, Name: a.DisplayName(), Kind: "agent", Source: "agent", Status: "offline",
				Group: a.Group, Owner: a.Owner, Tags: slices.Clone(a.Tags), Error: model.P(msg), PollMode: "default"}
			if a.DWServiceURL != "" {
				s.DWServiceURL = model.P(a.DWServiceURL)
			}
			model.Normalize(&s)
			p.systems[a.ID] = s
			p.syncPresent()
		}
		p.agentPower[a.ID] = nil
		p.source(name, false, now, nil, &msg)
		p.agentFailures[a.ID]++
		n := p.agentFailures[a.ID]
		p.mu.Unlock()
		p.Power.UpdateRemote(a.ID, nil, true, a.PowerLinks, a.PowerExcludeFromTotal)
		p.UPS.UpdateFromAgent(a.ID, nil, true, a.UPSPowers, a.UPSNames)
		if n >= 2 {
			return math.Min(10, math.Pow(2, float64(n-2)))
		}
		return 0
	}
	now, ts := p.now()
	sys := normalise.NormaliseAgent(a, payload, now, ts, p.rates)
	p.mu.Lock()
	delete(p.agentFailures, a.ID)
	p.systems[a.ID] = p.stamp(sys, now)
	p.syncPresent()
	p.agentPower[a.ID] = sys.ComponentPower
	lat := time.Since(start)
	p.source(name, true, now, &lat, nil)
	p.mu.Unlock()
	p.Power.UpdateRemote(a.ID, objs(payload["power_sources"]), false, a.PowerLinks, a.PowerExcludeFromTotal)
	p.UPS.UpdateFromAgent(a.ID, objs(payload["ups"]), false, a.UPSPowers, a.UPSNames)
	return 0
}

func objs(v any) []map[string]any {
	l, _ := v.([]any)
	out := []map[string]any{}
	for _, x := range l {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func (p *Poller) FetchProcesses(ctx context.Context, a config.Agent, sid string, limit int, sortBy string) (model.ProcessList, error) {
	return p.Backend.FetchProcesses(ctx, a, sid, limit, sortBy)
}

func (p *Poller) AgentRequest(ctx context.Context, a config.Agent, method, path string, body any) (int, jsonx.M, error) {
	return p.Backend.AgentRequest(ctx, a, method, path, body)
}

// AgentFor: the agent reporting for a system id (itself or linked).
func (p *Poller) AgentFor(id string) (config.Agent, bool) {
	if a, ok := p.agents[id]; ok {
		return a, true
	}
	for _, a := range p.Cfg.Agents {
		if p.linkOfLocked(a) == id {
			return a, true
		}
	}
	return config.Agent{}, false
}

// ------------------------------------------------------------- loop
func (p *Poller) bumpAfter() { p.Bump() }

// Bump publishes a new version to SSE waiters.
func (p *Poller) Bump() {
	p.mu.Lock()
	p.version++
	close(p.changed)
	p.changed = make(chan struct{})
	p.mu.Unlock()
}

// Version returns the current version and a channel closed on the next change.
func (p *Poller) Version() (int, <-chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.version, p.changed
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

// WriteHealth writes <data_dir>/health.json for `fairy admin health` (the CLI never needs a web login).
func (p *Poller) WriteHealth() {
	snap := p.Snapshot()
	counts := map[string]int{"online": 0, "stale": 0, "offline": 0, "stopped": 0, "paused": 0}
	ids := []string{}
	for _, s := range snap {
		if _, ok := counts[s.Status]; ok {
			counts[s.Status]++
		}
		ids = append(ids, s.ID)
	}
	p.mu.Lock()
	srcs := []model.SourceStatus{}
	for _, k := range p.sourceOrder {
		srcs = append(srcs, p.sources[k])
	}
	ver := p.version
	p.mu.Unlock()
	ps := []map[string]any{}
	for _, s := range p.Power.Overview().Sources {
		ps = append(ps, map[string]any{"id": s.ID, "status": s.Status, "error": s.Error})
	}
	us := []map[string]any{}
	for _, u := range p.UPS.Overview(false).UPS {
		us = append(us, map[string]any{"id": u.ID, "status": u.Status, "error": u.Error})
	}
	jobs := map[string]any{}
	for _, j := range p.Scheduler.Jobs() {
		runs, skipped := j.Stats()
		jobs[j.Key] = map[string]any{"runs": runs, "skipped": skipped, "effective_s": p.Scheduler.Effective(j),
			"last_ms": p.Scheduler.LastMS(j)}
	}
	data := map[string]any{"written_at": time.Now().UTC().Format(time.RFC3339Nano),
		"started_at": p.StartedAt.Format(time.RFC3339Nano), "version": ver,
		"control_disabled": p.Admin.ControlDisabled(), "systems": counts, "system_ids": ids, "sources": srcs,
		"power_sources": ps, "ups": us, "jobs": jobs}
	b, _ := json.MarshalIndent(data, "", " ")
	path := filepath.Join(p.Cfg.DataDir, "health.json")
	tmp := path + ".tmp"
	if err := os.MkdirAll(p.Cfg.DataDir, 0o700); err != nil {
		return
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		slog.Debug("health.json", "error", err)
		return
	}
	_ = os.Rename(tmp, path)
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

// ------------------------------------------------------------- per-host polling control
func (p *Poller) HasSystem(id string) bool {
	for _, s := range p.Snapshot() {
		if s.ID == id {
			return true
		}
	}
	return false
}

func (p *Poller) SetPoll(id string, v *scheduler.Override) error {
	err := p.Scheduler.SetOverride(id, v)
	p.mu.Lock()
	p.snapVer, p.snapJSONVer = -1, -1
	if v == nil || !v.Paused {
		delete(p.fingerprints, id)
	}
	p.mu.Unlock()
	p.Bump()
	return err
}

func fmtG(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

func (p *Poller) PollStatus(id string) model.PollStatus {
	sch := p.Scheduler
	req, has := sch.Overrides()[id]
	jobs := sch.Describe(id)
	var effs []float64
	for _, j := range jobs {
		if j.EffectiveS != nil {
			effs = append(effs, *j.EffectiveS)
		}
	}
	var notes []string
	for _, j := range jobs {
		if j.Clamped {
			kind, _, _ := strings.Cut(j.Job, ":")
			notes = append(notes, kind+" source polls at most every "+fmtG(j.FloorS)+" s")
		}
	}
	shared := false
	for _, j := range jobs {
		shared = shared || j.Shared
	}
	if shared && has && !req.Paused && len(effs) > 0 && req.Seconds > slices.Min(effs) {
		notes = append(notes, "shares one Proxmox API call with its cluster, which runs at the fastest rate any member asks for")
	}
	ps := model.PollStatus{SystemID: id, Mode: "default", Boosted: sch.Boosted(id), BoostEnabled: sch.BoostEnabled, Jobs: jobs}
	if has {
		if req.Paused {
			ps.Mode = "paused"
		} else {
			ps.Mode = "override"
			ps.RequestedS = model.P(req.Seconds)
		}
	}
	if len(effs) > 0 {
		ps.EffectiveS = model.P(slices.Min(effs))
	}
	if len(notes) > 0 {
		ps.Note = model.P(strings.Join(notes, "; "))
	}
	return ps
}

// ------------------------------------------------------------- views
// Snapshot is the merged, sorted system list (cached per version).
func (p *Poller) Snapshot() []model.System {
	p.mu.Lock()
	if p.snapVer == p.version && p.snap != nil {
		s := p.snap
		p.mu.Unlock()
		return s
	}
	ver := p.version
	p.mu.Unlock()
	out := p.buildSnapshot()
	p.mu.Lock()
	if p.version == ver {
		p.snap, p.snapVer = out, ver
	}
	p.mu.Unlock()
	return out
}

// SnapshotJSON: id -> JSON, cached per version (shared by every SSE client). Also returns the id order.
func (p *Poller) SnapshotJSON() (map[string][]byte, []string) {
	snap := p.Snapshot()
	order := make([]string, len(snap))
	for i, s := range snap {
		order[i] = s.ID
	}
	p.mu.Lock()
	if p.snapJSONVer == p.version && p.snapJSON != nil {
		m := p.snapJSON
		p.mu.Unlock()
		return m, order
	}
	ver := p.version
	p.mu.Unlock()
	out := make(map[string][]byte, len(snap))
	for _, s := range snap {
		b, _ := json.Marshal(s)
		out[s.ID] = b
	}
	p.mu.Lock()
	if p.version == ver {
		p.snapJSON, p.snapJSONVer = out, ver
	}
	p.mu.Unlock()
	return out, order
}

type pollFields struct {
	interval *float64
	mode     string
	boosted  bool
}

func (p *Poller) pollFields() map[string]pollFields {
	sch := p.Scheduler
	eff := map[string]*float64{}
	for _, j := range sch.Jobs() {
		ids := j.Systems
		if ids == nil {
			continue
		}
		list := ids()
		if len(list) == 0 {
			continue
		}
		e := sch.Effective(j)
		for _, i := range list {
			cur, ok := eff[i]
			if e != nil && (cur == nil || *e < *cur) {
				eff[i] = e
			} else if !ok {
				eff[i] = nil
			}
		}
	}
	ov := sch.Overrides()
	out := map[string]pollFields{}
	for i, e := range eff {
		req, has := ov[i]
		f := pollFields{interval: e, mode: "default", boosted: sch.Boosted(i)}
		if has && req.Paused {
			f.interval, f.mode = nil, "paused"
		} else if has {
			f.mode = "override"
		}
		out[i] = f
	}
	return out
}

func laterOf(a, b *time.Time) *time.Time {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case b.After(*a):
		return b
	}
	return a
}

func (p *Poller) buildSnapshot() []model.System {
	watts, upsMap := p.Power.LinkedWatts(), p.UPS.SystemsMap()
	poll := p.pollFields()
	p.mu.Lock()
	systems := make(map[string]model.System, len(p.systems))
	for k, v := range p.systems {
		systems[k] = v
	}
	// An agent running ON a Proxmox node (links_to node-…) enriches that node instead of being a second host.
	for _, a := range p.Cfg.Agents {
		node, okN := systems[p.linkOfLocked(a)]
		agent, okA := systems[a.ID]
		if !okN || !okA || node.Kind != "node" {
			continue
		}
		if len(agent.Temps) > 0 {
			node.Temps = agent.Temps
		}
		if len(agent.Fans) > 0 {
			node.Fans = agent.Fans
		}
		if len(agent.Voltages) > 0 {
			node.Voltages = agent.Voltages
		}
		if len(agent.GPUs) > 0 {
			node.GPUs = agent.GPUs
		}
		if agent.ComponentPower != nil {
			node.ComponentPower = agent.ComponentPower
		}
		if agent.Hardware != nil {
			node.Hardware = agent.Hardware
		}
		if agent.CPUModel != nil && *agent.CPUModel != "" {
			node.CPUModel = agent.CPUModel
		}
		if agent.PowerPending != nil {
			node.PowerPending = agent.PowerPending
		}
		node.ProcessesAvailable, node.FanControl = agent.ProcessesAvailable, agent.FanControl
		if node.DWServiceURL == nil {
			node.DWServiceURL = agent.DWServiceURL
		}
		node.UpdatedAt = laterOf(node.UpdatedAt, agent.UpdatedAt)
		systems[node.ID] = node
		delete(systems, a.ID)
	}
	pend := map[string]pendingNote{}
	for k, v := range p.pending {
		if time.Since(v.at) < 5*time.Second {
			pend[k] = v
		} else {
			delete(p.pending, k)
		}
	}
	p.mu.Unlock()
	list := make([]model.System, 0, len(systems))
	for _, s := range systems {
		list = append(list, s)
	}
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		an, bn := a.Kind != "node", b.Kind != "node"
		if an != bn {
			return !an
		}
		ap, bp := deref(a.Parent), deref(b.Parent)
		if ap != bp {
			return ap < bp
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		al, bl := strings.ToLower(a.Name), strings.ToLower(b.Name)
		if al != bl {
			return al < bl
		}
		return a.ID < b.ID
	})
	for i, s := range list {
		if w, ok := watts[s.ID]; ok {
			s.PowerW = model.P(w)
		}
		if u, ok := upsMap[s.ID]; ok {
			s.UPS = u
		}
		if f, ok := poll[s.ID]; ok {
			s.PollIntervalS, s.PollMode, s.Boosted = f.interval, f.mode, f.boosted
		}
		if n, ok := pend[s.ID]; ok {
			s.PowerPending = n.pending
		}
		if acts := p.powerActions(s); len(acts) > 0 {
			s.PowerActions = acts
		}
		if p.Scheduler.IsPaused(s.ID) {
			s.Status, s.Error = "paused", nil
		}
		list[i] = s
	}
	return list
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// powerActions: configured (not necessarily currently available) power actions, for list badges / menus.
func (p *Poller) powerActions(s model.System) []string {
	out := []string{}
	c, hasC := p.clusters[deref(s.ClusterID)]
	if s.Kind == "qemu" || s.Kind == "lxc" {
		if hasC && c.PowerTokenID != "" {
			out = append(out, "start", "shutdown", "reboot", "stop")
		}
		return out
	}
	a, hasA := p.AgentFor(s.ID)
	if (hasA && a.PowerControl) || (s.Kind == "node" && hasC && c.NodePower) {
		out = append(out, "shutdown", "reboot")
	}
	if o := p.overrides[s.ID]; o != nil && o.Wol != nil {
		out = append(out, "wake")
	}
	return out
}

func (p *Poller) Summary() model.Summary {
	systems := p.Snapshot()
	counts := model.Counts{Total: len(systems)}
	byKind := map[string]int{}
	for _, s := range systems {
		switch s.Status {
		case "online":
			counts.Online++
		case "offline":
			counts.Offline++
		case "stopped":
			counts.Stopped++
		case "stale":
			counts.Stale++
		case "paused":
			counts.Paused++
		}
		byKind[s.Kind]++
	}
	clusters := []model.ClusterTotals{}
	for _, c := range p.Cfg.Proxmox {
		ct := model.ClusterTotals{ID: c.ID, Name: c.ClusterName, Standalone: c.Standalone, Group: c.Group, Owner: c.Owner}
		busy := 0.0
		for _, s := range systems {
			if s.ClusterID == nil || *s.ClusterID != c.ID {
				continue
			}
			switch s.Kind {
			case "node":
				ct.NodesTotal++
				if s.Status == "online" {
					ct.NodesOnline++
					cores := 0
					if s.Cores != nil {
						cores = *s.Cores
					}
					ct.Cores += cores
					if s.CPUPercent != nil {
						busy += *s.CPUPercent * float64(cores)
					}
					if s.MemUsed != nil {
						ct.MemUsed += *s.MemUsed
					}
					if s.MemTotal != nil {
						ct.MemTotal += *s.MemTotal
					}
				}
			case "qemu", "lxc":
				ct.GuestsTotal++
				if s.Status == "online" {
					ct.GuestsRunning++
				}
			}
		}
		if ct.Cores > 0 {
			ct.CPUPercent = model.P(normalise.Round(busy/float64(ct.Cores), 1))
		}
		clusters = append(clusters, ct)
	}
	var main *model.ClusterTotals
	for i := range clusters {
		if !clusters[i].Standalone {
			main = &clusters[i]
			break
		}
	}
	if main == nil && len(clusters) > 0 {
		main = &clusters[0]
	}
	p.mu.Lock()
	srcs := []model.SourceStatus{}
	for _, k := range p.sourceOrder {
		srcs = append(srcs, p.sources[k])
	}
	p.mu.Unlock()
	return model.Summary{ControlDisabled: p.Admin.ControlDisabled(), GeneratedAt: time.Now().UTC(), Cluster: main,
		Clusters: clusters, Counts: counts, ByKind: byKind, Sources: srcs}
}

// ===================================================================== real HTTP backend
type httpBackend struct {
	p             *Poller
	pve           map[string]*http.Client
	pveSecrets    map[string]string
	powerSecrets  map[string]string
	agentTokens   map[string]string
	controlTokens map[string]string
	agents        *http.Client
}

func newHTTPBackend(p *Poller) (*httpBackend, error) {
	cfg := p.Cfg
	b := &httpBackend{p: p, pve: map[string]*http.Client{}, pveSecrets: map[string]string{},
		powerSecrets: map[string]string{}, agentTokens: map[string]string{}, controlTokens: map[string]string{},
		agents: devices.LANClient(0)} // never via a proxy; per-request timeouts via context
	// Resolve every agent token at startup: a missing/short token is a config error.
	for _, a := range cfg.Agents {
		t, err := config.EnvSecret(a.TokenEnv, "agent "+a.ID, 24)
		if err != nil {
			return nil, err
		}
		b.agentTokens[a.ID] = t
		if env := a.ControlEnv(); env != "" {
			ct, err := config.EnvSecret(env, "agent "+a.ID+" control", 24)
			if err != nil {
				return nil, err
			}
			if ct == t {
				return nil, fmt.Errorf("agent %s: control token must differ from the read token", a.ID)
			}
			b.controlTokens[a.ID] = ct
		}
	}
	for _, c := range cfg.Proxmox { // one client per cluster: each has its own CA / verify setting and token
		tc, err := power.TLSConfig(c.Verify(), c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("proxmox %s: %v", c.ID, err)
		}
		b.pve[c.ID] = devices.TLSClient(0, tc)
		if c.TokenSecretEnv != "" {
			s, err := config.EnvSecret(c.TokenSecretEnv, "proxmox "+c.ID, 1)
			if err != nil {
				return nil, err
			}
			b.pveSecrets[c.ID] = s
		} else {
			b.pveSecrets[c.ID] = p.Secrets.ProxmoxTokenSecret
		}
		if c.PowerTokenID != "" && c.PowerTokenSecretEnv != "" { // separate token: VM.PowerMgmt only
			s, err := config.EnvSecret(c.PowerTokenSecretEnv, "proxmox "+c.ID+" power", 1)
			if err != nil {
				return nil, err
			}
			b.powerSecrets[c.ID] = s
		}
	}
	return b, nil
}

func getJSON(ctx context.Context, c *http.Client, u string, hdr map[string]string, timeout time.Duration) (jsonx.M, int, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, devices.Errf("invalid url")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	var out jsonx.M
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		_ = json.Unmarshal(body, &out)
		return out, resp.StatusCode, &devices.HTTPStatusError{Code: resp.StatusCode}
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, resp.StatusCode, devices.Errf("invalid response")
		}
	}
	return out, resp.StatusCode, nil
}

func (b *httpBackend) pveAuth(c config.Proxmox) map[string]string {
	return map[string]string{"Authorization": "PVEAPIToken=" + c.TokenID + "=" + b.pveSecrets[c.ID]}
}

func (b *httpBackend) FetchProxmox(ctx context.Context, pve config.Proxmox, timeout time.Duration) ([]jsonx.M, error) {
	var last error = errors.New("no proxmox urls")
	for _, base := range pve.URLs { // failover across cluster nodes
		d, _, err := getJSON(ctx, b.pve[pve.ID], strings.TrimRight(base, "/")+"/api2/json/cluster/resources", b.pveAuth(pve), timeout)
		if err != nil {
			last = err
			continue
		}
		rows, ok := d["data"].([]any)
		if !ok {
			last = devices.Errf("invalid response")
			continue
		}
		return objs(rows), nil
	}
	return nil, last
}

func (b *httpBackend) FetchNodeStatus(ctx context.Context, pve config.Proxmox, node string) (jsonx.M, error) {
	u := strings.TrimRight(pve.URLs[0], "/") + "/api2/json/nodes/" + url.PathEscape(node) + "/status"
	d, _, err := getJSON(ctx, b.pve[pve.ID], u, b.pveAuth(pve), 0)
	if err != nil {
		return nil, err
	}
	return jsonx.Obj(d, "data"), nil
}

// PVEPower: POST with the separate power token. Fails over across the cluster's URLs only on connection errors,
// so an action is never sent twice.
func (b *httpBackend) PVEPower(ctx context.Context, clusterID, path string, data url.Values) (any, error) {
	pve := b.p.clusters[clusterID]
	secret := b.powerSecrets[clusterID]
	if secret == "" || pve.PowerTokenID == "" {
		return nil, apierr.New(409, "no power token configured")
	}
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	safe := strings.Join(segs, "/")
	last := "no urls"
	timeout := time.Duration((b.p.Cfg.RequestTimeout + 6) * float64(time.Second))
	for _, base := range pve.URLs {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		req, _ := http.NewRequestWithContext(cctx, http.MethodPost, strings.TrimRight(base, "/")+"/api2/json"+safe,
			strings.NewReader(data.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "PVEAPIToken="+pve.PowerTokenID+"="+secret)
		resp, err := b.pve[clusterID].Do(req)
		if err != nil {
			cancel()
			var ne interface{ Timeout() bool }
			if errors.As(err, &ne) && ne.Timeout() {
				return nil, apierr.New(502, "proxmox: timeout") // may have been delivered: never resend
			}
			last = normalise.Describe(err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		if resp.StatusCode == 403 {
			return nil, apierr.New(502, "proxmox refused the power token (needs VM.PowerMgmt / Sys.PowerMgmt)")
		}
		if resp.StatusCode >= 400 {
			return nil, apierr.Errf(502, "proxmox: http %d", resp.StatusCode)
		}
		var out jsonx.M
		_ = json.Unmarshal(body, &out)
		return out["data"], nil
	}
	return nil, apierr.New(502, "proxmox: "+last)
}

func (b *httpBackend) agentURL(a config.Agent, path string) string {
	return strings.TrimRight(a.URL, "/") + path
}

func (b *httpBackend) FetchAgentMetrics(ctx context.Context, a config.Agent, timeout time.Duration) (jsonx.M, error) {
	d, _, err := getJSON(ctx, b.agents, b.agentURL(a, "/metrics"), map[string]string{"Authorization": "Bearer " + b.agentTokens[a.ID]}, timeout)
	return d, err
}

func (b *httpBackend) FetchProcesses(ctx context.Context, a config.Agent, sid string, limit int, sortBy string) (model.ProcessList, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}, "sort": {sortBy}}
	d, code, err := getJSON(ctx, b.agents, b.agentURL(a, "/processes?"+q.Encode()),
		map[string]string{"Authorization": "Bearer " + b.agentTokens[a.ID]}, time.Duration((b.p.Cfg.RequestTimeout+2)*float64(time.Second)))
	if code == 403 && jsonx.S(d, "code") == "processes_disabled" {
		return model.ProcessList{SystemID: sid, DisabledByHost: true, Sort: sortBy, Processes: []model.Process{}}, nil
	}
	if err != nil {
		return model.ProcessList{}, err
	}
	return DecodeProcessList(d, sid)
}

// DecodeProcessList validates an agent /processes payload.
func DecodeProcessList(d jsonx.M, sid string) (model.ProcessList, error) {
	b, _ := json.Marshal(d)
	var pl model.ProcessList
	if err := json.Unmarshal(b, &pl); err != nil {
		return pl, devices.Errf("invalid response")
	}
	pl.SystemID = sid
	if pl.Sort == "" {
		pl.Sort = "cpu"
	}
	model.Normalize(&pl)
	return pl, nil
}

func (b *httpBackend) AgentRequest(ctx context.Context, a config.Agent, method, path string, body any) (int, jsonx.M, error) {
	tok := b.agentTokens[a.ID]
	if method == http.MethodPost {
		tok = b.controlTokens[a.ID]
	}
	var rd io.Reader
	if body != nil {
		bb, _ := json.Marshal(body)
		rd = strings.NewReader(string(bb))
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration((b.p.Cfg.RequestTimeout+8)*float64(time.Second))) // ipmi via powershell is slow
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, method, b.agentURL(a, path), rd)
	if err != nil {
		return 0, nil, devices.Errf("invalid url")
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.agents.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	out := jsonx.M{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return resp.StatusCode, nil, devices.Errf("invalid response")
		}
	}
	return resp.StatusCode, out, nil
}
