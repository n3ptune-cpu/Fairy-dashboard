package power

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/normalise"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
)

// power-day.json, in the data folder: the last 24 hours of every power source (day.go), so that the graph is
// still there after a restart or an update of the panel.
//
//	{"version":1,"step_s":60,"saved_at":"2026-11-14T16:05:00Z",
//	 "devices":{"agent-a/plug-a":{"start":29912345,"open_n":7,
//	   "min":[..],"avg":[..],"max":[..],"volts":[..],"amps":[..],"kwh":[..]}}}
//
// Per source: start is the Unix minute of the lists' first element, the lists are parallel with one element a
// minute, null is a minute without readings, and open_n is the number of samples in the last element (the
// minute that was still open: its average goes on from there after the restart). volts, amps and kwh are left
// out for a source that never had them.
//
// Like last-seen.json it decides nothing and never stops anything:
//
//   - Reading (Load, once, when the panel starts). No file: empty. A file that is not what this panel writes
//     (not JSON, cut off, empty, another version or step, more sources than MaxDayDevices, larger than
//     maxDayFile) is not used, with one line in the log, and the next write replaces it: there is nothing in
//     it to rescue, the readings come again. A source whose lists do not fit together is skipped; a value that
//     is not a possible reading makes its minute a gap; minutes after the panel's clock and minutes older than
//     24 hours are dropped. A symbolic link, a folder or a pipe at that name is never read (statefile.ReadFile)
//     and never written over (put).
//   - Writing never happens in the poller's own time. The history tick asks SaveIfDue, which looks at the
//     clock and starts a goroutine; that goroutine copies the buckets (under the lock, a memory copy), encodes
//     and writes them (statefile.WriteAtomic: a temporary file, synced, renamed). At most one write is on its
//     way: while one hangs on a dead disk no other is started, and nothing waits for it. A write that fails is
//     tried again with the next one, and logged once for as long as it fails the same way (failure).
//   - When: every daySaveEvery while something changed, and when the panel stops and when a host is removed
//     (Flush, which waits for the write, with an end: neither is the poller's own time). A write that nobody
//     waits for is started only by the history tick, and only after daySaveEvery: a program that runs for
//     less than that (a test) never has one going when it ends.
const (
	// DayFile is the file's name in the data folder.
	DayFile = "power-day.json"
	// dayFileVersion is the version this panel writes and reads.
	dayFileVersion = 1
	// daySaveEvery is the time between two writes.
	daySaveEvery = 5 * time.Minute
	// maxDayFile is the most bytes the file may have to be read: MaxDayDevices full days are about 6 MB.
	maxDayFile = 16 << 20
)

// dayFileOut is the file as it is written.
type dayFileOut struct {
	Version int                         `json:"version"`
	StepS   int                         `json:"step_s"`
	SavedAt time.Time                   `json:"saved_at"`
	Devices map[string]dayFileDeviceOut `json:"devices"`
}

type dayFileDeviceOut struct {
	Start uint32          `json:"start"`
	OpenN int             `json:"open_n"`
	Min   model.DayValues `json:"min"`
	Avg   model.DayValues `json:"avg"`
	Max   model.DayValues `json:"max"`
	Volts model.DayValues `json:"volts,omitempty"`
	Amps  model.DayAmps   `json:"amps,omitempty"`
	KWh   dayCounter      `json:"kwh,omitempty"`
}

// dayCounter is a list of kWh counter values as the devices gave them; NaN (none in that minute) is null.
type dayCounter []float64

func (v dayCounter) MarshalJSON() ([]byte, error) {
	b := make([]byte, 0, 2+len(v)*9)
	b = append(b, '[')
	for i, x := range v {
		if i > 0 {
			b = append(b, ',')
		}
		if math.IsNaN(x) || math.IsInf(x, 0) {
			b = append(b, "null"...)
			continue
		}
		b = strconv.AppendFloat(b, x, 'f', -1, 64)
	}
	return append(b, ']'), nil
}

// dayFileIn is the file as it is read: every number may be missing.
type dayFileIn struct {
	Version int                        `json:"version"`
	StepS   int                        `json:"step_s"`
	Devices map[string]dayFileDeviceIn `json:"devices"`
}

type dayFileDeviceIn struct {
	Start int64      `json:"start"`
	OpenN int64      `json:"open_n"`
	Min   []*float64 `json:"min"`
	Avg   []*float64 `json:"avg"`
	Max   []*float64 `json:"max"`
	Volts []*float64 `json:"volts"`
	Amps  []*float64 `json:"amps"`
	KWh   []*float64 `json:"kwh"`
}

