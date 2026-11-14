package adminstate

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"path/filepath"
	"slices"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
)

// "Run timings": the timings of "Update unpowered hosts" that an admin changed in the panel (the Updates
// page). Like "Edit host" (hostoptions.go) it is an OVERLAY: config.yaml stays the base and is never
// written. It is kept in state.json under
//
//	"run_timings": {"rev": 4,
//	    "values": {"off_after_s": 360, "shutdown_delay_s": 30},
//	    "by":     {"off_after_s": {"by": "<who>", "at": "<RFC 3339>"}, "shutdown_delay_s": {...}}}
//
// Each key of "values" is ONE decision about one timing (config.RunTimings has the six keys and their
// ranges), used instead of config.yaml's value of that key until it is taken out ("Reset"). A key that is
// not there follows config.yaml, also when config.yaml is edited later. Nothing else can be in the overlay:
// a key with another name is not read.
//
// The reader never fails, by hostoptions.go's rules. A value that is no whole number in its key's range (a
// hand edit: 0, 100, "360", 360.5) is left out and config.yaml's value counts: a number below a floor can
// not come into force through this file. A key this build does not know (in "values" or at the top) is not
// used and is kept: a newer build wrote it, and a save by this one writes it back as it was. Each thing that
// is left out is one WARN line, once.
//
// A panel from before this key leaves it alone (state.json is read and written back as one object): after
// going back to it config.yaml's timings count, and the decisions count again after the next update.

// runTimingsKey is the name of the object in state.json.
const runTimingsKey = "run_timings"

// TimingMark is who changed a timing last, and when.
type TimingMark struct {
	By string
	At time.Time
}

// RunTimings is the overlay. Rev counts the writes (it only ever grows).
type RunTimings struct {
	Rev    int
	Values map[string]int        // key -> the value that counts instead of config.yaml's
	Marks  map[string]TimingMark // key -> who decided it
	// Ignored is how many things the reader left out of what the file holds (0: none).
	Ignored int
	// unknown is what the file holds under keys this build does not know, at the top of "run_timings" and in
	// "values" and "by". Never used, never looked into; written back as it was.
	unknown struct{ top, values, by map[string]any }
}

// Value is the decision about key (ok false: none, config.yaml's value counts).
func (o RunTimings) Value(key string) (v int, ok bool) {
	v, ok = o.Values[key]
	return v, ok
}

// Set decides that key is v, whatever config.yaml says. The caller has checked v (config.RunTimingOf).
func (o *RunTimings) Set(key string, v int) {
	if o.Values == nil {
		o.Values = map[string]int{}
	}
	o.Values[key] = v
}

// Drop takes the decision about key out (config.yaml counts for it again). It reports whether there was one.
func (o *RunTimings) Drop(key string) bool {
	_, had := o.Values[key]
	delete(o.Values, key)
	delete(o.Marks, key)
	return had
}

// Clone returns a copy that shares nothing with o (the unknown keys, which nothing changes, are the same).
func (o RunTimings) Clone() RunTimings {
	out := o
	out.Values, out.Marks = maps.Clone(o.Values), maps.Clone(o.Marks)
	if out.Values == nil {
		out.Values = map[string]int{}
	}
	if out.Marks == nil {
		out.Marks = map[string]TimingMark{}
	}
	return out
}

