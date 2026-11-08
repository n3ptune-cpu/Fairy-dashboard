package power

import (
	"fmt"
	"math"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/model"
)

func str(v any) string {
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

func f(v any) *float64 {
	if x, ok := v.(float64); ok {
		return &x
	}
	return nil
}

// UpdateRemote merges agent-reported devices (`power_sources` in the agent's /metrics). rows=nil means the agent
// itself was unreachable this cycle. Device credentials never leave the agent.
func (m *Manager) UpdateRemote(agentID string, rows []map[string]any, unreachable bool, links map[string]string, exclude []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	aged := func(last *time.Time) string {
		if last != nil && now.Sub(*last).Seconds() < m.offlineAfter {
			return "stale"
		}
		return "unreachable"
	}
	if unreachable {
		for k, st := range m.remote {
			if st.Via != nil && *st.Via == agentID {
				st.Status = aged(st.LastSuccess)
				st.Error = model.P("agent unreachable")
				if st.Status != "stale" {
					st.Readings = []model.PowerReading{}
				}
				m.remote[k] = st
			}
		}
		return
	}
	seen := map[string]bool{}
	for _, row := range rows {
		rid := str(row["id"])
		if rid == "" {
			continue
		}
		key := agentID + "/" + rid
		seen[key] = true
		prev, hasPrev := m.remote[key]
		var link *string
		if l, ok := links[rid]; ok && l != "" {
			link = model.P(l)
		} else if h := str(row["system_hint"]); h == "self" || h == agentID {
			link = model.P(agentID)
		}
		name := str(row["name"])
		if name == "" {
			name = rid
		}
		var status string
		var last *time.Time
		readings := []model.PowerReading{}
		var errp *string
		if ok, _ := row["ok"].(bool); ok {
			status, last = "ok", &now
			readings = []model.PowerReading{{SourceID: key, Name: name, Watts: f(row["watts"]),
				Voltage: f(row["voltage"]), Current: f(row["current"]), EnergyKWhTotal: f(row["energy_kwh_total"]),
				SystemID: link, Timestamp: now}}
		} else {
			if hasPrev {
				last = prev.LastSuccess
			}
			status = aged(last)
			if hasPrev && status == "stale" {
				readings = prev.Readings
			}
			e := str(row["error"])
			if e == "" {
				e = "error"
			}
			if len(e) > 120 {
				e = e[:120]
			}
			errp = &e
		}
		typ := str(row["type"])
		if typ == "" {
			typ = "unknown"
		}
		incl := true
		for _, x := range exclude {
			if x == rid {
				incl = false
			}
		}
		if !hasPrev {
			m.remoteKeys = append(m.remoteKeys, key)
		}
		m.remote[key] = model.PowerSourceState{ID: key, Type: typ, Name: name, Status: status, IncludeInTotal: incl,
			Readings: readings, LastSuccess: last, Error: errp, Via: model.P(agentID)}
	}
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
