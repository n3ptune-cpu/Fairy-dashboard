package power

import (
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/configutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
)

// Limits for the devices one agent relays. An agent is a machine the panel does not control, and every source
// it reports is kept in memory and copied into each history sample, so the list is bounded: at most
// MaxRemoteSources rows per agent, ids as in a config file (configutil.ValidID), names and types cut to a length
// the page can show.
const (
	MaxRemoteSources = 16
	maxRemoteName    = 80
	maxRemoteType    = 32
	maxRemoteError   = 120
)

// clipText is a text of a relayed device as the panel keeps it: cleaned (textsafe.Clean) and cut to at most n
// bytes on a character boundary.
func clipText(s string, n int) string { return textsafe.Cut(s, n) }

// UpdateRemote merges agent-reported devices (`power_sources` in the agent's /metrics). rows=nil means the agent
// itself was unreachable this cycle. Device credentials never leave the agent. Rows over the limits above are
// dropped (the first MaxRemoteSources valid ones are kept) and logged once per agent while it lasts.
func (m *Manager) UpdateRemote(agentID string, rows []map[string]any, unreachable bool, links map[string]string, exclude []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	if unreachable {
		m.markAgentUnreachable(agentID, now)
		return
	}
	seen := map[string]bool{}
	dropped := 0
	for _, row := range rows {
		rid := jsonString(row["id"])
		if rid == "" {
			continue
		}
		key := agentID + "/" + rid
		if !configutil.ValidID(rid) || (len(seen) >= MaxRemoteSources && !seen[key]) {
			dropped++
			continue
		}
		seen[key] = true
		prev, hasPrev := m.remote[key]
		if !hasPrev {
			m.remoteKeys = append(m.remoteKeys, key)
		}
		m.remote[key] = m.remoteState(agentID, key, rid, row, prev, hasPrev, now, links, exclude)
	}
	m.dropUnreported(agentID, seen)
	m.noteUnreported(agentID, rows, links, exclude)
	switch {
	case dropped > 0 && !m.remoteWarned[agentID]:
		if m.remoteWarned == nil {
			m.remoteWarned = map[string]bool{}
		}
		m.remoteWarned[agentID] = true
		slog.Warn("power sources relayed by an agent were dropped (too many, or ids that are not lowercase letters, digits and dashes)",
			"agent", agentID, "kept", len(seen), "dropped", dropped, "limit", MaxRemoteSources)
	case dropped == 0:
		delete(m.remoteWarned, agentID)
	}
}

// noteUnreported says once in the log when the panel's entry of an agent names a device (in power_links or in
// power_exclude_from_total) that the agent's answer does not have: a typing mistake in the device's id, or a
// device that was taken out of the agent's own power_sources. Nothing is linked and nothing is left out of the
// total for such an id, and nothing else would say so: a plug that was meant to be left out of the total is
// then counted. Said again only when the list of such ids changes; an agent that could not be reached is not
// judged. (`fairy admin config check` cannot know this: it reads files and asks no machine.)
func (m *Manager) noteUnreported(agentID string, rows []map[string]any, links map[string]string, exclude []string) {
	reported := map[string]bool{}
	for _, row := range rows {
		reported[jsonString(row["id"])] = true
	}
	var missing []string
	for dev := range links {
		if !reported[dev] {
			missing = append(missing, dev)
		}
	}
	for _, dev := range exclude {
		if !reported[dev] && !slices.Contains(missing, dev) {
			missing = append(missing, dev)
		}
	}
	slices.Sort(missing)
	said := strings.Join(missing, ", ")
	if said == m.unreported[agentID] {
		return
	}
	if m.unreported == nil {
		m.unreported = map[string]string{}
	}
	if said == "" {
		delete(m.unreported, agentID)
		return
	}
	m.unreported[agentID] = said
	slog.Warn("config: the panel's entry of an agent names devices the agent does not report (power_links / power_exclude_from_total): nothing is linked or left out of the total for them",
		"agent", agentID, "devices", textsafe.Cut(said, 400))
}

// agedStatus is "stale" while the last good reading is younger than offline_after, then "unreachable".
func (m *Manager) agedStatus(last *time.Time, now time.Time) string {
	if last != nil && now.Sub(*last).Seconds() < m.offlineAfter {
		return "stale"
	}
	return "unreachable"
}

