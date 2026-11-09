package poller

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/normalise"
)

type pveRef struct {
	Cluster, Node, Kind string
	VMID                *int
}

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
	defer p.Bump()
	defer p.mu.Unlock()
	if err != nil {
		p.proxmoxFailedLocked(pve, src, proxmoxErr(err))
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
	p.recordPVERefsLocked(pve.ID, prefix, data)
	fresh := map[string]model.System{}
	ids := make([]string, 0, len(list))
	for _, s := range list {
		s = p.prepareProxmoxSystemLocked(pve, s, now)
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

// proxmoxFailedLocked marks the cluster's systems stale / offline (paused ones keep their frozen values) and
// logs the first failure of a run of failures. Caller holds p.mu.
func (p *Poller) proxmoxFailedLocked(pve config.Proxmox, src, msg string) {
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
}

// recordPVERefsLocked remembers which cluster, node and vmid each system id maps to (for power actions).
// Caller holds p.mu.
func (p *Poller) recordPVERefsLocked(clusterID, prefix string, data []jsonx.M) {
	for _, r := range data {
		typ, node := jsonx.S(r, "type"), jsonx.S(r, "node")
		if node == "" {
			continue
		}
		if typ == "node" {
			p.pveRefs[prefix+"node-"+normalise.Slug(node)] = pveRef{clusterID, node, "node", nil}
		} else if (typ == "qemu" || typ == "lxc") && jsonx.Num(r, "vmid") != nil {
			v := int(*jsonx.Num(r, "vmid"))
			p.pveRefs[fmt.Sprintf("%s%s-%d", prefix, typ, v)] = pveRef{clusterID, node, typ, &v}
		}
	}
}

// prepareProxmoxSystemLocked applies the `systems:` override, keeps last_seen for guests whose node is down,
// fills in the node's CPU model (fetched once, in the background), marks whether an agent provides processes,
// and keeps the frozen values of a paused system. Caller holds p.mu.
func (p *Poller) prepareProxmoxSystemLocked(pve config.Proxmox, s model.System, now time.Time) model.System {
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
	for _, a := range p.Agents() {
		if p.linkOfLocked(a) == s.ID {
			linked = true
		}
	}
	s.ProcessesAvailable = linked
	if old, ok := p.systems[s.ID]; ok && p.Scheduler.IsPaused(s.ID) {
		return old // paused: keep the frozen values
	}
	return p.stamp(s, now)
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
		slog.Debug("node status", "system", sid, "error", devices.Describe(err))
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

// proxmoxErr describes a Proxmox poll error; a certificate issued for another name says how to fix it.
func proxmoxErr(err error) string {
	var he x509.HostnameError
	if errors.As(err, &he) && he.Certificate != nil {
		names := strings.Join(he.Certificate.DNSNames, ", ")
		return fmt.Sprintf("the node's certificate is not issued for %q (it lists %s): run `pvecm updatecerts --force` and "+
			"`systemctl restart pveproxy` on the node, or set tls_server_name to one of those names", he.Host, names)
	}
	return devices.Describe(err)
}
