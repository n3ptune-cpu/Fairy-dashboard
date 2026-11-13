// Package power is the panel's wall power: a Runner per configured source (with backoff) and the Manager
// (overview, watts per system, history, devices relayed by agents). The sources themselves (one per device type:
// plugs, UPSes and BMCs, plus component power reported by agents) are fairy-agent's pkg/power, which the agent
// uses to relay the devices on its own network.
package power

import (
	"context"
	"sync"
	"time"

	powersrc "github.com/n3ptune-cpu/Fairy-agent/pkg/power"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/normalise"
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
	// remoteWarned: agents whose dropped rows were already logged (see UpdateRemote).
	remoteWarned map[string]bool
}

// NewManager builds a Runner per config entry. interval is the history sample interval (s), historyS the
// history length (s), clock a monotonic clock for backoff.
func NewManager(configs []map[string]any, interval float64, timeout time.Duration, offlineAfter float64,
	historyS int, agentPower AgentPowerFn, clock func() float64) (*Manager, error) {
	m := &Manager{IntervalS: interval, Timeout: timeout, offlineAfter: offlineAfter,
		maxHistory: max(1, int(float64(historyS)/interval)), remote: map[string]model.PowerSourceState{}}
	for _, raw := range configs {
		if err := config.PanelPowerSource(raw); err != nil { // a type that only an agent reads (homeassistant)
			return nil, err
		}
		s, err := powersrc.Build(raw, agentPower.metric())
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

// maxSumW bounds a sum of watts (a source's readings, the total, a system's linked sources). Every reading is
// within normalise.MaxReading when it enters (cleanReadings, remoteState); the sums are checked again, because
// two numbers that are each possible can add up to one that is not.
const maxSumW = 1e12

// finiteW returns w when it is a possible sum of watts, else nil.
func finiteW(w float64) *float64 { return normalise.Finite(&w, -maxSumW, maxSumW) }

// cleanReadings drops the values of rs that are not real numbers or lie outside what a device can measure (a
// plug answering 1e308 W, a UPS server answering "NaN"). invalid reports whether a watts value was among them:
// the reading is then not a measurement of the device's power.
func cleanReadings(rs []model.PowerReading) (out []model.PowerReading, invalid bool) {
	out = make([]model.PowerReading, len(rs))
	for i, r := range rs {
		if r.Watts != nil && normalise.Reading(r.Watts) == nil {
			invalid = true
		}
		r.Watts, r.Voltage, r.Current = normalise.Reading(r.Watts), normalise.Reading(r.Voltage), normalise.Reading(r.Current)
		r.EnergyKWhTotal = normalise.Finite(r.EnergyKWhTotal, -normalise.MaxEnergyKWh, normalise.MaxEnergyKWh)
		if y := r.Timestamp.Year(); y < 1970 || y > 9999 { // a time JSON cannot hold
			r.Timestamp = time.Now().UTC()
		}
		out[i] = r
	}
	return out, invalid
}

// sumW is the watts of one source: nil without readings, and nil when the sum is not a possible number.
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
	return finiteW(s)
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
	// Only real numbers go into the history (sumW and Total check their sums): one bad sample would otherwise
	// stay in every answer for the whole history length.
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
	return model.PowerOverview{GeneratedAt: time.Now().UTC(), Sources: states, TotalW: Total(states)}
}

// Total is the wall power of the sources counted in the total (include_in_total, not unreachable); nil when
// none of them has a reading.
func Total(states []model.PowerSourceState) *float64 {
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
	if n == 0 {
		return nil
	}
	return finiteW(normalise.Round(total, 1)) // checked after the sum and the rounding
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
	for id, w := range out { // checked after the sums: a system gets a possible number or none
		if finiteW(w) == nil {
			delete(out, id)
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

// Close releases the sources' connections.
func (m *Manager) Close() {
	for _, r := range m.Runners {
		r.Source.Close()
	}
}
