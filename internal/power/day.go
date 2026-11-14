package power

import (
	"log/slog"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/normalise"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
)

// The last 24 hours of every power source, one bucket a minute ("Last 24 hours" on the Power page and in a
// host's Power box; GET /api/power/day).
//
// The history the panel had (Manager.history) is the last hour in 5-second samples of watts, in memory. This
// is a second, coarser one next to it: per source a ring of DaySlots buckets, a bucket for each minute of the
// last day, with the lowest, the average and the highest watts of that minute's samples, the last volts and
// amperes, and the last value of the device's kWh counter.
//
// A gap is the absence of a bucket. A sample is taken only from a source that is "ok" with a reading that is
// not old (Manager.recordDay); a plug that does not answer, an agent that is off for five hours and the time
// the panel itself was not running leave minutes without a bucket, and nothing here ever fills them in: not
// with zero, not with the value before. The page breaks its line there, and the day's numbers are over the
// minutes that have readings.
//
// Memory is bounded by design: a bucket is 40 bytes, a source 1440 of them (57.6 kB, allocated with its first
// sample), and there are at most MaxDayDevices sources (3.7 MB). See room for what happens with one more.
//
// The clock is an argument of every method. A source's buckets carry their own minute, so a slot that was
// written a day ago is never taken for this minute, and a clock that jumps is handled in one place (add).
//
// It is kept over a restart in power-day.json (dayfile.go). Nothing but the graph reads it.
const (
	// DayStepS is the length of a bucket in seconds.
	DayStepS = 60
	// DaySlots is the number of buckets of a source: 24 hours.
	DaySlots = 24 * 3600 / DayStepS
	// MaxDayDevices is the most sources that have a day history.
	MaxDayDevices = 64
	// dayMaxAge is how old a reading of an "ok" source may be to count as a sample. A source that is polled
	// has a reading of the last seconds (the longest polling interval that can be set is 300 s). One whose
	// polling is paused stays "ok" with its last reading for as long as the pause lasts: its line ends here.
	dayMaxAge = 10 * time.Minute
	// maxDayID is the longest source id that is kept (an agent's id, "/", a device's id: far below it).
	maxDayID = 200
)

// dayBucket is one minute of one source. minute is the Unix minute it is for (0: the slot is empty); n the
// number of samples in it (0: none). volts, amps and kwh are NaN when no sample of the minute had them.
type dayBucket struct {
	kwh                        float64
	minute                     uint32
	min, avg, max, volts, amps float32
	n                          uint16
}

var nan32 = float32(math.NaN())

// dayDevice is the ring of one source: the bucket of minute m is b[m%DaySlots], and it is that minute's only
// when its own minute says so.
type dayDevice struct {
	b   []dayBucket
	cur uint32  // the minute of the newest bucket (0: none yet)
	sum float64 // the sum of the watts of bucket cur's samples (its average is sum / n)
}

// DaySample is one sample of one source: its watts, and what else its reading had.
type DaySample struct {
	ID        string
	W         float64
	V, A, KWh *float64
}

// DayStore holds the day history of every source, and writes it to its file (dayfile.go).
type DayStore struct {
	mu         sync.Mutex
	dev        map[string]*dayDevice
	fullWarned bool
	dirty      bool // something changed since the last write was started

	path      string
	writeFile func(path string, data []byte, perm os.FileMode) error
	saving    chan struct{} // not nil while a write is on its way; closed when it has ended
	savedAt   time.Time     // when the last write was started (or the file was read)
	saveErr   string        // the last write's error, as it was logged
}

// NewDayStore returns an empty store that keeps nothing on disk until Load names its file.
func NewDayStore() *DayStore {
	return &DayStore{dev: map[string]*dayDevice{}, writeFile: statefile.WriteAtomic}
}

// dayMinute is t as a Unix minute. ok is false for a time before 1970 or one that does not fit (the year
// 10136): nothing is recorded or shown for such a clock.
func dayMinute(t time.Time) (uint32, bool) {
	m := t.Unix() / DayStepS
	if t.Unix() < DayStepS || m > math.MaxUint32 {
		return 0, false
	}
	return uint32(m), true
}

// validDayID reports whether id can be a source's id: not empty, not long, no control characters.
func validDayID(id string) bool {
	if id == "" || len(id) > maxDayID {
		return false
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f || r == '�' {
			return false
		}
	}
	return true
}

