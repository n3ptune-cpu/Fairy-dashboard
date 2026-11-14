package poller

import (
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
)

// "Edit host": the links of the devices an agent reads can be changed in the panel. What an admin changed is
// an overlay in state.json (adminstate.HostOptions); the agent's entry (config.yaml's, or the one "Add host"
// made) is the base and is never changed.
//
// LinksOf is the ONE place where the two meet. It is asked by pollAgent, each time an agent's answer (or its
// silence) is handed to the power and the UPS manager, so that
//
//   - a change counts with the agent's next poll, without a restart, and
//   - the start of the panel and a running panel cannot differ: there is no copy of the overlay in the poll
//     job or anywhere else, the value is worked out when it is used.
//
// Nothing else reads the overlay to decide anything. No power action, no wake, no run of "Update unpowered
// hosts" and no off-clock asks here: the links say what is SHOWN where (TestNoControlPathReadsTheOverlay).

// linkNotes remembers what was said in the log about the overlay, so that each thing is said once and again
// only when it changes.
type linkNotes struct {
	mu      sync.Mutex
	rows    map[string]string // agent id -> the rows its overrides name that the panel does not have
	unknown string            // the agent ids with overrides that are no agent's
}

func (p *Poller) notes() *linkNotes { return &p.linkNotes }

// LinksOf returns the links that count for an agent right now: its entry's, with every field an admin
// changed in the panel replaced by the panel's value (adminstate.HostOption.Apply).
//
// An override is used only as far as it can mean something:
//
//   - an override for an id that is no agent's is never read (it is looked up by the agent's id), and is
//     said once in the log;
//   - a row id in an override that is no row's (no agent's id, no row the panel has, and not of the form a
//     node or guest of one of its Proxmox entries has) is left out, and said once in the log. state.json can
//     be edited by hand; a UPS that "powers" an id nobody has would show that id on the UPS page.
//
// Neither stops anything: the panel always starts, and the rest of the override counts.
func (p *Poller) LinksOf(a config.Agent) config.Links {
	base := a.Links()
	opts := p.Admin.HostOptions()
	p.noteUnknownAgents(slices.Sorted(maps.Keys(opts.Agents)))
	o, ok := opts.Agents[a.ID]
	if !ok || o.Empty() {
		p.noteRows(a.ID, nil)
		return base
	}
	l := o.Apply(base)
	var dropped []string
	known := map[string]bool{}
	keep := func(id string) bool {
		k, seen := known[id]
		if !seen {
			k = p.knownRow(id)
			known[id] = k
		}
		if !k && !slices.Contains(dropped, id) {
			dropped = append(dropped, id)
		}
		return k
	}
	if o.HasUPSPowers {
		for name, rows := range l.UPSPowers {
			l.UPSPowers[name] = slices.DeleteFunc(rows, func(id string) bool { return !keep(id) })
		}
	}
	if o.HasPowerLinks {
		for dev, to := range l.PowerLinks {
			if to != "" && !keep(to) {
				delete(l.PowerLinks, dev)
			}
		}
	}
	p.noteRows(a.ID, dropped)
	return l
}

// BaseLinks returns the links an agent's entry has without the overlay (config.yaml's value), and whether
// there is such an agent.
func (p *Poller) BaseLinks(agentID string) (config.Links, bool) {
	a, ok := p.AgentByID(agentID)
	return a.Links(), ok
}

// knownRow reports whether id can be a row of this panel: an agent's id, or an id of the form a node or a
// guest of one of the configured Proxmox entries has (such a row comes and goes with its Proxmox API's
// answers, and after a restart with the machine off it is not there yet). Every row there can be has one of
// the two; a row that is there now counts whatever its id.
func (p *Poller) knownRow(id string) bool {
	if _, ok := p.AgentByID(id); ok {
		return true
	}
	p.mu.Lock()
	_, has := p.systems[id]
	p.mu.Unlock()
	return has || PossibleRow(id, nil, p.Clusters())
}

// PossibleRow is the rule of knownRow for a configuration that is not running (`fairy admin config check`):
// id is the id of one of agents, or has the form of a node's or guest's id under one of the Proxmox entries
// (<id_prefix>node-<name>, <id_prefix>qemu-<vmid>, <id_prefix>lxc-<vmid>).
func PossibleRow(id string, agents []config.Agent, clusters []config.Proxmox) bool {
	if slices.ContainsFunc(agents, func(a config.Agent) bool { return a.ID == id }) {
		return true
	}
	for _, c := range clusters {
		for _, kind := range []string{"node-", "qemu-", "lxc-"} {
			if pre := c.Prefix() + kind; strings.HasPrefix(id, pre) && len(id) > len(pre) {
				return true
			}
		}
	}
	return false
}

// noteRows says once that an agent's overrides name rows the panel does not have (and again when the list
// changes).
func (p *Poller) noteRows(agentID string, dropped []string) {
	slices.Sort(dropped)
	said := strings.Join(dropped, ", ")
	n := p.notes()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.rows[agentID] == said {
		return
	}
	if said == "" {
		delete(n.rows, agentID)
		return
	}
	if n.rows == nil {
		n.rows = map[string]string{}
	}
	n.rows[agentID] = said
	slog.Warn("state.json: an \"Edit host\" override names rows the panel does not have: they are ignored (nothing is shown for them)",
		"agent", agentID, "rows", textsafe.Cut(said, 400))
}

// noteUnknownAgents says once which overrides are for an id that is no agent's (and again when the list
// changes). ids is every agent id the overlay has, sorted.
func (p *Poller) noteUnknownAgents(ids []string) {
	var unknown []string
	for _, id := range ids {
		if _, ok := p.AgentByID(id); !ok {
			unknown = append(unknown, id)
		}
	}
	said := strings.Join(unknown, ", ")
	n := p.notes()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.unknown == said {
		return
	}
	n.unknown = said
	if said == "" {
		return
	}
	slog.Warn("state.json: \"Edit host\" overrides for ids that are no agent's: they are ignored (a host that was taken out of config.yaml?)",
		"agents", textsafe.Cut(said, 400))
}
