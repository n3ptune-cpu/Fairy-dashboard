// Package ups makes UPSes a first-class feature: NUT polling (by the panel or relayed by an agent), state,
// transitions, events and history.
//
// Read-only by design: only LIST UPS / LIST VAR are ever sent (no INSTCMD, no SET VAR).
package ups

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/model"
	"github.com/n3ptune-cpu/Fairy/internal/normalise"
)

// Manager holds every UPS (polled by the panel or relayed by agents), their history and events.
type Manager struct {
	Sources      []*Source
	timeout      time.Duration
	offlineAfter float64
	eventsFile   string
	maxlen       int

	mu      sync.Mutex
	states  map[string]*model.UpsState
	order   []string
	history map[string][]model.UpsPoint
	events  []model.UpsEvent
	seq     int
	// OnEvent is called (outside the lock) for every new event (notifications).
	OnEvent func(model.UpsEvent)
}

// NewManager builds the configured sources. interval is the history sample interval (s), historyS the
// history length (s), eventsFile the JSON-lines event log ("" = none).
func NewManager(configs []map[string]any, interval float64, timeout time.Duration, offlineAfter float64,
	historyS int, eventsFile string) (*Manager, error) {
	m := &Manager{timeout: timeout, offlineAfter: offlineAfter, eventsFile: eventsFile,
		maxlen: max(1, int(float64(historyS)/interval)), states: map[string]*model.UpsState{},
		history: map[string][]model.UpsPoint{}}
	for _, raw := range configs {
		c, err := Build(raw)
		if err != nil {
			return nil, err
		}
		m.Sources = append(m.Sources, c)
	}
	return m, nil
}

var rowSkip = map[string]bool{"name": true, "status": true, "powers": true, "id": true, "via": true,
	"ups_name": true, "last_seen": true, "on_battery_since": true, "stale": true}

func (m *Manager) apply(key string, row map[string]any, name string, via *string, powers []string) {
	now := time.Now().UTC()
	prev := m.states[key]
	fields := map[string]any{}
	for k, v := range row {
		if !rowSkip[k] {
			fields[k] = v
		}
	}
	b, _ := json.Marshal(fields)
	st := &model.UpsState{}
	_ = json.Unmarshal(b, st)
	st.ID, st.Name, st.UpsName, st.Via, st.Powers, st.LastSeen = key, name, fmt.Sprint(row["name"]), via, powers, &now
	st.Status, _ = row["state"].(string)
	if st.Status == "" {
		st.Status = "unknown"
	}
	st.Stale = false
	if st.Flags == nil {
		st.Flags = []string{}
	}
	if st.Powers == nil {
		st.Powers = []string{}
	}
	if onBatt(st.Status) {
		if prev != nil && prev.OnBatterySince != nil {
			st.OnBatterySince = prev.OnBatterySince
		} else {
			st.OnBatterySince = &now
		}
	}
	m.transitions(prev, st)
	if prev == nil {
		m.order = append(m.order, key)
	}
	m.states[key] = st
}

func (m *Manager) fail(keys []string, errMsg string) {
	now := time.Now()
	for _, key := range keys {
		st := m.states[key]
		if st == nil {
			continue
		}
		cp := *st
		gone := st.LastSeen == nil || now.Sub(*st.LastSeen).Seconds() >= m.offlineAfter
		cp.Stale = true
		cp.Error = model.P(errMsg)
		if gone && st.Status != "unreachable" {
			cp.Status = "unreachable"
			m.event(&cp, "communication_lost", "warning", "lost communication with UPS ("+errMsg+")")
		}
		m.states[key] = &cp
	}
}

// PollSource polls one configured NUT source.
func (m *Manager) PollSource(ctx context.Context, c *Source, timeout time.Duration) {
	if timeout <= 0 {
		timeout = m.timeout
	}
	rows, err := c.Read(ctx, timeout)
	m.mu.Lock()
	evBefore := m.seq
	if err != nil {
		var keys []string
		for _, k := range m.order {
			if strings.HasPrefix(k, c.ID+"/") {
				keys = append(keys, k)
			}
		}
		m.fail(keys, normalise.Describe(err))
	} else {
		for _, row := range rows {
			n := fmt.Sprint(row["name"])
			label := n
			if c.Name != "" && len(rows) == 1 {
				label = c.Name
			} else if d, _ := row["description"].(string); d != "" {
				label = d
			}
			m.apply(c.ID+"/"+n, row, label, nil, c.Powers.For(n))
		}
	}
	evs := m.newEvents(evBefore)
	m.mu.Unlock()
	m.emit(evs)
}