// Record adds one sample for each source in samples, for the minute of now. present is every source the
// panel lists right now, also the ones without a sample (failing ones): a source that is listed never loses
// its place to a new one (room). nil: not known.
func (d *DayStore) Record(now time.Time, samples []DaySample, present map[string]bool) {
	m, ok := dayMinute(now)
	if !ok || len(samples) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range samples {
		if !validDayID(s.ID) || normalise.Reading(&s.W) == nil {
			continue
		}
		dv := d.dev[s.ID]
		if dv == nil {
			if !d.room(m, present) {
				if !d.fullWarned {
					d.fullWarned = true
					slog.Warn("power: the last 24 hours are kept for at most this many sources, and every one of them is in use: no day history for the others",
						"limit", MaxDayDevices, "first_without", s.ID)
				}
				continue
			}
			dv = &dayDevice{b: make([]dayBucket, DaySlots)}
			d.dev[s.ID] = dv
		}
		dv.add(m, s)
		d.dirty = true
	}
}

// room makes sure one more source fits and reports whether it does. With MaxDayDevices sources there: every
// source whose newest bucket is older than a day goes; if that frees nothing, the one source that the panel
// does not list any more (it was taken out of an agent's config, or its id was changed) and whose newest
// bucket is the oldest. A source that is listed is never taken out for another one. Caller holds d.mu.
func (d *DayStore) room(m uint32, present map[string]bool) bool {
	if len(d.dev) < MaxDayDevices {
		return true
	}
	for id, dv := range d.dev {
		if dv.expired(m) {
			delete(d.dev, id)
		}
	}
	if len(d.dev) < MaxDayDevices {
		d.fullWarned = false
		return true
	}
	if present == nil {
		return false
	}
	gone, at := "", uint32(math.MaxUint32)
	for id, dv := range d.dev {
		if !present[id] && (dv.cur < at || (dv.cur == at && id < gone)) {
			gone, at = id, dv.cur
		}
	}
	if gone == "" {
		return false
	}
	delete(d.dev, gone)
	d.fullWarned = false
	return true
}

// expired reports whether the source has no bucket in the 24 hours that end with minute m.
func (dv *dayDevice) expired(m uint32) bool { return uint64(dv.cur)+DaySlots <= uint64(m) }

// add puts one sample into the bucket of minute m.
//
// m is usually the newest minute or the one after it. Later than that (the panel was busy, the clock was set
// forwards): the minutes between stay without a bucket. Earlier than the newest bucket: the clock was set
// back. Every bucket after m is emptied then. They were measured, but under times that have not come yet by
// the clock the panel has now, and each would otherwise come back into the graph when the clock reaches its
// minute a second time, as a reading of then. Bucket m itself, when it is there, goes on: its average
// continues from its number of samples (that is also what happens to the open minute after a restart).
func (dv *dayDevice) add(m uint32, s DaySample) {
	if m < dv.cur {
		for i := range dv.b {
			if dv.b[i].minute > m {
				dv.b[i] = dayBucket{}
			}
		}
	}
	b := &dv.b[m%DaySlots]
	if m != dv.cur || b.minute != m {
		if b.minute == m && b.n > 0 {
			dv.sum = float64(b.avg) * float64(b.n)
		} else {
			*b = dayBucket{minute: m, volts: nan32, amps: nan32, kwh: math.NaN()}
			dv.sum = 0
		}
		dv.cur = m
	}
	w := float32(s.W)
	if b.n == 0 || w < b.min {
		b.min = w
	}
	if b.n == 0 || w > b.max {
		b.max = w
	}
	if b.n < math.MaxUint16 { // 65535 samples in a minute: the average stays what it is from there on
		b.n++
		dv.sum += s.W
		b.avg = float32(dv.sum / float64(b.n))
	}
	if v := normalise.Reading(s.V); v != nil {
		b.volts = float32(*v)
	}
	if a := normalise.Reading(s.A); a != nil {
		b.amps = float32(*a)
	}
	if k := normalise.Finite(s.KWh, -normalise.MaxEnergyKWh, normalise.MaxEnergyKWh); k != nil {
		b.kwh = *k
	}
}

