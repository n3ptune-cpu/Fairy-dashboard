package poller

import (
	"fmt"
	"log/slog"
	"slices"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// "Edit host", the machine itself: its name, its Wake-on-LAN details and its own two run timings can be
// changed in the panel (adminstate.HostEdit, state.json). The configuration (config.yaml's entry, or the one
// "Add host" made) is the base and is never changed. This file is where the two meet:
//
//   - the NAME is put on the row that is shown: in the snapshot (buildSnapshot) and on the rows ShownSystem
//     and RelaySystem hand to the other pages. The poller's own entry keeps the configuration's name;
//   - the WAKE-ON-LAN details are put on the entry PowerOverride returns, which is what the Wake button, the
//     wake itself and "Update unpowered hosts" ask. This is the one thing of "Edit host" that a control path
//     reads: a wake packet goes to the MAC addresses and through the relay that count here. Where it is sent
//     to (broadcast) and the password that goes with it are never part of a decision;
//   - its own RUN TIMINGS (after how long without an answer it counts as off, and the countdown of the
//     shutdown a run asks it for) are put on what WakeUpdateOverride returns, which is what the page of
//     "Update unpowered hosts", the checks of a start and the run itself ask. A decision counts only when it
//     is a value its key can have (runWith): a number below a floor never comes into force from here.
//
// All are worked out when they are used, from what state.json holds at that moment: there is no copy, so the
// start of the panel and a running panel cannot differ, and a change counts at once.

// machineIDs is every id the machine shown under id is known under, the ids of agents first: id itself, the
// other id of a Proxmox node with an agent where the configuration decides it (SameMachine), and where it
// does not, the agent a node's row belongs to (AgentFor) or the row an agent is shown as (the rule of
// WakeUpdateOverride: a setting is the machine's, whichever of its ids it is shown under).
func (p *Poller) machineIDs(id string) []string {
	ids := p.SameMachine(id)
	if _, isAgent := p.AgentByID(id); isAgent {
		if s := p.shownSystem(id); s != nil && !slices.Contains(ids, s.ID) {
			ids = append(ids, s.ID)
		}
	} else if a, ok := p.AgentFor(id); ok && !slices.Contains(ids, a.ID) {
		ids = append(ids, a.ID)
	}
	slices.SortStableFunc(ids, func(a, b string) int {
		_, x := p.AgentByID(a)
		_, y := p.AgentByID(b)
		switch {
		case x && !y:
			return -1
		case y && !x:
			return 1
		}
		return 0
	})
	return ids
}

// MachineIDs is every id under which something kept about the machine shown under id is found (machineIDs).
func (p *Poller) MachineIDs(id string) []string { return p.machineIDs(id) }

// HostEdit returns what was changed in the panel about the machine shown under id, and the id it is kept
// under (ok false: nothing). An entry is the machine's under every id the machine has.
func (p *Poller) HostEdit(id string) (key string, e adminstate.HostEdit, ok bool) {
	return p.hostEditIn(p.Admin.HostOptions(), id)
}

func (p *Poller) hostEditIn(opts adminstate.HostOptions, id string) (key string, e adminstate.HostEdit, ok bool) {
	if len(opts.Hosts) == 0 {
		return "", adminstate.HostEdit{}, false
	}
	for _, x := range p.machineIDs(id) {
		if e, ok := opts.Hosts[x]; ok && !e.Empty() {
			return x, e, true
		}
	}
	return "", adminstate.HostEdit{}, false
}

// HostKey is the id under which a change about the machine shown under id is kept: the one it has an entry
// under already, else its agent's id, else the row's id (a Proxmox node without an agent).
func (p *Poller) HostKey(id string) string {
	if key, _, ok := p.HostEdit(id); ok {
		return key
	}
	return p.machineIDs(id)[0]
}

// runWith lays the panel's decisions about one machine's own run timings over the two values the
// configuration has for it (0: it has none). A decision counts only when it is a value its key can have
// (config.MachineRunTimings: the ranges config.yaml is checked against). The store's reader leaves out
// everything else already; it is asked again here, so that no number below a floor comes into force whatever
// the reader is changed to.
func runWith(e adminstate.RunEdit, offAfterS, shutdownDelayS int) (int, int) {
	for _, t := range config.MachineRunTimings() {
		v := e.Value(t.Key)
		if v == 0 || !t.In(v) {
			continue
		}
		switch t.Key {
		case config.KeyOffAfter:
			offAfterS = v
		case config.KeyShutdownDelay:
			shutdownDelayS = v
		}
	}
	return offAfterS, shutdownDelayS
}

// RunTimingWarnings is for the start of the panel: one line for every machine whose own run timing was
// decided in "Edit host" and is BELOW what the wake_update: block of config.yaml sets for it now. The decision
// was made over another value (the block is newer, or was raised since), and it goes on counting: what is
// changed in the panel lies over config.yaml. Nobody is asked again when config.yaml changes, so it is said
// here, with the machine and both numbers. A decision that is not below the block's value is what the admin
// chose knowing a careful value; it is not named.
func (p *Poller) RunTimingWarnings() []string {
	opts := p.Admin.HostOptions()
	keys := make([]string, 0, len(opts.Hosts))
	for k := range opts.Hosts {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var out []string
	for _, key := range keys {
		off, delay := p.ConfigWakeUpdate(key)
		for _, t := range config.MachineRunTimings() {
			v, block := opts.Hosts[key].Run.Value(t.Key), off
			if t.Key == config.KeyShutdownDelay {
				block = delay
			}
			if v == 0 || !t.In(v) || block == 0 || v >= block {
				continue
			}
			out = append(out, fmt.Sprintf("host %s: %s is %d s, changed in \"Edit host\". config.yaml's wake_update: block for this machine says %d s. "+
				"The %d s of the panel count. To use config.yaml's value, press Reset in Edit host.", key, t.Key, v, block, v))
		}
	}
	return out
}

// named puts the name an admin gave a host on its row (a node or an agent; a guest has none).
func (p *Poller) named(opts adminstate.HostOptions, s *model.System) {
	if s == nil || (s.Kind != "node" && s.Kind != "agent") {
		return
	}
	if _, e, ok := p.hostEditIn(opts, s.ID); ok && e.Name != "" {
		s.Name = e.Name
	}
}

// BaseName is the name the row with this id has without what was changed in the panel ("": no such row).
func (p *Poller) BaseName(id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.systems[id].Name
}

// ConfigWol is the Wake-on-LAN the configuration sets for the host shown under id, without what was changed
// in the panel (nil: none).
func (p *Poller) ConfigWol(id string) *config.Wol {
	o := p.configPowerOverride(id)
	if o == nil || o.Wol == nil {
		return nil
	}
	w := *o.Wol
	w.MACs = slices.Clone(o.Wol.MACs)
	return &w
}

// WolRelays is every agent that may send wake packets (wol_relay: true, set in config.yaml or by pairing at
// the machine; never in the panel), in the order of the configuration.
func (p *Poller) WolRelays() []config.Agent {
	var out []config.Agent
	for _, a := range p.Agents() {
		if a.WolRelay {
			out = append(out, a)
		}
	}
	return out
}

// WolWith is the Wake-on-LAN that counts for the host shown under id when the configuration says base and
// the panel decided e: adminstate.WolEdit.Apply, and what only the running panel knows: the relay a decision
// names must be an agent that may send wake packets, and not the machine itself. unused: why the decisions
// are not used ("": they are); the result is then base.
func (p *Poller) WolWith(id string, base *config.Wol, e adminstate.WolEdit) (w *config.Wol, unused string) {
	w, unused = e.Apply(base)
	if unused != "" || w == nil || e.Relay == "" {
		return w, unused
	}
	switch a, ok := p.AgentByID(e.Relay); {
	case !ok:
		unused = "the relay " + e.Relay + " is no agent of this panel"
	case !a.WolRelay:
		unused = "the relay " + e.Relay + " does not send wake packets (wol_relay is set on the machine or in config.yaml)"
	case slices.Contains(p.machineIDs(id), e.Relay):
		unused = "a machine cannot send its own wake packet"
	}
	if unused != "" {
		w, _ = adminstate.WolEdit{}.Apply(base)
	}
	return w, unused
}

// panelWol lays the panel's Wake-on-LAN decisions for the host shown under id over o, the entry the
// configuration has for it (nil: none). A decision that cannot be used changes nothing and is said once in
// the log.
func (p *Poller) panelWol(opts adminstate.HostOptions, id string, o *config.SystemOverride) *config.SystemOverride {
	key, e, ok := p.hostEditIn(opts, id)
	if !ok || e.Wol == nil {
		return o
	}
	var base *config.Wol
	if o != nil {
		base = o.Wol
	}
	w, unused := p.WolWith(id, base, *e.Wol)
	p.noteWol(key, unused)
	if unused != "" || (o == nil && w == nil) {
		return o
	}
	out := config.SystemOverride{ID: id}
	if o != nil {
		out = *o
	}
	out.Wol = w
	return &out
}

// noteWol says once that the Wake-on-LAN decisions kept under key are not used (and again when the reason
// changes).
func (p *Poller) noteWol(key, unused string) {
	n := p.notes()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.wol[key] == unused {
		return
	}
	if unused == "" {
		delete(n.wol, key)
		return
	}
	if n.wol == nil {
		n.wol = map[string]string{}
	}
	n.wol[key] = unused
	slog.Warn("state.json: the \"Edit host\" Wake-on-LAN decisions of a host are not used: the configuration's count", "host", key, "why", unused)
}
