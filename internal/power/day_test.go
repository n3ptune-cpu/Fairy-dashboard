package power

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"
	"unsafe"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// The day history's clock in these tests is made up: every method takes its time as an argument.
var dayT0 = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

// at is the made-up clock at h hours, m minutes and s seconds after dayT0.
func at(h, m, s int) time.Time {
	return dayT0.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second)
}

func f(v float64) *float64 { return &v }

// rec records one sample of plug-a.
func rec(d *DayStore, t time.Time, w float64) {
	d.Record(t, []DaySample{{ID: "plug-a", W: w}}, nil)
}

// series is the day as the API sends it, written and read again: what a browser gets.
type daySeries struct {
	Source  string                 `json:"source"`
	StepS   int                    `json:"step_s"`
	Now     int64                  `json:"now"`
	Start   int64                  `json:"start"`
	Min     []*float64             `json:"min"`
	Avg     []*float64             `json:"avg"`
	Max     []*float64             `json:"max"`
	Volts   []*float64             `json:"volts"`
	Amps    []*float64             `json:"amps"`
	Summary *model.PowerDaySummary `json:"summary"`
}

func seriesOf(t *testing.T, d *DayStore, id string, now time.Time) daySeries {
	t.Helper()
	b, err := json.Marshal(d.Series(id, now))
	if err != nil {
		t.Fatalf("the day cannot be written as JSON: %v", err)
	}
	var out daySeries
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("the day is not JSON: %v\n%s", err, b)
	}
	if len(out.Min) != len(out.Avg) || len(out.Max) != len(out.Avg) || (out.Volts != nil && len(out.Volts) != len(out.Avg)) || (out.Amps != nil && len(out.Amps) != len(out.Avg)) {
		t.Fatalf("the lists are not parallel: %s", b)
	}
	return out
}

// minuteOf is the element of the list for the minute of t (nil: a gap, or outside the list).
func (s daySeries) minuteOf(l []*float64, t time.Time) *float64 {
	i := (t.Unix() - s.Start) / 60
	if s.Start == 0 || i < 0 || i >= int64(len(l)) {
		return nil
	}
	return l[i]
}

func near(a, b, eps float64) bool { return math.Abs(a-b) <= eps }

// A minute's bucket: the lowest, the average and the highest of its samples, and the last volts and amperes.
// The lists start at the first minute with readings and end at the last.
func TestDayBucketsAMinuteOfSamples(t *testing.T) {
	d := NewDayStore()
	for i, w := range []float64{40, 44, 100, 42, 39} { // five samples in the minute 12:00
		d.Record(at(0, 0, i*10), []DaySample{{ID: "plug-a", W: w, V: f(246.5 + float64(i)/10), A: f(0.085)}}, nil)
	}
	for i, w := range []float64{50, 52} { // two in 12:01
		d.Record(at(0, 1, i*10), []DaySample{{ID: "plug-a", W: w, V: f(247.1), A: f(0.2114)}}, nil)
	}
	s := seriesOf(t, d, "plug-a", at(0, 1, 30))
	if s.Source != "plug-a" || s.StepS != 60 || s.Now != at(0, 1, 30).Unix() || s.Start != at(0, 0, 0).Unix() || len(s.Avg) != 2 {
		t.Fatalf("the series: %+v", s)
	}
	if *s.Min[0] != 39 || *s.Avg[0] != 53 || *s.Max[0] != 100 || *s.Volts[0] != 246.9 || *s.Amps[0] != 0.085 {
		t.Errorf("12:00: min %v avg %v max %v volts %v amps %v", *s.Min[0], *s.Avg[0], *s.Max[0], *s.Volts[0], *s.Amps[0])
	}
	if *s.Min[1] != 50 || *s.Avg[1] != 51 || *s.Max[1] != 52 || *s.Volts[1] != 247.1 || *s.Amps[1] != 0.211 {
		t.Errorf("12:01: min %v avg %v max %v volts %v amps %v", *s.Min[1], *s.Avg[1], *s.Max[1], *s.Volts[1], *s.Amps[1])
	}
	sum := s.Summary
	if sum == nil || sum.From != at(0, 0, 0).Unix() || sum.To != at(0, 1, 0).Unix() || sum.CoveredS != 120 ||
		sum.MinW != 39 || sum.MaxW != 100 || sum.AvgW != 52 || sum.MinAt != at(0, 0, 0).Unix() || sum.MaxAt != at(0, 0, 0).Unix() {
		t.Errorf("the summary: %+v", sum)
	}
	if *sum.VoltsMin != 246.9 || *sum.VoltsMax != 247.1 || *sum.AmpsMin != 0.085 || *sum.AmpsMax != 0.211 {
		t.Errorf("volts and amperes of the day: %v %v %v %v", *sum.VoltsMin, *sum.VoltsMax, *sum.AmpsMin, *sum.AmpsMax)
	}
	// a device that never reports volts or amperes has no such lists, and no such numbers
	d.Record(at(0, 1, 0), []DaySample{{ID: "plug-b", W: 7}}, nil)
	if b := seriesOf(t, d, "plug-b", at(0, 1, 30)); b.Volts != nil || b.Amps != nil || b.Summary.VoltsMin != nil || b.Summary.AmpsMax != nil {
		t.Errorf("a device without volts and amperes: %+v", b)
	}
	// a source nobody recorded: empty lists that are lists, no summary
	raw, _ := json.Marshal(d.Series("plug-x", at(0, 1, 30)))
	if string(raw) != fmt.Sprintf(`{"source":"plug-x","step_s":60,"now":%d,"start":0,"min":[],"avg":[],"max":[],"summary":null}`, at(0, 1, 30).Unix()) {
		t.Errorf("an unknown source: %s", raw)
	}
}

