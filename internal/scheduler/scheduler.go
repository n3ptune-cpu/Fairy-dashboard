// Package scheduler: per-source polling.
//
//   - Every source (one Proxmox cluster, one agent, one plug, one UPS daemon) is a Job with its own interval.
//   - A job never overlaps itself: if the previous run is still going when it is due, that slot is skipped.
//   - Per-host overrides (1-300 s or "paused") come from the API and persist in <data_dir>/state.json.
//   - "Boost while viewing": while someone has a host's page open, its jobs run at 1 s (30 s grace afterwards).
//   - Each job has a floor (e.g. Redfish/BMC 10 s, Tuya 5 s) that clamps overrides and boosts.
package scheduler

import (
	"context"
	"log/slog"
	"math"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/model"
	"github.com/n3ptune-cpu/Fairy/internal/statefile"
)

const (
	MinInterval   = 1.0
	MaxInterval   = 300.0
	BoostInterval = 1.0
	BoostGrace    = 30.0
)

// Override: Paused or seconds.
type Override struct {
	Paused  bool
	Seconds float64
}

func (o Override) JSON() any {
	if o.Paused {
		return "paused"
	}
	return o.Seconds
}

// Fn runs one poll with the given per-request timeout; it may return a backoff (seconds) to delay the next run.
type Fn func(ctx context.Context, timeout time.Duration) float64

type Job struct {
	Key        string
	Fn         Fn
	DefaultS   float64
	FloorS     float64
	Systems    func() []string
	TimeoutCap float64
	Phase      int // RunAllOnce: phase 0 (Proxmox, agents) before phase 1 (sources derived from agents)

	nextDue      float64
	running      bool
	runs         int
	skipped      int
	lastDuration *float64
}

func (j *Job) systems() []string {
	if j.Systems == nil {
		return nil
	}
	return j.Systems()
}

type boostRef struct {
	n         int
	releaseAt float64
}

type Scheduler struct {
	mu           sync.Mutex
	jobs         map[string]*Job
	order        []string
	overrides    map[string]Override
	stateFile    string
	BoostEnabled bool
	Clock        func() float64
	boost        map[string]*boostRef
	dirty        bool
	wg           sync.WaitGroup
	ctx          context.Context
}

var start = time.Now()

// Monotonic seconds since process start.
func Monotonic() float64 { return time.Since(start).Seconds() }

func New(stateFile string, boostEnabled bool, clock func() float64) *Scheduler {
	if clock == nil {
		clock = Monotonic
	}
	s := &Scheduler{jobs: map[string]*Job{}, overrides: map[string]Override{}, stateFile: stateFile,
		BoostEnabled: boostEnabled, Clock: clock, boost: map[string]*boostRef{}, ctx: context.Background()}
	if stateFile != "" {
		s.overrides = Valid(statefile.Read(stateFile)["poll_overrides"])
	}
	return s
}

// Valid parses persisted overrides, dropping invalid entries.
func Valid(raw any) map[string]Override {
	out := map[string]Override{}
	m, ok := raw.(map[string]any)
	if !ok {
		return out
	}
	for k, v := range m {
		switch x := v.(type) {
		case string:
			if x == "paused" {
				out[k] = Override{Paused: true}
			}
		case float64:
			if x >= MinInterval && x <= MaxInterval {
				out[k] = Override{Seconds: x}
			}
		}
	}
	return out
}

func (s *Scheduler) has(j *Job, id string) bool { return slices.Contains(j.systems(), id) }

// Reload adopts overrides changed on disk (admin CLI) and reschedules affected jobs at once.
func (s *Scheduler) Reload(raw any) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	nw := Valid(raw)
	changed := map[string]bool{}
	for k, v := range nw {
		if o, ok := s.overrides[k]; !ok || o != v {
			changed[k] = true
		}
	}
	for k := range s.overrides {
		if _, ok := nw[k]; !ok {
			changed[k] = true
		}
	}
	if len(changed) == 0 {
		return false
	}
	s.overrides = nw
	for _, j := range s.jobs {
		for _, id := range j.systems() {
			if changed[id] {
				j.nextDue = 0
				break
			}
		}
	}
	s.dirty = true
	return true
}

