package normalise

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

var fanCfg = config.Agent{ID: "tower", FanControl: &config.FanControl{Vendor: "supermicro", ControlTokenEnv: "CTL"}}

func norm(cfg config.Agent, m jsonx.M) model.System {
	raw, _ := json.Marshal(m) // as from the wire
	var w jsonx.M
	_ = json.Unmarshal(raw, &w)
	s := NormaliseAgent(cfg, w, time.Now(), 1, NewRateTracker())
	if s.Helper != nil {
		ApplyHelperPause(cfg, &s, HelperUnavailable(w), nil) // as the poller does before it saw the helper run
	}
	return s
}

func TestMetricsWithoutHelperBlock(t *testing.T) {
	s := norm(config.Agent{ID: "laptop"}, testutil.AgentMetrics())
	if s.Helper != nil || s.PowerHelperReason != "" {
		t.Fatalf("Linux / older agents have no helper: %+v", s.Helper)
	}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), `"helper"`) {
		t.Fatal("no helper key in the API for hosts without one:", string(b))
	}
}

func TestMetricsWithHelperBlock(t *testing.T) {
	s := norm(fanCfg, testutil.HelperMetrics("running", "coretemp", "ipmi", "fans", "power"))
	h := s.Helper
	if h == nil || h.State != "running" || !h.Needed || h.Version != "0.5.0" || !h.Watchdog || h.Since == nil ||
		!h.Since.Equal(time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)) || fmt.Sprint(h.Features) != "[coretemp ipmi fans power]" ||
		len(h.Paused) != 0 || !s.FanControl || s.PowerHelperReason != "" {
		t.Fatalf("%+v", h)
	}
	if g := fmt.Sprint(HelperGroupsIn(s)); g != "[coretemp ipmi fans]" {
		t.Fatal("helper groups while running:", g)
	}
	b, _ := json.Marshal(s)
	if !strings.Contains(string(b), `"paused":[]`) {
		t.Fatal("paused must be a list, never null:", string(b))
	}
	// stopped: no version / watchdog / features from the agent; what it does comes from "unavailable"
	s = norm(fanCfg, testutil.HelperMetrics("stopped", "coretemp", "ipmi", "fans", "power"))
	h = s.Helper
	if h.State != "stopped" || h.Version != "" || h.Watchdog || fmt.Sprint(h.Features) != "[coretemp ipmi fans power]" ||
		s.PowerHelperReason != api.ReasonHelperStopped || h.Error != "" {
		t.Fatalf("stopped: %+v power %q", h, s.PowerHelperReason)
	}
	s = norm(fanCfg, testutil.HelperMetrics("error", "coretemp", "power"))
	if s.Helper.State != "error" || s.Helper.Error == "" || s.PowerHelperReason != api.ReasonHelperError || fmt.Sprint(s.Helper.Paused) != "[coretemp]" {
		t.Fatalf("error: %+v power %q", s.Helper, s.PowerHelperReason)
	}
}

// The real agent's types (pkg/agent/api) produce what the panel reads.
func TestHelperFromTheAgentsTypes(t *testing.T) {
	wd := true
	m := testutil.AgentMetrics()
	m["helper"] = api.HelperStatus{State: api.HelperRunning, Needed: true, Version: "0.5.0", Since: "2026-11-09T12:00:00Z",
		Watchdog: &wd, Features: []string{api.FeatureCoreTemp, api.FeaturePower}}
	if s := norm(config.Agent{ID: "x"}, m); s.Helper.State != "running" || !s.Helper.Watchdog || fmt.Sprint(s.Helper.Features) != "[coretemp power]" {
		t.Fatalf("%+v", s.Helper)
	}
	// fields of later agents (v0.5.1's "migration" block, new helper keys) are ignored, not an error
	m2 := testutil.HelperMetrics("running", "coretemp")
	m2["migration"] = map[string]any{"state": "needs_admin", "detail": "x", "steps": []any{1.0}}
	m2["helper"].(jsonx.M)["pipe"] = map[string]any{"name": `\\.\pipe\fairy`}
	if s := norm(config.Agent{ID: "x"}, m2); s.Status != "online" || s.Helper.State != "running" || s.CPUPercent == nil {
		t.Fatalf("unknown fields: %+v", s)
	}
	m["helper"] = api.HelperStatus{State: api.HelperStopped, Needed: true}
	m["unavailable"] = []api.Unavailable{{Source: api.FeatureCoreTemp, Reason: api.ReasonHelperStopped}, {Source: api.FeaturePower, Reason: api.ReasonHelperStopped}}
	m["power_control"] = map[string]any{"enabled": true, "pending": nil, "available": false, "reason": api.ReasonHelperStopped}
	s := norm(config.Agent{ID: "x"}, m)
	if fmt.Sprint(s.Helper.Paused) != "[coretemp]" || s.PowerHelperReason != api.ReasonHelperStopped || s.Helper.Since != nil {
		t.Fatalf("%+v %q", s.Helper, s.PowerHelperReason)
	}
}

