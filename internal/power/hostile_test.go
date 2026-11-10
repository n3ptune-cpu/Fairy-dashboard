package power

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	powersrc "github.com/n3ptune-cpu/Fairy-agent/pkg/power"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// One hostile agent cannot make the panel keep an arbitrary number of power sources (M1): 100,000 relayed rows
// leave 16, ids must look like config ids, texts are cut, and the history (which copies every source into each
// sample) stays bounded. What was dropped is logged once, not on every poll.
func TestRelayedSourcesAreCappedPerAgent(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)

	pm := manager(t, nil, nil, nil)
	long := strings.Repeat("N", 1<<16)
	flood := func(round int) []map[string]any {
		rows := make([]map[string]any, 0, 100_000)
		for i := range 100_000 {
			rows = append(rows, map[string]any{"id": fmt.Sprintf("p%d-%d", round, i), "name": long, "type": long, "ok": true, "watts": 1.0})
		}
		return rows
	}
	for round := range 3 { // new ids on every poll: the old ones go, never more than 16 stay
		pm.UpdateRemote("evil", flood(round), false, nil, nil)
		pm.SampleHistory()
		ov := pm.Overview()
		if len(ov.Sources) != MaxRemoteSources {
			t.Fatalf("round %d: %d sources kept", round, len(ov.Sources))
		}
		for _, s := range ov.Sources {
			if len(s.Name) > maxRemoteName || len(s.Type) > maxRemoteType || !strings.HasPrefix(s.ID, fmt.Sprintf("evil/p%d-", round)) {
				t.Fatalf("%+v", s)
			}
		}
	}
	pm.mu.Lock()
	keys, states := len(pm.remoteKeys), len(pm.remote)
	pm.mu.Unlock()
	if keys != MaxRemoteSources || states != MaxRemoteSources {
		t.Fatal("kept", keys, states)
	}
	h := pm.HistoryView("")
	if len(h.Points) != 3 {
		t.Fatal("points", len(h.Points))
	}
	for _, p := range h.Points {
		if len(p.Sources) > MaxRemoteSources {
			t.Fatalf("%d sources in one history sample", len(p.Sources))
		}
	}
	if b, err := json.Marshal(h); err != nil || len(b) > 16<<10 {
		t.Fatal("history", len(b), err)
	}
	if n := strings.Count(logs.String(), "were dropped"); n != 1 {
		t.Fatalf("dropped rows logged %d times:\n%s", n, logs.String())
	}

	// ids that could not come from a config file are refused; a well-behaved agent next to it is untouched
	pm.UpdateRemote("evil", []map[string]any{{"id": "../../etc", "ok": true}, {"id": "UPPER", "ok": true}, {"id": strings.Repeat("a", 64), "ok": true},
		{"id": "a b", "ok": true}, {"id": "fine-1", "ok": true, "watts": 5.0}, {"id": 7.0, "ok": true}, {"id": "", "ok": true}}, false, nil, nil)
	pm.UpdateRemote("laptop", []map[string]any{{"id": "desk", "name": "Desk plug", "type": "tuya", "ok": true, "watts": 80.0}}, false, nil, nil)
	ov := byID(pm.Overview())
	if len(ov) != 3 || ov["evil/fine-1"].Status != "ok" || ov["evil/7"].Status != "ok" || ov["laptop/desk"].Name != "Desk plug" || ov["laptop/desk"].Type != "tuya" {
		t.Fatalf("%+v", ov)
	}
	// the same id twice counts once, and the last row wins (as before)
	rows := []map[string]any{}
	for i := range 40 {
		rows = append(rows, map[string]any{"id": "same", "ok": true, "watts": float64(i)})
	}
	pm.UpdateRemote("laptop", rows, false, nil, nil)
	if d := byID(pm.Overview())["laptop/same"]; *d.Readings[0].Watts != 39 {
		t.Fatalf("%+v", d)
	}
	// still one log line while it lasts; once the agent behaved and then misbehaves again, that is logged again
	if n := strings.Count(logs.String(), "were dropped"); n != 1 {
		t.Fatalf("logged %d times", n)
	}
	pm.UpdateRemote("evil", []map[string]any{{"id": "fine-1", "ok": true, "watts": 5.0}}, false, nil, nil)
	pm.UpdateRemote("evil", flood(9), false, nil, nil)
	if n := strings.Count(logs.String(), "were dropped"); n != 2 {
		t.Fatalf("logged %d times", n)
	}
}

