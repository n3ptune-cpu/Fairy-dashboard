package poller

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/normalise"
)

// One machine, two ids. The agent of a Proxmox node (`node:`, or links_to with the node's id) is shown as part
// of the node, under the node's id (<id_prefix>node-<name>), and that id is where the node's `systems:` entry
// with its wol: is written. But the node's row is made by an answer of its Proxmox API, and the rows are kept
// in memory only: after a restart of the panel while the machine is off there is no such row, and the machine
// is shown under its agent's id until its Proxmox API answers. Everything that was looked up by the shown id
// alone was then not found: no Wake in the Power menu, "no Wake-on-LAN is set up for it" on the Updates page.
//
// This file says, from the configuration alone and never from what the panel has seen since it started, which
// node id belongs to which agent (nodeLinks). A wol: must never be attached to another machine (its MAC would
// wake somebody else's PC), so the rule only says yes where the configuration leaves no choice.

// nodeLink is the node an agent runs on, as the configuration says it.
type nodeLink struct {
	ID      string // the node's system id
	Cluster string // the id of the proxmox: entry it belongs to
}

// nodeCandidates is every node id the agent's entry can mean: links_to names one outright (when it has the
// form of a node's id of a configured proxmox: entry; a guest's id is none), node: names the node, and the
// node's id is <id_prefix>node-<name> of one of the proxmox: entries.
func nodeCandidates(a config.Agent, clusters []config.Proxmox) []nodeLink {
	if a.LinksTo != "" {
		for _, c := range clusters {
			if strings.HasPrefix(a.LinksTo, c.Prefix()+"node-") {
				return []nodeLink{{a.LinksTo, c.ID}}
			}
		}
		return nil
	}
	slug := normalise.Slug(a.Node)
	if slug == "" {
		return nil
	}
	out := make([]nodeLink, 0, len(clusters))
	for _, c := range clusters {
		out = append(out, nodeLink{c.Prefix() + "node-" + slug, c.ID}) // normalise.NodeID
	}
	return out
}

// nodeLinks returns, by agent id, the node each agent runs on where the configuration decides it, and one
// sentence for every agent whose entry names a node that it does not decide (for the log at the start).
//
// The rule:
//
//  1. The ids an agent's entry can mean are nodeCandidates: one for links_to, one per proxmox: entry for node:.
//  2. With one such id, that is the node. With several (several proxmox: entries, and node: does not say which
//     of them has the node), it is the one that has a `systems:` entry, when exactly one of them has: writing
//     that entry is how the configuration names the node's id. None has one: not decided (and nothing is
//     written under a node id that could be looked for). Two or more have one: not decided, with a warning.
//  3. The id must not be the id of an agent, and no other agent's entry may be able to mean the same id (two
//     agents with the same node:, or one linked to the node the other names). Otherwise: not decided for
//     either, with a warning.
//
// What it does not cover: an agent whose node: is found in several proxmox: entries by the panel's polls and
// that has no `systems:` entry, or more than one. Such a host is as it was: looked up by the id it is shown under.
func nodeLinks(agents []config.Agent, clusters []config.Proxmox, systems []config.SystemOverride) (map[string]nodeLink, []string) {
	written := map[string]bool{}
	for _, o := range systems {
		written[o.ID] = true
	}
	agentIDs := map[string]bool{}
	claims := map[string][]string{} // node id -> the agents whose entry can mean it
	all := make([][]nodeLink, len(agents))
	for i, a := range agents {
		agentIDs[a.ID] = true
		all[i] = nodeCandidates(a, clusters)
		for _, c := range all[i] {
			claims[c.ID] = append(claims[c.ID], a.ID)
		}
	}
	links := map[string]nodeLink{}
	var whys []string
	for i, a := range agents {
		cands := all[i]
		if len(cands) == 0 {
			continue
		}
		pick := cands
		if len(cands) > 1 {
			pick = nil
			for _, c := range cands {
				if written[c.ID] {
					pick = append(pick, c)
				}
			}
		}
		switch {
		case len(pick) == 0:
			continue
		case len(pick) > 1:
			ids := make([]string, len(pick))
			for i, c := range pick {
				ids[i] = c.ID
			}
			whys = append(whys, fmt.Sprintf("agent %s (node: %s): systems: has entries for %s, and the configuration does not say which of these nodes the agent runs on. "+
				"While the node's Proxmox API has not answered, the host is shown under the agent's id without a wol: or bmc_power: of those entries. "+
				"Write links_to: <the node's id> in the agent's entry in place of node:", a.ID, a.Node, strings.Join(ids, " and ")))
			continue
		}
		id := pick[0].ID
		if agentIDs[id] {
			whys = append(whys, fmt.Sprintf("agent %s: the id of its node, %s, is also the id of an agent, so the systems: entry %s is used for that agent only", a.ID, id, id))
			continue
		}
		if others := claims[id]; len(others) > 1 {
			names := slices.Clone(others)
			sort.Strings(names)
			whys = append(whys, fmt.Sprintf("agents %s: their entries can all mean the node %s, and the configuration does not say which of them runs on it. "+
				"While the node's Proxmox API has not answered, none of them is given the wol: or bmc_power: of the systems: entry %s", strings.Join(names, " and "), id, id))
			continue
		}
		links[a.ID] = pick[0]
	}
	slices.Sort(whys)
	return links, slices.Compact(whys)
}

