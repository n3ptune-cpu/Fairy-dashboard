package power

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// UpdateRemote merges agent-reported devices (`power_sources` in the agent's /metrics). rows=nil means the agent
// itself was unreachable this cycle. Device credentials never leave the agent.
func (m *Manager) UpdateRemote(agentID string, rows []map[string]any, unreachable bool, links map[string]string, exclude []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	if unreachable {
		m.markAgentUnreachable(agentID, now)
		return
	}
	seen := map[string]bool{}
	for _, row := range rows {
		rid := jsonString(row["id"])
		if rid == "" {
			continue
		}
		key := agentID + "/" + rid
		seen[key] = true
		prev, hasPrev := m.remote[key]
		if !hasPrev {
			m.remoteKeys = append(m.remoteKeys, key)
		}
		m.remote[key] = m.remoteState(agentID, key, rid, row, prev, hasPrev, now, links, exclude)
	}
	m.dropUnreported(agentID, seen)
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
	if l, ok := links[rid]; ok && l != "" {
		link = model.P(l)
	} else if h := jsonString(row["system_hint"]); h == "self" || h == agentID {
		link = model.P(agentID)
	}
	name := jsonString(row["name"])
	if name == "" {
		name = rid
	}
	var status string
	var last *time.Time
	readings := []model.PowerReading{}
	var errp *string
	if ok, _ := row["ok"].(bool); ok {
		status, last = "ok", &now
		readings = []model.PowerReading{{SourceID: key, Name: name, Watts: jsonNumber(row["watts"]),
			Voltage: jsonNumber(row["voltage"]), Current: jsonNumber(row["current"]), EnergyKWhTotal: jsonNumber(row["energy_kwh_total"]),
			SystemID: link, Timestamp: now}}
	} else {
		if hasPrev {
			last = prev.LastSuccess
		}
		status = m.agedStatus(last, now)
		if hasPrev && status == "stale" {
			readings = prev.Readings
		}
		e := jsonString(row["error"])
		if e == "" {
			e = "error"
		}
		if len(e) > 120 {
			e = e[:120]
		}
		errp = &e
	}
	typ := jsonString(row["type"])
	if typ == "" {
		typ = "unknown"
	}
	return model.PowerSourceState{ID: key, Type: typ, Name: name, Status: status,
		IncludeInTotal: !slices.Contains(exclude, rid), Readings: readings, LastSuccess: last, Error: errp,
		Via: model.P(agentID)}
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
