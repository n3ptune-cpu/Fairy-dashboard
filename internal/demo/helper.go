package demo

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
)

// simHelper is a Windows agent's elevated helper (Fairy-agent v0.5.0, the shapes of its pkg/agent/api):
// SRV-TOWER's runs (Core Temp, its BMC's sensors, fan control, power), Alice's laptop's is stopped (Core Temp
// readings paused, power actions wait for it), Bob's PC has none installed and needs none. Stop / Start from
// the panel change it; a start takes a few seconds.
type simHelper struct {
	state     string // running | stopped | starting | not_installed
	since     time.Time
	readyAt   time.Time // starting -> running
	needed    bool
	sources   []string     // what this agent gets through the helper (its "unavailable" list while it is stopped)
	coreTemps func() []any // the Core Temp readings while it runs
}

const helperStartS = 4

func newSimHelpers() map[string]*simHelper {
	t0 := time.Now().Add(-26 * time.Hour)
	return map[string]*simHelper{
		"srv-tower": {state: api.HelperRunning, since: t0, needed: true,
			sources: []string{api.FeatureCoreTemp, api.FeatureIPMI, api.FeatureFans, api.FeaturePower},
			coreTemps: func() []any {
				p := r1(wave(100, 40, 71, 0.3))
				return []any{temp("coretemp", "CPU Package", p), temp("coretemp", "Core 0", r1(p-2)), temp("coretemp", "Core 1", r1(p-4))}
			}},
		"alice-laptop": {state: api.HelperStopped, since: time.Now().Add(-40 * time.Minute), needed: true,
			sources:   []string{api.FeatureCoreTemp, api.FeaturePower},
			coreTemps: func() []any { return []any{temp("coretemp", "CPU Package", r1(wave(60, 52, 79, 0.8)))} }},
		"bob-pc": {state: api.HelperNotInstalled, since: t0},
	}
}

// status is the /metrics "helper" block (and the /helper/stop|start answer). Caller holds b.mu.
func (h *simHelper) status() api.HelperStatus {
	if h.state == api.HelperStarting && !time.Now().Before(h.readyAt) {
		h.state, h.since = api.HelperRunning, time.Now()
	}
	st := api.HelperStatus{State: h.state, Needed: h.needed, Since: h.since.UTC().Format(time.RFC3339)}
	if h.state == api.HelperRunning {
		wd := slices.Contains(h.sources, api.FeatureFans)
		st.Version, st.Watchdog, st.Features = "0.5.0", &wd, slices.Clone(h.sources)
	}
	return st
}

// applyHelper adds the helper block to a demo /metrics payload and, while the helper is not running, leaves out
// what only it can read and lists it as "unavailable", as the v0.5.0 agent does. Caller holds b.mu.
func (b *Backend) applyHelper(id string, m map[string]any) {
	h := b.helpers[id]
	if h == nil {
		return
	}
	st := h.status()
	m["helper"] = st
	if st.Running() {
		if h.coreTemps != nil {
			temps, _ := m["temps"].([]any)
			m["temps"] = append(h.coreTemps(), temps...)
		}
		return
	}
	if h.state == api.HelperNotInstalled || len(h.sources) == 0 {
		return
	}
	un := make([]api.Unavailable, 0, len(h.sources))
	for _, src := range h.sources {
		un = append(un, api.Unavailable{Source: src, Reason: api.ReasonHelperStopped})
	}
	m["unavailable"] = un
	if slices.Contains(h.sources, api.FeatureIPMI) { // the BMC's sensors come through the helper
		m["temps"] = dropIPMI(m["temps"], "sensor", func(v string) bool { return v == "IPMI" })
		m["fans"] = dropIPMI(m["fans"], "name", func(v string) bool { return strings.HasPrefix(v, "IPMI ") })
		m["voltages"] = dropIPMI(m["voltages"], "name", func(v string) bool { return strings.HasPrefix(v, "IPMI ") })
	}
	if pc, ok := m["power_control"].(map[string]any); ok && slices.Contains(h.sources, api.FeaturePower) {
		pc["available"], pc["reason"] = false, api.ReasonHelperStopped
	}
}

func dropIPMI(v any, key string, drop func(string) bool) []any {
	out := []any{}
	list, _ := v.([]any)
	for _, x := range list {
		if row, ok := x.(map[string]any); ok {
			if s, _ := row[key].(string); drop(s) {
				continue
			}
		}
		out = append(out, x)
	}
	return out
}

// simHelperControl answers POST /helper/stop and /helper/start: 200 with the helper block, or
// 409 {"error": "not_installed"}.
func (b *Backend) simHelperControl(id, path string) (int, jsonx.M) {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := b.helpers[id]
	if h == nil || h.state == api.HelperNotInstalled {
		return 409, jsonx.M{"error": api.ErrHelperNotInstalled.Error(), "code": "not_installed"}
	}
	now := time.Now()
	switch {
	case path == api.PathHelperStop && h.state != api.HelperStopped:
		h.state, h.since = api.HelperStopped, now
	case path == api.PathHelperStart && h.state == api.HelperStopped:
		h.state, h.since, h.readyAt = api.HelperStarting, now, now.Add(helperStartS*time.Second)
	}
	raw, _ := json.Marshal(h.status())
	var out jsonx.M
	_ = json.Unmarshal(raw, &out)
	return 200, out
}

// helperBlocks answers a fan or power request the way the v0.5.0 agent does while its helper is not running:
// /fan/status 200 with "available": false, changes 503 {"code": "helper_stopped"}. ok is false when the helper
// does not stand in the way.
func (b *Backend) helperBlocks(id, path string) (int, jsonx.M, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := b.helpers[id]
	if h == nil || h.status().Running() {
		return 0, nil, false
	}
	source := api.FeatureFans
	if path == "/power" {
		source = api.FeaturePower
	}
	if !slices.Contains(h.sources, source) {
		return 0, nil, false
	}
	if path == "/fan/status" {
		return 200, jsonx.M{"enabled": true, "vendor": "supermicro", "mode": nil, "error": nil, "overridden": false,
			"duty": map[string]any{}, "fans": []any{}, "events": []any{}, "available": false, "reason": api.ReasonHelperStopped}, true
	}
	what := map[string]string{"/power": "shut down or reboot this machine"}[path]
	if what == "" {
		what = "change the fans"
	}
	return 503, jsonx.M{"error": "helper stopped: start fairy-agent-helper to " + what, "code": api.ReasonHelperStopped}, true
}
