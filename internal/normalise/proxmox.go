package normalise

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

var tagSplit = regexp.MustCompile(`[;,\s]+`)

// Limits for what one Proxmox API may put into the panel (N1). A Proxmox API is not more trusted than an agent:
// a machine added with Add host as "a new Proxmox node" hands the panel its own API address, token, CA and
// certificate name, so whoever controls that machine controls what this API answers. Its answer is kept in
// memory, copied into every snapshot and sent to every open page, like an agent's, and it lists as many
// systems as it likes. So one source gets:
//
//   - at most MaxProxmoxNodes nodes. Proxmox VE clusters are built on corosync, which Proxmox supports up to
//     32 nodes; larger ones exist on enterprise hardware. 64 is twice the supported size.
//   - at most MaxProxmoxGuests VMs and containers. Proxmox sets no limit; a homelab cluster has tens to a few
//     hundred. 2000 leaves a wide margin and costs the panel about 6 MiB per source.
//   - the same text limits as an agent's answer (name, node, tags).
//
// What is over a limit is left out (the first rows are kept) and the panel logs it once while it lasts.
const (
	MaxProxmoxNodes  = 64
	MaxProxmoxGuests = 2000

	maxPVETags    = 32   // tags of one guest
	maxPVETagsRaw = 4096 // bytes of the tags text that are looked at
)

// ProxmoxLimit decides, row by row, which rows of one cluster/resources answer the panel takes. The zero value
// is ready; IncludeTemplates is the cluster's setting (a template that is not shown does not count).
type ProxmoxLimit struct {
	IncludeTemplates            bool
	nodes, guests               int
	DroppedNodes, DroppedGuests int // rows left out because their kind's limit was reached
}

// Take reports whether a row of this type is taken: nodes and guests up to their limits, nothing else
// (storage, pools and networks are not systems), and no template unless IncludeTemplates.
func (l *ProxmoxLimit) Take(typ string, template bool) bool {
	switch typ {
	case "node":
		if l.nodes >= MaxProxmoxNodes {
			l.DroppedNodes++
			return false
		}
		l.nodes++
		return true
	case "qemu", "lxc":
		if template && !l.IncludeTemplates {
			return false
		}
		if l.guests >= MaxProxmoxGuests {
			l.DroppedGuests++
			return false
		}
		l.guests++
		return true
	}
	return false
}

// LimitProxmoxRows applies a ProxmoxLimit to rows that are already decoded and returns the rows that are
// taken, with what was left out.
func LimitProxmoxRows(rows []jsonx.M, includeTemplates bool) ([]jsonx.M, ProxmoxLimit) {
	l := ProxmoxLimit{IncludeTemplates: includeTemplates}
	out := rows[:0:0]
	for _, r := range rows {
		if l.Take(jsonx.S(r, "type"), jsonx.Truthy(r["template"])) {
			out = append(out, r)
		}
	}
	return out, l
}

func splitTags(raw string) []string {
	out := []string{}
	if len(raw) > maxPVETagsRaw {
		raw = raw[:maxPVETagsRaw]
	}
	for _, t := range tagSplit.Split(raw, -1) {
		if t = ClipText(t, maxShort); t != "" && len(out) < maxPVETags {
			out = append(out, t)
		}
	}
	return out
}

// pct turns Proxmox's CPU fraction (0 to 1) into a percentage, 0 to 100. It is checked after the
// multiplication: a huge fraction times 100 is no longer a number, and such a value is dropped.
func pct(v any) *float64 {
	f := jsonx.NumV(v)
	if f == nil {
		return nil
	}
	return Clamp(model.P(Round(*f*100, 1)), 0, 100)
}

// ProxmoxOpts are the per-cluster settings for NormaliseProxmox; Prefix keeps ids unique across clusters (e.g.
// bob-qemu-100 vs qemu-100).
type ProxmoxOpts struct {
	IncludeTemplates bool
	Prefix           string
	ClusterID        *string
	Cluster          *string
	Group, Owner     *string
}

var guestStatus = map[string]string{"running": "online", "stopped": "stopped", "paused": "stopped", "suspended": "stopped"}

// NormaliseProxmox converts cluster/resources rows (nodes, VMs, containers) into systems; storage and, unless
// IncludeTemplates, templates are skipped.
func NormaliseProxmox(resources []jsonx.M, now time.Time, ts float64, rates *RateTracker, o ProxmoxOpts) []model.System {
	out := []model.System{}
	for _, r := range resources {
		rtype := jsonx.S(r, "type")
		if rtype != "node" && rtype != "qemu" && rtype != "lxc" {
			continue
		}
		if jsonx.Truthy(r["template"]) && !o.IncludeTemplates {
			continue
		}
		statusRaw := jsonx.S(r, "status")
		var sid, name, status string
		var parent *string
		var vmid *int
		if rtype == "node" {
			node := jsonx.S(r, "node")
			if node == "" {
				node = strings.TrimPrefix(jsonx.S(r, "id"), "node/")
			}
			node = ClipText(node, maxName)
			status = "offline"
			if statusRaw == "online" {
				status = "online"
			}
			sid, name = o.Prefix+"node-"+Slug(node), node
		} else {
			if vmid = VMID(r); vmid == nil { // no id, or one Proxmox itself would not accept
				continue
			}
			v := *vmid
			var ok bool
			if status, ok = guestStatus[statusRaw]; !ok {
				status = "offline" // "unknown" when its node is down
			}
			sid = fmt.Sprintf("%s%s-%d", o.Prefix, rtype, v)
			name = ClipText(jsonx.S(r, "name"), maxLong)
			if name == "" {
				name = fmt.Sprintf("%s %d", rtype, v)
			}
			parent = clipP(jsonx.Str(r, "node"), maxName)
		}
		live := status == "online"
		var rx, tx *float64
		if live {
			rx, tx = rates.Update(sid, ts, counter(r, "netin"), counter(r, "netout"), 15)
		}
		s := model.System{ID: sid, Name: name, Kind: rtype, Source: "proxmox", Parent: parent, VMID: vmid,
			Status: status, ClusterID: o.ClusterID, Cluster: o.Cluster, Group: o.Group, Owner: o.Owner,
			MemTotal: size(r, "maxmem"), DiskUsed: size(r, "disk"), DiskTotal: size(r, "maxdisk"),
			NetRxBps: rx, NetTxBps: tx, Tags: splitTags(jsonx.S(r, "tags")), PollMode: "default"}
		if live {
			s.CPUPercent = pct(r["cpu"])
			s.MemUsed = size(r, "mem")
			s.UptimeS = whole(r, "uptime", 0, MaxUnixTime)
		}
		if c := cores(r, "maxcpu"); c != nil && *c != 0 {
			s.Cores = c
		}
		if status == "online" || status == "stopped" {
			s.LastSeen = model.P(now)
		}
		model.Normalize(&s)
		out = append(out, s)
	}
	return out
}