// markAgentUnreachable ages every device relayed by the agent; readings are kept only while "stale".
func (m *Manager) markAgentUnreachable(agentID string, now time.Time) {
	for k, st := range m.remote {
		if st.Via != nil && *st.Via == agentID {
			st.Status = m.agedStatus(st.LastSuccess, now)
			st.Error = model.P("agent unreachable")
			if st.Status != "stale" {
				st.Readings = []model.PowerReading{}
			}
			m.remote[k] = st
		}
	}
}

// remoteState builds the state of one agent-reported device row. The device is linked to a system by the
// agent's `power_links`, or to the agent itself when the row says system_hint "self" (or the agent id).
func (m *Manager) remoteState(agentID, key, rid string, row map[string]any, prev model.PowerSourceState, hasPrev bool,
	now time.Time, links map[string]string, exclude []string) model.PowerSourceState {
	var link *string
	h := jsonString(row["system_hint"])
	own := h == "self" || h == agentID
	if l, ok := links[rid]; ok && l != "" {
		link = model.P(l)
	} else if own {
		link = model.P(agentID)
	}
	name := clipText(jsonString(row["name"]), maxRemoteName)
	if name == "" {
		name = rid
	}
	var status string
	var last *time.Time
	readings := []model.PowerReading{}
	var errp *string
	ok, _ := row["ok"].(bool)
	rowErr := jsonString(row["error"])
	if ok {
		var invalid bool
		readings, invalid = cleanReadings([]model.PowerReading{{SourceID: key, Name: name, Watts: jsonNumber(row["watts"]),
			Voltage: jsonNumber(row["voltage"]), Current: jsonNumber(row["current"]), EnergyKWhTotal: jsonNumber(row["energy_kwh_total"]),
			SystemID: link, Timestamp: now}})
		if invalid { // "ok" with watts no device can report (1e308): a failed row, not a reading without watts
			ok, rowErr = false, "the agent relayed a value that is not a possible reading"
		}
	}
	if ok {
		status, last = "ok", &now
	} else {
		readings = []model.PowerReading{}
		if hasPrev {
			last = prev.LastSuccess
		}
		status = m.agedStatus(last, now)
		if hasPrev && status == "stale" {
			readings = prev.Readings
		}
		e := clipText(rowErr, maxRemoteError)
		if e == "" {
			e = "error"
		}
		errp = &e
	}
	typ := clipText(jsonString(row["type"]), maxRemoteType)
	if typ == "" {
		typ = "unknown"
	}
	// A row that fails for longer than offline_after has no reading any more, and it stays: for as long as the
	// agent reports it, with the agent's words for why ("home assistant refused the token (http 401)", which an
	// agent goes on reporting through the hour it waits before it asks Home Assistant again), and with its
	// link (SystemID), which the readings carried alone until build 14. Without the link such a row was gone
	// for everyone who saw it because of the machine it is linked to, and its reason showed on no host's page.
	return model.PowerSourceState{ID: key, Type: typ, Name: name, Status: status,
		IncludeInTotal: !slices.Contains(exclude, rid), Readings: readings, LastSuccess: last, Error: errp,
		Via: model.P(agentID), SystemID: link, OwnHint: own}
}

// dropUnreported forgets the agent's devices that were not in this report (removed from the agent's config).
func (m *Manager) dropUnreported(agentID string, seen map[string]bool) {
	keys := m.remoteKeys[:0]
	for _, k := range m.remoteKeys {
		st := m.remote[k]
		if st.Via != nil && *st.Via == agentID && !seen[k] {
			delete(m.remote, k)
			continue
		}
		keys = append(keys, k)
	}
	m.remoteKeys = keys
}

// jsonString formats a JSON value as a string: whole numbers without a decimal point ("7", not "7.0"), nil as "".
func jsonString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		if x == math.Trunc(x) {
			return fmt.Sprintf("%d", int64(x))
		}
	}
	return fmt.Sprint(v)
}

// jsonNumber returns v as a number, or nil when it is not a JSON number.
func jsonNumber(v any) *float64 {
	if x, ok := v.(float64); ok {
		return &x
	}
	return nil
}