// A gap stays a gap. The device answers until 12:10, nothing comes for five hours (the agent that relays it
// was off), and it answers again from 17:10. The minutes between are null in every list: not 0, not the last
// value. The day's lowest, average and highest watts and its time with readings do not count them.
func TestDayAGapIsNullAndCountsAsNothing(t *testing.T) {
	d := NewDayStore()
	for m := 0; m < 10; m++ {
		rec(d, at(0, m, 0), 100)
	}
	for m := 10; m < 20; m++ {
		rec(d, at(5, m, 0), 200)
	}
	s := seriesOf(t, d, "plug-a", at(5, 20, 0))
	if len(s.Avg) != 5*60+20 {
		t.Fatalf("the lists have %d minutes, want from 12:00 to 17:19", len(s.Avg))
	}
	for i, v := range s.Avg {
		gap := i >= 10 && i < 5*60+10
		if gap != (v == nil) || gap != (s.Min[i] == nil) || gap != (s.Max[i] == nil) {
			t.Fatalf("minute %d: avg %v, min %v, max %v (a gap: %v)", i, v, s.Min[i], s.Max[i], gap)
		}
	}
	sum := s.Summary
	if sum.CoveredS != 20*60 || sum.MinW != 100 || sum.MaxW != 200 || sum.AvgW != 150 {
		t.Errorf("the summary counts the gap: %+v", sum)
	}
	// 10 minutes at 100 W and 10 at 200 W: 0.05 kWh, estimated. Five hours at zero would not change that
	// number, but five hours at the value before would (0.5 kWh more).
	if sum.Energy != "estimated" || !near(sum.EnergyKWh, 0.05, 1e-9) || sum.EnergyEstKWh != sum.EnergyKWh {
		t.Errorf("energy over the gap: %+v", sum)
	}
}

// The ring is a day long. A minute that is older than 24 hours is not in the answer, and its slot, which the
// same minute of the next day uses, is never read as that minute.
func TestDayKeepsTwentyFourHoursAndNoMore(t *testing.T) {
	d := NewDayStore()
	for m := 0; m < 30*60; m += 7 { // a sample every 7 minutes for 30 hours
		rec(d, dayT0.Add(time.Duration(m)*time.Minute), float64(m))
	}
	now := dayT0.Add(30 * time.Hour)
	s := seriesOf(t, d, "plug-a", now)
	first := now.Add(-time.Duration(DaySlots-1) * time.Minute)
	if s.Start < first.Unix() || len(s.Avg) > DaySlots {
		t.Fatalf("the lists start at %d (the window starts at %d) and have %d minutes", s.Start, first.Unix(), len(s.Avg))
	}
	n := 0
	for i, v := range s.Avg {
		if v == nil {
			continue
		}
		n++
		if min := (s.Start-dayT0.Unix())/60 + int64(i); *v != float64(min) || min%7 != 0 {
			t.Fatalf("element %d is %v, the sample of minute %d is %d", i, *v, min, min)
		}
	}
	if want := (30*60-1)/7 - (30*60-DaySlots)/7; n != want {
		t.Errorf("%d minutes with readings in the last 24 hours, want %d", n, want)
	}
	// two days later, without a sample: nothing is left, and nothing of the old ring shows
	if later := seriesOf(t, d, "plug-a", now.Add(48*time.Hour)); len(later.Avg) != 0 || later.Summary != nil || later.Start != 0 {
		t.Errorf("two days later: %+v", later)
	}
	if d.Has("plug-a", now.Add(48*time.Hour)) || !d.Has("plug-a", now) {
		t.Error("Has does not follow the 24 hours")
	}
}

