package poller

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

type pendingNote struct {
	at      time.Time
	pending *model.PowerPending
}

// NotePending shows a just-scheduled shutdown immediately (until the agent's own report catches up).
func (p *Poller) NotePending(id string, pending *model.PowerPending) {
	p.mu.Lock()
	p.pending[id] = pendingNote{time.Now(), pending}
	p.snapVer, p.snapJSONVer = -1, -1
	p.mu.Unlock()
	p.Bump()
}

// NoteSleep records that the panel sent a host to sleep at `at` (nil: cancelled): while it is offline after
// that it shows as asleep, until it is seen online again.
func (p *Poller) NoteSleep(id string, at *time.Time) {
	p.mu.Lock()
	if at == nil {
		delete(p.asleep, id)
	} else {
		p.asleep[id] = *at
	}
	p.snapVer, p.snapJSONVer = -1, -1
	p.mu.Unlock()
	p.Bump()
}

// asleepLocked says whether s is asleep from the panel, and forgets the note once the host is back (or after 30
// days). Caller holds p.mu.
func (p *Poller) asleepLocked(s model.System, now time.Time) bool {
	at, ok := p.asleep[s.ID]
	switch {
	case !ok:
		return false
	case now.Sub(at) > 30*24*time.Hour, s.Status == "online" && now.Sub(at) > 3*time.Minute:
		delete(p.asleep, s.ID)
		return false
	}
	return (s.Status == "offline" || s.Status == "stale") && now.After(at.Add(-5*time.Second))
}

// Snapshot is the merged, sorted system list (cached per version).
func (p *Poller) Snapshot() []model.System {
	p.mu.Lock()
	if p.snapVer == p.version && p.snap != nil {
		s := p.snap
		p.mu.Unlock()
		return s
	}
	ver := p.version
	p.mu.Unlock()
	out := p.buildSnapshot()
	p.mu.Lock()
	if p.version == ver {
		p.snap, p.snapVer = out, ver
	}
	p.mu.Unlock()
	return out
}

// SnapshotJSON returns each system's JSON by id, cached per version (shared by every SSE client). Also returns the
// id order.
func (p *Poller) SnapshotJSON() (map[string][]byte, []string) {
	snap := p.Snapshot()
	order := make([]string, len(snap))
	for i, s := range snap {
		order[i] = s.ID
	}
	p.mu.Lock()
	if p.snapJSONVer == p.version && p.snapJSON != nil {
		m := p.snapJSON
		p.mu.Unlock()
		return m, order
	}
	ver := p.version
	p.mu.Unlock()
	out := make(map[string][]byte, len(snap))
	for _, s := range snap {
		b, _ := json.Marshal(s)
		out[s.ID] = b
	}
	p.mu.Lock()
	if p.version == ver {
		p.snapJSON, p.snapJSONVer = out, ver
	}
	p.mu.Unlock()
	return out, order
}

type pollFields struct {
	interval *float64
	mode     string
	boosted  bool
}

func (p *Poller) pollFields() map[string]pollFields {
	sch := p.Scheduler
	eff := map[string]*float64{}
	for _, j := range sch.Jobs() {
		ids := j.Systems
		if ids == nil {
			continue
		}
		list := ids()
		if len(list) == 0 {
			continue
		}
		e := sch.Effective(j)
		for _, i := range list {
			cur, ok := eff[i]
			if e != nil && (cur == nil || *e < *cur) {
				eff[i] = e
			} else if !ok {
				eff[i] = nil
			}
		}
	}
	ov := sch.Overrides()
	out := map[string]pollFields{}
	for i, e := range eff {
		req, has := ov[i]
		f := pollFields{interval: e, mode: "default", boosted: sch.Boosted(i)}
		if has && req.Paused {
			f.interval, f.mode = nil, "paused"
		} else if has {
			f.mode = "override"
		}
		out[i] = f
	}
	return out
}

func laterOf(a, b *time.Time) *time.Time {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case b.After(*a):
		return b
	}
	return a
}

