package poller

import (
	"fmt"
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
	// Prefix is set when the proxmox: entry was worked out from the agent's own id (step c of the rule): the
	// id_prefix the id begins with. Fits lists every id_prefix the id begins with when there is more than one
	// ("alpha-, alpha-b-"); the longest of them is Prefix. Both are empty for links_to and for the one
	// proxmox: entry there is: nothing was inferred there.
	Prefix, Fits string
}

// nodeCandidates is every node id the agent's entry can mean: links_to names one outright (when it has the
// form of a node's id of a configured proxmox: entry; a guest's id is none), node: names the node, and the
// node's id is <id_prefix>node-<name> of one of the proxmox: entries.
func nodeCandidates(a config.Agent, clusters []config.Proxmox) []nodeLink {
	if a.LinksTo != "" {
		for _, c := range clusters {
			if strings.HasPrefix(a.LinksTo, c.Prefix()+"node-") {
				return []nodeLink{{ID: a.LinksTo, Cluster: c.ID}}
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
		out = append(out, nodeLink{ID: c.Prefix() + "node-" + slug, Cluster: c.ID}) // normalise.NodeID
	}
	return out
}

// nodeChoice picks, among the ids an agent's entry can mean (cands, from nodeCandidates), the one the
// configuration names; why says why it names none. Steps a to c of the rule (nodeLinks).
//
// # The longest id_prefix wins: what can go wrong
//
// Until build 13 an id that begins with the id_prefix of two entries (entries alpha- and alpha-b-, agent
// alpha-b-box-agent) was "not decided", and the agent needed links_to. An id_prefix that is the beginning of
// another one is the usual shape of a site with a cluster and standalone hosts next to it (<site>- and
// <site>-<host>-, which is also what "Add host" writes), so the usual naming needed links_to on every such
// host. Now the entry with the longest id_prefix the id begins with is taken. Two id_prefixes that both begin
// one id are equal only if they are the same text, and two entries with the same id_prefix are refused when
// the configuration is read (config.validateProxmox, managed.Check); if it ever happens, it is not decided.
//
// The pick is wrong for exactly one shape: the agent belongs to the entry with the SHORTER prefix, and its
// id happens to begin with the longer one. Entries alpha- (a cluster) and alpha-b- (a standalone host), and an
// agent on the cluster's node B2 that somebody called alpha-b2-agent: no; alpha-b-2-agent: yes, it begins
// with alpha-b-, and its node is taken to be alpha-b-node-<its node:> instead of alpha-node-<its node:>.
// (An agent of the longer entry is never given to the shorter one, and an agent whose id begins with one
// prefix only is as it was in build 13.)
//
// What a wrong pick can do. The link is asked for two things only, and neither sends anything to another
// machine's agent: a shutdown, a restart, an update always go to the agent's own address with its own token.
//
//  1. Wake-on-LAN and bmc_power (PowerOverride): while the agent's host is shown under the agent's id, a
//     wol:/bmc_power: written under the wrongly picked id is offered on its row, and "Update unpowered hosts"
//     uses it. That needs a systems: entry under alpha-b-node-<name>, so the standalone host alpha-b must
//     have a node of the SAME NAME as the agent's node, with a wol: of its own, and no agent of its own that
//     names it (two agents that can mean one node id: not decided for either, step d). Then "Power on" on
//     the agent's row wakes the other machine, and a run wakes it, waits for this agent, reports "did not
//     start", and leaves the other machine on (a run switches off only a machine whose own agent answered
//     after the wake). A machine is switched ON that should not be; nothing is switched off.
//  2. The switches kept per machine count under both ids (SameMachine): `power-control disable` and the
//     "Switch on" for runs set for alpha-b-node-<name> then also count for this agent's row. The first only
//     takes power control away. The second lets a run choose the agent's machine although the switch was
//     set for the other; it gets no further than 1., because the run's wake goes to the other machine.
//
// Without such a systems: entry and such a switch a wrong pick changes nothing that can be seen, except that
// the agent's own wol: (under alpha-node-<name>) is not found while its node's row is not there: what build 13
// did for every such agent.
//
// So that nothing is guessed in silence: every link that was worked out from an id_prefix is one line in the
// panel's log at its start and one line of `fairy admin config check` (nodeInferred: agent, node, the
// id_prefix, and that links_to overrides it). links_to (a) and the one proxmox: entry (b) come first, as before.
func nodeChoice(a config.Agent, cands []nodeLink, clusters []config.Proxmox) (pick nodeLink, why string) {
	if len(cands) == 1 { // links_to, or node: with the one proxmox: entry there is
		return cands[0], ""
	}
	// node: and several proxmox: entries. The entry is the one whose id_prefix the agent's own id begins with,
	// and of several the one with the longest. An empty id_prefix begins every id and so names nothing: it
	// never counts.
	var prefixes []string
	longest, same := "", false
	for i, c := range clusters {
		p := c.Prefix()
		if p == "" || !strings.HasPrefix(a.ID, p) {
			continue
		}
		prefixes = append(prefixes, p)
		switch {
		case len(p) > len(longest):
			pick, longest, same = cands[i], p, false
		case len(p) == len(longest): // the same id_prefix in two entries (refused when the configuration is read)
			same = true
		}
	}
	switch {
	case len(prefixes) == 0:
		return nodeLink{}, "its id begins with the id_prefix of none of the proxmox: entries"
	case same:
		return nodeLink{}, "its id begins with the id_prefix of more than one proxmox: entry, and none of them is the longest (" + strings.Join(prefixes, ", ") + ")"
	}
	pick.Prefix = longest
	if len(prefixes) > 1 {
		pick.Fits = strings.Join(prefixes, ", ")
	}
	return pick, ""
}

// nodeInferred is one sentence for every link that was worked out from an id_prefix (step c), in the order of
// the agents: the agent, the node, the id_prefix that decided, and how to say it otherwise. For the log at the
// start and for `fairy admin config check`; a link by links_to or by the one proxmox: entry has none.
func nodeInferred(agents []config.Agent, links map[string]nodeLink) []string {
	var out []string
	for _, a := range agents {
		l, ok := links[a.ID]
		if !ok || l.Prefix == "" {
			continue
		}
		how := ", which the agent's id begins with"
		if l.Fits != "" {
			how = " (the longest of " + l.Fits + ", which the agent's id begins with)"
		}
		out = append(out, fmt.Sprintf("agent %s (node: %s) -> node %s: inferred from id_prefix %s of the proxmox: entry %s%s; set links_to in the agent's entry to override",
			a.ID, a.Node, l.ID, l.Prefix, l.Cluster, how))
	}
	return out
}

// nodeLinks returns, by agent id, the node each agent runs on where the configuration decides it, and one
// sentence for every agent for which it does not although a `systems:` entry is written under an id its node
// can have (for the log at the start, and for `fairy admin config check`).
//
// The rule, from the configuration alone:
//
//	a. links_to with the id of a node (<id_prefix>node-... of a configured proxmox: entry): that id.
//	b. node: and exactly one proxmox: entry: <its id_prefix>node-<name>.
//	c. node: and several proxmox: entries: the entry whose id_prefix, which must not be empty, is the beginning
//	   of the agent's own id (agent lab-b-agent, id_prefix lab-b-). When several fit (lab- and lab-b-), the
//	   one with the longest id_prefix (nodeChoice says what that can get wrong). None fits: not decided. Which
//	   ids have a `systems:` entry plays no part: an entry under another entry's prefix is another machine's
//	   (two hosts whose node has the same name, the installer's "pve"). A link made by this step is said
//	   (nodeInferred).
//	d. Not decided either when that id is the id of an agent, or when another agent's entry can mean the same
//	   id (nodeCandidates: the same node:, or a links_to to it).
//	e. Not decided, and a `systems:` entry is written under one of the ids the agent's node can have: one
//	   warning that names the agent and says what to add (links_to: <the node's id>).
//
// An agent that is not decided is as it was before this file: looked up by the id it is shown under.
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
		pick, why := nodeChoice(a, cands, clusters)
		advice, plain := "write links_to: <the id of its node> in the agent's entry", true
		switch {
		case why != "":
		case agentIDs[pick.ID]:
			why, advice, plain = "the id of its node, "+pick.ID+", is also the id of an agent", "give that agent another id", false
		case len(claims[pick.ID]) > 1:
			var others []string
			for _, o := range claims[pick.ID] {
				if o != a.ID {
					others = append(others, o)
				}
			}
			why = "the entry of " + strings.Join(others, " and of ") + " can mean the same node, " + pick.ID
			advice, plain = "write links_to: <the id of its own node> in the entry of each of these agents", false
		default:
			links[a.ID] = pick
			continue
		}
		// e: not decided. Said only when something is written that would be looked for.
		var ids, found []string
		for _, c := range cands {
			ids = append(ids, c.ID)
			if written[c.ID] {
				found = append(found, c.ID)
			}
		}
		if len(found) == 0 {
			continue
		}
		name := "node: " + a.Node
		if a.LinksTo != "" {
			name = "links_to: " + a.LinksTo
		}
		if plain && len(found) == 1 {
			advice += " (links_to: " + found[0] + " if that entry is this machine's)"
		}
		whys = append(whys, fmt.Sprintf("agent %s (%s): %s, so the configuration does not say which node is the agent's (it can be %s). "+
			"systems: has an entry for %s: it is not used for this agent while it is shown under its own id (after a restart of the panel with the machine off, "+
			"until the node's Proxmox API answers). To say which node it is, %s",
			a.ID, name, why, strings.Join(ids, " or "), strings.Join(found, " and "), advice))
	}
	return links, whys
}