// SetOverride: nil = back to the configured default. Reschedules immediately and persists.
func (s *Scheduler) SetOverride(id string, v *Override) error {
	s.mu.Lock()
	if v == nil {
		delete(s.overrides, id)
	} else if v.Paused {
		s.overrides[id] = Override{Paused: true}
	} else {
		s.overrides[id] = Override{Seconds: math.Min(MaxInterval, math.Max(MinInterval, v.Seconds))}
	}
	snap := map[string]any{}
	for k, o := range s.overrides {
		snap[k] = o.JSON()
	}
	for _, j := range s.jobs {
		if s.has(j, id) {
			j.nextDue = 0
		}
	}
	s.dirty = true
	s.mu.Unlock()
	if s.stateFile != "" {
		_, err := statefile.Update(s.stateFile, func(d statefile.Data) error { d["poll_overrides"] = snap; return nil })
		return err
	}
	return nil
}

func (s *Scheduler) Overrides() map[string]Override {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Override, len(s.overrides))
	for k, v := range s.overrides {
		out[k] = v
	}
	return out
}

func (s *Scheduler) IsPaused(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.overrides[id].Paused
}

// ------------------------------------------------------------- boost while viewing
func (s *Scheduler) BoostAcquire(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.BoostEnabled {
		return
	}
	r := s.boost[id]
	if r == nil {
		r = &boostRef{}
		s.boost[id] = r
	}
	r.n++
	now := s.Clock()
	for _, j := range s.jobs {
		if s.has(j, id) {
			j.nextDue = math.Min(j.nextDue, now+BoostInterval)
		}
	}
	s.dirty = true
}

func (s *Scheduler) BoostRelease(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.boost[id]; r != nil {
		r.n = max(0, r.n-1)
		if r.n == 0 {
			r.releaseAt = s.Clock() + BoostGrace
		}
	}
}

func (s *Scheduler) Boosted(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.boosted(id)
}

func (s *Scheduler) boosted(id string) bool {
	r := s.boost[id]
	if r == nil {
		return false
	}
	if r.n > 0 || s.Clock() < r.releaseAt {
		return true
	}
	delete(s.boost, id)
	return false
}

// ------------------------------------------------------------- effective interval
func (s *Scheduler) requested(j *Job, id string) float64 {
	if o, ok := s.overrides[id]; ok && !o.Paused {
		return o.Seconds
	}
	return j.DefaultS
}

// effective: nil = paused. A shared job (a Proxmox cluster) runs as fast as the fastest of its non-paused
// systems asks; systems without an override count at the job default, so slowing one node never slows the rest.
func (s *Scheduler) effective(j *Job) *float64 {
	ids := j.systems()
	active := []string{}
	for _, id := range ids {
		if !s.overrides[id].Paused {
			active = append(active, id)
		}
	}
	if len(ids) > 0 && len(active) == 0 {
		return nil
	}
	base := j.DefaultS
	if len(active) > 0 {
		base = math.Inf(1)
		for _, id := range active {
			base = math.Min(base, s.requested(j, id))
		}
	}
	for _, id := range active {
		if s.boosted(id) {
			base = math.Min(base, BoostInterval)
			break
		}
	}
	v := math.Min(MaxInterval, math.Max(j.floor(), base))
	return &v
}

func (j *Job) floor() float64 { return math.Max(MinInterval, j.FloorS) }

func (s *Scheduler) Effective(j *Job) *float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.effective(j)
}

// TimeoutFor: per-request timeout below the interval (at least 0.8 s), capped by the source's own timeout.
func TimeoutFor(j *Job, interval float64) time.Duration {
	c := j.TimeoutCap
	if c <= 0 {
		c = 10
	}
	return time.Duration(math.Max(0.8, math.Min(c, interval*0.9)) * float64(time.Second))
}

