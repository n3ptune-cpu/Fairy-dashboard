package power

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// Changes to the day history's code that the review made and no test noticed. Each has its test here; the
// name says which line it is for.

// The longest id that gets a day is maxDayID bytes, in the store and from the file.
func TestDayIDOfTheLongestLengthAndOneMore(t *testing.T) {
	long, over := strings.Repeat("a", maxDayID), strings.Repeat("a", maxDayID+1)
	d := loaded(dayPath(t), at(0, 0, 0))
	d.Record(at(0, 0, 0), dayOf(long, over), nil)
	if !d.Has(long, at(0, 0, 0)) || d.Has(over, at(0, 0, 0)) || d.Len() != 1 {
		t.Fatalf("an id of %d bytes has a day: %v; one of %d: %v", maxDayID, d.Has(long, at(0, 0, 0)), maxDayID+1, d.Has(over, at(0, 0, 0)))
	}
	one := fmt.Sprintf(`{"start":%d,"min":[1],"avg":[1],"max":[1]}`, at(0, 0, 0).Unix()/60)
	file := fmt.Sprintf(`{"version":1,"step_s":60,"saved_at":"2026-11-14T12:00:30Z","devices":{%q:%s,%q:%s}}`, long, one, over, one)
	if err := os.WriteFile(d.path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	if e := loaded(d.path, at(0, 1, 0)); !e.Has(long, at(0, 1, 0)) || e.Has(over, at(0, 1, 0)) {
		t.Errorf("from the file: the id of %d bytes has a day: %v; the one of %d: %v", maxDayID, e.Has(long, at(0, 1, 0)), maxDayID+1, e.Has(over, at(0, 1, 0)))
	}
}

// The day is 1440 minutes: the minute of now and the 1439 before it. A reading 24 hours old to the minute is
// not in it, and a source whose newest reading is that old has no day.
func TestDayIsExactly1440Minutes(t *testing.T) {
	d := NewDayStore()
	rec(d, at(0, 0, 0), 10) // 12:00
	rec(d, at(0, 1, 0), 11) // 12:01
	rec(d, at(23, 59, 0), 12)
	// at 11:59:59 the next day all three are in it; the list is the whole day
	s := seriesOf(t, d, "plug-a", at(23, 59, 59))
	if len(s.Avg) != DaySlots || s.Start != at(0, 0, 0).Unix() || *s.Avg[0] != 10 || *s.Avg[DaySlots-1] != 12 || s.Summary.CoveredS != 180 {
		t.Fatalf("a minute before 24 hours are over: %d minutes from %d, %+v", len(s.Avg), s.Start, s.Summary)
	}
	// at 12:00:00 the reading of 12:00 the day before is not: the list starts at 12:01
	s = seriesOf(t, d, "plug-a", at(24, 0, 0))
	if len(s.Avg) != DaySlots-1 || s.Start != at(0, 1, 0).Unix() || *s.Avg[0] != 11 || s.Summary.CoveredS != 120 || s.Summary.MinW != 11 {
		t.Fatalf("24 hours after the first reading: %d minutes from %d (12:01 is %d), %+v", len(s.Avg), s.Start, at(0, 1, 0).Unix(), s.Summary)
	}
	// a source's day is over 24 hours after its newest reading, not a minute later
	if !d.Has("plug-a", at(47, 58, 59)) || d.Has("plug-a", at(47, 59, 0)) {
		t.Errorf("Has: a second before the newest reading is 24 hours old: %v; at that second: %v", d.Has("plug-a", at(47, 58, 59)), d.Has("plug-a", at(47, 59, 0)))
	}
	if s := seriesOf(t, d, "plug-a", at(47, 58, 59)); len(s.Avg) != 1 || *s.Avg[0] != 12 {
		t.Errorf("the last minute of the day: %+v", s.Avg)
	}
	// and it makes room for a new source exactly then (room)
	e := NewDayStore()
	var ids []string
	for i := 0; i < MaxDayDevices; i++ {
		ids = append(ids, fmt.Sprintf("plug-%02d", i))
	}
	e.Record(at(0, 0, 0), dayOf(ids...), nil)
	e.Record(at(23, 59, 59), dayOf("new"), nil)
	if e.Has("new", at(23, 59, 59)) {
		t.Fatal("a source whose day is not over made room")
	}
	e.Record(at(24, 0, 0), dayOf("new"), nil)
	if !e.Has("new", at(24, 0, 0)) || e.Len() != 1 {
		t.Errorf("24 hours after their newest reading the sources did not make room: %d sources", e.Len())
	}
}

// Forget takes the sources whose id begins with the host's "<id>/", not every id that has it somewhere.
func TestDayForgetIsByTheBeginningOfTheID(t *testing.T) {
	d := NewDayStore()
	d.Record(at(0, 0, 0), dayOf("pc-a/plug-a", "big-pc-a/plug-a", "x/pc-a/y"), nil)
	if !d.Forget("pc-a/", at(0, 0, 1)) {
		t.Fatal("nothing forgotten")
	}
	if d.Has("pc-a/plug-a", at(0, 0, 1)) || !d.Has("big-pc-a/plug-a", at(0, 0, 1)) || !d.Has("x/pc-a/y", at(0, 0, 1)) {
		t.Errorf("after the removal of pc-a: its plug %v, big-pc-a's plug %v, x/pc-a/y %v; want false, true, true",
			d.Has("pc-a/plug-a", at(0, 0, 1)), d.Has("big-pc-a/plug-a", at(0, 0, 1)), d.Has("x/pc-a/y", at(0, 0, 1)))
	}
	// the same for the sample that is on its way
	d.Record(at(0, 0, 2), dayOf("big-pc-a/plug-b", "pc-a/plug-b"), nil)
	if !d.Has("big-pc-a/plug-b", at(0, 0, 2)) || d.Has("pc-a/plug-b", at(0, 0, 2)) {
		t.Error("a new source of big-pc-a is kept out by the removal of pc-a, or one of pc-a is not")
	}
}

// A source that has no volts and no amperes has no list of them in the answer or in the file (not a list of
// nulls: 1440 of them twice, for most sources).
func TestDayListsThatASourceDoesNotHaveAreLeftOut(t *testing.T) {
	d := loaded(dayPath(t), at(0, 0, 0))
	for i := 0; i < 5; i++ {
		d.Record(at(0, i, 0), []DaySample{{ID: "plug-a", W: 40}, {ID: "plug-b", W: 40, V: f(246.5)}, {ID: "plug-c", W: 40, A: f(0.2), KWh: f(3)}}, nil)
	}
	keys := func(id string) string {
		raw, err := json.Marshal(d.Series(id, at(0, 5, 0)))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, k := range []string{`"volts":`, `"amps":`} {
			if strings.Contains(string(raw), k) {
				out = append(out, k)
			}
		}
		return strings.Join(out, " ")
	}
	for id, want := range map[string]string{"plug-a": "", "plug-b": `"volts":`, "plug-c": `"amps":`} {
		if got := keys(id); got != want {
			t.Errorf("%s: the answer has %s, want %s", id, got, want)
		}
	}
	saved(t, d, at(0, 5, 0))
	raw, _ := os.ReadFile(d.path)
	var file struct {
		Devices map[string]map[string]json.RawMessage `json:"devices"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string][3]bool{"plug-a": {false, false, false}, "plug-b": {true, false, false}, "plug-c": {false, true, true}} {
		dev := file.Devices[id]
		if got := [3]bool{dev["volts"] != nil, dev["amps"] != nil, dev["kwh"] != nil}; got != want {
			t.Errorf("%s in the file: volts, amps, kwh are there: %v, want %v", id, got, want)
		}
	}
}

// open_n is the number of samples of the file's last minute, the one that may go on. When that minute is not
// taken (it is after the panel's clock), the newest minute that is taken is a closed one: a sample that comes
// into it counts as its second.
func TestDayFileOpenCountIsForTheFilesLastMinuteOnly(t *testing.T) {
	path := dayPath(t)
	m0 := at(0, 0, 0).Unix() / 60
	file := fmt.Sprintf(`{"version":1,"step_s":60,"saved_at":%q,"devices":{"plug-a":{"start":%d,"open_n":9,"min":[10,20,30,40],"avg":[10,20,30,40],"max":[10,20,30,40]}}}`,
		at(0, 1, 30).UTC().Format(time.RFC3339), m0)
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	d := loaded(path, at(0, 1, 30)) // 12:01: the minutes 12:02 and 12:03 of the file are not taken
	if n := minutes(d, "plug-a", at(0, 1, 30)); n != 2 {
		t.Fatalf("%d minutes taken, want 12:00 and 12:01", n)
	}
	rec(d, at(0, 1, 40), 100)
	if s := seriesOf(t, d, "plug-a", at(0, 1, 50)); *s.Avg[1] != 60 {
		t.Errorf("12:01 after one more sample of 100 W: average %v, want 60 (the 20 of the file and 100); with the file's open_n it would be 28", *s.Avg[1])
	}
	// and when the file's last minute is taken, it goes on with its count
	e := loaded(path, at(0, 3, 30))
	rec(e, at(0, 3, 40), 140)
	if s := seriesOf(t, e, "plug-a", at(0, 3, 50)); *s.Avg[3] != 50 {
		t.Errorf("12:03 after one more sample of 140 W: average %v, want 50 (nine of 40 and one of 140)", *s.Avg[3])
	}
}

// The file's days never make the store larger than its limit, also when it has sources of its own already (a
// file that is read at the end of a hold).
func TestDayFileNeverFillsTheStoreOverItsLimit(t *testing.T) {
	path := dayPath(t)
	d := loaded(path, at(0, 0, 0))
	var ids []string
	for i := 0; i < MaxDayDevices; i++ {
		ids = append(ids, fmt.Sprintf("old-%02d", i))
	}
	d.Record(at(0, 0, 0), dayOf(ids...), nil)
	saved(t, d, at(1, 0, 0))
	e := loaded(path, at(0, 30, 0)) // the clock is behind: the file waits
	own := []string{"new-0", "new-1", "new-2", "old-00"}
	e.Record(at(0, 30, 0), dayOf(own...), nil)
	e.SaveIfDue(at(1, 0, 0))
	if !e.Flush(at(1, 0, 1), 5*time.Second) {
		t.Fatal("not written")
	}
	if e.Len() != MaxDayDevices {
		t.Fatalf("%d sources after the file came in, want the limit of %d", e.Len(), MaxDayDevices)
	}
	for _, id := range own {
		if !e.Has(id, at(1, 0, 1)) {
			t.Errorf("%s, which the panel measured itself, lost its place to the file", id)
		}
	}
	if minutes(e, "old-00", at(1, 0, 1)) != 2 {
		t.Errorf("old-00 has %d minutes, want the file's and the panel's own", minutes(e, "old-00", at(1, 0, 1)))
	}
}

// A clock that went back does not stop the writing until it is where it was: the next tick writes.
func TestDayFileIsWrittenAfterTheClockWentBack(t *testing.T) {
	d := loaded(dayPath(t), at(5, 0, 0))
	var writes atomic.Int32
	real := d.writeFile
	d.writeFile = func(p string, b []byte, m os.FileMode) error { writes.Add(1); return real(p, b, m) }
	wait := func() {
		t.Helper()
		d.mu.Lock()
		done := d.saving
		d.mu.Unlock()
		if done != nil {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the write did not end")
			}
		}
	}
	rec(d, at(5, 0, 0), 40)
	d.SaveIfDue(at(5, 4, 59))
	wait()
	if writes.Load() != 0 {
		t.Fatal("a write within five minutes of the start")
	}
	d.SaveIfDue(at(5, 5, 0))
	wait()
	if writes.Load() != 1 {
		t.Fatalf("%d writes after five minutes, want one", writes.Load())
	}
	rec(d, at(3, 0, 0), 40) // the clock was set back by two hours
	d.SaveIfDue(at(3, 0, 0))
	wait()
	if writes.Load() != 2 {
		t.Fatalf("%d writes; the tick after the clock went back by two hours did not write (the next would be in two hours and five minutes)", writes.Load())
	}
	rec(d, at(3, 1, 0), 40)
	d.SaveIfDue(at(3, 4, 59))
	wait()
	if writes.Load() != 2 {
		t.Error("after that the period is not five minutes again")
	}
}

// saved_at is written in UTC, whatever zone the panel's clock is given in.
func TestDayFileSavedAtIsUTC(t *testing.T) {
	d := loaded(dayPath(t), at(0, 0, 0))
	rec(d, at(0, 0, 0), 40)
	saved(t, d, at(0, 1, 7).In(time.FixedZone("east", 5*3600+1800)))
	raw, _ := os.ReadFile(d.path)
	if want := `"saved_at":"2026-11-14T12:01:07Z"`; !strings.Contains(string(raw), want) {
		t.Errorf("the file lacks %s: %.120s", want, raw)
	}
}

// What a source's sample is made of: a sum of watts that no source can have is no sample, and a reading whose
// time is after the panel's clock (the device's clock is ahead) is one.
func TestDaySampleOfReadingsThatAreOdd(t *testing.T) {
	now := time.Now().UTC()
	src := func(rs ...model.PowerReading) model.PowerSourceState {
		return model.PowerSourceState{ID: "plug-a", Status: "ok", Readings: rs}
	}
	big := maxSumW * 0.6
	if s, ok := daySample(src(model.PowerReading{Watts: &big, Timestamp: now}, model.PowerReading{Watts: &big, Timestamp: now}), now); ok {
		t.Errorf("two readings whose sum is %g W are a sample of %g W", 2*big, s.W)
	}
	for _, ahead := range []time.Duration{time.Second, time.Hour, 48 * time.Hour} {
		if s, ok := daySample(src(model.PowerReading{Watts: f(40), Timestamp: now.Add(ahead)}), now); !ok || s.W != 40 {
			t.Errorf("a reading whose time is %s after the panel's clock is no sample", ahead)
		}
	}
}

// The lists never have "-0" in them (a value that rounds to zero from below), which is not a number every
// reader takes.
func TestDayListsHaveNoMinusZero(t *testing.T) {
	negZero := float32(math.Copysign(0, -1))
	for name, c := range map[string]struct {
		v    json.Marshaler
		want string
	}{
		"watts": {model.DayValues{-0.004, negZero, -0.006, 0.004}, "[0,0,-0.01,0]"},
		"amps":  {model.DayAmps{-0.0004, negZero, -0.0006, 0.0004}, "[0,0,-0.001,0]"},
	} {
		got, err := c.v.MarshalJSON()
		if err != nil || string(got) != c.want {
			t.Errorf("%s: %s (%v), want %s", name, got, err, c.want)
		}
	}
}

// The manager tells the store which sources the panel lists, the failing ones too: a listed source that does
// not answer keeps its day when a new source needs a place, and one that is not listed any more gives its
// place. Without that list the store takes nothing out, and the new source would get no day.
func TestDayManagerSaysWhichSourcesAreListed(t *testing.T) {
	pm := manager(t, nil, nil, nil)
	rows := func(n int, bad string) []map[string]any {
		var out []map[string]any
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("plug-%02d", i)
			if id == bad {
				out = append(out, map[string]any{"id": id, "ok": false, "error": "no answer"})
			} else {
				out = append(out, map[string]any{"id": id, "ok": true, "watts": 40.0})
			}
		}
		return out
	}
	// 63 sources of four hosts, and one of a host that is not there any more
	pm.Day.Record(time.Now().Add(-2*time.Hour), dayOf("gone/plug-a"), nil)
	for _, host := range []string{"pc-a", "pc-b", "pc-c"} {
		pm.UpdateRemote(host, rows(MaxRemoteSources, ""), false, nil, nil)
	}
	pm.UpdateRemote("pc-d", rows(MaxRemoteSources-1, ""), false, nil, nil)
	pm.SampleHistory()
	if pm.Day.Len() != MaxDayDevices {
		t.Fatalf("the rig: %d sources", pm.Day.Len())
	}
	// pc-a's plug-03 has not answered for an hour (it is listed, with an error); a new host comes
	pm.Day.mu.Lock()
	pm.Day.dev["pc-a/plug-03"].cur -= 180 // its newest reading is the oldest of all
	pm.Day.mu.Unlock()
	pm.UpdateRemote("pc-a", rows(MaxRemoteSources, "plug-03"), false, nil, nil)
	pm.UpdateRemote("pc-e", rows(1, ""), false, nil, nil)
	pm.SampleHistory()
	now := time.Now()
	if !pm.Day.Has("pc-e/plug-00", now) {
		t.Fatal("the new host's plug has no day: nothing made room (was the store told what is listed?)")
	}
	if pm.Day.Has("gone/plug-a", now) || !pm.Day.Has("pc-a/plug-03", now) {
		t.Errorf("room was made by the wrong source: the one that is not listed has a day: %v; the listed one that does not answer: %v",
			pm.Day.Has("gone/plug-a", now), pm.Day.Has("pc-a/plug-03", now))
	}
	// one more new source: every source is listed now, so it gets no day and nobody loses one
	pm.UpdateRemote("pc-e", rows(2, ""), false, nil, nil)
	pm.SampleHistory()
	if pm.Day.Has("pc-e/plug-01", time.Now()) || !pm.Day.Has("pc-a/plug-03", time.Now()) || pm.Day.Len() != MaxDayDevices {
		t.Errorf("with every source listed: the new one has a day: %v, the failing listed one: %v, %d sources",
			pm.Day.Has("pc-e/plug-01", time.Now()), pm.Day.Has("pc-a/plug-03", time.Now()), pm.Day.Len())
	}
}
