package normalise

import (
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// The Windows agent's elevated helper (Fairy-agent v0.5.0+). /metrics carries
//
//	"helper": {"state": "running"|"stopped"|"starting"|"not_installed"|"error", "needed": bool, "version": "...",
//	           "since": RFC3339 or "", "watchdog": bool, "features": ["coretemp","ipmi","fans","power","update"],
//	           "error": "..."}
//
// Linux agents and older agents leave it out. The panel's own copy of the shape is model.Helper (the agent
// release that defines it is not tagged yet, so it is not imported from Fairy-agent's pkg/).

// HelperGroups are the readings the helper provides that the host page shows: Core Temp, the BMC's sensors and
// fan control. "power" (power actions) and "update" (installing updates) are helper features too, but no
// readings disappear when they stop.
var HelperGroups = []string{"coretemp", "ipmi", "fans"}

var (
	helperStates   = map[string]bool{"running": true, "stopped": true, "starting": true, "not_installed": true, "error": true}
	helperFeatures = map[string]bool{"coretemp": true, "ipmi": true, "fans": true, "power": true, "update": true}
	versionRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,39}$`)
)

// cleanText drops control characters and bounds the length of a string an agent sent for display.
func cleanText(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r < 0xa0) || r == 0x2028 || r == 0x2029 || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = strings.ToValidUTF8(s[:n], "") + "…"
	}
	return s
}

// agentHelper reads the optional `helper` block; nil when the agent sends none. Unknown states read as
// "unknown", unknown features are dropped, text is bounded: an odd payload cannot break the page.
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
	for _, f := range jsonx.List(h, "features") {
		if s, ok := f.(string); ok && helperFeatures[s] && !slices.Contains(out.Features, s) {
			out.Features = append(out.Features, s)
		}
	}
	if out.State == "error" || out.State == "stopped" || out.State == "unknown" {
		out.Error = cleanText(jsonx.S(h, "error"), 300)
	}
	return out
}

// agentServiceAccount is the account the agent runs as, when it says (top level, or in the helper block).
func agentServiceAccount(m jsonx.M) *string {
	v := jsonx.S(m, "service_account")
	if v == "" {
		v = jsonx.S(jsonx.Obj(m, "helper"), "service_account")
	}
	if v = cleanText(v, 80); v == "" {
		return nil
	}
	return &v
}

// helperAlias maps the names an agent may use for a helper-provided section to the panel's feature names.
var helperAlias = map[string]string{"coretemp": "coretemp", "core_temp": "coretemp", "ipmi": "ipmi", "bmc": "ipmi",
	"bmc_sensors": "ipmi", "sensors_bmc": "ipmi", "fans": "fans", "fan": "fans", "fan_control": "fans",
	"power": "power", "power_control": "power", "update": "update", "self_update": "update"}

// HelperStoppedReason is the reason the agent gives for a reading it cannot take while the helper is stopped.
const HelperStoppedReason = "helper_stopped"

// HelperUnavailable is the ADAPTER for where the agent says that a reading is missing because the helper is
// stopped: it returns the helper features (coretemp, ipmi, fans, power, update) the agent reports as unavailable
// with reason "helper_stopped". Fairy-agent v0.5.0 had not fixed the field's location when this was written, so it
// accepts each of the likely shapes; once the agent is released, keep the real one and drop the others.
//
//	"unavailable": {"coretemp": "helper_stopped", "ipmi": {"reason": "helper_stopped"}}   (top level or in "helper")
//	"unavailable": [{"feature": "coretemp", "reason": "helper_stopped"}, ...]             (or id / name / section)
//	"coretemp": {"available": false, "reason": "helper_stopped"}                          (a block per section)
func HelperUnavailable(m jsonx.M) map[string]bool {
	out := map[string]bool{}
	mark := func(name string, reason any) {
		r, _ := reason.(string)
		if g, ok := helperAlias[strings.ToLower(name)]; ok && strings.EqualFold(r, HelperStoppedReason) {
			out[g] = true
		}
	}
	scan := func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, r := range x {
				if o, ok := r.(map[string]any); ok {
					r = o["reason"]
				}
				mark(k, r)
			}
		case []any:
			for _, e := range jsonx.Objs(x) {
				for _, k := range []string{"feature", "id", "name", "section", "what"} {
					if n := jsonx.S(e, k); n != "" {
						mark(n, e["reason"])
						break
					}
				}
			}
		}
	}
	for _, k := range []string{"unavailable", "sensors_unavailable"} {
		scan(m[k])
		scan(jsonx.Obj(m, "helper")[k])
	}
	for name := range helperAlias {
		if b := jsonx.Obj(m, name); b != nil && !jsonx.Bool(b, "available", true) {
			mark(name, b["reason"])
		}
	}
	return out
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
	return state == "stopped" || state == "starting" || state == "error"
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