// PollAll polls every source once, then samples history (tests).
func (m *Manager) PollAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, s := range m.Sources {
		wg.Add(1)
		go func(s *Source) { defer wg.Done(); m.PollSource(ctx, s, 0) }(s)
	}
	wg.Wait()
	m.SampleHistory()
}

// SampleHistory appends one point per reachable UPS to its history ring.
func (m *Manager) SampleHistory() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	for _, k := range m.order {
		st := m.states[k]
		if st.Status == "unreachable" {
			continue
		}
		h := append(m.history[k], model.UpsPoint{T: now, Charge: st.BatteryCharge, Load: st.LoadPercent,
			Watts: st.Watts, RuntimeS: st.BatteryRuntimeS})
		if len(h) > m.maxlen {
			h = h[len(h)-m.maxlen:]
		}
		m.history[k] = h
	}
}

// UpdateFromAgent merges `ups` rows relayed by an agent. unreachable: the agent itself was unreachable.
func (m *Manager) UpdateFromAgent(agentID string, rows []map[string]any, unreachable bool, powers map[string][]string, names map[string]string) {
	m.mu.Lock()
	evBefore := m.seq
	var mine []string
	for _, k := range m.order {
		if v := m.states[k].Via; v != nil && *v == agentID {
			mine = append(mine, k)
		}
	}
	if unreachable {
		m.fail(mine, "agent unreachable")
	} else {
		seen := map[string]bool{}
		for _, row := range rows {
			n, _ := row["name"].(string)
			if n == "" {
				continue
			}
			key := agentID + "/" + n
			if ok, isBool := row["ok"].(bool); isBool && !ok {
				var hit []string
				for _, k := range mine {
					if k == key || n == "upsd" {
						hit = append(hit, k)
						seen[k] = true
					}
				}
				e := fmt.Sprint(row["error"])
				if row["error"] == nil || e == "" {
					e = "error"
				}
				if len(e) > 120 {
					e = e[:120]
				}
				m.fail(hit, e)
				continue
			}
			seen[key] = true
			label := names[n]
			for _, k := range []string{"description", "model"} {
				if label == "" {
					label, _ = row[k].(string)
				}
			}
			if label == "" {
				label = n
			}
			p := slices.Clone(powers[n])
			if p == nil {
				p = []string{}
			}
			m.apply(key, row, label, model.P(agentID), p)
		}
		for _, k := range mine {
			if !seen[k] { // agent no longer reports it (UPS removed / relay turned off)
				delete(m.states, k)
				m.order = slices.DeleteFunc(m.order, func(x string) bool { return x == k })
			}
		}
	}
	evs := m.newEvents(evBefore)
	m.mu.Unlock()
	m.emit(evs)
}

// Overview is GET /api/ups (with or without history).
func (m *Manager) Overview(history bool) model.UpsOverview {
	m.mu.Lock()
	defer m.mu.Unlock()
	ov := model.UpsOverview{GeneratedAt: time.Now().UTC(), UPS: []model.UpsState{}, History: map[string][]model.UpsPoint{}}
	for _, k := range m.order {
		ov.UPS = append(ov.UPS, *m.states[k])
	}
	if history {
		for k, v := range m.history {
			ov.History[k] = slices.Clone(v)
		}
	}
	return ov
}

// SystemsMap returns system id -> ids of the UPSes powering it.
func (m *Manager) SystemsMap() map[string][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string][]string{}
	for _, k := range m.order {
		st := m.states[k]
		for _, sid := range st.Powers {
			out[sid] = append(out[sid], st.ID)
		}
	}
	return out
}

// State returns a copy of one UPS's state (for tests).
func (m *Manager) State(key string) *model.UpsState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.states[key]; s != nil {
		cp := *s
		return &cp
	}
	return nil
}
