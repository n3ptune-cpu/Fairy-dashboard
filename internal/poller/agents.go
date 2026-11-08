package poller

import (
	"context"
	"log/slog"
	"math"
	"slices"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/normalise"
)

// AgentByID returns the configured agent with this id.
func (p *Poller) AgentByID(id string) (config.Agent, bool) {
	a, ok := p.agents[id]
	return a, ok
}

// RelaySystem returns the system an agent reports as (itself, or the node it enriches).
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
		msg := devices.Describe(err)
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
	p.Power.UpdateRemote(a.ID, jsonx.Objs(payload["power_sources"]), false, a.PowerLinks, a.PowerExcludeFromTotal)
	p.UPS.UpdateFromAgent(a.ID, jsonx.Objs(payload["ups"]), false, a.UPSPowers, a.UPSNames)
	return 0
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