// Describe lists the jobs feeding one system (for the poll menu).
func (s *Scheduler) Describe(id string) []model.PollJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []model.PollJob{}
	for _, key := range s.order {
		j := s.jobs[key]
		if !s.has(j, id) {
			continue
		}
		eff := s.effective(j)
		boosted := s.boosted(id)
		wanted := s.requested(j, id)
		if boosted {
			wanted = BoostInterval
		}
		pj := model.PollJob{Job: j.Key, DefaultS: j.DefaultS, FloorS: j.floor(), EffectiveS: eff,
			Clamped: eff != nil && wanted < j.floor(), Boosted: boosted, Paused: eff == nil,
			Shared: len(j.systems()) > 1, Runs: j.runs, SkippedOverlaps: j.skipped}
		if j.lastDuration != nil {
			pj.LastDurationMS = model.P(math.Round(*j.lastDuration*10000) / 10)
		}
		out = append(out, pj)
	}
	return out
}

// TakeDirty reports (and clears) whether anything changed since the last call.
func (s *Scheduler) TakeDirty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.dirty
	s.dirty = false
	return d
}

// ------------------------------------------------------------- running
func (s *Scheduler) Add(j *Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[j.Key]; !ok {
		s.order = append(s.order, j.Key)
	}
	s.jobs[j.Key] = j
}

func (s *Scheduler) Jobs() []*Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Job, 0, len(s.order))
	for _, k := range s.order {
		out = append(out, s.jobs[k])
	}
	return out
}

// Stats returns (runs, skipped) for tests.
func (j *Job) Stats() (int, int) { return j.runs, j.skipped }

func (s *Scheduler) run(ctx context.Context, j *Job, interval float64) {
	t0 := time.Now()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("job panicked", "job", j.Key, "panic", r)
		}
		d := time.Since(t0).Seconds()
		s.mu.Lock()
		j.running = false
		j.runs++
		j.lastDuration = &d
		s.dirty = true
		s.mu.Unlock()
	}()
	hard := time.Duration(math.Max(30, interval*4) * float64(time.Second))
	cctx, cancel := context.WithTimeout(ctx, hard)
	defer cancel()
	backoff := j.Fn(cctx, TimeoutFor(j, interval))
	if backoff > 0 { // source asks to wait longer (e.g. host offline)
		s.mu.Lock()
		j.nextDue = math.Max(j.nextDue, s.Clock()+backoff)
		s.mu.Unlock()
	}
}

// Tick starts every due job that is not still running.
func (s *Scheduler) Tick(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Clock()
	for _, key := range s.order {
		j := s.jobs[key]
		eff := s.effective(j)
		if eff == nil || now < j.nextDue {
			continue
		}
		j.nextDue = now + *eff
		if j.running { // previous run still in flight: skip this slot, never overlap
			j.skipped++
			continue
		}
		j.running = true
		s.wg.Add(1)
		go func(j *Job, iv float64) { defer s.wg.Done(); s.run(ctx, j, iv) }(j, *eff)
	}
}

// Wait waits for in-flight runs.
func (s *Scheduler) Wait() { s.wg.Wait() }

// RunAllOnce runs every non-paused job now and waits (tests, first poll), phase by phase.
func (s *Scheduler) RunAllOnce(ctx context.Context) {
	s.mu.Lock()
	phases := map[int]bool{}
	for _, j := range s.jobs {
		phases[j.Phase] = true
	}
	s.mu.Unlock()
	ps := make([]int, 0, len(phases))
	for p := range phases {
		ps = append(ps, p)
	}
	sort.Ints(ps)
	for _, p := range ps {
		var wg sync.WaitGroup
		s.mu.Lock()
		for _, key := range s.order {
			j := s.jobs[key]
			eff := s.effective(j)
			if j.Phase != p || eff == nil || j.running {
				continue
			}
			j.running = true
			wg.Add(1)
			go func(j *Job, iv float64) { defer wg.Done(); s.run(ctx, j, iv) }(j, *eff)
		}
		s.mu.Unlock()
		wg.Wait()
	}
}

// LastMS is the last run duration in ms (nil before the first run).
func (s *Scheduler) LastMS(j *Job) *float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j.lastDuration == nil {
		return nil
	}
	return model.P(math.Round(*j.lastDuration*10000) / 10)
}
