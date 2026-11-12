package poller

import (
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/normalise"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
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

// Snapshot is the merged, sorted system list (cached per version). Every system in it can be written as JSON:
// see encodeSystems.
func (p *Poller) Snapshot() []model.System {
	list, _ := p.snapshot()
	return list
}

// SnapshotJSON returns each system's JSON by id, cached per version (shared by every SSE client). Also returns the
// id order. No entry is ever empty or missing: a system that cannot be written is in it as its error entry.
func (p *Poller) SnapshotJSON() (map[string][]byte, []string) {
	snap, js := p.snapshot()
	order := make([]string, len(snap))
	for i, s := range snap {
		order[i] = s.ID
	}
	return js, order
}

// snapshot returns the system list and each system's JSON, built together and cached per version.
func (p *Poller) snapshot() ([]model.System, map[string][]byte) {
	p.mu.Lock()
	if p.snapVer == p.version && p.snap != nil && p.snapJSONVer == p.version && p.snapJSON != nil {
		s, js := p.snap, p.snapJSON
		p.mu.Unlock()
		return s, js
	}
	ver := p.version
	p.mu.Unlock()
	list, js := p.encodeSystems(p.buildSnapshot())
	p.mu.Lock()
	if p.version == ver {
		p.snap, p.snapVer, p.snapJSON, p.snapJSONVer = list, ver, js, ver
	}
	p.mu.Unlock()
	return list, js
}

// StatusError is the status of a system whose data cannot be shown (see errorEntry).
const StatusError = "error"

// encodeSystems writes every system as JSON, one at a time. Numbers and dates are checked where they enter the
// panel, so this should always work; if a system still cannot be written (a value that is not a number, a date
// outside the years 0 to 9999), that one system is replaced by its error entry. The others are not affected, the
// list never fails as a whole, and the machine stays visible instead of vanishing. Logged once per system while
// it lasts.
func (p *Poller) encodeSystems(list []model.System) ([]model.System, map[string][]byte) {
	js := make(map[string][]byte, len(list))
	bad := map[string]string{}
	for i, s := range list {
		e, b, err := EncodeSystem(s)
		if err != nil {
			bad[s.ID] = err.Error()
			list[i] = e
		}
		js[s.ID] = b
	}
	p.mu.Lock()
	for id, why := range bad {
		if !p.unencodable[id] {
			slog.Warn("a system's data cannot be written as JSON; it is shown with status \"error\" until that changes",
				"system", id, "error", why)
		}
	}
	p.unencodable = map[string]bool{}
	for id := range bad {
		p.unencodable[id] = true
	}
	p.mu.Unlock()
	return list, js
}

// EncodeSystem writes one system as JSON. A system that cannot be written is replaced by its error entry (see
// errorEntry): the entry and its JSON are returned together with the reason, so the caller can log it. The JSON
// is never empty. The web layer uses this for the copies it changes per viewer; encodeSystems for the snapshot.
func EncodeSystem(s model.System) (model.System, []byte, error) {
	b, err := json.Marshal(s)
	if err == nil {
		return s, b, nil
	}
	e := errorEntry(s, err)
	if b, err2 := json.Marshal(e); err2 == nil {
		return e, b, err
	}
	// cannot happen: the entry holds texts and config values only
	e = model.System{ID: s.ID, Name: s.ID, Kind: s.Kind, Source: s.Source, Status: StatusError,
		Error: model.P("this system's data cannot be shown"), PollMode: "default"}
	model.Normalize(&e)
	b, _ = json.Marshal(e)
	return e, b, err
}

// errorEntry is what the API shows for a system whose data cannot be written as JSON: a complete System (every
// field the page reads is there, lists are empty, values are null) with status "error" and the reason in
// `error`. It keeps what identifies the system and what comes from the panel's own configuration (name, kind,
// parent, cluster, site labels, links, polling mode, configured power actions) and drops everything measured.
func errorEntry(s model.System, err error) model.System {
	why := "invalid data"
	var notNumber *json.UnsupportedValueError
	switch {
	case errors.As(err, &notNumber):
		why = "a value that is not a number: " + normalise.ClipText(notNumber.Str, 40)
	case err != nil && strings.Contains(err.Error(), "year outside of range"):
		why = "a date outside the years 0 to 9999"
	case err != nil:
		why = normalise.ClipText(err.Error(), 160)
	}
	e := model.System{ID: s.ID, Name: s.Name, Kind: s.Kind, Source: s.Source, Parent: s.Parent, ParentID: s.ParentID, ClusterID: s.ClusterID,
		Cluster: s.Cluster, VMID: s.VMID, Status: StatusError,
		Error: model.P("this system sent data the panel cannot show (" + why + ")"),
		Group: s.Group, Owner: s.Owner, Tags: s.Tags, DWServiceURL: s.DWServiceURL, UPS: s.UPS, CustomModel: s.CustomModel,
		ProcessesAvailable: s.ProcessesAvailable, FanControl: s.FanControl, PowerActions: s.PowerActions,
		PollIntervalS: normalise.Finite(s.PollIntervalS, 0, normalise.MaxUnixTime), PollMode: s.PollMode, Boosted: s.Boosted,
		BMCRelayTargets: s.BMCRelayTargets, PowerHelperReason: s.PowerHelperReason}
	for _, t := range []struct{ from, to **time.Time }{{&s.LastSeen, &e.LastSeen}, {&s.UpdatedAt, &e.UpdatedAt}} {
		if v := *t.from; v != nil && v.Year() >= 1970 && v.Year() <= 9999 {
			*t.to = v
		}
	}
	model.Normalize(&e)
	return e
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
	models := p.Admin.HostModels() // the model names set in the panel (state.json), by system id
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
		// stale or offline, and the latest poll met a machine that runs (lifeSign): the host's page says so
		if q, ok := p.agentQuiet[id]; ok && q.sign != "" && s.Kind == "agent" && (s.Status == "offline" || s.Status == "stale") {
			s.MachineOn = q.sign
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
		if m, ok := models[s.ID]; ok && (s.Kind == "node" || s.Kind == "agent") {
			s.CustomModel = &m
		}
		if acts := p.powerActions(s); len(acts) > 0 {
			s.PowerActions = acts
		}
		if p.Scheduler.IsPaused(s.ID) {
			s.Status, s.Error = "paused", nil
		}
		if s.Error != nil { // the boundary: no snapshot hands out a long or uncleaned error text (T-06)
			if c := textsafe.ErrorText(*s.Error); c != *s.Error {
				s.Error = &c
			}
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
	c, hasC := p.ClusterCfg(model.Deref(s.ClusterID))
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
	if o := p.Override(s.ID); o != nil { // under cfgMu: Add host / Re-pair / Remove change the overrides
		if o.Wol != nil {
			out = append(out, "wake")
		}
		if o.BMCPower != nil {
			out = append(out, "bmc_on")
		}
	}
	return out
}
