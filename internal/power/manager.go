package power

import (
	"context"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/model"
	"github.com/n3ptune-cpu/Fairy/internal/normalise"
)

// Manager holds every power source: the configured ones (Runners) and the ones relayed by agents.
type Manager struct {
	Runners      []*Runner
	IntervalS    float64
	Timeout      time.Duration
	offlineAfter float64

	mu         sync.Mutex
	history    []model.PowerPoint
	maxHistory int
	remote     map[string]model.PowerSourceState // reported by agents for devices on their LAN
	remoteKeys []string
}

// NewManager builds a Runner per config entry. interval is the history sample interval (s), historyS the
// history length (s), clock a monotonic clock for backoff.
func NewManager(configs []map[string]any, interval float64, timeout time.Duration, offlineAfter float64,
	historyS int, agentPower AgentPowerFn, clock func() float64) (*Manager, error) {
	m := &Manager{IntervalS: interval, Timeout: timeout, offlineAfter: offlineAfter,
		maxHistory: max(1, int(float64(historyS)/interval)), remote: map[string]model.PowerSourceState{}}
	for _, raw := range configs {
		s, err := Build(raw, agentPower)
		if err != nil {
			return nil, err
		}
		iv := s.DefaultInterval()
		if b := s.Info(); b.Interval != nil {
			iv = *b.Interval
		}
		m.Runners = append(m.Runners, &Runner{Source: s, Interval: iv, offlineAfter: offlineAfter, clock: clock})
	}
	return m, nil
}

// PollAll polls every source once, then samples history (tests / first poll).
func (m *Manager) PollAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, r := range m.Runners {
		wg.Add(1)
		go func(r *Runner) { defer wg.Done(); r.Poll(ctx, m.Timeout) }(r)
	}
	wg.Wait()
	m.SampleHistory()
}

func sumW(rs []model.PowerReading) *float64 {
	if len(rs) == 0 {
		return nil
	}
	s := 0.0
	for _, r := range rs {
		if r.Watts != nil {
			s += *r.Watts
		}
	}
	return &s
}

// SampleHistory appends one point (total and per-source watts) to the history ring.
func (m *Manager) SampleHistory() {
	m.mu.Lock()
	empty := len(m.Runners) == 0 && len(m.remote) == 0
	m.mu.Unlock()
	if empty {
		return
	}
	ov := m.Overview()
	p := model.PowerPoint{T: ov.GeneratedAt, TotalW: ov.TotalW, Sources: map[string]*float64{}}
	for _, s := range ov.Sources {
		p.Sources[s.ID] = sumW(s.Readings)
	}
	m.mu.Lock()
	m.history = append(m.history, p)
	if len(m.history) > m.maxHistory {
		m.history = m.history[len(m.history)-m.maxHistory:]
	}
	m.mu.Unlock()
}

// Overview is GET /api/power: every source, and the total of the sources counted in it.
func (m *Manager) Overview() model.PowerOverview {
	states := make([]model.PowerSourceState, 0, len(m.Runners))
	for _, r := range m.Runners {
		states = append(states, r.State())
	}
	m.mu.Lock()
	for _, k := range m.remoteKeys {
		states = append(states, m.remote[k])
	}
	m.mu.Unlock()
	var total float64
	n := 0
	for _, s := range states {
		if !s.IncludeInTotal || s.Status == "unreachable" {
			continue
		}
		for _, rd := range s.Readings {
			if rd.Watts != nil {
				total += *rd.Watts
				n++
			}
		}
	}
	ov := model.PowerOverview{GeneratedAt: time.Now().UTC(), Sources: states}
	if n > 0 {
		ov.TotalW = model.P(normalise.Round(total, 1))
	}
	return ov
}

// LinkedWatts returns system id -> watts from the sources linked to that system.
func (m *Manager) LinkedWatts() map[string]float64 {
	out := map[string]float64{}
	for _, s := range m.Overview().Sources {
		if s.Status == "unreachable" {
			continue
		}
		for _, rd := range s.Readings {
			if rd.SystemID != nil && rd.Watts != nil {
				out[*rd.SystemID] = normalise.Round(out[*rd.SystemID]+*rd.Watts, 1)
			}
		}
	}
	return out
}

// HistoryView is GET /api/power/history; with a source id, every point holds only that source.
func (m *Manager) HistoryView(source string) model.PowerHistory {
	m.mu.Lock()
	pts := append([]model.PowerPoint{}, m.history...)
	m.mu.Unlock()
	if source != "" {
		for i, p := range pts {
			v := p.Sources[source]
			pts[i] = model.PowerPoint{T: p.T, TotalW: v, Sources: map[string]*float64{source: v}}
		}
	}
	return model.PowerHistory{IntervalS: m.IntervalS, Points: pts}
}

// Close releases the sources' connections (Tuya keeps one open).
func (m *Manager) Close() {
	for _, r := range m.Runners {
		r.Source.Close()
	}
}
