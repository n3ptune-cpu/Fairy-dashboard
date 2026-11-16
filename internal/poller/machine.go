package poller

import (
	"slices"
	"strings"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// Which machine is under a row, and which rows are the machine this panel runs on.
//
// A row is not always a machine. A guest (a VM, a container) runs on its node; an agent can run in a guest, or
// on a node beside the node's own agent, and then it has a row of its own that is no machine either. What is
// asked about a machine (which socket feeds it, whether a shutdown of it takes the panel down) must be asked of
// the machine under such a row. This file is the one place that goes down: the link of a socket, the three
// sides of a socket's lock (the reader's, Home Assistant's, the panel's: internal/web/plug.go), and
// panel_hosts for the power menu and "Update unpowered hosts" (powerctl.KillsPanel) all ask here, so that no
// two of them can take different ways. (Until the fourth round of stage 1a the way down was in internal/web,
// for the sockets alone, and the power menu had an older rule of its own.)

// ChainMax is the most rows a link is followed over before the machine under it. A real link goes over two (an
// agent, and the guest it runs in). The rows that were seen end a chain of links_to that leads back to itself;
// the bound ends one that is merely long.
const ChainMax = 8

// RowsOf makes the lookup of a row by its id that the functions of this file take, over one list of rows.
func RowsOf(snap []model.System) func(id string) *model.System {
	rows := make(map[string]*model.System, len(snap))
	for i := range snap {
		rows[snap[i].ID] = &snap[i]
	}
	return func(id string) *model.System { return rows[id] }
}

// MachineUnder follows a row (start, the id it is shown as) down to the machine under it, and returns that
// machine's row and the rows it went over, the first one first.
//
//   - a node, and a row that runs on nothing else (a PC with an agent): itself;
//   - a guest (a VM, a container): its node;
//   - the row of an agent that runs in a guest, or on a node whose row is another agent's (links_to:, node:):
//     where it runs (runsOn), and on from there.
//
// machine is nil when the way ends at no row: the id has none, a guest's node has none, the row an agent runs
// on has none (unless the configuration makes that row the agent's own machine under its other id: own), or
// the links lead back to a row that was seen. A socket with such a link is locked (SocketLockLinkUnknown).
//
// It is nil too when the way meets an id that names two things (twice: Poller.NamesTwo, an agent whose id is
// written like a guest's or a node's). The rows have one row for an id, and which of the two it is right now
// says nothing about which was meant: the way ends there, as at an id without a row.
func MachineUnder(start string, row func(id string) *model.System, runsOn func(agentID string) (to string, own bool), twice func(id string) bool) (machine *model.System, over []*model.System) {
	seen := map[string]bool{}
	for cur := start; ; {
		r := row(cur)
		if r == nil || seen[cur] || (twice != nil && twice(cur)) {
			return nil, over
		}
		seen[cur] = true
		switch r.Kind {
		case "node":
			return r, over
		case "qemu", "lxc":
			over = append(over, r)
			if r.ParentID == nil {
				return nil, over
			}
			if n := row(*r.ParentID); n != nil && n.Kind == "node" {
				return n, over
			}
			return nil, over
		}
		to, own := runsOn(r.ID)
		if to == "" || to == r.ID || (own && row(to) == nil) {
			return r, over
		}
		if len(over) == ChainMax {
			return nil, over
		}
		over, cur = append(over, r), to
	}
}

// RunsOn is where the agent with this id runs when that is not a machine of its own: the node or the guest its
// entry names (node:, links_to:). "" for an id that is no agent's and for an agent that names neither. own
// reports that the configuration makes that node this agent's own machine (the two are one machine with two
// ids: SameMachine): while the node has no row, the agent's row is the machine.
func (p *Poller) RunsOn(agentID string) (to string, own bool) {
	a, ok := p.AgentByID(agentID)
	if !ok {
		return "", false
	}
	if to = p.LinkOf(a); to == "" || to == agentID {
		return "", false
	}
	return to, slices.Contains(p.SameMachine(agentID), to)
}

// NamesTwo reports whether an id names two things at once: it is an agent's id, and it is written like the id
// of a node or a guest of a proxmox: entry (<id_prefix>node-<name>, <id_prefix>qemu-<number>,
// <id_prefix>lxc-<number>). The panel keeps one row for an id, so the agent's row and the guest's (or the
// node's) take each other's place, and a link or panel_hosts with that id can mean either: a guest's id is
// followed to its node, an agent's is not. Nothing can tell which was meant, so every way that meets such an
// id ends there (MachineUnder), and `fairy admin config check` has a WARN for the agent's entry: give the
// agent another id.
func (p *Poller) NamesTwo(id string) bool {
	if _, ok := p.AgentByID(id); !ok {
		return false
	}
	return ProxmoxIDForm(id, p.Clusters())
}

// ProxmoxIDForm reports whether id is written like the id of a node or a guest of one of these proxmox:
// entries: <id_prefix>node-<name>, <id_prefix>qemu-<number> or <id_prefix>lxc-<number>.
func ProxmoxIDForm(id string, clusters []config.Proxmox) bool {
	for _, c := range clusters {
		for _, kind := range []string{"node-", "qemu-", "lxc-"} {
			rest, ok := strings.CutPrefix(id, c.Prefix()+kind)
			if !ok || rest == "" {
				continue
			}
			if kind == "node-" || strings.Trim(rest, "0123456789") == "" {
				return true
			}
		}
	}
	return false
}

// MachineRows returns the ids under which the machine known as id can be linked: the id itself, the machine's
// other id (a Proxmox node and its agent are one machine with two ids), the id of the row it is shown as, and,
// for a guest (a VM or a container), its node with that node's ids: what feeds the node feeds the guest.
// known reports whether any of them is a row right now (row: the rows as they are, RowsOf).
//
// It goes down to the machine as the link of a socket does (MachineUnder): over an agent that runs in a guest,
// and over one whose entry names another agent that does, with the rows on the way. The steps before that one
// are for what has no row right now: the id an agent's entry names, and the node a guest's row names, count as
// they are written.
//
// So the ids are the row that is named, every row on the way down, and the machine at the end: the rows that
// go down when that machine does and take what was named with them. A guest beside it on the same node is not
// among them.
func (p *Poller) MachineRows(id string, row func(id string) *model.System) (ids map[string]bool, known bool) {
	ids = map[string]bool{}
	add := func(x string) {
		for _, y := range append(p.SameMachine(x), p.ShownID(x)) {
			if y != "" {
				ids[y] = true
			}
		}
	}
	add(id)
	if a, ok := p.AgentByID(id); ok {
		if to := p.LinkOf(a); to != "" { // the node, or the guest, the agent runs on (node:, links_to:)
			add(to)
		}
	}
	twice := p.NamesTwo(id) // an id that names two things: counted as it is written, and no row is taken for it
	for x := range ids {
		if r := row(x); r != nil && !twice {
			known = true
			if (r.Kind == "qemu" || r.Kind == "lxc") && r.ParentID != nil && *r.ParentID != "" {
				add(*r.ParentID)
			}
		}
	}
	m, over := MachineUnder(p.ShownID(id), row, p.RunsOn, p.NamesTwo)
	for _, r := range over {
		add(r.ID)
	}
	if m != nil {
		add(m.ID)
	}
	return ids, known
}

// PanelRows are the ids of the rows this panel runs on, among the rows as they are (snap):
//
//   - every id of panel_hosts as it is written (an id there needs no row), and what MachineRows has for it:
//     the row it is shown as, its other id, the guest it runs in when it is an agent in a guest, and the node
//     under it. So panel_hosts can name the machine, the guest the panel is in, or an agent that runs there,
//     with the same result for all three;
//   - every agent's row that is the same machine as one of those (runsOnOneOf): an agent that runs in the
//     guest the panel is in, an agent whose entry names such an agent, and an agent that runs on the panel's
//     node beside the node's own agent. A reboot through such an agent is a reboot of the panel's guest or
//     node.
//
// A guest beside the panel's guest is not one of them, and neither is an agent that runs in such a guest: they
// go down without the panel.
//
// Everything that asks "is this the panel's own machine" asks here:
//
//   - the lock of a socket (internal/web/plug.go): a socket that feeds one of these rows is the panel's;
//   - the power menu (powerctl.KillsPanel): "The panel itself runs here", and the name typed for a reboot;
//   - "Update unpowered hosts": such a machine takes no part in a run.
//
// Where panel_hosts cannot be followed to a machine (an id without a row, a guest whose node has no row, links
// that lead back to themselves) the ids are what is written and the rows the way went over, and no node: the
// panel cannot tell which machine that is, and does not guess one.
func (p *Poller) PanelRows(snap []model.System) map[string]bool {
	row := RowsOf(snap)
	rows := map[string]bool{}
	for _, h := range p.Cfg.PanelHosts {
		ids, _ := p.MachineRows(h, row) // with h itself, row or not
		for id := range ids {
			rows[id] = true
		}
	}
	for i := range snap {
		if p.runsOnOneOf(snap[i].ID, rows) {
			rows[snap[i].ID] = true
		}
	}
	return rows
}

// PanelUnknown are the ids of panel_hosts that cannot be followed to a machine among the rows as they are
// (snap): no row has the id (a guest of a Proxmox API that has not answered since the panel started, a typing
// mistake), the node of the guest or the row an agent runs on has none, the links lead back to themselves or
// over more rows than ChainMax, or the id names two things (NamesTwo). nil when every id is followed, and
// when panel_hosts is empty: a panel that was told nothing about its machine marks and locks nothing, as
// before.
//
// While there is such an id the panel does not know its own machine: the rows PanelRows has are the ids as
// they are written, and the node under the panel may be any row. So everything that could take the panel's
// machine down fails closed for every machine, until the row is there or panel_hosts is corrected:
//
//   - no socket is switched off (internal/web/plug.go, SocketLockPanelUnknown). "On" is not held back, and
//     neither is anything that is no cut of power;
//   - a reboot of a host (a node's row, an agent's row) asks for the typed name, as on a row the panel runs on
//     (powerctl.Capabilities); a shutdown asks for it always;
//   - no host can take part in a run of "Update unpowered hosts", which switches its machines off at the end.
//
// (Until the fix round after the fourth review such an id counted as it was written and nothing more: a panel
// in a VM, named by the VM's id, did not know its node for as long as that Proxmox API had not answered; the
// node's socket had no lock, a reboot of the node's agent was a click, and the node could join a run.)
func (p *Poller) PanelUnknown(snap []model.System) []string {
	row := RowsOf(snap)
	var unknown []string
	for _, h := range p.Cfg.PanelHosts {
		if m, _ := MachineUnder(p.ShownID(h), row, p.RunsOn, p.NamesTwo); m == nil {
			unknown = append(unknown, h)
		}
	}
	return unknown
}

// runsOnOneOf reports whether the row with this id is an agent's that runs on one of rows: the agent's entry
// names one of them (the guest it runs in or the node it runs on, as the id is written: rows has every id of
// its machines, and an id needs no row), or it names another agent for which that is so. It goes from agent
// to agent only, by what their entries name (RunsOn): the first guest or node on the way is where the agent
// runs, and when that is not one of rows the agent is another machine's. So an agent in a guest beside the
// panel's guest is not the panel's, though its node is. An agent whose entry names the node's own agent goes
// on to the node that agent names.
//
// What the panel can know about an agent that names a node while the configuration does not say that the node
// is its own (a second agent of a node; finding A of the third review's check) is the node it names. It may be
// that node, and is taken for it: one of rows when the node is. An agent that names nothing is a machine of
// its own. Links that lead back end at the id that was seen.
func (p *Poller) runsOnOneOf(id string, rows map[string]bool) bool {
	seen := map[string]bool{}
	for cur := id; !seen[cur]; {
		seen[cur] = true
		to, _ := p.RunsOn(cur)
		if to == "" {
			return false
		}
		if rows[to] {
			return true
		}
		cur = to
	}
	return false
}