// Memory is bounded by design: a bucket has a fixed size, a source a fixed number of them.
func TestDayBucketSizeIsWhatTheDesignSays(t *testing.T) {
	if got := unsafe.Sizeof(dayBucket{}); got != 40 {
		t.Errorf("a bucket is %d bytes; the design, the README and the memory bound say 40", got)
	}
	if DaySlots != 1440 || DayStepS != 60 || MaxDayDevices != 64 {
		t.Errorf("slots %d, step %d s, sources %d", DaySlots, DayStepS, MaxDayDevices)
	}
	d := NewDayStore()
	rec(d, at(0, 0, 0), 1)
	if dv := d.dev["plug-a"]; len(dv.b) != DaySlots || cap(dv.b) != DaySlots {
		t.Errorf("a source's ring has %d buckets (cap %d)", len(dv.b), cap(dv.b))
	}
}

// The clock is set back. Every bucket after the new time is emptied: left alone, each would come back into
// the graph when the clock reaches its minute a second time, as if it had been measured then. The bucket of
// the minute the clock is in now goes on, with the samples it had.
func TestDayClockSetBack(t *testing.T) {
	d := NewDayStore()
	for m := 0; m < 180; m++ { // three hours, 12:00 to 14:59
		rec(d, at(0, m, 0), 100)
		rec(d, at(0, m, 30), 300)
	}
	// set back by two hours, to 13:00:10: a new sample of 50 W
	rec(d, at(1, 0, 10), 50)
	s := seriesOf(t, d, "plug-a", at(1, 0, 20))
	if len(s.Avg) != 61 {
		t.Fatalf("after the step back the lists have %d minutes, want 12:00 to 13:00", len(s.Avg))
	}
	// 13:00 had 100 and 300; with 50 it has three samples
	if *s.Min[60] != 50 || *s.Max[60] != 300 || *s.Avg[60] != 150 {
		t.Errorf("the minute the clock went back to: min %v avg %v max %v, want 50 / 150 / 300", *s.Min[60], *s.Avg[60], *s.Max[60])
	}
	// the clock goes on to 14:00 without a sample (the device is off): the two hours are a gap, and the
	// readings that were taken under those times before the step are not shown as readings of now
	s = seriesOf(t, d, "plug-a", at(2, 0, 0))
	if len(s.Avg) != 61 {
		t.Fatalf("an hour later, without a sample: %d minutes; readings from before the step came back", len(s.Avg))
	}
	// and with samples again from 13:30
	rec(d, at(1, 30, 0), 70)
	s = seriesOf(t, d, "plug-a", at(1, 30, 30))
	if len(s.Avg) != 91 || *s.Avg[90] != 70 || s.Avg[61] != nil || s.Avg[89] != nil {
		t.Errorf("after the step back and half an hour without samples: %d minutes, 13:30 %v", len(s.Avg), s.Avg[len(s.Avg)-1])
	}

	// a step of two seconds over the edge of a minute (the time service corrected the clock): 12:05:01, then
	// 12:04:59. Nothing is lost but the one sample of the minute that has not come yet.
	e := NewDayStore()
	for m := 0; m < 5; m++ {
		rec(e, at(0, m, 20), 10)
		rec(e, at(0, m, 40), 20)
	}
	rec(e, at(0, 5, 1), 99)
	rec(e, at(0, 4, 59), 30)
	s = seriesOf(t, e, "plug-a", at(0, 4, 59))
	if len(s.Avg) != 5 || *s.Avg[4] != 20 || *s.Max[4] != 30 || *s.Min[4] != 10 || *s.Avg[3] != 15 {
		t.Fatalf("after a two-second step back: %d minutes, 12:04 avg %v", len(s.Avg), *s.Avg[4])
	}
	rec(e, at(0, 5, 3), 40)
	if s = seriesOf(t, e, "plug-a", at(0, 5, 4)); len(s.Avg) != 6 || *s.Avg[5] != 40 {
		t.Errorf("the minute after the step: %v (the sample from before the step must not be in it)", *s.Avg[5])
	}
}

