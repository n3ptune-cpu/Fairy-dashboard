package poller

import (
	"context"
	"log/slog"
	"math"
	"slices"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/release"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/normalise"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/ups"
)

// AgentByID returns the configured agent with this id.
func (p *Poller) AgentByID(id string) (config.Agent, bool) {
	p.cfgMu.RLock()
	defer p.cfgMu.RUnlock()
	a, ok := p.agents[id]
	return a, ok
}

// RelaySystem returns the system an agent reports as (itself, or the node it enriches).
func (p *Poller) RelaySystem(agentID string) *model.System {
	a, ok := p.AgentByID(agentID)
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

// LinkOf returns the Proxmox node (or guest) this agent runs on: links_to, or `node:` matched by node name.
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
		for _, c := range p.Clusters() {
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
		msg := textsafe.ErrorText(devices.Describe(err))
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
	info := agentInfoOf(payload, now)
	p.mu.Lock()
	if sys.Helper != nil {
		normalise.ApplyHelperPause(a, &sys, normalise.HelperUnavailable(payload), p.helperSeenLocked(a.ID, sys, now))
	} else {
		delete(p.helperSeen, a.ID)
	}
	p.agentInfo[a.ID] = info
	delete(p.agentFailures, a.ID)
	p.systems[a.ID] = p.stamp(sys, now)
	p.syncPresent()
	p.agentPower[a.ID] = sys.ComponentPower
	lat := time.Since(start)
	p.source(name, true, now, &lat, nil)
	p.mu.Unlock()
	p.Power.UpdateRemote(a.ID, jsonx.Objs(payload["power_sources"]), false, a.PowerLinks, a.PowerExcludeFromTotal)
	p.UPS.UpdateFromAgent(a.ID, ups.AgentRows(payload["ups"]), false, a.UPSPowers, a.UPSNames)
	return 0
}

// helperSeenFor is how long a helper reading counts as "this host has it" after it was last seen while the
// helper ran (a sensor that stopped answering a while before the helper was stopped is not announced as paused).
const helperSeenFor = 10 * time.Minute

// helperSeenLocked records which helper readings the host has while its helper runs, and returns the groups
// seen recently enough (nil: the panel never saw the helper running since it started). Caller holds p.mu.
func (p *Poller) helperSeenLocked(id string, s model.System, now time.Time) map[string]bool {
	seen := p.helperSeen[id]
	if s.Helper.State == "running" {
		if seen == nil {
			seen = map[string]time.Time{}
			p.helperSeen[id] = seen
		}
		for _, g := range normalise.HelperGroupsIn(s) {
			seen[g] = now
		}
		seen[""] = now // the last poll with the helper running
	}
	if seen == nil {
		return nil
	}
	out := map[string]bool{}
	for g, at := range seen {
		if g != "" && seen[""].Sub(at) <= helperSeenFor {
			out[g] = true
		}
	}
	return out
}

// AgentInfo is what the Updates page shows about an agent, from its last good /metrics answer.
type AgentInfo struct {
	Version    string         // agent_version
	SelfUpdate map[string]any // the agent's self_update block (nil: off or an older agent)
	Enabled    bool           // features.self_update
	SeenAt     time.Time
	Online     bool // the latest poll succeeded
}

func agentInfoOf(payload jsonx.M, now time.Time) AgentInfo {
	info := AgentInfo{SeenAt: now}
	if v, ok := payload["agent_version"].(string); ok && release.ValidVersion(v) {
		info.Version = v
	}
	if su, ok := payload["self_update"].(map[string]any); ok {
		info.SelfUpdate = SelfUpdateBlock(su)
	}
	if f, ok := payload["features"].(map[string]any); ok {
		info.Enabled, _ = f["self_update"].(bool)
	}
	return info
}

var updateStates = map[string]bool{"idle": true, "downloading": true, "applying": true, "failed": true, "unavailable": true}

// maxUnixTime bounds the timestamps an agent reports (seconds; year 2286), so the page can always show them.
const maxUnixTime = 1e10

// SelfUpdateBlock keeps only the fields of an agent's self_update block that the Updates page shows, with the
// right types and bounded lengths: an agent (or whoever answers in its place) cannot break the page with an
// odd payload. Unknown states read as "unknown".
func SelfUpdateBlock(su map[string]any) map[string]any {
	text := func(m map[string]any, k string, n int) (string, bool) {
		v, ok := m[k].(string)
		if !ok {
			return "", false
		}
		return normalise.ClipText(v, n), true
	}
	when := func(m map[string]any, k string) (float64, bool) {
		f := jsonx.Num(m, k)
		return model.Deref(f), f != nil && *f >= 0 && *f <= maxUnixTime
	}
	out := map[string]any{"state": "unknown"}
	if st, _ := su["state"].(string); updateStates[st] {
		out["state"] = st
	}
	if v, ok := text(su, "message", 300); ok {
		out["message"] = v
	}
	if v, ok := su["target"].(string); ok && release.ValidVersion(v) {
		out["target"] = v
	}
	if v, ok := when(su, "since"); ok {
		out["since"] = v
	}
	if last, ok := su["last"].(map[string]any); ok {
		l := map[string]any{"ok": jsonx.Bool(last, "ok", false)}
		for _, k := range []string{"from", "to"} {
			if v, ok := last[k].(string); ok && release.ValidVersion(v) {
				l[k] = v
			}
		}
		if v, ok := text(last, "id", 64); ok {
			l["id"] = v
		}
		if v, ok := text(last, "message", 300); ok {
			l["message"] = v
		}
		if v, ok := when(last, "at"); ok {
			l["at"] = v
		}
		out["last"] = l
	}
	return out
}

// AgentInfo returns the version and self-update state from the agent's last good poll.
func (p *Poller) AgentInfo(id string) (AgentInfo, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	i, ok := p.agentInfo[id]
	_, failing := p.agentFailures[id]
	i.Online = ok && !failing
	return i, ok
}

// FetchProcesses asks an agent for its top processes.
func (p *Poller) FetchProcesses(ctx context.Context, a config.Agent, sid string, limit int, sortBy string) (model.ProcessList, error) {
	return p.Backend.FetchProcesses(ctx, a, sid, limit, sortBy)
}

// AgentRequest calls an agent endpoint (GET with the read token, POST with the control token).
func (p *Poller) AgentRequest(ctx context.Context, a config.Agent, method, path string, body any) (int, jsonx.M, error) {
	return p.Backend.AgentRequest(ctx, a, method, path, body)
}

// AgentFor returns the agent reporting for a system id (itself or linked).
func (p *Poller) AgentFor(id string) (config.Agent, bool) {
	if a, ok := p.AgentByID(id); ok {
		return a, true
	}
	for _, a := range p.Agents() {
		if p.linkOfLocked(a) == id {
			return a, true
		}
	}
	return config.Agent{}, false
}
