package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// L36: an agent that answers the fan-status request with `null` (or an empty object, or nothing) made the panel
// write into a map that does not exist: the request died with a stack trace in the log. Now it is the same calm
// "invalid fan status" as any other answer that is not a fan status.
func TestEmptyFanStatusAnswer(t *testing.T) {
	e := setup(t)
	c := e.cookie(controller(), 0)
	var answer jsonx.M
	e.b.Reply = func(_, path string) (int, jsonx.M, error) {
		if path == "/fan/status" || path == "/fan/mode" {
			return 200, answer, nil
		}
		return 0, nil, nil
	}
	set := func(a jsonx.M) {
		e.b.Mu.Lock()
		answer = a
		e.b.Mu.Unlock()
	}
	for name, a := range map[string]jsonx.M{"null": nil, "empty object": {}} {
		set(a)
		r := e.do("GET", "/api/systems/tower/fan", nil, "Cookie", c)
		if r.code != 424 || !strings.Contains(r.raw2(), "invalid fan status") {
			t.Fatalf("%s: %d %s", name, r.code, r.raw)
		}
	}
	// a fan write answered with null or {}: not the agent's answer, so not "ok" (review of fix8, RF8-04; it was
	// 200 with an empty result object). The agent answers a fan change with {"mode": ...}.
	for name, a := range map[string]jsonx.M{"null": nil, "empty object": {}} {
		set(a)
		r := e.do("POST", "/api/systems/tower/fan/mode", map[string]any{"mode": "full"}, "Cookie", c, "X-Requested-With", "fairy")
		if r.code != 502 || r.body["detail"] != "no answer (http 200): it may have arrived" {
			t.Fatalf("fan write answered with %s: %d %s", name, r.code, r.raw)
		}
	}
	// a real fan status still comes through
	set(jsonx.M{"enabled": true, "vendor": "supermicro", "mode": "optimal", "duty": jsonx.M{}, "fans": []any{}, "events": []any{}})
	if r := e.do("GET", "/api/systems/tower/fan", nil, "Cookie", c); r.code != 200 || r.body["system_id"] != "tower" || r.body["mode"] != "optimal" {
		t.Fatalf("%d %s", r.code, r.raw)
	}
}

// The same end to end: the real HTTP client of the panel against a fake agent whose /fan/status answers the
// bytes `null`, `{}`, nothing, `[]` and a non-JSON text.
func TestEmptyFanStatusAnswerOverHTTP(t *testing.T) {
	var body atomic.Value
	body.Store("")
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fan/status" {
			_, _ = w.Write([]byte(body.Load().(string)))
			return
		}
		m := testutil.AgentMetrics()
		m["features"] = jsonx.M{"processes": true, "fan_control": true}
		_ = json.NewEncoder(w).Encode(m)
	}))
	defer agent.Close()
	cfg := testutil.Config(t, []string{"http://127.0.0.1:1"}, agent.URL, agent.URL)
	p, err := poller.New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.PollOnce(t.Context())
	s := New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, p, nil, "test")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	e := &env{t: t, s: s, srv: srv}
	c := e.cookie(controller(), 0)
	for _, b := range []string{"null", "{}", "", "[]", "nope", " null \n"} {
		body.Store(b)
		r := e.do("GET", "/api/systems/tower/fan", nil, "Cookie", c)
		if r.code/100 == 5 && r.code != 502 || r.code == 200 || !strings.Contains(r.raw2(), "detail") {
			t.Fatalf("%q: %d %s", b, r.code, r.raw)
		}
	}
	body.Store(`{"enabled":true,"mode":"full","fans":[{"name":"FAN1","rpm":900}]}`)
	if r := e.do("GET", "/api/systems/tower/fan", nil, "Cookie", c); r.code != 200 || r.body["mode"] != "full" {
		t.Fatalf("%d %s", r.code, r.raw)
	}
}