func TestHostileHelperBlock(t *testing.T) {
	m := testutil.AgentMetrics()
	m["helper"] = jsonx.M{"state": "pwned", "needed": "yes", "version": "<script>", "since": "yesterday",
		"features": []any{"coretemp", "rm -rf", 5.0, "coretemp"}, "error": "bad\x1b[31m\u202e" + strings.Repeat("x", 500)}
	m["power_control"] = jsonx.M{"enabled": true, "available": false, "reason": "<img>"}
	s := norm(config.Agent{ID: "x"}, m)
	h := s.Helper
	if h.State != "unknown" || h.Needed || h.Version != "" || h.Since != nil || fmt.Sprint(h.Features) != "[coretemp]" {
		t.Fatalf("%+v", h)
	}
	if strings.ContainsAny(h.Error, "\x1b\u202e") || len(h.Error) > 310 {
		t.Fatalf("text not cleaned: %q", h.Error)
	}
	if s.PowerHelperReason != api.ReasonHelperStopped {
		t.Fatalf("an unknown reason still means unavailable: %q", s.PowerHelperReason)
	}
	m["helper"] = "not an object"
	if s := norm(config.Agent{ID: "x"}, m); s.Helper != nil {
		t.Fatal("a non-object helper is no helper")
	}
}

// The "unavailable" list: only helper sources with a helper reason count; anything else is ignored.
func TestHelperUnavailableList(t *testing.T) {
	m := jsonx.M{"unavailable": []any{
		map[string]any{"source": "coretemp", "reason": "helper_stopped"},
		map[string]any{"source": "ipmi", "reason": "helper_error"},
		map[string]any{"source": "gpu", "reason": "helper_stopped"},
		map[string]any{"source": "fans", "reason": "bmc_busy"},
		map[string]any{"feature": "power", "reason": "helper_stopped"},
		"coretemp", 5.0, nil,
	}}
	if got := HelperUnavailable(m); fmt.Sprint(got) != "map[coretemp:true ipmi:true]" {
		t.Fatal(got)
	}
	for _, v := range []any{nil, "x", map[string]any{"coretemp": "helper_stopped"}} {
		if got := HelperUnavailable(jsonx.M{"unavailable": v}); len(got) != 0 {
			t.Fatal(v, got)
		}
	}
}

func TestHelperPausedOnlyWhatTheHostUses(t *testing.T) {
	all := []string{"coretemp", "ipmi", "fans", "power"}
	stopped := func(features ...string) *model.System {
		s := norm(fanCfg, testutil.HelperMetrics("stopped", features...))
		return &s
	}
	// never seen running: what the agent marks helper_stopped
	if s := stopped(all...); fmt.Sprint(s.Helper.Paused) != "[coretemp ipmi fans]" || !s.FanControl {
		t.Fatalf("fallback: %v fan tab %v", s.Helper.Paused, s.FanControl)
	}
	// seen running with Core Temp only (no BMC on this board): only Core Temp is announced as paused
	s := &model.System{}
	*s = NormaliseAgent(fanCfg, testutil.HelperMetrics("stopped", all...), time.Now(), 1, NewRateTracker())
	ApplyHelperPause(fanCfg, s, HelperUnavailable(testutil.HelperMetrics("stopped", all...)), map[string]bool{"coretemp": true})
	if fmt.Sprint(s.Helper.Paused) != "[coretemp]" {
		t.Fatalf("seen: %v", s.Helper.Paused)
	}
	// a helper that only does power and updates here: nothing to show
	if s := stopped("power", "update"); len(s.Helper.Paused) != 0 {
		t.Fatal(s.Helper.Paused)
	}
	// no Fans tab configured in the panel: fan control is not announced
	s2 := norm(config.Agent{ID: "laptop"}, testutil.HelperMetrics("stopped", all...))
	if fmt.Sprint(s2.Helper.Paused) != "[coretemp ipmi]" || s2.FanControl {
		t.Fatal(s2.Helper.Paused, s2.FanControl)
	}
	// running, not installed, unknown: nothing paused
	for _, st := range []string{"running", "not_installed", "bogus"} {
		m := testutil.HelperMetrics("stopped", all...)
		m["helper"].(jsonx.M)["state"] = st
		if s := norm(fanCfg, m); len(s.Helper.Paused) != 0 {
			t.Errorf("%s: %v", st, s.Helper.Paused)
		}
	}
	// starting and error count as down
	for _, st := range []string{"starting", "error"} {
		m := testutil.HelperMetrics("stopped", all...)
		m["helper"].(jsonx.M)["state"] = st
		if s := norm(fanCfg, m); len(s.Helper.Paused) != 3 {
			t.Errorf("%s: %v", st, s.Helper.Paused)
		}
	}
}