// fakeSource is a configured power source that answers whatever the test stores.
type fakeSource struct {
	id   string
	next atomic.Pointer[[]model.PowerReading]
}

func (f *fakeSource) Info() powersrc.Base      { return powersrc.Base{ID: f.id, Type: "fake", System: "pc"} }
func (f *fakeSource) DefaultInterval() float64 { return 2 }
func (f *fakeSource) FloorInterval() float64   { return 1 }
func (f *fakeSource) MaxBackoff() float64      { return 0 } // no waiting between the test's polls
func (f *fakeSource) Close()                   {}
func (f *fakeSource) Read(context.Context, time.Duration) ([]model.PowerReading, error) {
	return *f.next.Load(), nil
}

func (f *fakeSource) set(watts ...float64) {
	rs := []model.PowerReading{}
	for _, w := range watts {
		rs = append(rs, model.PowerReading{SourceID: f.id, Name: f.id, Watts: &w, Voltage: &w, Current: &w, EnergyKWhTotal: &w,
			SystemID: model.P("pc"), Timestamp: time.Now().UTC()})
	}
	f.next.Store(&rs)
}

// validJSON fails the test when v cannot be written as JSON.
func validJSON(t *testing.T, what string, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s cannot be written as JSON: %v", what, err)
	}
	return b
}

// Impossible numbers from a power source (M2): a plug or BMC the panel reads itself answering NaN, an infinity
// or 1e308 W. The power page, the history and the watts of the linked system stay real numbers, the source shows
// an error instead of a reading, and the history is whole again with the next good sample.
func TestHostilePowerReadings(t *testing.T) {
	src := &fakeSource{id: "plug"}
	good := &fakeSource{id: "other"}
	good.set(80)
	clock := func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	pm := manager(t, nil, nil, clock)
	pm.Runners = []*Runner{{Source: src, Interval: 2, offlineAfter: 60, clock: clock}, {Source: good, Interval: 2, offlineAfter: 60, clock: clock}}
	ctx := context.Background()
	check := func(what string) model.PowerOverview {
		t.Helper()
		ov := pm.Overview()
		validJSON(t, what+": overview", ov)
		validJSON(t, what+": history", pm.HistoryView(""))
		validJSON(t, what+": history of the source", pm.HistoryView("plug"))
		validJSON(t, what+": linked watts", pm.LinkedWatts())
		return ov
	}
	src.set(120)
	pm.PollAll(ctx)
	if ov := check("good"); *ov.TotalW != 200 || pm.LinkedWatts()["pc"] != 200 {
		t.Fatalf("%+v", ov)
	}
	for _, bad := range [][]float64{{math.NaN()}, {math.Inf(1)}, {math.Inf(-1)}, {1e308, 1e308}, {1e308}, {-1e308}, {2e9}} {
		src.set(bad...)
		pm.PollAll(ctx)
		ov := check(fmt.Sprint(bad))
		st := byID(ov)["plug"]
		if st.Status == "ok" || st.Error == nil || !strings.Contains(*st.Error, "not a possible reading") {
			t.Fatalf("%v: %+v", bad, st)
		}
		// while it fails the last good reading stays (stale), as for any failing source; nothing impossible is added
		if *ov.TotalW != 200 || *st.Readings[0].Watts != 120 {
			t.Fatalf("%v: total %v", bad, *ov.TotalW)
		}
	}
	// a reading whose watts are fine keeps them; its other impossible values are dropped
	w, nan := 150.0, math.NaN()
	rs := []model.PowerReading{{SourceID: "plug", Name: "plug", Watts: &w, Voltage: &nan, Current: model.P(math.Inf(1)), EnergyKWhTotal: model.P(1e300),
		Timestamp: time.Date(20000, 1, 1, 0, 0, 0, 0, time.UTC)}}
	src.next.Store(&rs)
	pm.PollAll(ctx)
	st := byID(check("partly bad"))["plug"]
	if rd := st.Readings[0]; st.Status != "ok" || *rd.Watts != 150 || rd.Voltage != nil || rd.Current != nil || rd.EnergyKWhTotal != nil || rd.Timestamp.Year() > 9999 {
		t.Fatalf("%+v", st)
	}
	// back to normal: the next sample is a normal one, and no sample in the history is broken
	src.set(100)
	pm.PollAll(ctx)
	ov := check("recovered")
	h := pm.HistoryView("")
	last := h.Points[len(h.Points)-1]
	if byID(ov)["plug"].Status != "ok" || *ov.TotalW != 180 || *last.TotalW != 180 || *last.Sources["plug"] != 100 {
		t.Fatalf("%+v", last)
	}
	for _, p := range h.Points {
		for id, v := range p.Sources {
			if v != nil && (math.IsNaN(*v) || math.IsInf(*v, 0) || math.Abs(*v) > 1e6) {
				t.Fatalf("history holds %v for %s", *v, id)
			}
		}
	}
}

