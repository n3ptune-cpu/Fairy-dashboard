package web

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
)

// sseEvents splits what a stream wrote into event name -> data, and fails on any event whose data is not JSON.
func sseEvents(t *testing.T, raw []byte) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, block := range strings.Split(string(raw), "\n\n") {
		name, data := "", ""
		for _, line := range strings.Split(block, "\n") {
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				name = v
			} else if v, ok := strings.CutPrefix(line, "data: "); ok {
				data = v
			}
		}
		if name == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatalf("event %q is not valid JSON (%v): %.200q", name, err, data)
		}
		out[name] = m
	}
	return out
}

// M2, web layer: the copy of a system that the panel changes for one viewer (here the guest qemu-120, whose
// node the viewer may not see) is written on its own. If that copy cannot be written as JSON, the viewer gets
// that one system as its error entry: the list keeps its other systems, the stream's event stays valid JSON,
// and the entry does not bring back what was removed for this viewer.
func TestViewerCopyThatCannotBeWrittenBecomesAnErrorEntry(t *testing.T) {
	e := setupSites(t)
	u := person(subStraw)
	u.Auth = "session"
	c := &reqCtx{s: e.s, w: httptest.NewRecorder(), r: httptest.NewRequest("GET", "/api/stream", nil), user: &u}
	g := e.s.grantOf(c, &u)
	if g.SeesAll() {
		t.Fatal("the test needs a viewer with sites")
	}
	v := e.s.newView(g)
	good, order := v.SystemsJSON()
	if !slices.Contains(order, "qemu-120") || !slices.Contains(order, "cyrene") {
		t.Fatalf("visible: %v", order)
	}

	// the view's own copy of the snapshot gets a value JSON cannot hold (the poller's memory is not touched)
	v.snap = slices.Clone(v.snap)
	for i := range v.snap {
		if v.snap[i].ID == "qemu-120" {
			if v.snap[i].Parent == nil {
				t.Fatal("qemu-120 has no node: nothing would be cleaned")
			}
			v.snap[i].CPUPercent = model.P(math.NaN())
		}
	}
	got, order2 := v.SystemsJSON()
	if !slices.Equal(order, order2) {
		t.Fatalf("a system left the list: %v -> %v", order, order2)
	}
	for _, id := range order2 {
		if !json.Valid(got[id]) {
			t.Fatalf("%s: not valid JSON: %q", id, got[id])
		}
		if id != "qemu-120" && !bytes.Equal(got[id], good[id]) {
			t.Errorf("%s changed although only qemu-120 cannot be written", id)
		}
	}
	var entry map[string]any
	if err := json.Unmarshal(got["qemu-120"], &entry); err != nil {
		t.Fatal(err)
	}
	if entry["status"] != poller.StatusError || entry["cpu_percent"] != nil || entry["name"] != "truenas" ||
		!strings.Contains(entry["error"].(string), "cannot show") {
		t.Fatalf("qemu-120: %v", entry)
	}
	if entry["parent"] != nil { // the hidden node was removed for this viewer and must stay removed
		t.Fatalf("the error entry names the hidden node: %v", entry["parent"])
	}
	assertHidden(t, "straw", "error entry", string(got["qemu-120"]), hiddenFromStraw)

	// the live stream with that view: first frame and a delta, both valid JSON with every visible system
	st := &streamState{s: e.s, v: v, last: map[string][]byte{}, quiet: time.Now()}
	var out bytes.Buffer
	st.writeSystems(&out)
	ev := sseEvents(t, out.Bytes())
	list, _ := ev["systems"]["systems"].([]any)
	if len(list) != len(order) {
		t.Fatalf("stream: %d systems instead of %d: %s", len(list), len(order), out.Bytes())
	}
	assertHidden(t, "straw", "stream", out.String(), hiddenFromStraw)
	st.sent = good // as if the good state had been sent before: the next tick sends the changed system
	out.Reset()
	st.writeSystems(&out)
	changed, _ := sseEvents(t, out.Bytes())["systems_delta"]["changed"].([]any)
	if len(changed) != 1 || changed[0].(map[string]any)["status"] != poller.StatusError {
		t.Fatalf("delta: %s", out.Bytes())
	}
}

