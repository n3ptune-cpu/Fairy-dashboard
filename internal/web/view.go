package web

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/n3ptune-cpu/Fairy/internal/access"
	"github.com/n3ptune-cpu/Fairy/internal/apierr"
	"github.com/n3ptune-cpu/Fairy/internal/model"
	"github.com/n3ptune-cpu/Fairy/internal/normalise"
	"github.com/n3ptune-cpu/Fairy/internal/power"
)

// Site of a system, power source or UPS, and where it came from.
const (
	fromPanel  = "panel"  // assigned on the Users page (or with `fairy admin sites assign`)
	fromConfig = "config" // `site:` in config.yaml
	fromNode   = "node"   // a guest inherits its Proxmox node's site
	fromLink   = "link"   // a power source / UPS follows the hosts it is attached to
)

type siteRef struct {
	Site string `json:"site"`
	From string `json:"from"`
	// Default is what the site would be without this target's own panel assignment.
	Default     string `json:"default"`
	DefaultFrom string `json:"default_from"`
}

// sites works out which site every system belongs to: the panel's assignment, else the config.yaml default
// (`systems:` entry, the agent's or linked agent's `site:`, the Proxmox cluster's `site:` for nodes), else for a
// guest its node's site. "" = no site: admins only.
func (s *Server) sites(d access.Data, snap []model.System) map[string]siteRef {
	p := s.Poller
	out := make(map[string]siteRef, len(snap))
	nodeOf := map[[2]string]string{} // (cluster id, node name) -> node system id
	for _, x := range snap {
		if x.Kind == "node" {
			nodeOf[[2]string{model.Deref(x.ClusterID), x.Name}] = x.ID
		}
	}
	configSite := func(x model.System) string {
		if o := p.Override(x.ID); o != nil && o.Site != "" {
			return o.Site
		}
		if x.Kind == "qemu" || x.Kind == "lxc" {
			return ""
		}
		if a, ok := p.AgentFor(x.ID); ok && a.Site != "" {
			return a.Site
		}
		if c, ok := p.ClusterCfg(model.Deref(x.ClusterID)); ok && x.Kind == "node" {
			return c.Site
		}
		return ""
	}
	// withAssign applies the panel assignment on top of the default.
	withAssign := func(id, def, defFrom string) siteRef {
		if def == "" {
			defFrom = ""
		}
		r := siteRef{Site: def, From: defFrom, Default: def, DefaultFrom: defFrom}
		if v := d.Assign[id]; v != "" {
			r.Site, r.From = v, fromPanel
		}
		return r
	}
	for _, x := range snap {
		if x.Kind != "qemu" && x.Kind != "lxc" {
			def := configSite(x)
			out[x.ID] = withAssign(x.ID, def, fromConfig)
		}
	}
	for _, x := range snap {
		if x.Kind != "qemu" && x.Kind != "lxc" {
			continue
		}
		def, from := configSite(x), fromConfig
		if def == "" {
			if n, ok := nodeOf[[2]string{model.Deref(x.ClusterID), model.Deref(x.Parent)}]; ok && out[n].Site != "" {
				def, from = out[n].Site, fromNode
			} else if c, ok := p.ClusterCfg(model.Deref(x.ClusterID)); ok {
				def = c.Site
			}
		}
		out[x.ID] = withAssign(x.ID, def, from)
	}
	return out
}

// view is what one grant may see, computed from the current snapshot. For a grant that sees everything (admins,
// all-sites read tokens) every method returns exactly what the poller has, unfiltered.
type view struct {
	s      *Server
	g      access.Grant
	all    bool
	data   access.Data
	snap   []model.System
	site   map[string]siteRef
	vis    map[string]bool // visible system ids
	upsVis map[string]bool // visible UPS ids
}

func (s *Server) newView(g access.Grant) *view {
	v := &view{s: s, g: g, all: g.SeesAll(), snap: s.Poller.Snapshot()}
	if v.all {
		return v
	}
	v.data = s.Access.Load()
	v.site = s.sites(v.data, v.snap)
	v.vis = map[string]bool{}
	for _, x := range v.snap {
		if g.Sees(v.site[x.ID].Site) {
			v.vis[x.ID] = true
		}
	}
	v.upsVis = map[string]bool{}
	for _, u := range s.Poller.UPS.Overview(false).UPS {
		if v.upsVisible(u) {
			v.upsVis[u.ID] = true
		}
	}
	return v
}