// buildSnapshot merges agents into the nodes they run on, sorts the list and adds the fields that come from
// other parts of the poller (linked watts, UPSes, polling mode, pending power actions, configured actions).
func (p *Poller) buildSnapshot() []model.System {
	watts, upsMap := p.Power.LinkedWatts(), p.UPS.SystemsMap()
	poll := p.pollFields()
	p.mu.Lock()
	systems := make(map[string]model.System, len(p.systems))
	for k, v := range p.systems {
		systems[k] = v
	}
	// An agent running ON a Proxmox node (links_to node-…) enriches that node instead of being a second host.
	for _, a := range p.Agents() {
		node, okN := systems[p.linkOfLocked(a)]
		agent, okA := systems[a.ID]
		if !okN || !okA || node.Kind != "node" {
			continue
		}
		systems[node.ID] = mergeAgentIntoNode(node, agent)
		delete(systems, a.ID)
	}
	pend := p.freshPendingLocked()
	now := time.Now()
	for id, s := range systems {
		if p.asleepLocked(s, now) {
			s.Asleep = true
			systems[id] = s
		}
	}
	p.mu.Unlock()
	list := make([]model.System, 0, len(systems))
	for _, s := range systems {
		list = append(list, s)
	}
	sortSystems(list)
	for i, s := range list {
		if w, ok := watts[s.ID]; ok {
			s.PowerW = model.P(w)
		}
		if u, ok := upsMap[s.ID]; ok {
			s.UPS = u
		}
		if f, ok := poll[s.ID]; ok {
			s.PollIntervalS, s.PollMode, s.Boosted = f.interval, f.mode, f.boosted
		}
		if n, ok := pend[s.ID]; ok {
			s.PowerPending = n.pending
		}
		if acts := p.powerActions(s); len(acts) > 0 {
			s.PowerActions = acts
		}
		if p.Scheduler.IsPaused(s.ID) {
			s.Status, s.Error = "paused", nil
		}
		list[i] = s
	}
	return list
}

// mergeAgentIntoNode copies what an agent on a Proxmox node measures (sensors, GPUs, hardware, power,
// pending shutdown, processes / fan control) onto the node's entry.
func mergeAgentIntoNode(node, agent model.System) model.System {
	if len(agent.Temps) > 0 {
		node.Temps = agent.Temps
	}
	if len(agent.Fans) > 0 {
		node.Fans = agent.Fans
	}
	if len(agent.Voltages) > 0 {
		node.Voltages = agent.Voltages
	}
	if len(agent.GPUs) > 0 {
		node.GPUs = agent.GPUs
	}
	if agent.ComponentPower != nil {
		node.ComponentPower = agent.ComponentPower
	}
	if agent.Battery != nil {
		node.Battery = agent.Battery
	}
	if agent.Hardware != nil {
		node.Hardware = agent.Hardware
	}
	if agent.CPUModel != nil && *agent.CPUModel != "" {
		node.CPUModel = agent.CPUModel
	}
	if agent.PowerPending != nil {
		node.PowerPending = agent.PowerPending
	}
	node.ProcessesAvailable, node.FanControl = agent.ProcessesAvailable, agent.FanControl
	if node.DWServiceURL == nil {
		node.DWServiceURL = agent.DWServiceURL
	}
	node.UpdatedAt = laterOf(node.UpdatedAt, agent.UpdatedAt)
	return node
}

// freshPendingLocked returns the power actions noted in the last 5 s and forgets older notes. Caller holds p.mu.
func (p *Poller) freshPendingLocked() map[string]pendingNote {
	pend := map[string]pendingNote{}
	for k, v := range p.pending {
		if time.Since(v.at) < 5*time.Second {
			pend[k] = v
		} else {
			delete(p.pending, k)
		}
	}
	return pend
}

// sortSystems orders the list as the UI shows it: nodes first, then by parent node, kind, name and id.
func sortSystems(list []model.System) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		an, bn := a.Kind != "node", b.Kind != "node"
		if an != bn {
			return !an
		}
		ap, bp := model.Deref(a.Parent), model.Deref(b.Parent)
		if ap != bp {
			return ap < bp
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		al, bl := strings.ToLower(a.Name), strings.ToLower(b.Name)
		if al != bl {
			return al < bl
		}
		return a.ID < b.ID
	})
}

// powerActions: configured (not necessarily currently available) power actions, for list badges / menus.
func (p *Poller) powerActions(s model.System) []string {
	out := []string{}
	c, hasC := p.clusters[model.Deref(s.ClusterID)]
	if s.Kind == "qemu" || s.Kind == "lxc" {
		if hasC && c.PowerTokenID != "" {
			out = append(out, "start", "shutdown", "reboot", "stop")
		}
		return out
	}
	a, hasA := p.AgentFor(s.ID)
	if (hasA && a.PowerControl) || (s.Kind == "node" && hasC && c.NodePower) {
		out = append(out, "shutdown", "reboot")
		if s.Kind != "node" && hasA && a.PowerControl && model.Deref(s.Sleep) {
			out = append(out, "sleep")
		}
	}
	if o := p.overrides[s.ID]; o != nil && o.Wol != nil {
		out = append(out, "wake")
	}
	if o := p.overrides[s.ID]; o != nil && o.BMCPower != nil {
		out = append(out, "bmc_on")
	}
	return out
}
