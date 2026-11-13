package poller

import (
	"log/slog"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
)

// When a machine was last seen, over a restart of the panel. The time of an agent's last good answer was in
// memory only (model.System.LastSeen, AgentInfo.SeenAt), so a machine that is off when the panel starts was
// shown with "last seen never" until it came back. The time is therefore also kept in the data folder
// (last-seen.json, see adminstate.AgentSeen: a file of its own, next to state.json, which has the agents' last
// versions; adminstate says why it is not a key of that file), read once when the poller is made, and put
// into the row that is SHOWN for an agent that has not answered since (buildSnapshot).
//
// It is for showing, and for nothing else:
//
//   - The poller's own entry of the agent is not touched (p.systems: its LastSeen stays empty until the agent
//     answers), so "stale or offline" is decided as before (degrade), and a machine that has not answered since
//     the start is "offline", never "stale", however recent the kept time is.
//   - AgentInfo and AgentSeen do not return it, and the clock of "no answer for a quarter of an hour"
//     (agentQuiet, the steady clock of this process) starts with the panel as before: a time read from the
//     file never makes a machine "off for 15 minutes" for "Update unpowered hosts", and never makes it "on".
//   - A file that cannot be used is not used: the panel starts, says so once in its log, and shows "never" as
//     it did before there was a file. A plain file that is damaged is moved aside at that start
//     (last-seen.json.bad), so that the times are kept again from the first answer on; a link or a folder at
//     that name is never touched, and then nothing is kept until it is taken away.
//
// Writing. An agent that answers has a new time with every poll, and no file is rewritten with every poll
// (lastversion.go has what that costs). So:
//
//   - at most one write in seenSaveEvery for all agents together: the write holds every agent's time that is
//     not in the file yet, so the time in the file is at most that old for a machine that answers;
//   - when an agent stops answering (the first poll that fails after a good one), its time is written then,
//     but such writes are at least seenSaveSoon apart: the time of a machine that went off is in the file
//     with the poll that noticed, not a minute later, and an agent that answers every other poll does not
//     rewrite the file every other poll;
//   - when the panel stops (FlushSeen, with FlushVersions);
//   - when an agent is removed, its entry is taken out.
//
// The first good answer after a start is written at once. A failed write is logged once per error text and
// tried again with the next one; what is shown comes from memory.
const (
	seenSaveEvery = time.Minute
	seenSaveSoon  = 5 * time.Second
)

// loadLastSeen reads the kept times (New). A file that cannot be used is said once and is nobody's problem:
// nothing is kept from it, and the start goes on.
func (p *Poller) loadLastSeen() {
	p.lastSeen, p.seenSaved = map[string]time.Time{}, map[string]time.Time{}
	seen, err := p.Admin.AgentSeen()
	if err != nil && (statefile.IsDamaged(err) || statefile.IsNotAFile(err)) {
		// damaged, or not a file of a state file's size: when it is a plain file, it is moved out of the way
		// once and the file is started over (adminstate.SetAgentSeenAside; anything else is left alone)
		if aside, moveErr := p.Admin.SetAgentSeenAside(); moveErr == nil {
			slog.Warn("when the machines were last seen is not known from before this start: the file could not be used and was moved aside, a new one is started",
				"error", err, "moved_to", aside)
			err = nil
		}
	}
	if err != nil {
		slog.Warn("when the machines were last seen is not known from before this start: the file is not used", "error", err)
	}
	for id, t := range seen {
		p.lastSeen[id], p.seenSaved[id] = t, t
	}
}

// noteSeenLocked takes the time of a good answer and reports whether the times have to be written now
// (flushSeen, after the caller has released p.mu). Caller holds p.mu.
func (p *Poller) noteSeenLocked(id string, now time.Time) bool {
	p.lastSeen[id] = now
	return p.seenDueLocked(now, seenSaveEvery)
}

// noteGoneLocked is called with the first poll of an agent that fails after a good one: its last time is
// final until it answers again, and is written soon. Caller holds p.mu.
func (p *Poller) noteGoneLocked(id string, now time.Time) bool {
	t, ok := p.lastSeen[id]
	if !ok || t.Equal(p.seenSaved[id]) {
		return false
	}
	return p.seenDueLocked(now, seenSaveSoon)
}

// seenDueLocked reports whether the last write is at least every ago (or the clock went back), and if so
// counts this moment as a write. Caller holds p.mu.
func (p *Poller) seenDueLocked(now time.Time, every time.Duration) bool {
	if !p.seenWriteAt.IsZero() && !now.Before(p.seenWriteAt) && now.Sub(p.seenWriteAt) < every {
		return false
	}
	p.seenWriteAt = now
	return true
}

// forgetSeenLocked takes the agent's time out, in memory and with the next write in the file (the agent was
// removed, or a host is added under its id: that host was never seen). It reports whether there is something
// to write. Caller holds p.mu.
func (p *Poller) forgetSeenLocked(id string) bool {
	_, mem := p.lastSeen[id]
	_, file := p.seenSaved[id]
	delete(p.lastSeen, id)
	delete(p.seenSaved, id)
	if file {
		p.seenDrop = append(p.seenDrop, id)
	}
	return mem || file
}

// keptSeenLocked is the time to show for an agent whose row has none: the time of its last good answer, from
// this start or from before it. A time that is later than the panel's clock is not shown (the clock was
// ahead when it was written, or was set back since). Caller holds p.mu.
func (p *Poller) keptSeenLocked(id string, now time.Time) *time.Time {
	t, ok := p.lastSeen[id]
	if !ok || t.After(now) {
		return nil
	}
	return &t
}

// FlushSeen writes every time that is not in the file yet. The poller calls it when it stops.
func (p *Poller) FlushSeen() { p.flushSeen() }

// flushSeen writes the times that are not in the file, and takes the entries of removed agents out. One write
// at a time (seenWrite), each with its copy taken while it holds that lock, so an older copy never replaces a
// newer one in the file. Never called with p.mu held.
func (p *Poller) flushSeen() {
	p.seenWrite.Lock()
	defer p.seenWrite.Unlock()
	p.mu.Lock()
	set := map[string]time.Time{}
	for id, t := range p.lastSeen {
		if !t.Equal(p.seenSaved[id]) {
			set[id] = t
		}
	}
	drop := p.seenDrop
	p.seenDrop = nil
	p.mu.Unlock()
	if len(set) == 0 && len(drop) == 0 {
		return
	}
	err := p.Admin.SetAgentSeen(set, drop)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.seenDrop = append(drop, p.seenDrop...)
		if msg := err.Error(); msg != p.seenErr {
			p.seenErr = msg
			slog.Warn("could not keep when the machines were last seen for the next start (it is tried again)", "error", err)
		}
		return
	}
	p.seenErr = ""
	for id, t := range set {
		if _, still := p.lastSeen[id]; still { // not removed meanwhile
			p.seenSaved[id] = t
		}
	}
}
