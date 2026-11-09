package demo

import (
	"slices"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
)

// simHelper is a Windows agent's elevated helper (Fairy-agent v0.5.0): NSX-CYRENE's runs (Core Temp, its BMC's
// sensors, fan control, power, updates), Neptune's laptop's is stopped (Core Temp readings paused), Straw's PC
// has none installed and needs none. Stop / Start from the panel change it; a start takes a few seconds.
type simHelper struct {
	state     string // running | stopped | starting | not_installed
	since     time.Time
	readyAt   time.Time // starting -> running
	needed    bool
	features  []string
	watchdog  bool
	runsAs    string
	coreTemps func() []any // the Core Temp readings while it runs
}

const helperStartS = 4

func newSimHelpers() map[string]*simHelper {
	t0 := time.Now().Add(-26 * time.Hour)
	return map[string]*simHelper{
		"nsx-cyrene": {state: "running", since: t0, needed: true, watchdog: true, runsAs: `NT SERVICE\fairy-agent`,
			features: []string{"coretemp", "ipmi", "fans", "power", "update"},
			coreTemps: func() []any {
				p := r1(wave(100, 40, 71, 0.3))
				return []any{temp("coretemp", "CPU Package", p), temp("coretemp", "Core 0", r1(p-2)), temp("coretemp", "Core 1", r1(p-4))}
			}},
		"neptune-laptop": {state: "stopped", since: time.Now().Add(-40 * time.Minute), needed: true, runsAs: `NT SERVICE\fairy-agent`,
			features:  []string{"coretemp", "power", "update"},
			coreTemps: func() []any { return []any{temp("coretemp", "CPU Package", r1(wave(60, 52, 79, 0.8)))} }},
		"straw-pc": {state: "not_installed", features: []string{}},
	}
}

// applyHelper adds the helper block to a demo /metrics payload and, while the helper is not running, leaves out
// what only it can read (and marks it helper_stopped, as the v0.5.0 agent does). Caller holds b.mu.
func (b *Backend) applyHelper(id string, m map[string]any) {
	h := b.helpers[id]
	if h == nil {
		return
	}
	if h.state == "starting" && !time.Now().Before(h.readyAt) {
		h.state, h.since = "running", time.Now()
	}
	since := ""
	if !h.since.IsZero() {
		since = h.since.UTC().Format(time.RFC3339)
	}
	version := "0.5.0"
	if h.state == "not_installed" {
		version = ""
	}
	m["helper"] = map[string]any{"state": h.state, "needed": h.needed, "version": version, "since": since,
		"watchdog": h.watchdog && h.state == "running", "features": slices.Clone(h.features), "error": ""}
	if h.runsAs != "" {
		m["service_account"] = h.runsAs
	}
	if h.state == "running" {
		if h.coreTemps != nil {
			temps, _ := m["temps"].([]any)
			m["temps"] = append(h.coreTemps(), temps...)
		}
		return
	}
	unavailable := map[string]any{}
	for _, f := range h.features {
		if f == "coretemp" || f == "ipmi" || f == "fans" {
			unavailable[f] = "helper_stopped"
		}
	}
	if len(unavailable) > 0 && h.state != "not_installed" {
		m["unavailable"] = unavailable
	}
	if slices.Contains(h.features, "ipmi") { // the BMC's sensors come through the helper
		m["temps"] = dropIPMI(m["temps"], "sensor", func(v string) bool { return v == "IPMI" })
		m["fans"] = dropIPMI(m["fans"], "name", func(v string) bool { return strings.HasPrefix(v, "IPMI ") })
		m["voltages"] = dropIPMI(m["voltages"], "name", func(v string) bool { return strings.HasPrefix(v, "IPMI ") })
	}
	if slices.Contains(h.features, "fans") {
		jsonx.Obj(m, "features")["fan_control"] = false
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

// simHelperControl answers POST /helper/stop and /helper/start.
func (b *Backend) simHelperControl(id, action string) (int, jsonx.M) {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := b.helpers[id]
	if h == nil || h.state == "not_installed" {
		return 409, jsonx.M{"error": "not_installed"}
	}
	now := time.Now()
	switch {
	case action == "stop" && h.state != "stopped":
		h.state, h.since = "stopped", now
	case action == "start" && h.state == "stopped":
		h.state, h.since, h.readyAt = "starting", now, now.Add(helperStartS*time.Second)
	}
	return 200, jsonx.M{"state": h.state}
}

// helperBlocksFans: NSX-CYRENE's fans cannot be driven while its helper is not running.
func (b *Backend) helperBlocksFans(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := b.helpers[id]
	return h != nil && h.state != "running" && slices.Contains(h.features, "fans")
}