// links is nodeLinks for the configuration as it is now (config.yaml and the hosts added in the panel).
func (p *Poller) links() (map[string]nodeLink, []string) {
	p.cfgMu.RLock()
	agents, systems := p.Cfg.Agents, p.Cfg.Systems
	p.cfgMu.RUnlock()
	return nodeLinks(agents, p.Clusters(), systems)
}

// NodeLinkWarnings is one sentence for every agent whose entry names a Proxmox node that the configuration
// does not decide (nodeLinks). The poller logs them once when it is made.
func (p *Poller) NodeLinkWarnings() []string {
	_, whys := p.links()
	return whys
}

// NodeOf returns the id of the Proxmox node the agent with this id runs on and the id of that node's proxmox:
// entry, when the configuration alone decides it (nodeLinks); ok is false for every other id.
func (p *Poller) NodeOf(agentID string) (nodeID, clusterID string, ok bool) {
	if a, isAgent := p.AgentByID(agentID); !isAgent || (a.Node == "" && a.LinksTo == "") {
		return "", "", false // every row but that of a node's agent: nothing to work out
	}
	links, _ := p.links()
	l, ok := links[agentID]
	return l.ID, l.Cluster, ok
}

// SameMachine returns the ids one machine is shown under: id itself and, for the agent of a Proxmox node or
// for such a node, the other of the two (nodeLinks). A setting an admin made for the machine while it was shown
// under one of them is the machine's under the other too.
func (p *Poller) SameMachine(id string) []string {
	links, _ := p.links()
	if l, ok := links[id]; ok {
		return []string{id, l.ID}
	}
	for agentID, l := range links {
		if l.ID == id { // at most one agent: nodeLinks gives a node to no two
			return []string{id, agentID}
		}
	}
	return []string{id}
}

// PowerOverride returns the `systems:` entry that holds the Wake-on-LAN and BMC power-on settings of the host
// shown under id: the entry with that id, or, for a host that is shown under its agent's id because its
// node's row is not there (yet), the entry written under the node's id (nodeLinks). An entry under the shown
// id that has a wol: or a bmc_power: of its own always counts first. nil: there is none.
func (p *Poller) PowerOverride(id string) *config.SystemOverride {
	o := p.Override(id)
	if o != nil && (o.Wol != nil || o.BMCPower != nil) {
		return o
	}
	if nodeID, _, ok := p.NodeOf(id); ok {
		if n := p.Override(nodeID); n != nil && (n.Wol != nil || n.BMCPower != nil) {
			return n
		}
	}
	return o
}

// PowerDisabledAs reports whether an administrator turned power control off for the host shown under id
// (`fairy admin host <id> power-control disable`), and the id the switch is kept under. The switch of a node
// counts for its host also while that is shown under its agent's id: Wake-on-LAN is on offer there now
// (PowerOverride), and it must not be on offer past the switch.
func (p *Poller) PowerDisabledAs(id string) (string, bool) {
	if p.Admin.PowerDisabled(id) {
		return id, true
	}
	if nodeID, _, ok := p.NodeOf(id); ok && p.Admin.PowerDisabled(nodeID) {
		return nodeID, true
	}
	return "", false
}

// IsNodeAgent reports whether the agent's entry says that it runs on a Proxmox node (node:, or links_to with a
// node's id), whether or not the panel has that node's row.
func (p *Poller) IsNodeAgent(a config.Agent) bool {
	return len(nodeCandidates(a, p.Clusters())) > 0
}
