package admin

import (
	"fmt"
	"slices"
	"strings"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
)

// panelHostsCheck is about the ids the panel follows down to a machine: panel_hosts (the machine the panel
// itself runs on), and the ids of the agents on the way. A warning (exit code 1) is a state in which the
// running panel fails closed and somebody has to put the file right:
//
//   - an id in panel_hosts that no row can have (no agent's id, and not written like the id of a node or a
//     guest of a proxmox: entry). While an id of panel_hosts has no row the panel does not know which machine
//     it runs on (poller.PanelUnknown): it switches no socket off, a reboot of any host asks for the typed
//     name, and no host takes part in "Update unpowered hosts". An id of the form a node or a guest has cannot
//     be checked here: which nodes and guests there are is known to the running panel only, and there the
//     lock's sentence and the Power menu say it;
//   - an agent whose id is exactly the id of a node the file itself names (another agent's `node:`, or its
//     own): the id names two things (poller.NamesTwo), and every link that meets it is a link to no row;
//   - links_to that leads from agent to agent over more agents than the panel follows (poller.ChainMax, counted
//     as the panel counts them: every agent on the way that names where it runs), or back to an agent it came
//     over: such a way ends at no machine.
//
// A note (said, never a reason for exit code 1) is for an agent whose id only LOOKS like the id of a node or a
// guest (<id_prefix>node-<name>, <id_prefix>qemu-<number>, <id_prefix>lxc-<number>). Whether there is such a
// node or guest is not in the file, and the running panel locks nothing for what an id looks like: only when
// its Proxmox API lists a node or a guest with exactly that id does the id name two things. (Until the fix
// round after the fifth review this was a warning, and the running panel went by the look: the agents
// lab-node-red and lab-node-pve-agent, next to no node of those names, had their sockets locked.)
//
// An empty panel_hosts has no line here: the panel then marks and locks nothing as its own, as it always did
// (socketLinkCheck has a note when an entry has socket_links or home_assistant_system).
func panelHostsCheck(cfg *config.Panel) (warn, said []string) {
	c := *cfg
	c.Agents, c.Systems, c.Proxmox = slices.Clone(cfg.Agents), slices.Clone(cfg.Systems), slices.Clone(cfg.Proxmox)
	_, _, _ = managed.NewStore(cfg.DataDir).Merge(&c) // with the hosts and nodes added in the panel
	agents := map[string]config.Agent{}
	for _, a := range c.Agents {
		agents[a.ID] = a
	}
	for _, h := range cfg.PanelHosts {
		if _, isAgent := agents[h]; !isAgent && !poller.ProxmoxIDForm(h, c.Proxmox) {
			warn = append(warn, fmt.Sprintf("panel_hosts: %s is no agent's id and not the id of a node or a guest of a proxmox: entry, so no row can have it. "+
				"While an id of panel_hosts has no row the panel cannot tell which machine it runs on: it switches no socket off, asks for the typed name before a reboot of any host, "+
				"and lets no host take part in \"Update unpowered hosts\". A typing mistake in the id?", h))
		}
	}
	const then = "a socket that is linked to this id is locked, home_assistant_system with it switches no socket of that agent, and panel_hosts with it leaves the panel without a machine it knows as its own"
	named := poller.NamedNodes(&c)
	for _, a := range c.Agents {
		switch by := named[a.ID]; {
		case len(by) > 0:
			warn = append(warn, fmt.Sprintf("agent %s: its id is also the id of the Proxmox node that the entry of agent %s names (node:). "+
				"The panel keeps one row for an id, so this agent and that node take each other's place, and the id names two things. The panel does not guess which is meant: "+
				then+". Give the agent another id", a.ID, strings.Join(by, ", ")))
		case poller.ProxmoxIDForm(a.ID, c.Proxmox):
			said = append(said, fmt.Sprintf("agent %s: its id looks like the id of a node or a guest of a proxmox: entry (<id_prefix>node-<name>, <id_prefix>qemu-<number>, <id_prefix>lxc-<number>). "+
				"Nothing is locked for what an id looks like. But if that entry has a node or a guest with exactly this id, the id names two things, and from the moment its Proxmox API lists it "+
				then+". If there is one, give the agent another id", a.ID))
		}
	}
	for _, a := range c.Agents {
		seen := map[string]bool{a.ID: true}
		// (counted as the running panel counts, poller.MachineUnder: every agent on the way that names where it
		// runs, with links_to or node:)
		for cur, n := a, 0; cur.LinksTo != "" || cur.Node != ""; {
			next, isAgent := agents[cur.LinksTo]
			back := cur.LinksTo != "" && isAgent && seen[next.ID]
			if n++; n > poller.ChainMax || back {
				warn = append(warn, fmt.Sprintf("agent %s: links_to leads from agent to agent over more than %d agents, or back to one of them. The panel does not follow such a way to a machine: "+
					"a socket that is linked to this agent is locked, and panel_hosts with it leaves the panel without a machine it knows as its own. Write the node or the guest the agent runs on in links_to",
					a.ID, poller.ChainMax))
				break
			}
			if cur.LinksTo == "" || !isAgent {
				break // a node or a guest: the way ends at a machine
			}
			seen[next.ID], cur = true, next
		}
	}
	return warn, said
}
