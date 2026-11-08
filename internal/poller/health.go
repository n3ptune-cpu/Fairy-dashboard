package poller

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/model"
)

// WriteHealth writes <data_dir>/health.json for `fairy admin health` (the CLI never needs a web login).
func (p *Poller) WriteHealth() {
	snap := p.Snapshot()
	counts := map[string]int{"online": 0, "stale": 0, "offline": 0, "stopped": 0, "paused": 0}
	ids := []string{}
	for _, s := range snap {
		if _, ok := counts[s.Status]; ok {
			counts[s.Status]++
		}
		ids = append(ids, s.ID)
	}
	p.mu.Lock()
	srcs := []model.SourceStatus{}
	for _, k := range p.sourceOrder {
		srcs = append(srcs, p.sources[k])
	}
	ver := p.version
	p.mu.Unlock()
	ps := []map[string]any{}
	for _, s := range p.Power.Overview().Sources {
		ps = append(ps, map[string]any{"id": s.ID, "status": s.Status, "error": s.Error})
	}
	us := []map[string]any{}
	for _, u := range p.UPS.Overview(false).UPS {
		us = append(us, map[string]any{"id": u.ID, "status": u.Status, "error": u.Error})
	}
	jobs := map[string]any{}
	for _, j := range p.Scheduler.Jobs() {
		runs, skipped := j.Stats()
		jobs[j.Key] = map[string]any{"runs": runs, "skipped": skipped, "effective_s": p.Scheduler.Effective(j),
			"last_ms": p.Scheduler.LastMS(j)}
	}
	data := map[string]any{"written_at": time.Now().UTC().Format(time.RFC3339Nano),
		"started_at": p.StartedAt.Format(time.RFC3339Nano), "version": ver,
		"control_disabled": p.Admin.ControlDisabled(), "systems": counts, "system_ids": ids, "sources": srcs,
		"power_sources": ps, "ups": us, "jobs": jobs}
	b, _ := json.MarshalIndent(data, "", " ")
	path := filepath.Join(p.Cfg.DataDir, "health.json")
	tmp := path + ".tmp"
	if err := os.MkdirAll(p.Cfg.DataDir, 0o700); err != nil {
		return
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		slog.Debug("health.json", "error", err)
		return
	}
	_ = os.Rename(tmp, path)
}