// The sums are checked again after adding (M2, rerunB-03): numbers that are each possible must not add up to one
// that is not, wherever they came from.
func TestPowerSumsAreCheckedAfterAdding(t *testing.T) {
	big := math.MaxFloat64
	rd := func(w float64) model.PowerReading { return model.PowerReading{Watts: &w, SystemID: model.P("pc")} }
	states := []model.PowerSourceState{{ID: "a", Status: "ok", IncludeInTotal: true, Readings: []model.PowerReading{rd(big), rd(big)}},
		{ID: "b", Status: "ok", IncludeInTotal: true, Readings: []model.PowerReading{rd(big)}}}
	if tot := Total(states); tot != nil {
		t.Fatalf("total %v", *tot)
	}
	if s := sumW(states[0].Readings); s != nil {
		t.Fatalf("sum %v", *s)
	}
	if s := sumW([]model.PowerReading{rd(40), rd(40.5)}); *s != 80.5 {
		t.Fatal("normal sum")
	}
	if tot := Total([]model.PowerSourceState{{Status: "ok", IncludeInTotal: true, Readings: []model.PowerReading{rd(80.04), rd(20)}}}); *tot != 100 {
		t.Fatalf("normal total %v", *tot)
	}
}

// The same from an agent (M2): `{"power_sources":[{"id":"desk","ok":true,"watts":1e308}]}` and friends. The row
// becomes a failed row with a reason, the power page and the history stay valid, the next good row heals it.
func TestHostileRelayedPowerRows(t *testing.T) {
	pm := manager(t, nil, nil, nil)
	row := func(id string, w any) map[string]any {
		return map[string]any{"id": id, "name": "Plug " + id, "type": "tuya", "ok": true, "watts": w, "voltage": 1e308, "current": -1e308,
			"energy_kwh_total": 1e308, "system_hint": "self"}
	}
	pm.UpdateRemote("laptop", []map[string]any{row("desk", 80.0), row("tv", 20.0)}, false, nil, nil)
	pm.SampleHistory()
	pm.UpdateRemote("laptop", []map[string]any{row("desk", 1e308), row("tv", 1e308), row("new", -1e308), row("fine", 5.0), row("nowatts", nil)}, false, nil, nil)
	pm.SampleHistory()
	ov := pm.Overview()
	validJSON(t, "overview", ov)
	validJSON(t, "history", pm.HistoryView(""))
	by := byID(ov)
	for _, id := range []string{"laptop/desk", "laptop/tv", "laptop/new"} {
		st := by[id]
		if st.Status == "ok" || st.Error == nil || !strings.Contains(*st.Error, "not a possible reading") || st.Type != "tuya" || st.Name == "" {
			t.Fatalf("%s: %+v", id, st)
		}
	}
	if by["laptop/desk"].Status != "stale" || by["laptop/new"].Status != "unreachable" || len(by["laptop/new"].Readings) != 0 {
		t.Fatalf("%+v", by)
	}
	if st := by["laptop/fine"]; st.Status != "ok" || *st.Readings[0].Watts != 5 || st.Readings[0].Voltage != nil || st.Readings[0].Current != nil ||
		st.Readings[0].EnergyKWhTotal != nil {
		t.Fatalf("%+v", st)
	}
	if st := by["laptop/nowatts"]; st.Status != "ok" || st.Readings[0].Watts != nil { // a source without watts is not an error
		t.Fatalf("%+v", st)
	}
	if *ov.TotalW != 105 || pm.LinkedWatts()["laptop"] != 105 { // the stale readings (80 + 20) and the good row
		t.Fatalf("total %v linked %v", *ov.TotalW, pm.LinkedWatts())
	}
	pm.UpdateRemote("laptop", []map[string]any{row("desk", 81.0), row("tv", 19.0)}, false, nil, nil)
	pm.SampleHistory()
	h := pm.HistoryView("")
	if last := h.Points[len(h.Points)-1]; *last.TotalW != 100 || *last.Sources["laptop/desk"] != 81 {
		t.Fatalf("%+v", last)
	}
}