// agentSystem is the system an agent reports as: itself, or the Proxmox node it enriches.
func (v *view) agentSystem(agentID string) string {
	a, ok := v.s.Poller.AgentByID(agentID)
	if !ok {
		return ""
	}
	for _, x := range v.snap {
		if x.ID == agentID {
			return agentID
		}
	}
	return v.s.Poller.LinkOf(a)
}

func (v *view) agentVisible(agentID string) bool { return v.all || v.vis[v.agentSystem(agentID)] }

// followsLinks is the visibility of a power source or UPS: its own panel assignment if it has one, else the hosts
// it is attached to (visible when at least one of them is), else the host of the agent relaying it; admins only
// when none of these exist.
func (v *view) followsLinks(target string, linked []string, via *string) bool {
	if v.all {
		return true
	}
	if site := v.data.Assign[target]; site != "" {
		return v.g.Sees(site)
	}
	if len(linked) > 0 {
		return slices.ContainsFunc(linked, func(id string) bool { return v.vis[id] })
	}
	return via != nil && v.agentVisible(*via)
}

func (v *view) upsVisible(u model.UpsState) bool {
	return v.followsLinks(access.UPSPrefix+u.ID, u.Powers, u.Via)
}

// powerLinks returns the systems a power source is attached to (its readings and its `system:` in config.yaml).
func (v *view) powerLinks(st model.PowerSourceState) []string {
	var out []string
	for _, r := range st.Readings {
		if r.SystemID != nil {
			out = append(out, *r.SystemID)
		}
	}
	for _, raw := range v.s.Cfg.PowerSources {
		if id, _ := raw["id"].(string); id == st.ID {
			if sys, _ := raw["system"].(string); sys != "" {
				out = append(out, sys)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func (v *view) powerVisible(st model.PowerSourceState) bool {
	return v.followsLinks(access.PowerPrefix+st.ID, v.powerLinks(st), st.Via)
}

// clean removes references to hidden systems and UPSes from a visible system (a guest's hidden node, UPS ids).
// changed reports whether anything was removed.
func (v *view) clean(x model.System) (model.System, bool) {
	changed := false
	if x.Parent != nil && (x.Kind == "qemu" || x.Kind == "lxc") {
		seen := false
		for _, n := range v.snap {
			if n.Kind == "node" && n.Name == *x.Parent && model.Deref(n.ClusterID) == model.Deref(x.ClusterID) {
				seen = v.vis[n.ID]
				break
			}
		}
		if !seen {
			x.Parent, changed = nil, true
		}
	}
	if slices.ContainsFunc(x.UPS, func(id string) bool { return !v.upsVis[id] }) {
		x.UPS = slices.DeleteFunc(slices.Clone(x.UPS), func(id string) bool { return !v.upsVis[id] })
		changed = true
	}
	return x, changed
}

// Systems is the visible system list, in snapshot order.
func (v *view) Systems() []model.System {
	if v.all {
		return v.snap
	}
	out := make([]model.System, 0, len(v.vis))
	for _, x := range v.snap {
		if v.vis[x.ID] {
			x, _ = v.clean(x)
			out = append(out, x)
		}
	}
	return out
}

// Find returns a visible system; hidden and unknown ids get the same 404.
func (v *view) Find(id string) (model.System, error) {
	for _, x := range v.snap {
		if x.ID == id && (v.all || v.vis[id]) {
			if !v.all {
				x, _ = v.clean(x)
			}
			return x, nil
		}
	}
	return model.System{}, apierr.New(404, "system not found")
}

// Has reports whether a system id is visible.
func (v *view) Has(id string) bool {
	_, err := v.Find(id)
	return err == nil
}

// SystemsJSON is every visible system's JSON by id plus the order (the shared cache, re-encoded only for
// systems whose JSON had to be cleaned).
func (v *view) SystemsJSON() (map[string][]byte, []string) {
	snap, order := v.s.Poller.SnapshotJSON()
	if v.all {
		return snap, order
	}
	out := map[string][]byte{}
	var ids []string
	for _, x := range v.snap {
		b, ok := snap[x.ID]
		if !v.vis[x.ID] || !ok {
			continue
		}
		if cx, changed := v.clean(x); changed {
			b, _ = json.Marshal(cx)
		}
		out[x.ID] = b
		ids = append(ids, x.ID)
	}
	return out, ids
}

// Summary is the summary over the visible systems: counts and cluster totals only add up what the grant may
// see, clusters without a visible system are left out, and so is the status of sources for hidden hosts.
func (v *view) Summary() model.Summary {
	p := v.s.Poller
	if v.all {
		return p.Summary()
	}
	return p.SummaryOf(v.Systems(), func(name string) bool {
		kind, id, _ := strings.Cut(name, ":")
		switch kind {
		case "agent":
			return v.agentVisible(id)
		case "proxmox":
			return slices.ContainsFunc(v.snap, func(x model.System) bool { return v.vis[x.ID] && model.Deref(x.ClusterID) == id })
		}
		return false
	})
}

// cleanPower hides links to hidden systems and the relay agent of a hidden host.
func (v *view) cleanPower(st model.PowerSourceState) model.PowerSourceState {
	rs := make([]model.PowerReading, len(st.Readings))
	for i, r := range st.Readings {
		if r.SystemID != nil && !v.vis[*r.SystemID] {
			r.SystemID = nil
		}
		rs[i] = r
	}
	st.Readings = rs
	if st.Via != nil && !v.agentVisible(*st.Via) {
		st.Via = nil
	}
	return st
}

// Power is the visible power sources and their total.
func (v *view) Power() model.PowerOverview {
	ov := v.s.Poller.Power.Overview()
	if v.all {
		return ov
	}
	srcs := []model.PowerSourceState{}
	for _, st := range ov.Sources {
		if v.powerVisible(st) {
			srcs = append(srcs, v.cleanPower(st))
		}
	}
	ov.Sources = srcs
	ov.TotalW = power.Total(srcs)
	return ov
}

// PowerHistory is the history of the visible sources; each point's total is the sum of the visible sources that
// count in the total.
func (v *view) PowerHistory(source string) model.PowerHistory {
	p := v.s.Poller.Power
	if v.all {
		return p.HistoryView(source)
	}
	inTotal := map[string]bool{}
	for _, st := range v.Power().Sources {
		inTotal[st.ID] = st.IncludeInTotal
	}
	if _, ok := inTotal[source]; source != "" && !ok {
		source = "\x00hidden" // answered like an unknown source: every value null
	}
	h := p.HistoryView(source)
	for i, pt := range h.Points {
		srcs := map[string]*float64{}
		var total float64
		n := 0
		for id, w := range pt.Sources {
			if _, ok := inTotal[id]; !ok {
				continue
			}
			srcs[id] = w
			if w != nil && inTotal[id] {
				total += *w
				n++
			}
		}
		pt.Sources, pt.TotalW = srcs, nil
		if source != "" {
			pt.TotalW = srcs[source]
		} else if n > 0 {
			pt.TotalW = model.P(normalise.Round(total, 1))
		}
		h.Points[i] = pt
	}
	return h
}

// UPS is the visible UPSes (their lists of powered systems only show visible ones) and their history.
func (v *view) UPS(history bool) model.UpsOverview {
	ov := v.s.Poller.UPS.Overview(history)
	if v.all {
		return ov
	}
	list := []model.UpsState{}
	for _, u := range ov.UPS {
		if !v.upsVis[u.ID] {
			continue
		}
		u.Powers = slices.DeleteFunc(slices.Clone(u.Powers), func(id string) bool { return !v.vis[id] })
		if u.Via != nil && !v.agentVisible(*u.Via) {
			u.Via = nil
		}
		list = append(list, u)
	}
	for id := range ov.History {
		if !v.upsVis[id] {
			delete(ov.History, id)
		}
	}
	ov.UPS = list
	return ov
}

// UPSEvents is the UPS events after seq that concern visible UPSes, oldest first.
func (v *view) UPSEvents(seq int) []model.UpsEvent {
	evs := v.s.Poller.UPS.EventsSince(seq)
	if v.all {
		return evs
	}
	return slices.DeleteFunc(evs, func(e model.UpsEvent) bool { return !v.upsVis[e.UpsID] })
}
