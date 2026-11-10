package poller

import (
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/normalise"
)

// Summary is GET /api/summary: status counts, per-cluster totals and the state of every polled source.
func (p *Poller) Summary() model.Summary {
	return p.SummaryOf(p.Snapshot(), nil)
}

// SummaryOf is the summary of some systems only (what a viewer may see): counts and cluster totals are added up
// over those systems, clusters without any of them are left out, and so are the polled sources for which
// keepSource returns false. keepSource nil keeps every source and every cluster (the full summary).
func (p *Poller) SummaryOf(systems []model.System, keepSource func(name string) bool) model.Summary {
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
	for _, c := range p.Clusters() {
		ct := clusterTotals(c, systems)
		if keepSource != nil && ct.NodesTotal+ct.GuestsTotal == 0 {
			continue
		}
		clusters = append(clusters, ct)
	}
	totals := hostTotals(systems, p.guestAgents())
	p.mu.Lock()
	srcs := []model.SourceStatus{}
	for _, k := range p.sourceOrder {
		if keepSource == nil || keepSource(k) {
			srcs = append(srcs, p.sources[k])
		}
	}
	p.mu.Unlock()
	return model.Summary{ControlDisabled: p.Admin.ControlDisabled(), GeneratedAt: time.Now().UTC(),
		Cluster: mainCluster(clusters), Clusters: clusters, Totals: totals, Counts: counts, ByKind: byKind, Sources: srcs}
}

// clusterTotals adds up one Proxmox cluster: online nodes' cores and memory, a core-weighted CPU %, and guests.
func clusterTotals(c config.Proxmox, systems []model.System) model.ClusterTotals {
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
				cores := model.Deref(s.Cores)
				ct.Cores += cores
				if s.CPUPercent != nil {
					busy += *s.CPUPercent * float64(cores)
				}
				ct.MemUsed = normalise.AddBytes(ct.MemUsed, model.Deref(s.MemUsed))
				ct.MemTotal = normalise.AddBytes(ct.MemTotal, model.Deref(s.MemTotal))
			}
		case "qemu", "lxc":
			ct.GuestsTotal++
			if s.Status == "online" {
				ct.GuestsRunning++
			}
		}
	}
	if ct.Cores > 0 {
		ct.CPUPercent = weightedCPU(busy, ct.Cores)
	}
	return ct
}

// hostTotals adds up the hosts among systems: Proxmox nodes and agent hosts (an agent on a node is already
// merged into it; agents in skip run inside a guest, whose resources are part of its node's). Offline, stale and
// paused hosts are counted as hosts but not towards usage; CPU % is weighted by cores.
func hostTotals(systems []model.System, skip map[string]bool) model.HostTotals {
	var t model.HostTotals
	busy := 0.0
	for _, s := range systems {
		if s.Kind != "node" && (s.Kind != "agent" || skip[s.ID]) {
			continue
		}
		t.Hosts++
		if s.Status != "online" {
			continue
		}
		t.HostsOnline++
		cores := model.Deref(s.Cores)
		t.Cores += cores
		if s.CPUPercent != nil {
			busy += *s.CPUPercent * float64(cores)
		}
		t.MemUsed = normalise.AddBytes(t.MemUsed, model.Deref(s.MemUsed))
		t.MemTotal = normalise.AddBytes(t.MemTotal, model.Deref(s.MemTotal))
	}
	if t.Cores > 0 {
		t.CPUPercent = weightedCPU(busy, t.Cores)
	}
	return t
}

// weightedCPU is the core-weighted CPU % of some hosts (busy: the sum of percent x cores). It is checked after
// the sum, the division and the rounding: whatever the hosts reported, the summary holds a percentage or nothing.
func weightedCPU(busy float64, cores int) *float64 {
	return normalise.Clamp(model.P(normalise.Round(busy/float64(cores), 1)), 0, 100)
}

// guestAgents is the set of agents whose links_to is a VM or container (they stay separate entries but are not
// hosts of their own).
func (p *Poller) guestAgents() map[string]bool {
	out := map[string]bool{}
	agents := p.Agents()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range agents {
		if a.LinksTo == "" {
			continue
		}
		if s, ok := p.systems[a.LinksTo]; ok && (s.Kind == "qemu" || s.Kind == "lxc") {
			out[a.ID] = true
		}
	}
	return out
}

// mainCluster is the first non-standalone cluster (else the first one); nil without Proxmox.
func mainCluster(clusters []model.ClusterTotals) *model.ClusterTotals {
	for i := range clusters {
		if !clusters[i].Standalone {
			return &clusters[i]
		}
	}
	if len(clusters) > 0 {
		return &clusters[0]
	}
	return nil
}