// The clock is set forwards (or the panel was stopped): the minutes between have no bucket.
func TestDayClockSetForwardsLeavesAGap(t *testing.T) {
	d := NewDayStore()
	rec(d, at(0, 0, 0), 10)
	rec(d, at(3, 0, 0), 20)
	s := seriesOf(t, d, "plug-a", at(3, 0, 30))
	if len(s.Avg) != 181 || *s.Avg[0] != 10 || *s.Avg[180] != 20 {
		t.Fatalf("%d minutes", len(s.Avg))
	}
	for i := 1; i < 180; i++ {
		if s.Avg[i] != nil {
			t.Fatalf("minute %d has a value in a gap: %v", i, *s.Avg[i])
		}
	}
	// a clock no history can be kept for is ignored, and nothing breaks
	d.Record(time.Unix(-5, 0), []DaySample{{ID: "plug-a", W: 1}}, nil)
	d.Record(time.Date(12000, 1, 1, 0, 0, 0, 0, time.UTC), []DaySample{{ID: "plug-a", W: 1}}, nil)
	if got := seriesOf(t, d, "plug-a", at(3, 0, 30)); len(got.Avg) != 181 {
		t.Errorf("a sample with an impossible time changed the day: %d minutes", len(got.Avg))
	}
	if got := d.Series("plug-a", time.Unix(-5, 0)); len(got.Avg) != 0 || got.Summary != nil {
		t.Errorf("asked with an impossible time: %+v", got)
	}
}

// What is not a possible reading never gets in: watts that are not a number make no sample, and volts,
// amperes or a counter that are not possible are left out of a sample that is otherwise good.
func TestDayTakesOnlyPossibleNumbers(t *testing.T) {
	d := NewDayStore()
	for _, w := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 1e300} {
		rec(d, at(0, 0, 0), w)
	}
	if d.Len() != 0 {
		t.Fatalf("a sample that is not a number made a source: %d", d.Len())
	}
	d.Record(at(0, 0, 0), []DaySample{{ID: "plug-a", W: 5, V: f(math.NaN()), A: f(math.Inf(1)), KWh: f(1e300)}}, nil)
	d.Record(at(0, 0, 0), []DaySample{{ID: "", W: 5}, {ID: "bad\x00id", W: 5}, {ID: string(make([]byte, 300)), W: 5}}, nil)
	s := seriesOf(t, d, "plug-a", at(0, 0, 30))
	if d.Len() != 1 || len(s.Avg) != 1 || *s.Avg[0] != 5 || s.Volts != nil || s.Amps != nil || s.Summary.Energy != "estimated" {
		t.Errorf("sources %d, the day: %+v", d.Len(), s)
	}
}

