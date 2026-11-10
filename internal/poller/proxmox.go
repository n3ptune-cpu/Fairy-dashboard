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

// maxNodeLookups is how many CPU-model requests (one per node, once) one cluster has running at the same time.
// The rest wait for a later poll. A cluster that lists many nodes, or whose API never answers such a request,
// holds two connections, and never keeps another cluster's nodes waiting.
const maxNodeLookups = 2

// maxCPUModel is how much of a node's CPU model text is kept.
const maxCPUModel = 256

// noteProxmoxLimit logs, once while it lasts, that a cluster's answer listed more nodes or guests than the
// panel takes (normalise.MaxProxmoxNodes / MaxProxmoxGuests), and logs when it fits again.
func (p *Poller) noteProxmoxLimit(clusterID string, l normalise.ProxmoxLimit) {
	over := l.DroppedNodes > 0 || l.DroppedGuests > 0
	p.limitMu.Lock()
	was := p.pveOverLimit[clusterID]
	if over {
		p.pveOverLimit[clusterID] = true
	} else {
		delete(p.pveOverLimit, clusterID)
	}
	p.limitMu.Unlock()
	switch {
	case over && !was:
		slog.Warn("the Proxmox API lists more than the panel takes from one source: the rest is NOT shown "+
			"(the first rows are kept). If this cluster really is that large, the limits are in internal/normalise/proxmox.go",
			"cluster", clusterID, "nodes_left_out", l.DroppedNodes, "max_nodes", normalise.MaxProxmoxNodes,
			"guests_left_out", l.DroppedGuests, "max_guests", normalise.MaxProxmoxGuests)
	case !over && was:
		slog.Info("the Proxmox API's list fits the panel's limits again", "cluster", clusterID)
	}
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
	// The limits hold whatever backend fetched the rows (the HTTP backend applies them while it decodes, so a
	// long list is never in memory; this is the same rule again for rows that are already here).
	data, limit := normalise.LimitProxmoxRows(data, pve.IncludeTemplates)
	if limit.DroppedNodes > 0 || limit.DroppedGuests > 0 {
		p.noteProxmoxLimit(pve.ID, limit)
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
	refs := p.recordPVERefsLocked(pve.ID, prefix, data)
	fresh := map[string]model.System{}
	ids := make([]string, 0, len(list))
	for _, s := range list {
		s = p.prepareProxmoxSystemLocked(pve, s, now)
		fresh[s.ID] = s
		ids = append(ids, s.ID)
	}
	var gone []string
	for sid, v := range p.systems { // drop deleted guests
		if v.Source == "proxmox" && v.ClusterID != nil && *v.ClusterID == pve.ID {
			delete(p.systems, sid)
			if _, still := fresh[sid]; !still {
				gone = append(gone, sid)
			}
		}
	}
	// ... and everything the poller kept for them. A cluster that lists new ids on every poll would otherwise
	// leave an entry behind for each id it ever named, in four maps that only grew.
	p.forgetSystemsLocked(gone)
	for ref, r := range p.pveRefs {
		if r.Cluster == pve.ID && !refs[ref] {
			delete(p.pveRefs, ref)
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

// forgetSystemsLocked drops what the poller keeps per system id for systems that are gone: the fingerprint of
// the last values, a node's CPU model, the network counters. Caller holds p.mu.
func (p *Poller) forgetSystemsLocked(ids []string) {
	for _, sid := range ids {
		delete(p.fingerprints, sid)
		delete(p.nodeCPU, sid)
	}
	p.rates.Forget(ids...)
}

// recordPVERefsLocked remembers which cluster, node and vmid each system id maps to (for power actions), and
// returns the ids it recorded: the caller forgets the cluster's other ones. Caller holds p.mu.
func (p *Poller) recordPVERefsLocked(clusterID, prefix string, data []jsonx.M) map[string]bool {
	refs := make(map[string]bool, len(data))
	for _, r := range data {
		typ, node := jsonx.S(r, "type"), jsonx.S(r, "node")
		if node == "" || len(node) > maxNodeName {
			continue // no node, or a name no Proxmox node can have: there is nothing a power action could name
		}
		var id string
		if typ == "node" {
			id = prefix + "node-" + normalise.Slug(node)
			p.pveRefs[id] = pveRef{clusterID, node, "node", nil}
		} else if vmid := normalise.VMID(r); (typ == "qemu" || typ == "lxc") && vmid != nil { // the same ids as NormaliseProxmox
			id = fmt.Sprintf("%s%s-%d", prefix, typ, *vmid)
			p.pveRefs[id] = pveRef{clusterID, node, typ, vmid}
		}
		if id != "" {
			refs[id] = true
		}
	}
	return refs
}

// maxNodeName is the longest node name kept for power actions and status requests (a host name label is at
// most 63 bytes; the limit leaves room for a full name).
const maxNodeName = 253

// prepareProxmoxSystemLocked applies the `systems:` override, keeps last_seen for guests whose node is down,
// fills in the node's CPU model (fetched once, in the background), marks whether an agent provides processes,
// and keeps the frozen values of a paused system. Caller holds p.mu.
func (p *Poller) prepareProxmoxSystemLocked(pve config.Proxmox, s model.System, now time.Time) model.System {
	// Override takes cfgMu inside p.mu, the same order as p.Agents() below.
	s = normalise.ApplyOverride(s, p.Override(s.ID))
	if old, ok := p.systems[s.ID]; ok && s.LastSeen == nil { // keep last_seen for guests whose node is down
		s.LastSeen = old.LastSeen
	}
	if s.Kind == "node" {
		if m, ok := p.nodeCPU[s.ID]; ok {
			s.CPUModel = model.P(m)
		} else if _, busy := p.nodeInflight[s.ID]; s.Status == "online" && !busy && p.nodeLookupsLocked(pve.ID) < maxNodeLookups {
			p.nodeInflight[s.ID] = pve.ID
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

// nodeLookupsLocked is how many CPU-model requests of this cluster are running. Caller holds p.mu.
func (p *Poller) nodeLookupsLocked(clusterID string) int {
	n := 0
	for _, c := range p.nodeInflight {
		if c == clusterID {
			n++
		}
	}
	return n
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
		if _, still := p.systems[sid]; still { // the node may be gone by now: keep nothing for it
			p.nodeCPU[sid] = normalise.ClipText(strings.TrimSpace(m), maxCPUModel)
		}
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