// The audit's plug: a Tasmota device answering Power [1e308, 1e308]. Each number is possible, the sum (made in
// the shared device code) is not. Read by the panel itself, through the real source code.
//
// What the panel is handed depends on the agent module it is built with (the device code is shared):
//   - up to v0.5.3 the sum arrives as an infinity, and the panel makes the source a failed one with the reason;
//   - after it, the shared code leaves a sum that cannot be a reading out (watts: null) and keeps the row, so
//     the source is "ok" without watts, like any source that reports no watts.
//
// Either way the impossible number is in no total, no linked watts and no history point, and every answer is
// valid JSON. (The first case can go once go.mod requires the newer module.)
func TestHostileTasmotaPlug(t *testing.T) {
	var body atomic.Value
	body.Store(`{"StatusSNS":{"ENERGY":{"Power":[1e308,1e308],"Voltage":230,"Current":0.5,"Total":12}}}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body.Load().(string))) }))
	defer srv.Close()
	pm := manager(t, []map[string]any{{"id": "plug", "type": "tasmota", "url": srv.URL, "system": "pc"}}, nil, nil)
	pm.PollAll(context.Background())
	ov := pm.Overview()
	validJSON(t, "overview", ov)
	validJSON(t, "history", pm.HistoryView(""))
	st := ov.Sources[0]
	failed := st.Status != "ok" && st.Error != nil
	noWatts := st.Status == "ok" && st.Error == nil && len(st.Readings) == 1 && st.Readings[0].Watts == nil
	if failed == noWatts || ov.TotalW != nil || len(pm.LinkedWatts()) != 0 {
		t.Fatalf("%+v", st)
	}
	for _, r := range st.Readings { // whatever else the plug said is shown only if it is a possible reading
		for _, v := range []*float64{r.Watts, r.Voltage, r.Current, r.EnergyKWhTotal} {
			if v != nil && (math.IsNaN(*v) || math.Abs(*v) > 1e9) {
				t.Fatalf("%+v", r)
			}
		}
	}
	// the device is normal again: so is its source (after its back-off) and the history
	body.Store(`{"StatusSNS":{"ENERGY":{"Power":[10,15],"Voltage":230,"Current":0.1,"Total":12}}}`)
	pm.Runners[0].mu.Lock()
	pm.Runners[0].nextAttempt = 0
	pm.Runners[0].mu.Unlock()
	pm.PollAll(context.Background())
	ov = pm.Overview()
	h := pm.HistoryView("")
	validJSON(t, "history", h)
	if st := ov.Sources[0]; st.Status != "ok" || *ov.TotalW != 25 || pm.LinkedWatts()["pc"] != 25 || *h.Points[len(h.Points)-1].TotalW != 25 ||
		h.Points[0].TotalW != nil {
		t.Fatalf("%+v", st)
	}
}
