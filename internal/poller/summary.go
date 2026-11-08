package poller

import (
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/model"
	"github.com/n3ptune-cpu/Fairy/internal/normalise"
)

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