// At most MaxDayDevices sources have a day. One more: a source whose last reading is older than a day goes
// first; then one the panel does not list any more, the one that was silent longest; and when all of them are
// listed, the new one gets no day and the others keep theirs.
func TestDayHasAHardLimitOfSources(t *testing.T) {
	d := NewDayStore()
	present := map[string]bool{}
	var all []DaySample
	for i := 0; i < MaxDayDevices; i++ {
		id := fmt.Sprintf("agent-%c/plug-%02d", 'a'+i/MaxRemoteSources, i) // four hosts with sixteen each
		present[id] = true
		all = append(all, DaySample{ID: id, W: float64(i)})
	}
	d.Record(at(0, 0, 0), all, present)
	if d.Len() != MaxDayDevices {
		t.Fatalf("%d sources", d.Len())
	}
	// every one is listed: number 65 gets nothing, again and again, and nothing is taken out for it
	present["agent-z/new"] = true
	for i := 0; i < 3; i++ {
		d.Record(at(0, 1+i, 0), append(all, DaySample{ID: "agent-z/new", W: 1}), present)
	}
	if d.Len() != MaxDayDevices || d.Has("agent-z/new", at(0, 3, 0)) {
		t.Fatalf("with every source listed: %d sources, the new one has a day: %v", d.Len(), d.Has("agent-z/new", at(0, 3, 0)))
	}
	// two are not listed any more (taken out of the agent's config); 07 was silent longer than 09
	delete(present, "agent-a/plug-07")
	delete(present, "agent-a/plug-09")
	rest := func(skip ...string) []DaySample {
		var out []DaySample
		for _, s := range all {
			if s.ID != skip[0] && (len(skip) < 2 || s.ID != skip[1]) {
				out = append(out, s)
			}
		}
		return out
	}
	d.Record(at(0, 5, 0), append(rest("agent-a/plug-07"), DaySample{ID: "agent-z/new", W: 1}), present)
	if d.Len() != MaxDayDevices || !d.Has("agent-z/new", at(0, 5, 0)) || d.Has("agent-a/plug-07", at(0, 5, 0)) || !d.Has("agent-a/plug-09", at(0, 5, 0)) {
		t.Fatalf("the source that left the list first did not make room: 07 %v, 09 %v, new %v",
			d.Has("agent-a/plug-07", at(0, 5, 0)), d.Has("agent-a/plug-09", at(0, 5, 0)), d.Has("agent-z/new", at(0, 5, 0)))
	}
	// a day later every source of before is older than a day: they all go for the first new one
	d.Record(at(30, 0, 0), []DaySample{{ID: "agent-c/late", W: 2}}, map[string]bool{"agent-c/late": true})
	if d.Len() != 1 || !d.Has("agent-c/late", at(30, 0, 0)) {
		t.Errorf("after a day: %d sources", d.Len())
	}
	// without a list of what is there (nil), nothing that still has readings is taken out
	e := NewDayStore()
	e.Record(at(0, 0, 0), all, nil)
	e.Record(at(0, 1, 0), []DaySample{{ID: "agent-z/new", W: 1}}, nil)
	if e.Len() != MaxDayDevices || e.Has("agent-z/new", at(0, 1, 0)) {
		t.Errorf("without a list: %d sources", e.Len())
	}
}

// Remove of a host drops the days of the devices it relayed, and of no other source.
func TestDayForgetsAHostsSources(t *testing.T) {
	d := NewDayStore()
	d.Record(at(0, 0, 0), []DaySample{{ID: "pc-a/plug-a", W: 1}, {ID: "pc-a/plug-b", W: 1}, {ID: "pc-ab/plug-a", W: 1}, {ID: "pc-a", W: 1}, {ID: "plug-a", W: 1}}, nil)
	if d.Forget("", at(0, 0, 0)) || d.Forget("pc-z/", at(0, 0, 0)) || d.Len() != 5 {
		t.Fatal("Forget without a prefix, or of a host that has no source, dropped something")
	}
	if !d.Forget("pc-a/", at(0, 0, 0)) {
		t.Fatal("Forget does not say that it dropped the host's sources")
	}
	for id, want := range map[string]bool{"pc-a/plug-a": false, "pc-a/plug-b": false, "pc-ab/plug-a": true, "pc-a": true, "plug-a": true} {
		if d.Has(id, at(0, 0, 30)) != want {
			t.Errorf("%s after the host pc-a was removed: has a day %v, want %v", id, !want, want)
		}
	}
}

// ---- energy over the day

// bucketsOf builds a day for dayEnergy and daySummary: minute i has watts w(i) (NaN: a gap) and the counter
// k(i) (NaN: none).
func bucketsOf(n int, w, k func(i int) float64) []dayBucket {
	bs := make([]dayBucket, n)
	for i := range bs {
		x := w(i)
		if math.IsNaN(x) {
			continue
		}
		bs[i] = dayBucket{minute: uint32(1000 + i), n: 1, min: float32(x), avg: float32(x), max: float32(x), volts: nan32, amps: nan32, kwh: math.NaN()}
		if k != nil {
			bs[i].kwh = k(i)
		}
	}
	return bs
}