// window is the source's buckets of the 24 hours that end with minute m, oldest first, from the first minute
// that has one to the last that has one: start is the minute of element 0, and an element with n == 0 is a
// minute without a bucket. nil when there is none at all. Buckets of minutes after m are not in it.
func (dv *dayDevice) window(m uint32) (start uint32, out []dayBucket) {
	first := uint32(1)
	if m >= DaySlots {
		first = m - DaySlots + 1
	}
	lo, hi := uint32(0), uint32(0)
	for x := first; ; x++ {
		if b := &dv.b[x%DaySlots]; b.minute == x && b.n > 0 {
			if lo == 0 {
				lo = x
			}
			hi = x
		}
		if x == m {
			break
		}
	}
	if lo == 0 {
		return 0, nil
	}
	out = make([]dayBucket, hi-lo+1)
	for x := lo; x <= hi; x++ {
		if b := dv.b[x%DaySlots]; b.minute == x && b.n > 0 {
			out[x-lo] = b
		}
	}
	return lo, out
}

// Has reports whether the source has a bucket in the 24 hours that end now.
func (d *DayStore) Has(id string, now time.Time) bool {
	m, ok := dayMinute(now)
	d.mu.Lock()
	defer d.mu.Unlock()
	dv := d.dev[id]
	return ok && dv != nil && !dv.expired(m)
}

// Len is the number of sources that have a day history.
func (d *DayStore) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.dev)
}

// Forget drops the day of every source whose id starts with prefix (a host was removed: its devices'
// "<agent id>/"), and has the file written with the next history tick.
func (d *DayStore) Forget(prefix string) {
	if prefix == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for id := range d.dev {
		if strings.HasPrefix(id, prefix) {
			delete(d.dev, id)
			d.dirty = true
			d.savedAt = time.Time{}
		}
	}
}

// Series is GET /api/power/day for one source: its last 24 hours as lists, and the day in numbers.
func (d *DayStore) Series(id string, now time.Time) model.PowerDay {
	out := model.PowerDay{Source: id, StepS: DayStepS, Now: now.Unix(),
		Min: model.DayValues{}, Avg: model.DayValues{}, Max: model.DayValues{}}
	m, ok := dayMinute(now)
	if !ok {
		return out
	}
	d.mu.Lock()
	var start uint32
	var bs []dayBucket
	if dv := d.dev[id]; dv != nil {
		start, bs = dv.window(m)
	}
	d.mu.Unlock()
	if len(bs) == 0 {
		return out
	}
	out.Start = int64(start) * DayStepS
	out.Min, out.Avg, out.Max = make(model.DayValues, len(bs)), make(model.DayValues, len(bs)), make(model.DayValues, len(bs))
	volts, amps := make(model.DayValues, len(bs)), make(model.DayAmps, len(bs))
	hasV, hasA := false, false
	for i, b := range bs {
		if b.n == 0 {
			out.Min[i], out.Avg[i], out.Max[i], volts[i], amps[i] = nan32, nan32, nan32, nan32, nan32
			continue
		}
		out.Min[i], out.Avg[i], out.Max[i], volts[i], amps[i] = b.min, b.avg, b.max, b.volts, b.amps
		hasV = hasV || !math.IsNaN(float64(b.volts))
		hasA = hasA || !math.IsNaN(float64(b.amps))
	}
	if hasV {
		out.Volts = volts
	}
	if hasA {
		out.Amps = amps
	}
	out.Summary = daySummary(start, bs)
	return out
}

