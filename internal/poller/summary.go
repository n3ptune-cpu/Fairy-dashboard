package poller

import (
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/config"
	"github.com/n3ptune-cpu/Fairy/internal/model"
	"github.com/n3ptune-cpu/Fairy/internal/normalise"
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
	for _, c := range p.Cfg.Proxmox {
		ct := clusterTotals(c, systems)
		if keepSource != nil && ct.NodesTotal+ct.GuestsTotal == 0 {
			continue
		}
		clusters = append(clusters, ct)
	}
	p.mu.Lock()
	srcs := []model.SourceStatus{}
	for _, k := range p.sourceOrder {
		if keepSource == nil || keepSource(k) {
			srcs = append(srcs, p.sources[k])
		}
	}
	p.mu.Unlock()
	return model.Summary{ControlDisabled: p.Admin.ControlDisabled(), GeneratedAt: time.Now().UTC(),
		Cluster: mainCluster(clusters), Clusters: clusters, Counts: counts, ByKind: byKind, Sources: srcs}
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
	return ct
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