// M2, web layer: the `summary`, `ups` and `power` events, the `ups_event` and the REST answers built from the
// same types. One part that cannot be written is replaced by its stub; the rest of the payload is the same,
// and what is sent is always valid JSON.
func TestPayloadsWithOnePartThatCannotBeWritten(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	year33658 := time.Unix(1e12, 0)
	now := time.Now().UTC()

	t.Run("power", func(t *testing.T) {
		in := model.PowerOverview{GeneratedAt: now, TotalW: &inf, Sources: []model.PowerSourceState{
			{ID: "plug", Type: "tasmota", Name: "Desk plug", Status: "ok", IncludeInTotal: true, Readings: []model.PowerReading{}, NextAttemptInS: &nan},
			{ID: "pdu", Type: "shelly", Name: "Rack", Status: "ok", Readings: []model.PowerReading{}, LastSuccess: &now},
		}}
		b, written, ok := safeJSON("power", in)
		if !ok || !json.Valid(b) {
			t.Fatalf("%v %s", ok, b)
		}
		got := written.(model.PowerOverview)
		if got.TotalW != nil || len(got.Sources) != 2 {
			t.Fatalf("%+v", got)
		}
		if s := got.Sources[0]; s.ID != "plug" || s.Name != "Desk plug" || s.Status != "unreachable" || s.Error == nil || !s.IncludeInTotal {
			t.Fatalf("plug: %+v", s)
		}
		if s := got.Sources[1]; s.Status != "ok" || s.LastSuccess == nil {
			t.Fatalf("the good source changed: %+v", s)
		}
		if in.Sources[0].Status != "ok" || in.TotalW == nil {
			t.Fatal("the input was changed")
		}
	})

	t.Run("ups", func(t *testing.T) {
		in := model.UpsOverview{GeneratedAt: now, UPS: []model.UpsState{
			{ID: "ups1", Name: "APC", UpsName: "apc", Status: "online", Flags: []string{}, Powers: []string{"laptop"}, BatteryCharge: &nan},
			{ID: "ups2", Name: "Eaton", UpsName: "eaton", Status: "on_battery", Flags: []string{"OB"}, Powers: []string{}},
		}, History: map[string][]model.UpsPoint{"ups1": {{T: year33658}}, "ups2": {{T: now}}}}
		b, written, ok := safeJSON("ups", in)
		if !ok || !json.Valid(b) {
			t.Fatalf("%v %s", ok, b)
		}
		got := written.(model.UpsOverview)
		if u := got.UPS[0]; u.ID != "ups1" || u.Status != "unreachable" || !u.Stale || u.Error == nil || !slices.Equal(u.Powers, []string{"laptop"}) {
			t.Fatalf("ups1: %+v", u)
		}
		if u := got.UPS[1]; u.Status != "on_battery" || !slices.Equal(u.Flags, []string{"OB"}) {
			t.Fatalf("the good UPS changed: %+v", u)
		}
		if len(got.History["ups1"]) != 0 || len(got.History["ups2"]) != 1 {
			t.Fatalf("history: %+v", got.History)
		}
		if !strings.Contains(string(b), `"history":{"ups1":[],`) {
			t.Fatalf("%s", b)
		}
	})

	t.Run("summary", func(t *testing.T) {
		in := model.Summary{GeneratedAt: now, ControlDisabled: &model.ControlState{By: "n", At: year33658, Reason: "r"},
			Clusters: []model.ClusterTotals{{ID: "a", Name: "A", Cores: 8, CPUPercent: &nan}, {ID: "b", Name: "B", Cores: 4, CPUPercent: model.P(12.5)}},
			Totals:   model.HostTotals{Hosts: 3, HostsOnline: 2, CPUPercent: &inf},
			Counts:   model.Counts{Total: 5, Online: 4}, ByKind: map[string]int{"agent": 2},
			Sources: []model.SourceStatus{{Name: "pve1", OK: true, LatencyMS: &nan}, {Name: "laptop", OK: true, LatencyMS: model.P(3.0)}}}
		in.Cluster = &in.Clusters[0]
		b, written, ok := safeJSON("summary", in)
		if !ok || !json.Valid(b) {
			t.Fatalf("%v %s", ok, b)
		}
		got := written.(model.Summary)
		if got.ControlDisabled == nil || got.ControlDisabled.By != "n" || got.Cluster.CPUPercent != nil || got.Cluster.Cores != 8 ||
			got.Clusters[0].CPUPercent != nil || *got.Clusters[1].CPUPercent != 12.5 || got.Totals.CPUPercent != nil || got.Totals.Hosts != 3 ||
			got.Counts.Online != 4 || got.Sources[0].LatencyMS != nil || !got.Sources[0].OK || *got.Sources[1].LatencyMS != 3 {
			t.Fatalf("%s", b)
		}
		if in.Clusters[0].CPUPercent == nil || in.Sources[0].LatencyMS == nil {
			t.Fatal("the input was changed")
		}
	})

	t.Run("ups_event", func(t *testing.T) {
		b, written, ok := safeJSON("ups_event", model.UpsEvent{Seq: 7, T: year33658, UpsID: "ups1", Kind: "on_battery", Message: "m"})
		if got := written.(model.UpsEvent); !ok || !json.Valid(b) || got.Seq != 7 || got.Kind != "on_battery" || got.T.Year() != time.Now().UTC().Year() {
			t.Fatalf("%v %s", ok, b)
		}
	})

	t.Run("systems", func(t *testing.T) {
		list := model.SystemList{GeneratedAt: now, Systems: []model.System{{ID: "a", Name: "a", Kind: "agent", Status: "online", CPUPercent: &nan},
			{ID: "b", Name: "b", Kind: "agent", Status: "online", CPUPercent: model.P(3.0)}}}
		b, _, ok := safeJSON("systems", list)
		var got struct{ Systems []map[string]any }
		if err := json.Unmarshal(b, &got); !ok || err != nil || len(got.Systems) != 2 {
			t.Fatalf("%v %v %s", ok, err, b)
		}
		if got.Systems[0]["status"] != poller.StatusError || got.Systems[1]["status"] != "online" || got.Systems[1]["cpu_percent"] != 3.0 {
			t.Fatalf("%s", b)
		}
	})

	// a type this code does not know: nothing is written (the caller sends nothing, or a 500)
	if b, _, ok := safeJSON("other", map[string]any{"x": nan}); ok || b != nil {
		t.Fatalf("%s", b)
	}

	// through the HTTP layer: the answer is 200 and valid JSON, not a 500 for the whole list
	e := setup(t)
	rec := httptest.NewRecorder()
	c := &reqCtx{s: e.s, w: rec, r: httptest.NewRequest("GET", "/api/systems", nil)}
	c.json(200, model.SystemList{GeneratedAt: now, Systems: []model.System{{ID: "a", Kind: "agent", Status: "online", CPUPercent: &nan}, {ID: "b", Kind: "agent", Status: "online"}}})
	if rec.Code != 200 || !json.Valid(rec.Body.Bytes()) || !strings.Contains(rec.Body.String(), `"status":"error"`) || !strings.Contains(rec.Body.String(), `"status":"online"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	c = &reqCtx{s: e.s, w: rec, r: httptest.NewRequest("GET", "/api/other", nil)}
	c.json(200, map[string]any{"x": nan})
	if rec.Code != 500 || !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}

	// the stream's writer: an event is never written with empty data
	st := &streamState{s: e.s, v: e.s.newView(e.s.grantOf(c, &model.User{Sub: "nobody", Auth: "session"})), last: map[string][]byte{}}
	var out bytes.Buffer
	st.writeChanged(&out)
	sseEvents(t, out.Bytes())
	// a power payload with one source that cannot be written: the event is sent once, as valid JSON with that
	// source "unreachable"; the same payload again sends nothing (also when the generated_at moves); the good
	// payload afterwards is sent
	bad := model.PowerOverview{GeneratedAt: now, TotalW: model.P(12.0), Sources: []model.PowerSourceState{
		{ID: "plug", Name: "Desk plug", Status: "ok", Readings: []model.PowerReading{}, NextAttemptInS: &nan},
		{ID: "pdu", Name: "Rack", Status: "ok", Readings: []model.PowerReading{}}}}
	out.Reset()
	st.writeEvent(&out, "power", bad)
	src, _ := sseEvents(t, out.Bytes())["power"]["sources"].([]any)
	if len(src) != 2 || src[0].(map[string]any)["status"] != "unreachable" || src[1].(map[string]any)["status"] != "ok" {
		t.Fatalf("power event: %s", out.Bytes())
	}
	out.Reset()
	bad.GeneratedAt = now.Add(time.Second)
	st.writeEvent(&out, "power", bad)
	if out.Len() != 0 {
		t.Fatalf("the unchanged payload was sent again: %s", out.Bytes())
	}
	bad.Sources[0].NextAttemptInS = nil
	st.writeEvent(&out, "power", bad)
	if src, _ := sseEvents(t, out.Bytes())["power"]["sources"].([]any); len(src) != 2 || src[0].(map[string]any)["status"] != "ok" {
		t.Fatalf("power event after the source recovered: %s", out.Bytes())
	}
	out.Reset()
	st.writeEvent(&out, "power", bad)
	st.writeEvent(&out, "other", map[string]any{"x": nan}) // nothing valid can be written: nothing is sent
	if out.Len() != 0 {
		t.Fatalf("%s", out.Bytes())
	}

	// GET /api/systems?status=error is a valid filter (it answered 422)
	if r := e.do("GET", "/api/systems?status=error", nil, "Cookie", e.cookie(controller(), 0)); r.code != 200 || len(r.body["systems"].([]any)) != 0 {
		t.Fatalf("%d %s", r.code, r.raw)
	}
}