func none(int) float64 { return math.NaN() }

// A device with no counter at all: average watts x time, all of it estimated.
func TestDayEnergyWithoutACounterIsEstimated(t *testing.T) {
	bs := bucketsOf(1440, func(int) float64 { return 60 }, nil) // 60 W for 24 h = 1.44 kWh
	s := daySummary(1000, bs)
	if s.Energy != "estimated" || !near(s.EnergyKWh, 1.44, 1e-6) || s.EnergyEstKWh != s.EnergyKWh {
		t.Errorf("no counter: %+v", s)
	}
	// with a gap of five hours the gap adds nothing: 19 h x 60 W = 1.14 kWh
	gap := bucketsOf(1440, func(i int) float64 {
		if i >= 600 && i < 900 {
			return math.NaN()
		}
		return 60
	}, none)
	if s := daySummary(1000, gap); s.Energy != "estimated" || !near(s.EnergyKWh, 1.14, 1e-6) || s.CoveredS != 19*3600 || s.AvgW != 60 || s.MinW != 60 {
		t.Errorf("no counter, a gap of five hours: %+v", s)
	}
}

// A counter that works: its increase is the energy, whatever the watts say in between (within reason), and a
// gap inside it is covered by the counter. The minute of the counter's first value counts with its watts (a
// counter value says nothing about the time before it): 0.002 kWh of such a day.
func TestDayEnergyFromTheCounter(t *testing.T) {
	// 120 W for a day = 2.88 kWh; the counter has steps of 0.001 kWh and starts at 512.300
	k := func(i int) float64 { return 512.3 + math.Floor(float64(i)*0.002*1000)/1000 }
	s := daySummary(1000, bucketsOf(1440, func(int) float64 { return 120 }, k))
	if s.Energy != "counter" || !near(s.EnergyKWh, 2.88, 1e-9) || s.EnergyEstKWh != 0.002 {
		t.Errorf("a counter over the whole day: %+v", s)
	}
	// the same day with five hours in which nobody could ask the plug: the counter went on counting
	gap := bucketsOf(1440, func(i int) float64 {
		if i >= 600 && i < 900 {
			return math.NaN()
		}
		return 120
	}, k)
	if s := daySummary(1000, gap); s.Energy != "counter" || !near(s.EnergyKWh, 2.88, 1e-9) || s.EnergyEstKWh != 0.002 || s.CoveredS != 19*3600 {
		t.Errorf("a counter over a gap: %+v", s)
	}
	// a counter with coarse steps (0.1 kWh): its 28 steps are the counter's, and the 39 minutes since the last
	// step, which no step has counted yet, count with their watts (0.078 kWh): the whole day, 2.88
	coarse := func(i int) float64 { return math.Floor(float64(i)*0.002*10) / 10 }
	if s := daySummary(1000, bucketsOf(1440, func(int) float64 { return 120 }, coarse)); s.Energy != "mixed" || !near(s.EnergyKWh, 2.88, 1e-9) || !near(s.EnergyEstKWh, 0.08, 1e-9) {
		t.Errorf("a counter with steps of 0.1 kWh: %+v", s)
	}
}