// NodeWarnings is nodeLinks' sentences for a configuration: one for every agent whose node the configuration
// does not decide although a `systems:` entry is written under an id that node can have.
func NodeWarnings(cfg *config.Panel) []string {
	_, whys := nodeLinks(cfg.Agents, cfg.Proxmox, cfg.Systems)
	return whys
}

// NodeInferred is nodeInferred for a configuration: one sentence for every agent whose node was worked out
// from an id_prefix.
func NodeInferred(cfg *config.Panel) []string {
	links, _ := nodeLinks(cfg.Agents, cfg.Proxmox, cfg.Systems)
	return nodeInferred(cfg.Agents, links)
}

// links is nodeLinks for the configuration as it is now (config.yaml and the hosts added in the panel).
func (p *Poller) links() (map[string]nodeLink, []string) {
	p.cfgMu.RLock()
	agents, systems := p.Cfg.Agents, p.Cfg.Systems
	p.cfgMu.RUnlock()
	return nodeLinks(agents, p.Clusters(), systems)
}

// NodeLinkWarnings is NodeWarnings for the configuration as it is now. The poller logs the sentences once
// when it is made.
func (p *Poller) NodeLinkWarnings() []string {
	_, whys := p.links()
	return whys
}

// NodeLinksInferred is NodeInferred for the configuration as it is now. The poller logs the sentences once
// when it is made.
func (p *Poller) NodeLinksInferred() []string {
	p.cfgMu.RLock()
	agents := p.Cfg.Agents
	p.cfgMu.RUnlock()
	links, _ := p.links()
	return nodeInferred(agents, links)
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
	if a, isAgent := p.AgentByID(id); isAgent {
		if a.Node == "" && a.LinksTo == "" {
			return []string{id} // an agent that names no node
		}
	} else if !strings.Contains(id, "node-") {
		return []string{id} // no agent's id and not of a node's form (a guest)
	}
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
// (`fairy admin host <id> power-control disable`), and the id the switch is kept under. The switch is the
// machine's: for the agent of a Proxmox node and for that node it counts under either id (SameMachine).
// Wake-on-LAN is on offer under the agent's id now (PowerOverride) and must not pass the node's switch; and a
// switch set under the agent's id, the only id the machine has on the pages while it is off after a restart,
// must still hold when a run goes on under the node's row and asks for the shutdown there.
func (p *Poller) PowerDisabledAs(id string) (string, bool) {
	for _, x := range p.SameMachine(id) {
		if p.Admin.PowerDisabled(x) {
			return x, true
		}
	}
	return "", false
}

// IsNodeAgent reports whether the agent's entry says that it runs on a Proxmox node (node:, or links_to with a
// node's id), whether or not the panel has that node's row.
func (p *Poller) IsNodeAgent(a config.Agent) bool {
	return len(nodeCandidates(a, p.Clusters())) > 0
}
