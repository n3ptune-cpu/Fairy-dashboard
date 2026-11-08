package power

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	powersrc "github.com/n3ptune-cpu/Fairy-agent/pkg/power"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// Runner polls one configured source with exponential backoff while it fails.
type Runner struct {
	Source       powersrc.Source
	Interval     float64
	offlineAfter float64

	mu          sync.Mutex
	readings    []model.PowerReading
	lastSuccess *time.Time
	err         *string
	failures    int
	nextAttempt float64
	clock       func() float64
}

// ID is the source's id.
func (r *Runner) ID() string { return r.Source.Info().ID }

// Poll reads the source once (respecting its backoff).
func (r *Runner) Poll(ctx context.Context, timeout time.Duration) {
	mono := r.clock()
	r.mu.Lock()
	if mono < r.nextAttempt {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type res struct {
		rd  []model.PowerReading
		err error
	}
	ch := make(chan res, 1)
	go func() {
		rd, err := r.Source.Read(cctx, timeout)
		ch <- res{rd, err}
	}()
	var out res
	select {
	case out = <-ch:
	case <-cctx.Done():
		out = res{nil, devices.Errf("timeout")}
	}
	b := r.Source.Info()
	r.mu.Lock()
	defer r.mu.Unlock()
	if out.err != nil { // never let a source crash the poller
		r.failures++
		msg := devices.Describe(out.err)
		r.err = &msg
		r.nextAttempt = mono + math.Min(r.Source.MaxBackoff(), r.Interval*math.Pow(2, float64(r.failures-1)))
		if r.failures == 1 {
			slog.Warn("power source failed", "id", b.ID, "type", b.Type, "error", msg)
		}
		return
	}
	if r.failures > 0 {
		slog.Info("power source recovered", "id", b.ID)
	}
	now := time.Now().UTC()
	r.readings, r.lastSuccess, r.err, r.failures, r.nextAttempt = out.rd, &now, nil, 0, 0
}

// State is the source's current state: ok, stale (failing, but last reading still fresh) or unreachable.
func (r *Runner) State() model.PowerSourceState {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.Source.Info()
	status := "unreachable"
	if r.failures == 0 && r.lastSuccess != nil {
		status = "ok"
	} else if r.lastSuccess != nil && time.Since(*r.lastSuccess).Seconds() < r.offlineAfter {
		status = "stale"
	}
	st := model.PowerSourceState{ID: b.ID, Type: b.Type, Name: b.DisplayName(), Status: status,
		IncludeInTotal: b.IncludeInTotal == nil || *b.IncludeInTotal, Readings: []model.PowerReading{},
		LastSuccess: r.lastSuccess, Error: r.err}
	if status != "unreachable" && r.readings != nil {
		st.Readings = r.readings
	}
	if wait := r.nextAttempt - r.clock(); wait > 0 {
		st.NextAttemptInS = model.P(math.Round(wait*10) / 10)
	}
	return st
}

// SystemOf returns the linked system of a local source (for scheduler job membership).
func (r *Runner) SystemOf() string { return r.Source.Info().System }
