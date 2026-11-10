package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
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
		r := e.do("GET", "/api/systems/cyrene/fan", nil, "Cookie", c)
		if r.code != 424 || !strings.Contains(r.raw2(), "invalid fan status") {
			t.Fatalf("%s: %d %s", name, r.code, r.raw)
		}
	}
	// a fan write answered with null: still a result object, as for {}
	set(nil)
	r := e.do("POST", "/api/systems/cyrene/fan/mode", map[string]any{"mode": "full"}, "Cookie", c, "X-Requested-With", "fairy")
	if res, ok := r.body["result"].(map[string]any); r.code != 200 || !ok || len(res) != 0 {
		t.Fatalf("fan write: %d %s", r.code, r.raw)
	}
	// a real fan status still comes through
	set(jsonx.M{"enabled": true, "vendor": "supermicro", "mode": "optimal", "duty": jsonx.M{}, "fans": []any{}, "events": []any{}})
	if r := e.do("GET", "/api/systems/cyrene/fan", nil, "Cookie", c); r.code != 200 || r.body["system_id"] != "cyrene" || r.body["mode"] != "optimal" {
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
		r := e.do("GET", "/api/systems/cyrene/fan", nil, "Cookie", c)
		if r.code/100 == 5 && r.code != 502 || r.code == 200 || !strings.Contains(r.raw2(), "detail") {
			t.Fatalf("%q: %d %s", b, r.code, r.raw)
		}
	}
	body.Store(`{"enabled":true,"mode":"full","fans":[{"name":"FAN1","rpm":900}]}`)
	if r := e.do("GET", "/api/systems/cyrene/fan", nil, "Cookie", c); r.code != 200 || r.body["mode"] != "full" {
		t.Fatalf("%d %s", r.code, r.raw)
	}
}