// The counter restarts at 0 in the middle of the day (the plug lost its power, or its cloud did). Only
// increases count: the day is what it counted before the restart plus what it counted after it. Never the
// difference of the last and the first value (which is negative here), and never the new value as a jump.
func TestDayEnergyCounterRestartsMidDay(t *testing.T) {
	// 150 W all day = 0.0025 kWh a minute. Minutes 0..719: 87.000 -> 88.7975; restart; minutes 720..1439: 0 -> 1.7975
	k := func(i int) float64 {
		if i < 720 {
			return 87 + float64(i)*0.0025
		}
		return float64(i-720) * 0.0025
	}
	s := daySummary(1000, bucketsOf(1440, func(int) float64 { return 150 }, k))
	// (the counter's first minute and the minute of the restart count with their watts: 0.005 kWh)
	if want := 2*719*0.0025 + 0.005; s.Energy != "counter" || !near(s.EnergyKWh, want, 1e-3) || s.EnergyKWh < 0 || s.EnergyEstKWh != 0.005 {
		t.Errorf("a restart at noon: %+v, want %.4f kWh from the counter", s, want)
	}
	// the restart happens in a gap of five hours (the site was unreachable), and the counter comes back with
	// a small value, 0.004: what was used before the gap and after it counts, the gap is not known
	k = func(i int) float64 {
		if i < 600 {
			return 87 + float64(i)*0.0025
		}
		return 0.004 + float64(i-900)*0.0025
	}
	w := func(i int) float64 {
		if i >= 600 && i < 900 {
			return math.NaN()
		}
		return 150
	}
	s = daySummary(1000, bucketsOf(1440, w, k))
	if want := 599*0.0025 + 539*0.0025 + 0.005; s.Energy != "counter" || !near(s.EnergyKWh, want, 1e-3) {
		t.Errorf("a restart in a gap: %+v, want %.4f kWh", s, want)
	}
	// a counter that goes down by its last digits and up again (a late value from the cloud) is neither
	// negative energy nor a restart: the late value is left out, and the day is what the counter went up by
	k = func(i int) float64 {
		v := 40 + float64(i)*0.0025
		if i%100 == 50 {
			v -= 0.004
		}
		return v
	}
	if s := daySummary(1000, bucketsOf(1440, func(int) float64 { return 150 }, k)); s.Energy != "counter" || !near(s.EnergyKWh, 3.6, 0.001) {
		t.Errorf("a counter that steps back by a digit now and then: %+v, want 3.6 kWh", s)
	}
}

// The counter is stuck at 0 all day while the plug measures 80 W. A counter that did not move says nothing:
// the energy is the watts' (1.92 kWh), and it is said to be estimated. Not 0.
func TestDayEnergyCounterStuckAtZero(t *testing.T) {
	s := daySummary(1000, bucketsOf(1440, func(int) float64 { return 80 }, func(int) float64 { return 0 }))
	if s.Energy != "estimated" || !near(s.EnergyKWh, 1.92, 1e-6) || s.EnergyEstKWh != s.EnergyKWh {
		t.Errorf("a counter stuck at 0: %+v", s)
	}
	// stuck at another value: the same
	if s := daySummary(1000, bucketsOf(1440, func(int) float64 { return 80 }, func(int) float64 { return 0.004 })); s.Energy != "estimated" || !near(s.EnergyKWh, 1.92, 1e-6) {
		t.Errorf("a counter stuck at 0.004: %+v", s)
	}
	// stuck for most of the day and counting for the last hour: the 23 hours count with their watts, the last
	// hour is the counter's
	k := func(i int) float64 { return math.Max(0, float64(i-1380)) * 0.00133 }
	if s := daySummary(1000, bucketsOf(1440, func(int) float64 { return 80 }, k)); s.Energy != "mixed" || !near(s.EnergyKWh, 1.92, 2e-3) || !near(s.EnergyEstKWh, 1.841, 2e-3) {
		t.Errorf("a counter that was stuck for 23 hours: %+v", s)
	}
	// a device that really uses next to nothing with a counter that does not move: the watts, which are small
	if s := daySummary(1000, bucketsOf(1440, func(int) float64 { return 0.4 }, func(int) float64 { return 12 })); s.Energy != "estimated" || !near(s.EnergyKWh, 0.0096, 1e-3) {
		t.Errorf("an idle device: %+v", s)
	}
}

// The counter appears in the middle of the day (energy_entity was added to the agent's config at noon): the
// hours before it are estimated from the watts, the hours with it are the counter's, and the answer says so.
func TestDayEnergyCounterAppearsMidDay(t *testing.T) {
	k := func(i int) float64 {
		if i < 720 {
			return math.NaN()
		}
		return 5 + float64(i-720)*0.004 // 240 W
	}
	s := daySummary(1000, bucketsOf(1440, func(int) float64 { return 240 }, k))
	// before noon: 12 h x 240 W = 2.88 kWh estimated, and the minute of the counter's first value; after it:
	// 719 x 0.004 = 2.876 from the counter
	if s.Energy != "mixed" || !near(s.EnergyEstKWh, 2.884, 1e-6) || !near(s.EnergyKWh, 2.88+2.88, 1e-6) {
		t.Errorf("a counter from noon on: %+v", s)
	}
	// and one that goes away at noon (the sensor became unavailable; the watts go on)
	k = func(i int) float64 {
		if i >= 720 {
			return math.NaN()
		}
		return 5 + float64(i)*0.004
	}
	s = daySummary(1000, bucketsOf(1440, func(int) float64 { return 240 }, k))
	if s.Energy != "mixed" || !near(s.EnergyEstKWh, 2.884, 1e-6) || !near(s.EnergyKWh, 2.88+2.88, 1e-6) {
		t.Errorf("a counter until noon: %+v", s)
	}
}

