package adminstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
)

// "Run timings": the overlay of the timings of "Update unpowered hosts" in state.json ("run_timings").

func readTimings(t *testing.T, dir string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	d := map[string]any{}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("state.json is not JSON: %v\n%s", err, b)
	}
	o, _ := d[runTimingsKey].(map[string]any)
	return o
}

// A decision is stored as one number under its key, with who made it and when; a decision that is taken out
// leaves nothing; a write that changes nothing is no write; the counter only grows.
func TestRunTimingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)
	if o, ignored := a.RunTimingsRead(); len(o.Values) != 0 || o.Rev != 0 || ignored != nil {
		t.Fatalf("no file: %+v %v", o, ignored)
	}
	after, changed, err := a.EditRunTimings("nora", func(o *RunTimings) error {
		o.Set(config.KeyOffAfter, 360)
		o.Set(config.KeyShutdownDelay, 30)
		return nil
	})
	if err != nil || !changed || after.Rev != 1 || after.Values[config.KeyOffAfter] != 360 || after.Values[config.KeyShutdownDelay] != 30 || len(after.Values) != 2 {
		t.Fatalf("two decisions: %+v %v %v", after, changed, err)
	}
	if m := after.Marks[config.KeyOffAfter]; m.By != "nora" || time.Since(m.At) > time.Minute || m.At.Location() != time.UTC {
		t.Fatalf("who and when: %+v", m)
	}
	raw := readTimings(t, dir)
	if got, _ := json.Marshal(raw["values"]); string(got) != `{"off_after_s":360,"shutdown_delay_s":30}` || raw["rev"] != 1.0 {
		t.Fatalf("the file: %v", raw)
	}
	if by := raw["by"].(map[string]any)["off_after_s"].(map[string]any); by["by"] != "nora" || by["at"] == "" {
		t.Fatalf("the file's by: %v", raw["by"])
	}
	// another panel process (and the CLI) reads the same
	if o := New(dir).RunTimings(); o.Rev != 1 || o.Values[config.KeyOffAfter] != 360 {
		t.Fatalf("read again: %+v", o)
	}
	if v, ok := New(dir).RunTimings().Value(config.KeyOffAfter); !ok || v != 360 {
		t.Fatalf("Value: %d %v", v, ok)
	}
	if _, ok := New(dir).RunTimings().Value(config.KeySettle); ok {
		t.Fatal("a key nobody decided has a value")
	}
	if o, ignored, err := ReadRunTimings(dir); err != nil || ignored != nil || o.Values[config.KeyShutdownDelay] != 30 {
		t.Fatalf("ReadRunTimings: %+v %v %v", o, ignored, err)
	}
	// the same again: nothing is written, the counter and the marks stay
	stat0, _ := os.Stat(filepath.Join(dir, "state.json"))
	same, changed, err := a.EditRunTimings("omar", func(o *RunTimings) error { o.Set(config.KeyOffAfter, 360); return nil })
	stat1, _ := os.Stat(filepath.Join(dir, "state.json"))
	if err != nil || changed || same.Rev != 1 || same.Marks[config.KeyOffAfter].By != "nora" || !stat0.ModTime().Equal(stat1.ModTime()) {
		t.Fatalf("the same value again: %+v %v %v", same, changed, err)
	}
	// one key changed by somebody else: only its mark moves
	after, changed, err = a.EditRunTimings("omar", func(o *RunTimings) error { o.Set(config.KeyShutdownDelay, 45); return nil })
	if err != nil || !changed || after.Rev != 2 || after.Marks[config.KeyShutdownDelay].By != "omar" || after.Marks[config.KeyOffAfter].By != "nora" {
		t.Fatalf("one key changed: %+v", after)
	}
	// Reset: the decision and its mark go; the last one leaves an empty object and a counter
	after, changed, err = a.EditRunTimings("omar", func(o *RunTimings) error {
		if !o.Drop(config.KeyOffAfter) || o.Drop(config.KeySettle) {
			return errors.New("Drop's answer is wrong")
		}
		return nil
	})
	if err != nil || !changed || after.Rev != 3 || len(after.Values) != 1 || len(after.Marks) != 1 {
		t.Fatalf("a reset: %+v %v", after, err)
	}
	if _, _, err = a.EditRunTimings("omar", func(o *RunTimings) error { o.Drop(config.KeyShutdownDelay); return nil }); err != nil {
		t.Fatal(err)
	}
	raw = readTimings(t, dir)
	if got, _ := json.Marshal(raw); string(got) != `{"by":{},"rev":4,"values":{}}` {
		t.Fatalf("after every reset: %s", got)
	}
	// fn's error stops the write and comes back as it is
	stop := errors.New("no")
	if _, _, err := a.EditRunTimings("omar", func(o *RunTimings) error { o.Set(config.KeyOffAfter, 300); return stop }); !errors.Is(err, stop) {
		t.Fatalf("fn's error: %v", err)
	}
	if o := a.RunTimings(); len(o.Values) != 0 || o.Rev != 4 {
		t.Fatalf("after a stopped write: %+v", o)
	}
	// other keys of state.json are not touched by a write here, and a write there does not touch this one
	if err := a.SetWakeUpdate("pc-a", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.EditRunTimings("nora", func(o *RunTimings) error { o.Set(config.KeySettle, 90); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := a.SetWakeUpdate("pc-b", true); err != nil {
		t.Fatal(err)
	}
	if o := a.RunTimings(); o.Values[config.KeySettle] != 90 || !a.WakeUpdateEnabled("pc-a") || !a.WakeUpdateEnabled("pc-b") {
		t.Fatalf("next to the other keys: %+v", o)
	}
}

// What is stored is always a value its key can have: the writer refuses anything else (the API checks before
// it, this is the second line), for every key at both ends of its range.
func TestRunTimingsWriterStoresOnlyValuesInRange(t *testing.T) {
	a := New(t.TempDir())
	for _, tm := range config.RunTimings() {
		for _, v := range []int{tm.Min - 1, tm.Max + 1, 0, -1} {
			if _, changed, err := a.EditRunTimings("nora", func(o *RunTimings) error { o.Set(tm.Key, v); return nil }); err == nil || changed {
				t.Errorf("%s = %d was stored", tm.Key, v)
			}
		}
		for _, v := range []int{tm.Min, tm.Max} {
			if after, _, err := a.EditRunTimings("nora", func(o *RunTimings) error { o.Set(tm.Key, v); return nil }); err != nil || after.Values[tm.Key] != v {
				t.Errorf("%s = %d: %v", tm.Key, v, err)
			}
		}
	}
	if _, _, err := a.EditRunTimings("nora", func(o *RunTimings) error { o.Set("url", 300); return nil }); err == nil {
		t.Error("a key that is no timing was stored")
	}
	if o := a.RunTimings(); len(o.Values) != len(config.RunTimings()) {
		t.Fatalf("stored: %+v", o.Values)
	}
}

// The reader never fails and never lets a value into force that config.yaml would refuse: whatever is not a
// whole number in its key's range is left out (config.yaml's value counts), with a line that names the key.
func TestRunTimingsReaderIsTolerant(t *testing.T) {
	for _, c := range []struct {
		name, state string
		values      string // what counts, as "key=value ..." sorted
		rev         int
		ignored     []string // a piece of each line
	}{
		{"nothing", `{}`, "", 0, nil},
		{"what Fairy writes", `{"run_timings": {"rev": 2, "values": {"off_after_s": 360, "run_approval_wait_s": 900}, "by": {"off_after_s": {"by": "nora", "at": "2026-11-14T10:00:00Z"}}}}`,
			"off_after_s=360 run_approval_wait_s=900", 2, nil},
		{"not an object", `{"run_timings": [1, 2]}`, "", 0, []string{`"run_timings" is not the object Fairy writes (found a list)`}},
		{"null", `{"run_timings": null}`, "", 0, []string{"found null"}},
		{"values not an object", `{"run_timings": {"rev": 1, "values": 360}}`, "", 1, []string{`"values" is not an object (found a number)`}},
		{"below the floor", `{"run_timings": {"values": {"off_after_s": 60, "shutdown_delay_s": 5, "settle_s": 90}}}`, "settle_s=90", 0,
			[]string{"off_after_s is not a whole number in 300-3600 (found 60): config.yaml's value counts", "shutdown_delay_s is not a whole number in 10-3600 (found 5)"}},
		{"over the top", `{"run_timings": {"values": {"off_after_s": 3601, "agent_requests_per_min": 31, "online_timeout_s": 1801, "run_approval_wait_s": 3601, "settle_s": 601}}}`, "", 0,
			[]string{"agent_requests_per_min", "off_after_s", "online_timeout_s", "run_approval_wait_s", "settle_s"}},
		{"zero and negative", `{"run_timings": {"values": {"off_after_s": 0, "settle_s": -60}}}`, "", 0, []string{"found 0", "found -60"}},
		{"a fraction, a text, a bool, a list", `{"run_timings": {"values": {"off_after_s": 360.5, "settle_s": "60", "shutdown_delay_s": true, "online_timeout_s": [300]}}}`, "", 0,
			[]string{"off_after_s is not a whole number in 300-3600 (found 360.5)", "online_timeout_s", "found a text", "found true / false"}},
		{"a huge number", `{"run_timings": {"values": {"off_after_s": 1e300}}}`, "", 0, []string{"off_after_s is not a whole number in 300-3600 (found a number)"}},
		{"a key that is no timing", `{"run_timings": {"values": {"url": 300, "off_after_s": 600}, "token_env": "X"}}`, "off_after_s=600", 0,
			[]string{`the key "token_env" is not one Fairy writes: it is not used`, `"url" is no timing this version knows: it is not used`}},
		{"a bad counter", `{"run_timings": {"rev": "7", "values": {"settle_s": 20}}}`, "settle_s=20", 0, []string{`"rev" is not a whole number`}},
		{"too many values", `{"run_timings": {"values": {` + manyValues(65) + `}}}`, "", 0, []string{"far more than there are timings"}},
		{"by is not an object", `{"run_timings": {"values": {"settle_s": 20}, "by": 7}}`, "settle_s=20", 0, nil},
	} {
		dir := t.TempDir()
		writeState(t, dir, c.state)
		o, ignored := New(dir).RunTimingsRead()
		var got []string
		for _, tm := range config.RunTimings() {
			if v, ok := o.Value(tm.Key); ok {
				got = append(got, fmt.Sprintf("%s=%d", tm.Key, v))
				if !tm.In(v) {
					t.Errorf("%s: %s = %d is in force and out of range", c.name, tm.Key, v)
				}
			}
		}
		// in the list's order; the cases name them sorted by key
		if strings.Join(sortedStrings(got), " ") != c.values || len(o.Values) != len(got) || o.Rev != c.rev {
			t.Errorf("%s: values %v (rev %d), want %q (rev %d)", c.name, got, o.Rev, c.values, c.rev)
		}
		if len(ignored) != len(c.ignored) || o.Ignored != len(c.ignored) {
			t.Errorf("%s: ignored %q, want %d lines", c.name, ignored, len(c.ignored))
			continue
		}
		all := strings.Join(ignored, "\n")
		for _, piece := range c.ignored {
			if !strings.Contains(all, piece) {
				t.Errorf("%s: no line with %q in\n%s", c.name, piece, all)
			}
		}
	}
}

func manyValues(n int) string {
	var parts []string
	for i := 0; i < n; i++ {
		parts = append(parts, fmt.Sprintf(`"k%d": 300`, i))
	}
	return strings.Join(parts, ", ")
}

func sortedStrings(l []string) []string {
	out := append([]string(nil), l...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// What is ignored is said once in the log, however often the panel asks, and never with a value that is a
// text of the file; the next write drops what is broken and keeps what is under a key this build does not
// know (a newer build wrote it), also through a Reset of everything this build knows.
func TestRunTimingsIgnoredIsSaidOnceAndKeysOfANewerBuildSurvive(t *testing.T) {
	var logs bytes.Buffer
	var mu sync.Mutex
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return logs.Write(p) }), nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	warns := func() int {
		mu.Lock()
		defer mu.Unlock()
		return strings.Count(logs.String(), "level=WARN")
	}
	dir := t.TempDir()
	writeState(t, dir, `{"wake_update_hosts": ["pc-a"], "run_timings": {"rev": 5, "later": {"x": 1},
		"values": {"off_after_s": 120, "settle_s": "secret-looking text", "shutdown_delay_s": 30, "wake_gap_s": 15},
		"by": {"shutdown_delay_s": {"by": "nora", "at": "2026-11-14T10:00:00Z"}, "wake_gap_s": {"by": "omar"}}}}`)
	a := New(dir)
	for i := 0; i < 40; i++ {
		o, ignored := a.RunTimingsRead()
		if len(ignored) != 4 || len(o.Values) != 1 || o.Values[config.KeyShutdownDelay] != 30 || o.Marks[config.KeyShutdownDelay].By != "nora" || o.Rev != 5 {
			t.Fatalf("read: %+v %q", o, ignored)
		}
		_ = a.RunTimings()
	}
	if n := warns(); n != 4 {
		t.Fatalf("%d WARN lines for four ignored things asked 80 times, want 4:\n%s", n, logs.String())
	}
	mu.Lock()
	said := logs.String()
	mu.Unlock()
	if !strings.Contains(said, "a run timing changed in the panel is ignored") || !strings.Contains(said, "state.json") || strings.Contains(said, "secret-looking") {
		t.Fatalf("the WARN lines: %s", said)
	}
	if _, _, err := a.EditRunTimings("nora", func(o *RunTimings) error { o.Set(config.KeyOnlineTimeout, 600); return nil }); err != nil {
		t.Fatal(err)
	}
	raw := readTimings(t, dir)
	values, by := raw["values"].(map[string]any), raw["by"].(map[string]any)
	if values["wake_gap_s"] != 15.0 || values["shutdown_delay_s"] != 30.0 || values["online_timeout_s"] != 600.0 || len(values) != 3 || raw["rev"] != 6.0 ||
		fmt.Sprint(raw["later"]) != "map[x:1]" || fmt.Sprint(by["wake_gap_s"]) != "map[by:omar]" || by["shutdown_delay_s"].(map[string]any)["by"] != "nora" {
		t.Fatalf("after a write: %v", raw)
	}
	// every decision this build knows is reset: the newer build's stay
	if _, _, err := a.EditRunTimings("nora", func(o *RunTimings) error {
		for _, tm := range config.RunTimings() {
			o.Drop(tm.Key)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw = readTimings(t, dir)
	if got, _ := json.Marshal(raw); string(got) != `{"by":{"wake_gap_s":{"by":"omar"}},"later":{"x":1},"rev":7,"values":{"wake_gap_s":15}}` {
		t.Fatalf("after a reset of everything: %s", got)
	}
	if !a.WakeUpdateEnabled("pc-a") {
		t.Fatal("another key of state.json was lost")
	}
	for i := 0; i < 3; i++ {
		_ = a.SetWakeUpdate(fmt.Sprintf("pc-%d", i), true)
		_ = a.RunTimings()
	}
	if n := warns(); n != 4 {
		t.Fatalf("%d WARN lines after more writes, want still 4 (what stays in the file is said once):\n%s", n, logs.String())
	}
	// unreadable: no decisions (config.yaml counts), no error for the reader; a write fails; the CLI's reader says so
	writeState(t, dir, `{"run_timings": {"values": {"off_after_s": 360}}`) // cut off
	b := New(dir)
	if o, ignored := b.RunTimingsRead(); len(o.Values) != 0 || ignored != nil {
		t.Fatalf("a state.json that is not JSON: %+v %v", o, ignored)
	}
	if _, _, err := b.EditRunTimings("nora", func(o *RunTimings) error { o.Set(config.KeySettle, 20); return nil }); err == nil {
		t.Fatal("a write to a state.json that is not JSON did not fail")
	}
	if _, _, err := ReadRunTimings(dir); err == nil {
		t.Fatal("ReadRunTimings of a state.json that is not JSON gave no error")
	}
	if o, ignored, err := ReadRunTimings(t.TempDir()); err != nil || len(o.Values) != 0 || ignored != nil {
		t.Fatalf("ReadRunTimings of a folder without state.json: %+v %v %v", o, ignored, err)
	}
}

// A panel from before "Run timings" (b16 without it, build 15 and older) reads state.json as one object and
// writes it back as the object it read: it uses nothing of "run_timings" and loses nothing of it. Everything
// such a panel reads out of the file is the same with the key and without it, and its own writes keep the key
// byte for byte. (The same was run with b16's own code: TIMINGS-DESIGN.md, section 9.)
func TestAnOlderPanelLeavesRunTimingsAlone(t *testing.T) {
	const rest = `"wake_update_hosts": ["pc-a"], "host_models": {"pc-a": "Tower"}, "poll_overrides": {"pc-a": 5},
		"host_options": {"rev": 2, "agents": {"nas": {"ups_powers": {"rack": {"pc-a": true}}}}}`
	const mine = `"run_timings": {"rev": 3, "values": {"off_after_s": 360, "shutdown_delay_s": 30, "run_approval_wait_s": 900}, "by": {"off_after_s": {"by": "nora", "at": "2026-11-14T10:00:00Z"}}}`
	with, without := t.TempDir(), t.TempDir()
	writeState(t, with, `{`+rest+`, `+mine+`}`)
	writeState(t, without, `{`+rest+`}`)
	a, b := New(with), New(without)
	// what the older panel reads: every reader that existed before this key
	if a.ControlDisabled() != nil || b.ControlDisabled() != nil || a.PowerDisabled("pc-a") || !a.WakeUpdateEnabled("pc-a") || a.WakeUpdateEnabled("pc-b") {
		t.Fatal("the switches read differently next to run_timings")
	}
	oa, ia := a.HostOptionsRead()
	ob, ib := b.HostOptionsRead()
	if len(ia) != 0 || len(ib) != 0 || oa.Rev != ob.Rev || !oa.For("nas").Same(ob.For("nas")) || !oa.For("nas").UPS["rack"]["pc-a"] {
		t.Fatalf("Edit host next to run_timings: %+v %q / %+v %q", oa, ia, ob, ib)
	}
	if statefile.Check(filepath.Join(with, "state.json")) != nil {
		t.Fatal("the file with run_timings does not pass the check of the panel's start")
	}
	// what the older panel writes: each of its writers, on the file with the key
	before, _ := json.Marshal(readTimings(t, with))
	if err := a.SetWakeUpdate("pc-b", true); err != nil {
		t.Fatal(err)
	}
	if err := a.SetPoll("pc-a", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.SetPowerDisabled("pc-a", true); err != nil {
		t.Fatal(err)
	}
	if err := a.SetAgentVersion("pc-a", "19-9126ac4", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.EditHostOptions("nora", func(o *HostOptions) error {
		e := o.Agents["nas"]
		e.SetExclude("corner", true)
		o.Agents["nas"] = e
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.SetControlDisabled(true, "nora", "test"); err != nil {
		t.Fatal(err)
	}
	if after, _ := json.Marshal(readTimings(t, with)); string(after) != string(before) || !strings.Contains(string(after), `"off_after_s":360`) {
		t.Fatalf("run_timings after the older writers:\n%s\nbefore:\n%s", after, before)
	}
	if o := New(with).RunTimings(); o.Values[config.KeyOffAfter] != 360 || o.Values[config.KeyRunApprovalWait] != 900 || o.Rev != 3 {
		t.Fatalf("read again by this build: %+v", o)
	}
}
