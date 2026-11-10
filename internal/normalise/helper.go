package normalise

import (
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
)

// The Windows agent's elevated helper (Fairy-agent v0.5.0+; contract types in Fairy-agent's pkg/agent/api).
// /metrics carries
//
//	"helper": {"state": "running"|"stopped"|"starting"|"not_installed"|"error", "needed": bool, "version": "...",
//	           "since": RFC 3339, "watchdog": bool, "features": ["coretemp","ipmi","fans","power"], "error": "..."}
//	"unavailable": [{"source": "coretemp"|"ipmi"|"fans"|"power", "reason": "helper_stopped"|"helper_error"}]
//
// version, watchdog and features only while the helper runs; "unavailable" only while it does not, listing what
// this agent normally gets through it. Linux agents and older agents send neither. The panel keeps its own
// model.Helper (with Paused, filled by the poller); the names come from the agent's api package.

// HelperGroups are the readings the helper provides that the host page shows: Core Temp, the BMC's sensors and
// fan control. Power actions (and updates) are helper features too, but no readings disappear when they stop.
var HelperGroups = []string{api.FeatureCoreTemp, api.FeatureIPMI, api.FeatureFans}

var (
	helperStates = map[string]bool{api.HelperRunning: true, api.HelperStopped: true, api.HelperStarting: true,
		api.HelperNotInstalled: true, api.HelperError: true}
	helperFeatures = map[string]bool{api.FeatureCoreTemp: true, api.FeatureIPMI: true, api.FeatureFans: true,
		api.FeaturePower: true, api.FeatureUpdate: true}
	helperReasons = map[string]bool{api.ReasonHelperStopped: true, api.ReasonHelperError: true}
	versionRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,39}$`)
)

// agentHelper reads the optional `helper` block; nil when the agent sends none. Unknown states read as
// "unknown", unknown features are dropped, text is bounded: an odd payload cannot break the page. Features are
// what the helper does on this host: while it runs the agent lists them, while it does not the "unavailable"
// sources say it.
func agentHelper(m jsonx.M) *model.Helper {
	h := jsonx.Obj(m, "helper")
	if h == nil {
		return nil
	}
	out := &model.Helper{State: "unknown", Needed: jsonx.Bool(h, "needed", false),
		Watchdog: jsonx.Bool(h, "watchdog", false), Features: []string{}, Paused: []string{}}
	if st := jsonx.S(h, "state"); helperStates[st] {
		out.State = st
	}
	if v := jsonx.S(h, "version"); versionRe.MatchString(v) {
		out.Version = v
	}
	if v := jsonx.S(h, "since"); v != "" && len(v) <= 40 {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil && t.Year() >= 2000 && t.Year() < 2286 {
			out.Since = model.P(t.UTC())
		}
	}
	add := func(f string) {
		if helperFeatures[f] && !slices.Contains(out.Features, f) {
			out.Features = append(out.Features, f)
		}
	}
	for _, f := range jsonx.List(h, "features") {
		if s, ok := f.(string); ok {
			add(s)
		}
	}
	if out.State != api.HelperRunning {
		un := HelperUnavailable(m)
		for _, f := range []string{api.FeatureCoreTemp, api.FeatureIPMI, api.FeatureFans, api.FeaturePower} {
			if un[f] {
				add(f)
			}
		}
	}
	if out.State != api.HelperRunning && out.State != api.HelperStarting {
		out.Error = textsafe.Clip(jsonx.S(h, "error"), 300)
	}
	return out
}

// HelperUnavailable is the ADAPTER for the agent's "unavailable" list: the helper sources (coretemp, ipmi,
// fans, power) it reports as missing because the helper is stopped or not answering. Anything else in the list
// (other sources or reasons, entries that are not objects) is ignored.
func HelperUnavailable(m jsonx.M) map[string]bool {
	out := map[string]bool{}
	for _, e := range jsonx.Objs(m["unavailable"]) {
		src, reason := jsonx.S(e, "source"), jsonx.S(e, "reason")
		if helperFeatures[src] && helperReasons[reason] {
			out[src] = true
		}
	}
	return out
}

// powerHelperReason is the agent's power_control "reason" when power actions are unavailable because of the
// helper ("helper_stopped" / "helper_error"); "" when they are available.
func powerHelperReason(m jsonx.M) string {
	pc := jsonx.Obj(m, "power_control")
	if pc == nil || jsonx.Bool(pc, "available", true) {
		return ""
	}
	if r := jsonx.S(pc, "reason"); helperReasons[r] {
		return r
	}
	return api.ReasonHelperStopped
}

// HelperGroupsIn returns the helper-provided readings present in s: Core Temp temperatures (sensor "coretemp"),
// BMC sensors (sensor "IPMI", "IPMI ..." fans and voltages, in-band BMC watts) and fan control.
func HelperGroupsIn(s model.System) []string {
	var out []string
	if slices.ContainsFunc(s.Temps, func(t model.Temperature) bool { return t.Sensor == "coretemp" }) {
		out = append(out, "coretemp")
	}
	ipmi := slices.ContainsFunc(s.Temps, func(t model.Temperature) bool { return t.Sensor == "IPMI" }) ||
		slices.ContainsFunc(s.Fans, func(f model.Fan) bool { return strings.HasPrefix(f.Name, "IPMI ") }) ||
		slices.ContainsFunc(s.Voltages, func(v model.Voltage) bool { return strings.HasPrefix(v.Name, "IPMI ") }) ||
		(s.ComponentPower != nil && s.ComponentPower.BMCW != nil)
	if ipmi {
		out = append(out, "ipmi")
	}
	if s.FanControl {
		out = append(out, "fans")
	}
	return out
}

// helperDown: the helper's readings are missing in this state.
func helperDown(state string) bool {
	return state == api.HelperStopped || state == api.HelperStarting || state == api.HelperError
}

// ApplyHelperPause fills s.Helper.Paused: the helper's readings this host shows while the helper runs and that
// are missing now. Only groups the helper provides here count (its features, or what the agent marks
// "helper_stopped"); when the panel saw the helper running (seen: the groups that had data then, nil when it never
// did) only those, so a host whose helper does nothing visible gets no "paused" line. While fan control is
// paused the Fans tab stays (with its controls disabled) if the panel has fan control configured for the agent.
func ApplyHelperPause(cfg config.Agent, s *model.System, unavailable map[string]bool, seen map[string]bool) {
	h := s.Helper
	if h == nil {
		return
	}
	h.Paused = []string{}
	if !helperDown(h.State) {
		return
	}
	for _, g := range HelperGroups {
		if !slices.Contains(h.Features, g) && !unavailable[g] {
			continue
		}
		switch {
		case seen != nil && !seen[g]:
			continue
		case seen == nil && len(unavailable) > 0 && !unavailable[g]:
			continue
		case g == "fans" && cfg.FanControl == nil:
			continue // the panel has no Fans tab for this host
		}
		h.Paused = append(h.Paused, g)
	}
	if slices.Contains(h.Paused, "fans") {
		s.FanControl = true
	}
}
