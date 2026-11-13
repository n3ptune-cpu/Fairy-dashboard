package poller

import "github.com/n3ptune-cpu/Fairy-dashboard/internal/model"

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
// machine, under both of its ids: an answer of the agent ends the mark of its own entry and of its node's, and
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
		if n, okN := p.systems[p.linkOfLocked(a)]; okN && n.Kind == "node" && n.ID != a.ID {
			ids = append(ids, n.ID)
		}
		for _, sid := range ids {
			s, ok := p.systems[sid]
			if !ok || (s.Status != "stale" && s.Status != "offline") {
				continue // a node that its cluster shows as online, an error entry: not this function's to say
			}
			p.knownOff[sid] = true
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

// wentOff is what degrade makes of an entry that is not fresh any more: offline, without the values of a
// machine that runs.
func wentOff(s model.System) model.System {
	s.Status = "offline"
	s.CPUPercent = nil
	s.NetRxBps, s.NetTxBps = nil, nil
	s.PowerPending = nil
	return s
}