// A counter value that no plug can have reached in the time (0 one minute, 5000 the next: a glitch) is not an
// increase. It is not the day's energy, and the day does not turn into a spike.
func TestDayEnergyIgnoresAJumpNoDeviceCanMake(t *testing.T) {
	k := func(i int) float64 {
		if i < 700 {
			return 3 + float64(i)*0.001 // 60 W
		}
		return 5003 + float64(i)*0.001
	}
	s := daySummary(1000, bucketsOf(1440, func(int) float64 { return 60 }, k))
	if s.EnergyKWh > 1.5 || s.EnergyKWh < 1.4 || s.Energy != "counter" {
		t.Errorf("a counter that jumps by 5000 kWh in a minute: %+v, want about 1.44 kWh", s)
	}
	// a big consumer is not a glitch: 3 kW for two hours through the counter
	k = func(i int) float64 { return float64(i) * 0.05 }
	if s := daySummary(1000, bucketsOf(120, func(int) float64 { return 3000 }, k)); s.Energy != "counter" || !near(s.EnergyKWh, 6, 1e-6) || s.EnergyEstKWh != 0.05 {
		t.Errorf("3 kW for two hours: %+v", s)
	}
}

// Recorded through the store, the same: the counter's value is the last one of each minute.
func TestDayEnergyThroughTheStore(t *testing.T) {
	d := NewDayStore()
	for m := 0; m < 120; m++ {
		k := 0.0 // stuck
		d.Record(at(0, m, 0), []DaySample{{ID: "plug-a", W: 90, KWh: &k}, {ID: "plug-b", W: 90, KWh: f(10 + float64(m)*0.0015)}, {ID: "plug-c", W: 90}}, nil)
	}
	now := at(2, 0, 0)
	a, b, c := seriesOf(t, d, "plug-a", now).Summary, seriesOf(t, d, "plug-b", now).Summary, seriesOf(t, d, "plug-c", now).Summary
	if a.Energy != "estimated" || !near(a.EnergyKWh, 0.18, 1e-9) {
		t.Errorf("stuck at 0: %+v", a)
	}
	if b.Energy != "counter" || !near(b.EnergyKWh, 0.18, 1e-3) {
		t.Errorf("a counter: %+v", b)
	}
	if c.Energy != "estimated" || !near(c.EnergyKWh, 0.18, 1e-9) {
		t.Errorf("no counter: %+v", c)
	}
}

// A counter's first minute, and the minute it starts again in, have no increase yet and count with their
// watts. A few such minutes in a day do not make the day's energy "mixed": the page would say "partly
// estimated" for a thousandth of it. Above 2 % it does.
func TestDayEnergyIsTheCountersWhenOnlyMinutesOfItAreEstimated(t *testing.T) {
	k := func(i int) float64 {
		if i == 1439 { // the panel was restarted a minute ago, and the counter with it
			return 0
		}
		return 40 + float64(i)*0.002
	}
	s := daySummary(1000, bucketsOf(1440, func(int) float64 { return 120 }, k))
	if s.Energy != "counter" || s.EnergyEstKWh != 0.004 || !near(s.EnergyKWh, 1438*0.002+0.004, 1e-9) {
		t.Errorf("two estimated minutes in a day: %+v", s)
	}
	k = func(i int) float64 { // the last hour without a counter value: 4 % of the day
		if i >= 1380 {
			return math.NaN()
		}
		return 40 + float64(i)*0.002
	}
	if s := daySummary(1000, bucketsOf(1440, func(int) float64 { return 120 }, k)); s.Energy != "mixed" || !near(s.EnergyEstKWh, 0.122, 1e-9) {
		t.Errorf("an estimated hour in a day: %+v", s)
	}
}
