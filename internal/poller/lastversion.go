package poller

import (
	"log/slog"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/release"
)

// The last known agent version. What an agent reported is only in memory (agentInfo), so after a restart of
// the panel a host that is off had no version until it came back. The version of each agent's last good
// answer is therefore also kept in state.json (agent_versions, see adminstate.AgentVersions) and read once
// when the poller is made. It is written when an agent reports another version than the one that is kept:
// after an update, or the first time an agent answers; not on every poll.
//
// And not more often than once in versionSaveEvery for one agent (r2-p2-11): an agent that reports another
// valid version at every poll made the panel rewrite state.json at every poll, 87 times in 90 seconds, each
// time a lock, a read, a rewrite and a rename of the file that also holds the kill switch and the session
// cut-off, and a reload of it by every reader. The first version of an agent is written at once, a change
// more than a minute after the last write at once, a change sooner than that with the agent's first answer
// after the minute; what is shown comes from memory and is always the latest. A version that still waits when
// the panel stops is written then (FlushVersions).
//
// It is what the agent said then, not what it runs now: AgentInfo and AgentSeen do not return it, so nothing
// that decides on the agent's present state (a wake / update run, a re-pair) uses it. The Updates page shows
// it for a host that does not answer, marked "last seen".

// loadLastVersions reads the kept versions (New). An entry that is not a version the panel would take from an
// agent is not used.
func (p *Poller) loadLastVersions() {
	p.lastVersion, p.versionSavedAt, p.versionUnsaved = map[string]string{}, map[string]time.Time{}, map[string]bool{}
	for id, v := range p.Admin.AgentVersions() {
		if release.ValidVersion(v) {
			p.lastVersion[id] = v
		}
	}
}

// versionSaveEvery is how often at most one agent's version is written to state.json.
const versionSaveEvery = time.Minute

// noteVersionLocked takes the version of a good answer and reports whether it has to be written now
// (saveVersion, after the caller has released p.mu). Caller holds p.mu.
func (p *Poller) noteVersionLocked(id, version string, now time.Time) bool {
	if version == "" {
		return false
	}
	if p.lastVersion[id] != version {
		p.lastVersion[id] = version
		p.versionUnsaved[id] = true
	}
	if !p.versionUnsaved[id] {
		return false
	}
	if last, ok := p.versionSavedAt[id]; ok && !now.Before(last) && now.Sub(last) < versionSaveEvery {
		return false // written less than a minute ago: this one waits (it is in memory, and FlushVersions has it)
	}
	p.versionSavedAt[id] = now
	delete(p.versionUnsaved, id)
	return true
}

// FlushVersions writes every agent's version that still waits for its minute. The poller calls it when it
// stops, so the last value an agent reported is not lost by the limit.
func (p *Poller) FlushVersions() {
	p.mu.Lock()
	now, _ := p.now()
	waiting := map[string]string{}
	for id := range p.versionUnsaved {
		waiting[id] = p.lastVersion[id]
		p.versionSavedAt[id] = now
	}
	clear(p.versionUnsaved)
	p.mu.Unlock()
	for id, v := range waiting {
		if v != "" {
			p.saveVersion(id, now)
		}
	}
}

// saveVersion writes what the panel knows of one agent's last version at the moment of the write: the version
// in memory, or, when there is none (the agent was removed), nothing: the agent's entry is taken out. A
// failure is logged and not tried again before the version changes: state.json is then unreadable or not the
// panel's to write, which the panel reports in other places. Never called with p.mu held.
//
// It used to write the version its caller handed it, and the callers decide under p.mu and write after they
// have released it: a poll that had taken a new version in, and a Remove of that host in the same instant
// (also the first half of every Re-pair), wrote one after the other in either order. When the poll's write
// came last, state.json kept a version for a host that was gone, and after the next start a host added under
// that id later was shown with it (fourth review of build 14, F3). Now one write is made at a time
// (versionWrite), and each reads what is true under p.mu while it holds that lock: the later write is never
// older than the one before it, whoever asked for it. (flushSeen does the same for the times.)
func (p *Poller) saveVersion(id string, now time.Time) {
	p.versionWrite.Lock()
	defer p.versionWrite.Unlock()
	p.mu.Lock()
	version := p.lastVersion[id]
	p.mu.Unlock()
	if version != "" && now.IsZero() { // asked for by a Remove, and a host added under the id since has answered
		now, _ = p.now()
	}
	if err := p.Admin.SetAgentVersion(id, version, now); err != nil {
		slog.Warn("could not keep the agent's version for the next start", "agent", id, "error", err)
	}
}

// LastAgentVersion is the version the agent reported in its last good answer, also one from before the panel
// started ("": none known). It says nothing about whether the agent answers now.
func (p *Poller) LastAgentVersion(id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastVersion[id]
}