// runTimings reads the overlay out of a state.json object. It never fails: ignored is one line for each
// thing that was left out (config.yaml's value counts for each of them).
func runTimings(d statefile.Data) (o RunTimings, ignored []string) {
	o = RunTimings{Values: map[string]int{}, Marks: map[string]TimingMark{}}
	raw, present := d[runTimingsKey]
	if !present {
		return o, nil
	}
	say := func(format string, a ...any) {
		if o.Ignored++; len(ignored) < MaxIgnoredLines {
			ignored = append(ignored, fmt.Sprintf(format, a...))
		}
	}
	top, ok := raw.(map[string]any)
	if !ok {
		say("%q is not the object Fairy writes (found %s): none of it is used", runTimingsKey, kindOf(raw))
		return o, ignored
	}
	if v, has := top["rev"]; has {
		if f, isNum := v.(float64); isNum && f >= 0 && f <= 1<<52 && f == math.Trunc(f) {
			o.Rev = int(f)
		} else {
			say("%q: \"rev\" is not a whole number (found %s): it counts as 0", runTimingsKey, kindOf(v))
		}
	}
	for _, k := range slices.Sorted(maps.Keys(top)) {
		if k != "rev" && k != "values" && k != "by" {
			say("%q: the key %q is not one Fairy writes: it is not used (and left in the file as it is)", runTimingsKey, clipKey(k))
			if o.unknown.top == nil {
				o.unknown.top = map[string]any{}
			}
			o.unknown.top[k] = top[k]
		}
	}
	marks, _ := top["by"].(map[string]any)
	if rawValues, has := top["values"]; has {
		values, ok := rawValues.(map[string]any)
		if !ok {
			say("%q: \"values\" is not an object (found %s): none of it is used", runTimingsKey, kindOf(rawValues))
			return o, ignored
		}
		if len(values) > 64 { // six keys exist; a long list is not something Fairy wrote
			say("%q has %d values, far more than there are timings: none of it is used", runTimingsKey, len(values))
			return o, ignored
		}
		for _, k := range slices.Sorted(maps.Keys(values)) {
			t, known := config.RunTimingOf(k)
			if !known {
				say("%q: %q is no timing this version knows: it is not used (and left in the file as it is)", runTimingsKey, clipKey(k))
				if o.unknown.values == nil {
					o.unknown.values = map[string]any{}
				}
				o.unknown.values[k] = values[k]
				if m, has := marks[k]; has {
					if o.unknown.by == nil {
						o.unknown.by = map[string]any{}
					}
					o.unknown.by[k] = m
				}
				continue
			}
			f, isNum := values[k].(float64)
			if !isNum || f != math.Trunc(f) || f < float64(t.Min) || f > float64(t.Max) {
				say("%q: %s is not a whole number in %d-%d (found %s): config.yaml's value counts", runTimingsKey, k, t.Min, t.Max, shownNumber(values[k]))
				continue
			}
			o.Values[k] = int(f)
			if m, _ := marks[k].(map[string]any); m != nil {
				var mark TimingMark
				by, _ := m["by"].(string)
				at, _ := m["at"].(string)
				mark.By = clipKey(by)
				mark.At, _ = time.Parse(time.RFC3339, at)
				o.Marks[k] = mark
			}
		}
	}
	return o, ignored
}

// shownNumber names a value of the file in a log line: a number as it is (bounded), anything else by its kind.
func shownNumber(v any) string {
	if f, ok := v.(float64); ok && !math.IsInf(f, 0) && !math.IsNaN(f) && math.Abs(f) < 1e12 {
		return fmt.Sprintf("%v", f)
	}
	return kindOf(v)
}

// encode is the overlay as it is written to state.json: what this build knows, and next to it, untouched,
// what it read under keys it does not know.
func (o RunTimings) encode() map[string]any {
	values, by := map[string]any{}, map[string]any{}
	for k, v := range o.unknown.values {
		values[k] = v
	}
	for k, v := range o.unknown.by {
		by[k] = v
	}
	for k, v := range o.Values {
		values[k] = v
		if m, ok := o.Marks[k]; ok && (m.By != "" || !m.At.IsZero()) {
			e := map[string]any{}
			if m.By != "" {
				e["by"] = m.By
			}
			if !m.At.IsZero() {
				e["at"] = m.At.UTC().Format(time.RFC3339)
			}
			by[k] = e
		}
	}
	out := map[string]any{}
	for k, v := range o.unknown.top {
		out[k] = v
	}
	out["rev"], out["values"], out["by"] = o.Rev, values, by
	return out
}

// RunTimings returns the overlay as the panel uses it right now (none while state.json is unreadable:
// config.yaml's timings count, and no run starts then anyway: ControlDisabled). What the file holds that is
// not used is said once, as one WARN line each.
func (a *State) RunTimings() RunTimings {
	o, _ := a.RunTimingsRead()
	return o
}