// Load names the store's file and reads it. It never fails: what cannot be used is not used, and the log says
// so once. From then on the store writes to that file. now is the panel's clock.
func (d *DayStore) Load(path string, now time.Time) {
	removeDayTemp(path)
	devs, err := readDayFile(path, now)
	if err != nil {
		slog.Warn("power: the last 24 hours are not known from before this start: the file is not used (the graphs start empty)",
			"file", path, "error", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.path, d.savedAt = path, now
	for id, dv := range devs {
		if _, ok := d.dev[id]; !ok && len(d.dev) < MaxDayDevices {
			d.dev[id] = dv
		}
	}
}

// removeDayTemp removes what a write of the file left behind when the panel was killed in the middle of it:
// the temporary files of statefile.WriteAtomic for this file (.power-day.json.<random>.tmp), each as large as
// the file. Only the panel writes this file, and at its start it is not writing: every such file is a
// leftover. Only plain files are removed (as internal/notifications does for its list).
func removeDayTemp(path string) {
	dir, prefix := filepath.Dir(path), "."+filepath.Base(path)+"."
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if n := e.Name(); strings.HasPrefix(n, prefix) && strings.HasSuffix(n, ".tmp") && e.Type().IsRegular() {
			if err := os.Remove(filepath.Join(dir, n)); err == nil {
				slog.Info("power: removed the temporary file of a write of the last 24 hours that an earlier stop cut off", "path", filepath.Join(dir, n))
			}
		}
	}
}

// readDayFile reads the file. No file is no error and no source.
func readDayFile(path string, now time.Time) (map[string]*dayDevice, error) {
	m, ok := dayMinute(now)
	if !ok {
		return nil, errors.New("the panel's clock is not a time the history can be kept for")
	}
	raw, err := statefile.ReadFile(path, maxDayFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var f dayFileIn
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("it is not what the panel writes (%d bytes: %v)", len(raw), err)
	}
	if f.Version != dayFileVersion || f.StepS != DayStepS {
		return nil, fmt.Errorf("it is of another version (version %d, step %d s; this panel reads version %d with %d s)", f.Version, f.StepS, dayFileVersion, DayStepS)
	}
	if len(f.Devices) > MaxDayDevices {
		return nil, fmt.Errorf("it holds %d sources, more than the %d this panel ever writes", len(f.Devices), MaxDayDevices)
	}
	out := map[string]*dayDevice{}
	for id, in := range f.Devices {
		if dv := in.device(id, m); dv != nil {
			out[id] = dv
		}
	}
	return out, nil
}

// device is one source of the file as a ring, or nil when nothing of it can be used: a bad id, lists that do
// not fit together or are longer than a day, no minute of the 24 hours that end with minute now.
func (in dayFileDeviceIn) device(id string, now uint32) *dayDevice {
	n := len(in.Avg)
	fits := func(l []*float64) bool { return len(l) == 0 || len(l) == n }
	if !validDayID(id) || n == 0 || n > DaySlots || len(in.Min) != n || len(in.Max) != n ||
		!fits(in.Volts) || !fits(in.Amps) || !fits(in.KWh) ||
		in.Start < 1 || in.Start+int64(n) > math.MaxUint32 {
		return nil
	}
	at := func(l []*float64, i int, lo, hi float64) float64 {
		if len(l) == 0 {
			return math.NaN()
		}
		if v := normalise.Finite(l[i], lo, hi); v != nil {
			return *v
		}
		return math.NaN()
	}
	const r = normalise.MaxReading
	dv := &dayDevice{b: make([]dayBucket, DaySlots)}
	for i := range n {
		minute := uint32(in.Start) + uint32(i)
		if minute > now || uint64(minute)+DaySlots <= uint64(now) {
			continue // after the panel's clock, or older than a day
		}
		lo, avg, hi := at(in.Min, i, -r, r), at(in.Avg, i, -r, r), at(in.Max, i, -r, r)
		if math.IsNaN(lo) || math.IsNaN(avg) || math.IsNaN(hi) || lo > hi {
			continue // a minute without readings, or one whose numbers are not a minute's readings
		}
		dv.b[minute%DaySlots] = dayBucket{minute: minute, n: 1, min: float32(lo), avg: float32(avg), max: float32(hi),
			volts: float32(at(in.Volts, i, -r, r)), amps: float32(at(in.Amps, i, -r, r)),
			kwh: at(in.KWh, i, -normalise.MaxEnergyKWh, normalise.MaxEnergyKWh)}
		dv.cur = minute // the lists are in the order of time
	}
	if dv.cur == 0 {
		return nil
	}
	b := &dv.b[dv.cur%DaySlots]
	if dv.cur == uint32(in.Start)+uint32(n-1) && in.OpenN > 1 { // the file's last minute: it may still be open
		b.n = uint16(min(in.OpenN, math.MaxUint16))
	}
	dv.sum = float64(b.avg) * float64(b.n)
	return dv
}

// snapshot is the file's content for the 24 hours that end now. Sources without a minute in them are taken
// out of the store with it (they would be skipped at the next start anyway).
func (d *DayStore) snapshot(now time.Time) dayFileOut {
	out := dayFileOut{Version: dayFileVersion, StepS: DayStepS, SavedAt: now.UTC().Truncate(time.Second), Devices: map[string]dayFileDeviceOut{}}
	m, ok := dayMinute(now)
	if !ok {
		return out
	}
	type win struct {
		start uint32
		bs    []dayBucket
	}
	wins := map[string]win{}
	d.mu.Lock()
	for id, dv := range d.dev {
		start, bs := dv.window(m)
		if dv.expired(m) {
			delete(d.dev, id)
		}
		if len(bs) > 0 {
			wins[id] = win{start, bs}
		}
	}
	d.mu.Unlock()
	for id, w := range wins { // encoded without the lock
		n := len(w.bs)
		o := dayFileDeviceOut{Start: w.start, OpenN: int(w.bs[n-1].n), Min: make(model.DayValues, n), Avg: make(model.DayValues, n), Max: make(model.DayValues, n)}
		volts, amps, kwh := make(model.DayValues, n), make(model.DayAmps, n), make(dayCounter, n)
		hasV, hasA, hasK := false, false, false
		for i, b := range w.bs {
			if b.n == 0 {
				o.Min[i], o.Avg[i], o.Max[i], volts[i], amps[i], kwh[i] = nan32, nan32, nan32, nan32, nan32, math.NaN()
				continue
			}
			o.Min[i], o.Avg[i], o.Max[i], volts[i], amps[i], kwh[i] = b.min, b.avg, b.max, b.volts, b.amps, b.kwh
			hasV = hasV || !math.IsNaN(float64(b.volts))
			hasA = hasA || !math.IsNaN(float64(b.amps))
			hasK = hasK || !math.IsNaN(b.kwh)
		}
		if hasV {
			o.Volts = volts
		}
		if hasA {
			o.Amps = amps
		}
		if hasK {
			o.KWh = kwh
		}
		out.Devices[id] = o
	}
	return out
}

// SaveIfDue starts a write when one is due: the store has a file, something changed, no write is on its way,
// and the last one was started daySaveEvery ago (or the clock went back since). It never waits: the write
// runs in its own goroutine. The history tick calls it.
func (d *DayStore) SaveIfDue(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.path == "" || !d.dirty || d.saving != nil {
		return
	}
	if !d.savedAt.IsZero() && !now.Before(d.savedAt) && now.Sub(d.savedAt) < daySaveEvery {
		return
	}
	d.start(now)
}

// start begins a write and returns the channel that is closed when it has ended. Caller holds d.mu, and no
// write is on its way.
func (d *DayStore) start(now time.Time) chan struct{} {
	done := make(chan struct{})
	d.saving, d.savedAt, d.dirty = done, now, false
	path, write := d.path, d.writeFile
	go func() {
		err := putDayFile(path, write, d.snapshot(now))
		d.mu.Lock()
		if err != nil {
			d.dirty = true // what this write held is still not in the file
			if msg := failure(err); msg != d.saveErr {
				d.saveErr = msg
				slog.Warn("power: could not keep the last 24 hours for the next start (it is tried again; the graphs go on from memory)",
					"file", path, "error", err)
			}
		} else {
			d.saveErr = ""
		}
		d.saving = nil
		d.mu.Unlock()
		close(done)
	}()
	return done
}

// failure is what went wrong in err without the name of the file it went wrong with: a write's error names
// its temporary file, and that name is new with every write. Two writes that fail the same way (a full disk, a
// folder that cannot be written) have the same failure, and the log says it once.
func failure(err error) string {
	var pe *fs.PathError
	var le *os.LinkError
	switch {
	case errors.As(err, &le):
		return le.Op + ": " + le.Err.Error()
	case errors.As(err, &pe):
		return pe.Op + ": " + pe.Err.Error()
	}
	return err.Error()
}

// putDayFile encodes the file and puts it in place. Whatever is at that name and is not a plain file stays.
func putDayFile(path string, write func(string, []byte, os.FileMode) error, f dayFileOut) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if st, err := os.Lstat(path); err == nil && !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a plain file: it is left alone, and nothing is kept until it is taken away", path)
	}
	return write(path, data, 0o600)
}

// Flush writes what is not in the file yet and waits for that write, at most wait in all: the panel is
// stopping, and a disk that does not answer must not keep it from stopping. It reports whether everything is
// written.
func (d *DayStore) Flush(now time.Time, wait time.Duration) bool {
	t := time.NewTimer(wait)
	defer t.Stop()
	for range 2 { // a write that is on its way holds an older copy: wait for it, then write the newest
		d.mu.Lock()
		done := d.saving
		if done == nil {
			if d.path == "" || !d.dirty {
				d.mu.Unlock()
				return true
			}
			done = d.start(now)
		}
		d.mu.Unlock()
		select {
		case <-done:
		case <-t.C:
			return false
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.saving == nil && !d.dirty
}
