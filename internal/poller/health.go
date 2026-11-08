package poller

import (
	"log/slog"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/health"
	"github.com/n3ptune-cpu/Fairy/internal/model"
)

// WriteHealth writes <data_dir>/health.json for `fairy admin health` (the CLI never needs a web login).
func (p *Poller) WriteHealth() {
	if err := health.Write(p.Cfg.DataDir, p.healthReport()); err != nil {
		slog.Debug("health.json", "error", err)
	}
}

func (p *Poller) healthReport() health.Report {
	r := health.Report{
		Systems:      map[string]int{"online": 0, "stale": 0, "offline": 0, "stopped": 0, "paused": 0},
		SystemIDs:    []string{},
		Sources:      []model.SourceStatus{},
		PowerSources: []health.Source{},
		UPS:          []health.Source{},
		Jobs:         map[string]health.Job{},
	}
	for _, s := range p.Snapshot() {
		if _, ok := r.Systems[s.Status]; ok {
			r.Systems[s.Status]++
		}
		r.SystemIDs = append(r.SystemIDs, s.ID)
	}
	p.mu.Lock()
	for _, k := range p.sourceOrder {
		r.Sources = append(r.Sources, p.sources[k])
	}
	r.Version = p.version
	p.mu.Unlock()
	for _, s := range p.Power.Overview().Sources {
		r.PowerSources = append(r.PowerSources, health.Source{ID: s.ID, Status: s.Status, Error: s.Error})
	}
	for _, u := range p.UPS.Overview(false).UPS {
		r.UPS = append(r.UPS, health.Source{ID: u.ID, Status: u.Status, Error: u.Error})
	}
	for _, j := range p.Scheduler.Jobs() {
		runs, skipped := j.Stats()
		r.Jobs[j.Key] = health.Job{Runs: runs, Skipped: skipped, EffectiveS: p.Scheduler.Effective(j),
			LastMS: p.Scheduler.LastMS(j)}
	}
	r.WrittenAt = time.Now().UTC().Format(time.RFC3339Nano)
	r.StartedAt = p.StartedAt.Format(time.RFC3339Nano)
	r.ControlDisabled = p.Admin.ControlDisabled()
	return r
}
