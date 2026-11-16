package poller

import (
	"slices"

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
func MachineUnder(start string, row func(id string) *model.System, runsOn func(agentID string) (to string, own bool)) (machine *model.System, over []*model.System) {
	seen := map[string]bool{}
	for cur := start; ; {
		r := row(cur)
		if r == nil || seen[cur] {
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
	for x := range ids {
		if r := row(x); r != nil {
			known = true
			if (r.Kind == "qemu" || r.Kind == "lxc") && r.ParentID != nil && *r.ParentID != "" {
				add(*r.ParentID)
			}
		}
	}
	m, over := MachineUnder(p.ShownID(id), row, p.RunsOn)
	for _, r := range over {
		add(r.ID)
	}
	if m != nil {
		add(m.ID)
	}
	return ids, known
}

// PanelRows are the ids of the rows this panel runs on: every id of panel_hosts as it is written (an id there
// needs no row), and what MachineRows has for it: the row it is shown as, its other id, the guest it runs in
// when it is an agent in a guest, and the node under it. So panel_hosts can name the machine, the guest the
// panel is in, or an agent that runs there, with the same result for all three.
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
func (p *Poller) PanelRows(row func(id string) *model.System) map[string]bool {
	rows := map[string]bool{}
	for _, h := range p.Cfg.PanelHosts {
		ids, _ := p.MachineRows(h, row) // with h itself, row or not
		for id := range ids {
			rows[id] = true
		}
	}
	return rows
}