// r2-p3-06: an agent's fan status was passed on uncut: 10 kB in every text and lists as long as the 1 MiB answer
// allows (the Fans tab was 88,000 px wide). Texts are cut and lists capped now, as for the process list.
func TestFanStatusIsCut(t *testing.T) {
	e := setup(t)
	c := e.cookie(controller(), 0)
	long := strings.Repeat("W", 10_000)
	many := func(n int, f func(i int) any) []any {
		l := make([]any, n)
		for i := range l {
			l[i] = f(i)
		}
		return l
	}
	duty := jsonx.M{"0": 40, "1": 55, long: 1}
	for i := 2; i < 300; i++ {
		duty[fmt.Sprint(i)] = i % 100
	}
	answer := jsonx.M{"enabled": true, "vendor": long, "mode": long, "auto_mode": long, "error": long, "reason": long, "duty": duty,
		"unsupported_modes": many(300, func(int) any { return long }),
		"fans":              many(3000, func(int) any { return jsonx.M{"name": long, "rpm": 900} }),
		"events":            many(3000, func(i int) any { return jsonx.M{"t": float64(i), "action": long, "detail": long} })}
	e.b.Reply = func(_, path string) (int, jsonx.M, error) {
		if path == "/fan/status" {
			return 200, answer, nil
		}
		return 0, nil, nil
	}
	r := e.do("GET", "/api/systems/tower/fan", nil, "Cookie", c)
	if r.code != 200 {
		t.Fatalf("%d %s", r.code, r.raw2()[:200])
	}
	var fs model.FanStatus
	if err := json.Unmarshal([]byte(r.raw2()), &fs); err != nil {
		t.Fatal(err)
	}
	cut := func(what, s string, n int) {
		t.Helper()
		if len(s) > n+len("…") || !strings.HasSuffix(s, "…") {
			t.Errorf("%s: %d bytes, want at most %d and the mark of a cut", what, len(s), n)
		}
	}
	cut("vendor", *fs.Vendor, maxFanWord)
	cut("mode", *fs.Mode, maxFanWord)
	cut("auto_mode", *fs.AutoMode, maxFanWord)
	cut("error", *fs.Error, 256)
	cut("reason", fs.Reason, maxFanWord)
	if len(fs.Unsupported) != maxFanModes || len(fs.Fans) != 64 || len(fs.Events) != maxFanEvents || len(fs.Duty) != maxFanZones {
		t.Fatalf("lists: %d modes, %d fans, %d events, %d zones", len(fs.Unsupported), len(fs.Fans), len(fs.Events), len(fs.Duty))
	}
	cut("an unsupported mode", fs.Unsupported[0], maxFanWord)
	cut("a fan's name", fs.Fans[63].Name, maxFanName)
	cut("an event's action", fs.Events[0].Action, maxFanWord)
	cut("an event's detail", fs.Events[0].Detail, maxFanDetail)
	if fs.Events[maxFanEvents-1].T != 2999 || fs.Events[0].T != 2950 {
		t.Errorf("the newest events are kept: first %v, last %v", fs.Events[0].T, fs.Events[maxFanEvents-1].T)
	}
	if fs.Duty["0"] != 40 || fs.Duty["1"] != 55 || fs.Duty["15"] != 15 {
		t.Errorf("zones 0 to 15 are kept: %v", fs.Duty)
	}
	if n := len(r.raw2()); n > 40_000 {
		t.Errorf("the answer is %d bytes", n)
	}
	t.Logf("answer: %d bytes (agent's: about %d)", len(r.raw2()), 6300*10_000)

	// a real board's status is passed on as it is
	answer = jsonx.M{"enabled": true, "vendor": "supermicro", "mode": "optimal", "auto_mode": "optimal", "duty": jsonx.M{"0": 40, "1": 55},
		"unsupported_modes": []any{"heavyio"}, "fans": []any{jsonx.M{"name": "FAN1", "rpm": 900}},
		"events": []any{jsonx.M{"t": 1.5, "action": "mode", "detail": "optimal (by alice)"}}}
	r = e.do("GET", "/api/systems/tower/fan", nil, "Cookie", c)
	fs = model.FanStatus{}
	if err := json.Unmarshal([]byte(r.raw2()), &fs); err != nil || r.code != 200 {
		t.Fatal(r.code, err)
	}
	if *fs.Vendor != "supermicro" || *fs.Mode != "optimal" || fs.Duty["1"] != 55 || fs.Unsupported[0] != "heavyio" || fs.Fans[0].Name != "FAN1" ||
		fs.Events[0].Detail != "optimal (by alice)" {
		t.Errorf("a real status changed: %s", r.raw2())
	}
}