// daySummary is the day in numbers, over the minutes that have a bucket. nil when none has.
func daySummary(start uint32, bs []dayBucket) *model.PowerDaySummary {
	var s model.PowerDaySummary
	n, sum := 0, 0.0
	var vLo, vHi, aLo, aHi *float64
	span := func(lo, hi **float64, v float32) {
		x := float64(v)
		if math.IsNaN(x) {
			return
		}
		if *lo == nil || x < **lo {
			*lo = model.P(x)
		}
		if *hi == nil || x > **hi {
			*hi = model.P(x)
		}
	}
	for i, b := range bs {
		if b.n == 0 {
			continue
		}
		at := (int64(start) + int64(i)) * DayStepS
		if n == 0 {
			s.From = at
		}
		s.To = at
		if n == 0 || float64(b.min) < s.MinW {
			s.MinW, s.MinAt = float64(b.min), at
		}
		if n == 0 || float64(b.max) > s.MaxW {
			s.MaxW, s.MaxAt = float64(b.max), at
		}
		sum += float64(b.avg)
		n++
		span(&vLo, &vHi, b.volts)
		span(&aLo, &aHi, b.amps)
	}
	if n == 0 {
		return nil
	}
	total, est := dayEnergy(bs, s.MaxW)
	s.CoveredS = n * DayStepS
	s.AvgW = normalise.Round(sum/float64(n), 2)
	s.MinW, s.MaxW = normalise.Round(s.MinW, 2), normalise.Round(s.MaxW, 2)
	s.EnergyKWh, s.EnergyEstKWh = normalise.Round(total, 3), normalise.Round(est, 3)
	switch {
	case total-est <= 1e-9:
		s.Energy, s.EnergyEstKWh = "estimated", s.EnergyKWh
	case est <= 1e-9:
		s.Energy, s.EnergyEstKWh = "counter", 0
	default:
		s.Energy = "mixed"
	}
	round := func(p *float64, n int) *float64 {
		if p == nil {
			return nil
		}
		return model.P(normalise.Round(*p, n))
	}
	s.VoltsMin, s.VoltsMax, s.AmpsMin, s.AmpsMax = round(vLo, 1), round(vHi, 1), round(aLo, 3), round(aHi, 3)
	return &s
}

const (
	// energyFloorW: a counter increase that would need more than this many watts for the whole time between
	// two values (or ten times the day's highest watts, if that is more) is not an increase a device behind
	// a plug can have had: the counter jumped.
	energyFloorW = 4000.0
	// energySlackKWh is how far a counter may lag behind half of the watts of the same time and still count:
	// counters have coarse steps (0.001 kWh and worse).
	energySlackKWh = 0.1
)

// dayEnergy is the energy of the day's buckets in kWh, and the part of it that is estimated from the watts
// (average watts of a minute x one minute) and not read from the device's counter.
//
// A plug's counter is not a clean number that only grows. It restarts at 0 after an outage, it can stay at 0
// for hours, and it moves in coarse steps. So nothing is ever taken as "last value minus first value":
//
//   - The minutes that have a counter value form runs: values that follow each other and do not go down. A
//     value that is lower than the one before ends the run and starts the next (the counter restarted: never
//     negative energy, and the new value itself is not counted as an increase). So does an increase no device
//     can have had in that time (maxW is the day's highest watts; see energyFloorW).
//   - A run counts with its increase when the counter moved and is not far below the watts of the same
//     minutes. A counter that did not move, or that shows far less than the watts (stuck at 0 and counting
//     again later), counts with those watts instead: estimated.
//   - Minutes with watts and no counter value count with their watts: before the first counter value, after
//     the last, between two runs, and all of them for a device without a counter.
//
// A minute without a bucket adds nothing to the watts part: it is not zero watts, it is not known. Inside a
// run the counter's increase does include what the device used while nobody could ask it.
func dayEnergy(bs []dayBucket, maxW float64) (total, est float64) {
	const stepH = DayStepS / 3600.0
	capKW := math.Max(energyFloorW, 10*maxW) / 1000
	var (
		inRun      bool
		runC, runW float64 // the run's increase of the counter, and the watts of its minutes, in kWh
		tail       float64 // the watts of the minutes since the run's last counter value
		prevK      float64
		prevI      int
	)
	closeRun := func() {
		if !inRun {
			return
		}
		if runC > 0 && runC >= 0.5*runW-energySlackKWh {
			total += runC
		} else {
			total, est = total+runW, est+runW
		}
		total, est = total+tail, est+tail
		inRun, runC, runW, tail = false, 0, 0, 0
	}
	for i, b := range bs {
		if b.n == 0 {
			continue
		}
		e := float64(b.avg) * stepH / 1000
		if math.IsNaN(b.kwh) {
			if inRun {
				tail += e
			} else {
				total, est = total+e, est+e
			}
			continue
		}
		if inRun {
			if d := b.kwh - prevK; d < 0 || d > float64(i-prevI+1)*stepH*capKW {
				closeRun()
			} else {
				runC, runW, tail = runC+d, runW+tail+e, 0
			}
		}
		if !inRun {
			inRun, runC, runW, tail = true, 0, e, 0
		}
		prevK, prevI = b.kwh, i
	}
	closeRun()
	return total, est
}
