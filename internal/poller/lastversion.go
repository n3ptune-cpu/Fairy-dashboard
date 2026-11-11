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
// It is what the agent said then, not what it runs now: AgentInfo and AgentSeen do not return it, so nothing
// that decides on the agent's present state (a wake / update run, a re-pair) uses it. The Updates page shows
// it for a host that does not answer, marked "last seen".

// loadLastVersions reads the kept versions (New). An entry that is not a version the panel would take from an
// agent is not used.
func (p *Poller) loadLastVersions() {
	p.lastVersion = map[string]string{}
	for id, v := range p.Admin.AgentVersions() {
		if release.ValidVersion(v) {
			p.lastVersion[id] = v
		}
	}
}

// noteVersionLocked takes the version of a good answer and reports whether it has to be written (saveVersion,
// after the caller has released p.mu). Caller holds p.mu.
func (p *Poller) noteVersionLocked(id, version string) bool {
	if version == "" || p.lastVersion[id] == version {
		return false
	}
	p.lastVersion[id] = version
	return true
}

// saveVersion writes one agent's last known version ("": the agent was removed). A failure is logged and not
// tried again before the version changes: state.json is then unreadable or not the panel's to write, which
// the panel reports in other places.
func (p *Poller) saveVersion(id, version string, now time.Time) {
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
