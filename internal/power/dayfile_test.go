package power

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	powersrc "github.com/n3ptune-cpu/Fairy-agent/pkg/power"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// captureLog collects what the code under test logs.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var mu sync.Mutex
	buf := &bytes.Buffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&mu, buf}, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func dayPath(t *testing.T) string { return filepath.Join(t.TempDir(), DayFile) }

// saved writes the store to its file now and waits for it.
func saved(t *testing.T, d *DayStore, now time.Time) {
	t.Helper()
	if !d.Flush(now, 10*time.Second) {
		t.Fatal("the day history was not written")
	}
}

// loaded is a new store that has read the file at path.
func loaded(path string, now time.Time) *DayStore {
	d := NewDayStore()
	d.Load(path, now)
	return d
}

// The file round trip: what a panel wrote at its stop is what the next start shows, lists, gaps, volts,
// amperes and the counter (through which the energy is computed) included. The file is one the panel's own
// rules allow: mode 0600, written in one step, nothing left next to it.
func TestDayFileRoundTrip(t *testing.T) {
	path := dayPath(t)
	d := loaded(path, at(0, 0, 0))
	for m := 0; m < 300; m++ {
		if m >= 100 && m < 160 { // an hour without readings
			continue
		}
		d.Record(at(0, m, 0), []DaySample{
			{ID: "agent-a/plug-a", W: 40 + float64(m%7), V: f(246.5), A: f(0.085 + float64(m)/10000), KWh: f(3 + float64(m)*0.0007)},
			{ID: "plug-b", W: 0.3}}, nil)
		d.Record(at(0, m, 30), []DaySample{{ID: "agent-a/plug-a", W: 60 + float64(m%7), V: f(247.25), A: f(0.3331), KWh: f(3 + float64(m)*0.0007 + 0.0003)}}, nil)
	}
	now := at(5, 0, 0)
	want := map[string]daySeries{"agent-a/plug-a": seriesOf(t, d, "agent-a/plug-a", now), "plug-b": seriesOf(t, d, "plug-b", now)}
	saved(t, d, now)

	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() {
		t.Fatalf("the file: %v %v", st, err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("the file's mode is %v, want 0600", st.Mode().Perm())
	}
	if names, _ := os.ReadDir(filepath.Dir(path)); len(names) != 1 {
		t.Errorf("the folder holds %d files after a write, want the one", len(names))
	}
	raw, _ := os.ReadFile(path)
	var head struct {
		Version int       `json:"version"`
		StepS   int       `json:"step_s"`
		SavedAt time.Time `json:"saved_at"`
		Devices map[string]struct {
			Start int64      `json:"start"`
			OpenN int        `json:"open_n"`
			Avg   []*float64 `json:"avg"`
			Volts []*float64 `json:"volts"`
			KWh   []*float64 `json:"kwh"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		t.Fatalf("the file is not JSON: %v", err)
	}
	a, b := head.Devices["agent-a/plug-a"], head.Devices["plug-b"]
	if head.Version != 1 || head.StepS != 60 || !head.SavedAt.Equal(now) || len(head.Devices) != 2 ||
		a.Start != at(0, 0, 0).Unix()/60 || len(a.Avg) != 300 || a.Avg[100] != nil || a.Avg[159] != nil || a.Avg[160] == nil || a.OpenN != 2 ||
		len(a.KWh) != 300 || b.Volts != nil || b.KWh != nil || b.OpenN != 1 {
		t.Fatalf("the file's content: version %d, step %d, saved %v, %d sources, plug-a start %d with %d minutes", head.Version, head.StepS, head.SavedAt, len(head.Devices), a.Start, len(a.Avg))
	}
	if len(raw) > 300*2*60 { // "about 30 to 60 kB per device and day": well under 60 bytes a minute
		t.Errorf("the file has %d bytes for 2 x 300 minutes", len(raw))
	}

	e := loaded(path, now.Add(20*time.Second)) // the panel starts again 20 seconds later
	for id, w := range want {
		got := seriesOf(t, e, id, now)
		gj, _ := json.Marshal(got)
		wj, _ := json.Marshal(w)
		if string(gj) != string(wj) {
			t.Errorf("%s after the restart is not what it was:\n got %.300s\nwant %.300s", id, gj, wj)
		}
	}
	// the file is not written again while nothing changed
	before, _ := os.Stat(path)
	e.SaveIfDue(now.Add(time.Hour))
	if !e.Flush(now.Add(time.Hour), time.Second) {
		t.Fatal("Flush with nothing to write did not come back")
	}
	if after, _ := os.Stat(path); !os.SameFile(before, after) {
		t.Error("the file was written again although nothing had changed")
	}
}

// A restart in the middle of a minute. The minute had four samples before the stop; two come after the start.
// Its average is over all six, and its lowest and highest are of all six.
func TestDayRestartInTheMiddleOfAMinute(t *testing.T) {
	path := dayPath(t)
	d := loaded(path, at(0, 0, 0))
	for i, w := range []float64{10, 20, 30, 100} {
		rec(d, at(0, 7, i*5), w)
	}
	saved(t, d, at(0, 7, 20))

	e := loaded(path, at(0, 7, 40))
	rec(e, at(0, 7, 45), 4)
	rec(e, at(0, 7, 50), 16)
	s := seriesOf(t, e, "plug-a", at(0, 7, 55))
	if len(s.Avg) != 1 || *s.Min[0] != 4 || *s.Max[0] != 100 || *s.Avg[0] != 30 {
		t.Fatalf("the minute of the restart: min %v avg %v max %v, want 4 / 30 / 100 (six samples)", *s.Min[0], *s.Avg[0], *s.Max[0])
	}
	// and a restart that takes ten minutes: the old minute stays as it was, the minutes between are a gap
	saved(t, e, at(0, 7, 55))
	g := loaded(path, at(0, 17, 0))
	rec(g, at(0, 17, 5), 50)
	s = seriesOf(t, g, "plug-a", at(0, 17, 10))
	if len(s.Avg) != 11 || *s.Avg[0] != 30 || *s.Avg[10] != 50 || s.Avg[1] != nil || s.Avg[9] != nil {
		t.Errorf("after ten minutes without the panel: %d minutes", len(s.Avg))
	}
}

// A file that cannot be used never stops the start and is never half used: the store is empty, the log says
// so once, and the next write replaces the file.
func TestDayFileThatCannotBeUsedStartsEmpty(t *testing.T) {
	good := func() []byte {
		p := dayPath(t)
		d := loaded(p, at(0, 0, 0))
		rec(d, at(0, 1, 0), 5)
		saved(t, d, at(0, 2, 0))
		b, _ := os.ReadFile(p)
		return b
	}()
	many := map[string]any{}
	for i := 0; i <= MaxDayDevices; i++ {
		many[fmt.Sprintf("plug-%d", i)] = map[string]any{"start": at(0, 0, 0).Unix() / 60, "min": []float64{1}, "avg": []float64{1}, "max": []float64{1}}
	}
	tooMany, _ := json.Marshal(map[string]any{"version": 1, "step_s": 60, "devices": many})
	for name, content := range map[string][]byte{
		"empty":            {},
		"spaces":           []byte("  \n"),
		"not JSON":         []byte("power day history\n"),
		"cut off":          good[:len(good)/2],
		"cut off by one":   good[:len(good)-1],
		"a list":           []byte("[1,2,3]"),
		"null":             []byte("null"),
		"another version":  bytes.Replace(good, []byte(`"version":1`), []byte(`"version":2`), 1),
		"no version":       bytes.Replace(good, []byte(`"version":1,`), nil, 1),
		"another step":     bytes.Replace(good, []byte(`"step_s":60`), []byte(`"step_s":300`), 1),
		"too many sources": tooMany,
		"a list of text":   bytes.Replace(good, []byte(`"avg":[5]`), []byte(`"avg":["5"]`), 1),
		"too large":        append(append([]byte(`{"version":1,"step_s":60,"devices":{},"pad":"`), bytes.Repeat([]byte("x"), maxDayFile)...), `"}`...),
	} {
		t.Run(name, func(t *testing.T) {
			log := captureLog(t)
			path := dayPath(t)
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			d := loaded(path, at(0, 3, 0))
			if d.Len() != 0 {
				t.Fatalf("%d sources were taken from a file that cannot be used", d.Len())
			}
			if n := strings.Count(log.String(), "the file is not used"); n != 1 {
				t.Errorf("the log says it %d times, want once:\n%s", n, log)
			}
			if strings.Contains(log.String(), "xxxxxxxx") || strings.Contains(log.String(), "power day history") {
				t.Errorf("the log repeats the file's content:\n%.400s", log)
			}
			// the panel goes on, and its next write puts a good file there
			rec(d, at(0, 4, 0), 9)
			saved(t, d, at(0, 5, 0))
			if e := loaded(path, at(0, 5, 30)); !e.Has("plug-a", at(0, 5, 30)) {
				t.Error("the file was not replaced by a good one")
			}
		})
	}
	// no file at all: empty, and not a word
	log := captureLog(t)
	if d := loaded(dayPath(t), at(0, 3, 0)); d.Len() != 0 || log.Len() != 0 {
		t.Errorf("no file: %d sources, log %q", d.Len(), log)
	}
}

// What is wrong inside a file that is otherwise the panel's: only the part that is wrong is left out.
func TestDayFileLeavesOutWhatIsWrongInIt(t *testing.T) {
	start := at(0, 0, 0).Unix() / 60
	nums := func(v ...any) []any { return v }
	dev := func(o map[string]any) map[string]any {
		out := map[string]any{"start": start, "open_n": 1, "min": nums(1, 2, 3), "avg": nums(2, 3, 4), "max": nums(3, 4, 5)}
		for k, v := range o {
			out[k] = v
		}
		return out
	}
	long := make([]any, DaySlots+1)
	for i := range long {
		long[i] = 1
	}
	file := map[string]any{"version": 1, "step_s": 60, "saved_at": "2026-11-14T12:02:00Z", "devices": map[string]any{
		"good":             dev(nil),
		"":                 dev(nil),
		"bad\u0001id":      dev(nil),
		"lists-differ":     dev(map[string]any{"min": nums(1, 2)}),
		"volts-differ":     dev(map[string]any{"volts": nums(230, 231)}),
		"too-long":         dev(map[string]any{"min": long, "avg": long, "max": long}),
		"no-lists":         map[string]any{"start": start},
		"start-zero":       dev(map[string]any{"start": 0}),
		"start-negative":   dev(map[string]any{"start": -5}),
		"start-huge":       dev(map[string]any{"start": 1e15}),
		"all-gaps":         dev(map[string]any{"min": nums(nil, nil, nil), "avg": nums(nil, nil, nil), "max": nums(nil, nil, nil)}),
		"impossible-watts": dev(map[string]any{"avg": nums(2, 1e300, 4), "max": nums(3, 1e300, 5)}),
		"min-over-max":     dev(map[string]any{"min": nums(1, 9, 3)}),
		"bad-extras":       dev(map[string]any{"volts": nums(230, 1e300, nil), "amps": nums(-1e300, 0.2, 0.3), "kwh": nums(1, 1e300, 3)}),
		"open-n-huge":      dev(map[string]any{"open_n": 1e12}),
		"open-n-negative":  dev(map[string]any{"open_n": -7}),
		// the future: written by a clock that was ahead, or the clock was set back while the panel was off
		"future":      dev(map[string]any{"start": start + 100}),
		"part-future": dev(map[string]any{"start": start + 1}),
		// older than a day
		"old":      dev(map[string]any{"start": start - 3000}),
		"part-old": dev(map[string]any{"start": start - DaySlots + 1}), // its last minute is the window's first
	}}
	raw, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	path := dayPath(t)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	now := at(0, 2, 30) // the minute 12:02: the lists' three minutes are 12:00, 12:01, 12:02
	d := loaded(path, now)
	minutes := func(id string) (n int) {
		for _, v := range seriesOf(t, d, id, now).Avg {
			if v != nil {
				n++
			}
		}
		return n
	}
	for id, want := range map[string]int{"good": 3, "": 0, "bad\u0001id": 0, "lists-differ": 0, "volts-differ": 0, "too-long": 0, "no-lists": 0,
		"start-zero": 0, "start-negative": 0, "start-huge": 0, "all-gaps": 0, "impossible-watts": 2, "min-over-max": 2, "bad-extras": 3,
		"open-n-huge": 3, "open-n-negative": 3, "future": 0, "part-future": 2, "old": 0, "part-old": 1} {
		if got := minutes(id); got != want {
			t.Errorf("%q: %d minutes were taken from the file, want %d", id, got, want)
		}
		if (want > 0) != d.Has(id, now) {
			t.Errorf("%q: has a day %v", id, d.Has(id, now))
		}
	}
	// a source that has nothing in the last 24 hours takes no place among the MaxDayDevices
	if d.Len() != 8 {
		t.Errorf("%d sources were kept from the file, want the 8 that have a minute in the last 24 hours", d.Len())
	}
	s := seriesOf(t, d, "bad-extras", now)
	if *s.Volts[0] != 230 || s.Volts[1] != nil || s.Volts[2] != nil || s.Amps[0] != nil || *s.Amps[1] != 0.2 {
		t.Errorf("impossible volts and amperes: %v %v", s.Volts, s.Amps)
	}
	// "part-future": the minute that was after the clock is not there, and does not come back when the clock
	// gets to it without a sample
	if s := seriesOf(t, d, "part-future", now.Add(5*time.Minute)); len(s.Avg) != 2 {
		t.Errorf("a minute from the file that was after the panel's clock is shown later: %d minutes", len(s.Avg))
	}
	// the open minute of a file with an absurd number of samples: one new sample still moves the average
	d.Record(now, []DaySample{{ID: "open-n-negative", W: 10}}, nil)
	if s := seriesOf(t, d, "open-n-negative", now); *s.Avg[2] != 7 {
		t.Errorf("open_n -7 was not read as one sample: the average with one more is %v, want 7", *s.Avg[2])
	}
}

// The file's name is taken by something that is not a plain file: a link (to a file that must not be read or
// replaced), a folder. It is not read, not followed and not written over; the panel runs, and the graphs work
// from memory.
func TestDayFileThatIsNotAPlainFileIsLeftAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links need a privilege on Windows")
	}
	for _, kind := range []string{"link", "folder"} {
		t.Run(kind, func(t *testing.T) {
			log := captureLog(t)
			dir := t.TempDir()
			path := filepath.Join(dir, DayFile)
			target := filepath.Join(dir, "elsewhere.json")
			// what the link points to is a good day file of another panel: it must not be taken
			o := loaded(target, at(0, 0, 0))
			o.Record(at(0, 1, 0), []DaySample{{ID: "plug-z", W: 5}}, nil)
			saved(t, o, at(0, 2, 0))
			want, _ := os.ReadFile(target)
			if kind == "link" {
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			d := loaded(path, at(0, 3, 0))
			if d.Len() != 0 || !strings.Contains(log.String(), "the file is not used") {
				t.Fatalf("%d sources; log:\n%s", d.Len(), log)
			}
			rec(d, at(0, 4, 0), 9)
			if d.Flush(at(0, 5, 0), 5*time.Second) {
				t.Error("Flush says the day was written")
			}
			if !strings.Contains(log.String(), "is not a plain file") {
				t.Errorf("the log does not say why nothing is kept:\n%s", log)
			}
			st, err := os.Lstat(path)
			if err != nil || st.Mode().IsRegular() {
				t.Fatalf("what was at the file's name was replaced: %v %v", st, err)
			}
			if got, _ := os.ReadFile(target); !bytes.Equal(got, want) {
				t.Error("the file the link points to was changed")
			}
			if s := seriesOf(t, d, "plug-a", at(0, 4, 30)); len(s.Avg) != 1 {
				t.Error("the graph does not work from memory")
			}
		})
	}
}

// A write that fails (the disk is full, the folder is gone) is said once in the log, however often it fails
// with the same words, and tried again; when it works again, the file has everything, and a later failure is
// said again.
func TestDayFileWriteThatFailsIsSaidOnceAndTriedAgain(t *testing.T) {
	log := captureLog(t)
	path := dayPath(t)
	d := loaded(path, at(0, 0, 0))
	var fail atomic.Bool
	var writes atomic.Int32
	real := d.writeFile
	d.writeFile = func(p string, b []byte, m os.FileMode) error {
		writes.Add(1)
		if fail.Load() {
			return errors.New("no space left on device")
		}
		return real(p, b, m)
	}
	wait := func() {
		t.Helper()
		for i := 0; ; i++ {
			d.mu.Lock()
			busy := d.saving != nil
			d.mu.Unlock()
			if !busy {
				return
			}
			if i > 5000 {
				t.Fatal("a write did not end")
			}
			time.Sleep(time.Millisecond)
		}
	}
	fail.Store(true)
	for i := 1; i <= 4; i++ { // four periods, four tries
		rec(d, at(0, i*5, 0), float64(i))
		d.SaveIfDue(at(0, i*5, 1))
		wait()
	}
	if writes.Load() != 4 {
		t.Fatalf("%d writes were tried in four periods, want 4", writes.Load())
	}
	if n := strings.Count(log.String(), "could not keep the last 24 hours"); n != 1 {
		t.Errorf("the failure is in the log %d times, want once:\n%s", n, log)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a file is there although every write failed")
	}
	// within the period nothing is tried again
	d.SaveIfDue(at(0, 21, 0))
	wait()
	if writes.Load() != 4 {
		t.Error("a write was tried again 59 s after the last one failed")
	}
	fail.Store(false)
	d.SaveIfDue(at(0, 25, 1)) // nothing new was recorded since: what failed is still to be written
	wait()
	if s := seriesOf(t, loaded(path, at(0, 26, 0)), "plug-a", at(0, 26, 0)); s.Summary == nil || s.Summary.CoveredS != 4*60 {
		t.Fatalf("after the disk works again the file does not have the four minutes: %+v", s.Summary)
	}
	fail.Store(true)
	rec(d, at(0, 30, 0), 1)
	d.SaveIfDue(at(0, 30, 2))
	wait()
	if n := strings.Count(log.String(), "could not keep the last 24 hours"); n != 2 {
		t.Errorf("a failure after a good write is in the log %d times in all, want twice", n)
	}
	// a folder that cannot be made (its place is a file): the same, for root as for everyone else
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := loaded(filepath.Join(blocked, "data", DayFile), at(0, 0, 0))
	rec(e, at(0, 1, 0), 1)
	if e.Flush(at(0, 2, 0), 5*time.Second) {
		t.Error("Flush says the day was written into a folder that cannot exist")
	}
	if s := seriesOf(t, e, "plug-a", at(0, 1, 30)); len(s.Avg) != 1 {
		t.Error("without a file the graph does not work from memory")
	}
}

// Polling is never held up by the disk. The write hangs (a disk that does not answer): the history tick,
// which records the samples and asks for the write, comes back at once, tick after tick; no second write is
// started behind the first; the graph is answered from memory; and the stop waits for the write only as long
// as it was told to.
func TestDayFileWriteThatHangsHoldsNothingUp(t *testing.T) {
	path := dayPath(t)
	d := loaded(path, at(0, 0, 0))
	release := make(chan struct{})
	var started atomic.Int32
	real := d.writeFile
	d.writeFile = func(p string, b []byte, m os.FileMode) error {
		started.Add(1)
		<-release
		return real(p, b, m)
	}
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	rec(d, at(0, 0, 0), 1)
	d.SaveIfDue(at(0, 5, 0))
	for i := 0; started.Load() == 0; i++ {
		if i > 5000 {
			t.Fatal("the write was not started")
		}
		time.Sleep(time.Millisecond)
	}
	// 600 ticks (50 minutes of the made-up clock) while the write hangs
	begin := time.Now()
	for i := 0; i < 600; i++ {
		now := at(0, 5, 0).Add(time.Duration(i) * 5 * time.Second)
		d.Record(now, []DaySample{{ID: "plug-a", W: float64(i)}}, map[string]bool{"plug-a": true})
		d.SaveIfDue(now)
		d.Series("plug-a", now)
	}
	if took := time.Since(begin); took > 3*time.Second {
		t.Fatalf("600 history ticks took %v while a write hangs", took)
	}
	time.Sleep(200 * time.Millisecond) // a second write would be on its way by now (ten periods have passed for the store)
	if started.Load() != 1 {
		t.Errorf("%d writes were started while the first one hangs, want 1", started.Load())
	}
	if s := seriesOf(t, d, "plug-a", at(0, 55, 0)); s.Summary == nil || s.Summary.MaxW != 599 {
		t.Errorf("the graph from memory while the write hangs: %+v", s.Summary)
	}
	// the stop: told to wait 150 ms, it comes back and says that not everything is written
	begin = time.Now()
	if d.Flush(at(0, 56, 0), 150*time.Millisecond) {
		t.Error("Flush says everything is written while the write hangs")
	}
	if took := time.Since(begin); took < 100*time.Millisecond || took > 2*time.Second {
		t.Errorf("Flush with 150 ms to wait took %v", took)
	}
	// the disk answers again: the write that hung ends, and the next one writes what came meanwhile
	close(release)
	if !d.Flush(at(0, 56, 0), 10*time.Second) {
		t.Fatal("after the disk answers again the day was not written")
	}
	if s := seriesOf(t, loaded(path, at(0, 57, 0)), "plug-a", at(0, 57, 0)); s.Summary == nil || s.Summary.MaxW != 599 {
		t.Errorf("the file after the write that hung: %+v", s.Summary)
	}
}

// The same through the manager: SampleHistory is what the poller's history job calls every few seconds. With a
// write that hangs it still returns at once, and the hour's history and the overview are served.
func TestSampleHistoryIsNotHeldUpByTheDayFile(t *testing.T) {
	pm := manager(t, nil, nil, nil)
	pm.Day.Load(dayPath(t), time.Now().Add(-time.Hour)) // read an hour ago: a write is due with the first tick
	release := make(chan struct{})
	var started atomic.Int32
	pm.Day.writeFile = func(string, []byte, os.FileMode) error {
		started.Add(1)
		<-release
		return errors.New("the disk went away")
	}
	defer close(release)
	row := map[string]any{"id": "plug-a", "name": "Plug A", "type": "homeassistant", "ok": true, "watts": 40.0}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			pm.UpdateRemote("pc-a", []map[string]any{row}, false, nil, nil)
			pm.SampleHistory()
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("SampleHistory waits for the day file's write")
	}
	for i := 0; started.Load() == 0 && i < 5000; i++ { // the write's goroutine encodes before it writes
		time.Sleep(time.Millisecond)
	}
	if started.Load() != 1 {
		t.Errorf("%d writes were started, want the one that hangs", started.Load())
	}
	if n := len(pm.HistoryView("").Points); n != 200 {
		t.Errorf("the hour's history has %d points, want 200", n)
	}
	if !pm.Day.Has("pc-a/plug-a", time.Now()) {
		t.Error("the day has no sample")
	}
}

// What gives a sample: a source that is ok, with watts, with a reading that is not old. A source that fails
// ("stale": it still shows its last reading; "unreachable": it shows none) gives none, and neither does an
// agent that cannot be reached. That is what makes a gap a gap.
func TestDaySamplesOnlyGoodReadings(t *testing.T) {
	now := time.Now().UTC()
	rd := func(w *float64, age time.Duration) model.PowerReading {
		return model.PowerReading{Watts: w, Timestamp: now.Add(-age)}
	}
	src := func(status string, rs ...model.PowerReading) model.PowerSourceState {
		return model.PowerSourceState{ID: "plug-a", Status: status, Readings: rs}
	}
	for name, c := range map[string]struct {
		st    model.PowerSourceState
		ok    bool
		watts float64
	}{
		"ok":                      {src("ok", rd(f(40), 0)), true, 40},
		"ok, ten seconds old":     {src("ok", rd(f(40), 10*time.Second)), true, 40},
		"stale":                   {src("stale", rd(f(40), 0)), false, 0},
		"unreachable":             {src("unreachable"), false, 0},
		"ok without a reading":    {src("ok"), false, 0},
		"ok without watts":        {src("ok", model.PowerReading{Voltage: f(230), Timestamp: now}), false, 0},
		"ok, polling paused":      {src("ok", rd(f(40), 11*time.Minute)), false, 0},
		"two readings":            {src("ok", rd(f(40), 0), rd(f(2.5), 0)), true, 42.5},
		"one of two without":      {src("ok", rd(f(40), 0), rd(nil, 0)), true, 40},
		"one of two readings old": {src("ok", rd(f(40), 0), rd(f(2.5), time.Hour)), false, 0},
	} {
		got, ok := daySample(c.st, now)
		if ok != c.ok || (ok && got.W != c.watts) {
			t.Errorf("%s: sample %v (%v W), want %v (%v W)", name, ok, got.W, c.ok, c.watts)
		}
	}
	// volts, amperes and the counter of a source with two readings (two outlets of one strip)
	two := src("ok",
		model.PowerReading{Watts: f(40), Voltage: f(246.5), Current: f(0.17), EnergyKWhTotal: f(3), Timestamp: now},
		model.PowerReading{Watts: f(10), Voltage: f(246.9), Current: f(0.04), EnergyKWhTotal: f(1.5), Timestamp: now})
	if s, ok := daySample(two, now); !ok || s.W != 50 || *s.V != 246.5 || !near(*s.A, 0.21, 1e-9) || *s.KWh != 4.5 {
		t.Errorf("two readings: %+v", s)
	}
	two.Readings[1].EnergyKWhTotal = nil // one outlet has no counter: a sum of one counter is not the strip's
	if s, _ := daySample(two, now); s.KWh != nil {
		t.Errorf("a counter that only one of two readings has: %v", *s.KWh)
	}

	// through the manager, with the real states of a relayed device
	pm, err := NewManager(nil, 5, time.Second, 0.05, 3600, nil, func() float64 { return 0 }) // offline_after: 50 ms
	if err != nil {
		t.Fatal(err)
	}
	good := map[string]any{"id": "plug-a", "name": "Plug A", "type": "homeassistant", "ok": true, "watts": 40.0, "voltage": 246.5, "current": 0.085, "energy_kwh_total": 0.004}
	bad := map[string]any{"id": "plug-a", "name": "Plug A", "type": "homeassistant", "ok": false, "error": refusedWords}
	samples := func() int {
		pm.Day.mu.Lock()
		defer pm.Day.mu.Unlock()
		dv := pm.Day.dev["pc-a/plug-a"]
		if dv == nil {
			return 0
		}
		n := 0
		for _, b := range dv.b {
			n += int(b.n)
		}
		return n
	}
	pm.UpdateRemote("pc-a", []map[string]any{good}, false, nil, nil)
	pm.SampleHistory()
	if samples() != 1 {
		t.Fatalf("a good reading gave %d samples", samples())
	}
	s := seriesOf(t, pm.Day, "pc-a/plug-a", time.Now())
	if last := len(s.Avg) - 1; *s.Avg[last] != 40 || *s.Volts[last] != 246.5 || *s.Amps[last] != 0.085 {
		t.Errorf("the sample: %v W, %v V, %v A", *s.Avg[last], *s.Volts[last], *s.Amps[last])
	}
	pm.UpdateRemote("pc-a", []map[string]any{bad}, false, nil, nil) // the device fails: stale, it still shows 40 W
	if st := byID(pm.Overview())["pc-a/plug-a"]; st.Status != "stale" || len(st.Readings) != 1 {
		t.Fatalf("the device right after it failed: %+v", st)
	}
	pm.SampleHistory()
	pm.UpdateRemote("pc-a", nil, true, nil, nil) // the agent cannot be reached
	pm.SampleHistory()
	time.Sleep(80 * time.Millisecond) // longer than offline_after: unreachable, no reading
	pm.UpdateRemote("pc-a", nil, true, nil, nil)
	pm.SampleHistory()
	if samples() != 1 {
		t.Errorf("a device that fails gave samples: %d in all, want the 1 from before", samples())
	}
	if n := len(pm.HistoryView("").Points); n != 4 {
		t.Errorf("the hour's history has %d points, want 4 (it is not changed by this)", n)
	}
	pm.UpdateRemote("pc-a", []map[string]any{good}, false, nil, nil)
	pm.SampleHistory()
	if samples() != 2 {
		t.Errorf("a device that answers again gave %d samples in all, want 2", samples())
	}
}

// A configured source (a plug the panel reads itself) goes the same way, through PollAll.
func TestDaySamplesAConfiguredSource(t *testing.T) {
	powerSourceForTest(t, "daytest", 55.5)
	pm := manager(t, []map[string]any{{"id": "plug-a", "type": "daytest"}}, nil, nil)
	pm.PollAll(context.Background())
	s := seriesOf(t, pm.Day, "plug-a", time.Now())
	if len(s.Avg) == 0 || *s.Avg[len(s.Avg)-1] != 55.5 || s.Summary.Energy != "estimated" {
		t.Fatalf("a configured source's day: %+v", s)
	}
}

// fixedSource is a power source for tests: it reads the same watts every time.
type fixedSource struct {
	powersrc.Defaults
	b powersrc.Base
	w float64
}

func (s *fixedSource) Info() powersrc.Base { return s.b }

func (s *fixedSource) Read(context.Context, time.Duration) ([]model.PowerReading, error) {
	w := s.w
	return []model.PowerReading{powersrc.NewReading(s.b, devices.Reading{Watts: &w})}, nil
}

// powerSourceForTest registers the power source type typ, which reads watts.
func powerSourceForTest(t *testing.T, typ string, watts float64) {
	t.Helper()
	powersrc.Extra[typ] = func(raw map[string]any) (powersrc.Source, error) {
		b, err := powersrc.NewBase(raw)
		if err != nil {
			return nil, err
		}
		return &fixedSource{b: b, w: watts}, nil
	}
	t.Cleanup(func() { delete(powersrc.Extra, typ) })
}

// Nothing writes the file behind a caller's back before a period has passed. A panel that runs for less than
// five minutes (every test that makes a poller) starts no write that could still be going when it ends: such a
// write put a file into a test's folder while the folder was being removed (seen once in a full run, in a test
// of Edit host's Remove; Forget used to ask for a write with the next tick).
func TestDayFileIsNotWrittenBehindACallersBackWithinAPeriod(t *testing.T) {
	path := dayPath(t)
	d := loaded(path, at(0, 0, 0))
	var started atomic.Int32
	real := d.writeFile
	d.writeFile = func(p string, b []byte, m os.FileMode) error { started.Add(1); return real(p, b, m) }
	for i := 0; i < 59; i++ { // 4 min 55 s of ticks, with a host removed in the middle
		now := at(0, 0, i*5)
		d.Record(now, []DaySample{{ID: "pc-a/plug-a", W: 1}}, nil)
		if i < 30 {
			d.Record(now, []DaySample{{ID: "pc-b/plug-b", W: 2}}, nil)
		}
		if i == 30 && !d.Forget("pc-b/") {
			t.Fatal("nothing was forgotten")
		}
		d.SaveIfDue(now)
	}
	time.Sleep(100 * time.Millisecond)
	if started.Load() != 0 {
		t.Fatalf("%d writes were started within the first period", started.Load())
	}
	if _, err := os.Lstat(path); err == nil {
		t.Fatal("the file was written")
	}
	// what was forgotten is written when the caller says so, and is done when Flush returns
	if !d.Flush(at(0, 4, 56), 10*time.Second) || started.Load() != 1 {
		t.Fatalf("Flush: %d writes", started.Load())
	}
	if e := loaded(path, at(0, 5, 0)); e.Has("pc-b/plug-b", at(0, 5, 0)) || !e.Has("pc-a/plug-a", at(0, 5, 0)) {
		t.Error("the file after the Flush still has the forgotten host's source, or lacks the other")
	}
}