// RunTimingsRead is RunTimings with the lines about what was left out (for the dialog).
func (a *State) RunTimingsRead() (RunTimings, []string) {
	if a.state.Err() != nil {
		return RunTimings{Values: map[string]int{}, Marks: map[string]TimingMark{}}, nil
	}
	o, ignored := runTimings(a.state.Get())
	if len(ignored) > 0 {
		a.runTimingsWarn(ignored)
	}
	return o, ignored
}

// runTimingsWarn logs each ignored line once (hostOptionsWarn's rule: again only after a restart).
func (a *State) runTimingsWarn(ignored []string) {
	a.shapeMu.Lock()
	defer a.shapeMu.Unlock()
	if v, logged := a.shapeLog[runTimingsKey]; logged && v == a.state.Version {
		return
	}
	if a.shapeLog == nil {
		a.shapeLog = map[string]int{}
	}
	a.shapeLog[runTimingsKey] = a.state.Version
	if len(a.runTimingsSaid) > 256 {
		a.runTimingsSaid = nil
	}
	for _, line := range ignored {
		if a.runTimingsSaid[line] {
			continue
		}
		if a.runTimingsSaid == nil {
			a.runTimingsSaid = map[string]bool{}
		}
		a.runTimingsSaid[line] = true
		slog.Warn("state.json: a run timing changed in the panel is ignored: "+line, "path", a.StatePath, "key", runTimingsKey)
	}
}

// ReadRunTimings reads the overlay of the panel whose data folder is dataDir, for a command that is not the
// running panel (`fairy admin config check`). Nothing is logged and nothing is written. err: state.json
// itself cannot be read.
func ReadRunTimings(dataDir string) (o RunTimings, ignored []string, err error) {
	d, err := statefile.Read(filepath.Join(dataDir, "state.json"))
	if err != nil {
		return RunTimings{Values: map[string]int{}, Marks: map[string]TimingMark{}}, nil, err
	}
	o, ignored = runTimings(d)
	return o, ignored, nil
}

// EditRunTimings changes the overlay in one locked read-modify-write of state.json: fn gets what the file
// holds NOW and changes it (Set, Drop). Two admins who save at the same moment are run one after the other,
// each on what the other left. fn's error stops the write and is returned as it is.
//
// by is who changes it; it and the time are put on every key whose decision differs afterwards. after is the
// stored state after the call, changed reports whether anything was written. A value fn leaves that is not
// one its key can have is refused (nothing is written): what this function stores is always in range. What
// the reader left out because it is not of the form Fairy writes is dropped by the same write; what is under
// a key this build does not know is written back as it was.
func (a *State) EditRunTimings(by string, fn func(o *RunTimings) error) (after RunTimings, changed bool, err error) {
	_, err = statefile.Update(a.StatePath, func(d statefile.Data) error {
		cur, _ := runTimings(d)
		before := cur.Clone()
		if err := fn(&cur); err != nil {
			return err
		}
		cur = cur.Clone()
		now := time.Now().UTC().Truncate(time.Second)
		for k, v := range cur.Values {
			t, known := config.RunTimingOf(k)
			if !known || !t.In(v) {
				return fmt.Errorf("run timings: %s cannot be %d", clipKey(k), v)
			}
			if old, had := before.Values[k]; !had || old != v {
				cur.Marks[k] = TimingMark{By: clipKey(by), At: now}
			}
		}
		for k := range cur.Marks {
			if _, has := cur.Values[k]; !has {
				delete(cur.Marks, k)
			}
		}
		after = cur
		if maps.Equal(before.Values, cur.Values) {
			after.Rev = before.Rev
			return errNoChange
		}
		cur.Rev = before.Rev + 1
		after = cur
		d[runTimingsKey] = cur.encode()
		return nil
	})
	if errors.Is(err, errNoChange) {
		return after, false, nil
	}
	a.state.Refresh()
	if err != nil {
		return RunTimings{}, false, err
	}
	return after, true, nil
}
