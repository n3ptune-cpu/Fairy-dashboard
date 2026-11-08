package normalise

import (
	"sync"
)

// RateTracker turns cumulative byte counters into bytes/s between two successful polls.
//
// hold_s: Proxmox only refreshes its counters every ~10 s (pvestatd), so polling it at 2 s sees the same numbers
// several times in a row. With holdS > 0 an unchanged sample keeps the last rate (instead of flashing 0 then
// spiking) and the next real change is divided by the full time since the last change.
type RateTracker struct {
	mu   sync.Mutex
	prev map[string]rateSample
	last map[string][2]*float64
}

type rateSample struct {
	ts     float64
	rx, tx *int64
}

func NewRateTracker() *RateTracker {
	return &RateTracker{prev: map[string]rateSample{}, last: map[string][2]*float64{}}
}

func eqp(a, b *int64) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

func (r *RateTracker) Update(key string, ts float64, rx, tx *int64, holdS float64) (*float64, *float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev, ok := r.prev[key]
	if ok && holdS > 0 && eqp(rx, prev.rx) && eqp(tx, prev.tx) && ts-prev.ts < holdS {
		l := r.last[key]
		return l[0], l[1]
	}
	r.prev[key] = rateSample{ts, rx, tx}
	if !ok || ts <= prev.ts {
		return nil, nil
	}
	dt := ts - prev.ts
	rate := func(cur, old *int64) *float64 {
		if cur == nil || old == nil || *cur < *old {
			return nil
		}
		v := Round(float64(*cur-*old)/dt, 1)
		return &v
	}
	out := [2]*float64{rate(rx, prev.rx), rate(tx, prev.tx)}
	r.last[key] = out
	return out[0], out[1]
}
