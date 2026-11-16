package poller

import (
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// A machine that is known to be off. A machine that stops answering is "stale" for offline_after (60 s) before
// it is "offline" (degrade): one missed poll is not a machine that is off, and the panel does not know better.
// A run of "Update unpowered hosts" does know better. It asked for the shutdown, saw the machine's countdown,
// and after the countdown's time the machine gave nothing at all for 20 s, over at least two polls in a row
// (wakerun: offConfirm, offPolls): that is what its report calls "switched off again", chip "is off". The
// machine's row said "stale" (the amber dot) for up to a minute after that, next to a report that said off.
//
// The report is the one that is right, so the row follows it: the run tells the poller (MarkOff), and the
// machine's row is "offline" from then on, until the machine answers again.
//
// "The machine's row" is what the page shows for it: the agent's own entry, and the entry of the Proxmox node
// the agent is a part of. A standalone Proxmox host that is off has no API that answers, so its node's row is
// "stale" in the same way and for the same minute.
//
// Nothing is marked for a machine whose agent answers, or whose node its cluster shows as online: MarkOff
// only ever turns "stale" into "offline", it never contradicts an answer. A sign of life ends it for the
// machine, under both of its ids (the node's row is one agent's, rowIsOf: its ids are that agent's and the
// node's): an answer of the agent ends the mark of its own entry and of its node's, and
// a poll of the node's Proxmox API that shows the node online ends both too. (Until the review of build 14 an
// answer of the agent left the node's mark standing: with the agent back and the node's API not yet, the
// machine's row said "offline" for a machine that answered.) A good poll that lists the node as offline ends
// the node's mark only: the cluster then says what the node is.

// MarkOff is called for a machine that was seen to go off after a shutdown the panel asked for (id: the id it
// is shown under, its agent's or its node's). It reports whether a row changed.
func (p *Poller) MarkOff(id string) bool {
	a, ok := p.AgentFor(id)
	if !ok {
		return false
	}
	p.mu.Lock()
	changed := false
	if _, failing := p.agentFailures[a.ID]; failing { // its agent does not answer now
		ids := []string{a.ID}
		if n, okN := p.systems[p.linkOfLocked(a)]; okN && n.Kind == "node" && n.ID != a.ID && p.rowIsOf(n.ID, a.ID) {
			ids = append(ids, n.ID)
		}
		for _, sid := range ids {
			s, ok := p.systems[sid]
			if !ok || (s.Status != "stale" && s.Status != "offline") {
				continue // a node that its cluster shows as online, an error entry: not this function's to say
			}
			p.knownOff[sid] = true
			p.knownOffAt[sid] = p.mono() // seen to go off now: the mark's age starts here, also when it was set before
			if s.Status == "stale" {
				p.systems[sid] = wentOff(s)
				changed = true
			}
		}
	}
	if changed {
		p.version++
	}
	p.mu.Unlock()
	return changed
}

// KnownOff reports whether the row with this id is marked as one of a machine that was seen to go off (MarkOff)
// and has shown no sign of life since. It is the one thing the panel keeps that says "off" and not only "does
// not answer". The mark does not end with time: it is what keeps the row "offline", for as long as the machine
// gives no sign of life.
func (p *Poller) KnownOff(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.knownOff[id]
}

// KnownOffWithin reports whether the row is marked (KnownOff) and the machine was seen to go off no longer than
// d ago, by the steady clock. It is for whoever takes the mark as a sign that the machine is off NOW: the
// switching of a plug's socket asks it before it takes a machine for off (internal/web, plug.go). A machine can
// be started by hand without the panel hearing of it (another system booted, its agent stopped, a cable), and
// the mark of such a machine stays; an old mark says less and less.
func (p *Poller) KnownOffWithin(id string, d time.Duration) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.knownOff[id] && p.mono()-p.knownOffAt[id] <= d
}

// rowIsOf reports whether the row of the node with this id is this agent's (AgentFor: the first agent of the
// configuration whose link is that node). The mark is the machine's under both of its ids, and only the
// agent the node's row belongs to is that machine: a second agent that names the same node (one in a guest
// with its host's node:, a second one on the host) has a row of its own, and what a run saw of IT says
// nothing about the node. Until the second review of build 14 (R7) the node of ANY agent with that link was
// marked, and unmarked by any such agent's answer: the node's row read "offline" when the guest's agent had
// been switched off, and "stale" again when it answered while the host was off, until the next poll of the
// node's Proxmox API or of its own agent. Safe with p.mu held (AgentFor takes cfgMu, clustersMu and idsMu).
func (p *Poller) rowIsOf(nodeID, agentID string) bool {
	first, ok := p.AgentFor(nodeID)
	return ok && first.ID == agentID
}

// wentOff is what degrade makes of an entry that is not fresh any more: offline, without the values of a
// machine that runs.
func wentOff(s model.System) model.System {
	s.Status = "offline"
	s.CPUPercent = nil
	s.NetRxBps, s.NetTxBps = nil, nil
	s.PowerPending = nil
	return s
}
