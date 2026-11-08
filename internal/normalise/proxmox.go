package normalise

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy/internal/model"
)

var tagSplit = regexp.MustCompile(`[;,\s]+`)

func splitTags(raw string) []string {
	out := []string{}
	for _, t := range tagSplit.Split(raw, -1) {
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

func pct(v any) *float64 {
	f := jsonx.NumV(v)
	if f == nil {
		return nil
	}
	return model.P(Round(*f*100, 1))
}

// ProxmoxOpts are the per-cluster settings for NormaliseProxmox; Prefix keeps ids unique across clusters (e.g.
// straw-qemu-100 vs qemu-100).
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
			status = "offline"
			if statusRaw == "online" {
				status = "online"
			}
			sid, name = o.Prefix+"node-"+Slug(node), node
		} else {
			vf := jsonx.Num(r, "vmid")
			if vf == nil {
				if s := jsonx.S(r, "vmid"); s != "" {
					if n, err := strconv.Atoi(s); err == nil {
						f := float64(n)
						vf = &f
					}
				}
			}
			if vf == nil {
				continue
			}
			v := int(*vf)
			vmid = &v
			var ok bool
			if status, ok = guestStatus[statusRaw]; !ok {
				status = "offline" // "unknown" when its node is down
			}
			sid = fmt.Sprintf("%s%s-%d", o.Prefix, rtype, v)
			name = jsonx.S(r, "name")
			if name == "" {
				name = fmt.Sprintf("%s %d", rtype, v)
			}
			parent = jsonx.Str(r, "node")
		}
		live := status == "online"
		var rx, tx *float64
		if live {
			rx, tx = rates.Update(sid, ts, jsonx.Int(r, "netin"), jsonx.Int(r, "netout"), 15)
		}
		s := model.System{ID: sid, Name: name, Kind: rtype, Source: "proxmox", Parent: parent, VMID: vmid,
			Status: status, ClusterID: o.ClusterID, Cluster: o.Cluster, Group: o.Group, Owner: o.Owner,
			MemTotal: jsonx.Int(r, "maxmem"), DiskUsed: jsonx.Int(r, "disk"), DiskTotal: jsonx.Int(r, "maxdisk"),
			NetRxBps: rx, NetTxBps: tx, Tags: splitTags(jsonx.S(r, "tags")), PollMode: "default"}
		if live {
			s.CPUPercent = pct(r["cpu"])
			s.MemUsed = jsonx.Int(r, "mem")
			s.UptimeS = jsonx.Int(r, "uptime")
		}
		if c := jsonx.IntP(r, "maxcpu"); c != nil && *c != 0 {
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
