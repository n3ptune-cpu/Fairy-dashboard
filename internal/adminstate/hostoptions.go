package adminstate

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/configutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
)

// "Edit host": what an admin changed in the panel about the links of the devices an agent reads. It is an
// OVERLAY on the agent's entry (config.yaml's, or the one "Add host" made): config.yaml stays the base and is
// never written. The overlay is kept in state.json, next to the model names (hostmodel.go), under
//
//	"host_options": {"rev": 7, "agents": {"<agent id>": {
//	    "ups_powers": {"<UPS name>": ["<row id>", ...]},
//	    "power_links": {"<device id>": "<row id>"},
//	    "power_exclude_from_total": ["<device id>", ...],
//	    "by": "<who>", "at": "<RFC 3339>"}}}
//
// Keyed by the id of the agent that READS the devices: that is where config.yaml has these fields. A field
// that is there REPLACES config.yaml's value of that field for that agent, whole; a field that is not there
// leaves config.yaml's value. "Reset" takes a field out. Only these three fields exist here: nothing that
// says where an agent is, which token it has or what the panel may do to a machine can be in the overlay,
// and a key with another name is not read.
//
// The reader never fails. Whatever is not what Fairy writes is left out, config.yaml's value counts for it,
// and one WARN line says so (per change of the file): the panel always starts. `fairy check-config`, which a
// `fairy update` asks before it installs a version, does not read state.json at all.

// hostOptionsKey is the name of the object in state.json.
const hostOptionsKey = "host_options"

// MaxHostOptionAgents is how many agents may have overrides. A panel has far fewer agents; a longer list is
// not something Fairy wrote, and none of it is used.
const MaxHostOptionAgents = 256

// HostOption is the overrides of one agent's entry. Has* says which fields are overridden; the value of a
// field that is not overridden is nil and means nothing.
type HostOption struct {
	Links                                   config.Links
	HasUPSPowers, HasPowerLinks, HasExclude bool
	By                                      string // who changed it last (the audit log's name for them)
	At                                      time.Time
}

// Empty reports whether no field is overridden.
func (o HostOption) Empty() bool { return !o.HasUPSPowers && !o.HasPowerLinks && !o.HasExclude }

// Has reports whether the named field (config.Field...) is overridden.
func (o HostOption) Has(field string) bool {
	switch field {
	case config.FieldUPSPowers:
		return o.HasUPSPowers
	case config.FieldPowerLinks:
		return o.HasPowerLinks
	case config.FieldExclude:
		return o.HasExclude
	}
	return false
}

// Apply returns the links that count for an agent whose entry says base: base, with every overridden field
// replaced. The result shares nothing with base or o.
func (o HostOption) Apply(base config.Links) config.Links {
	out, over := base.Clone(), o.Links.Clone()
	if o.HasUPSPowers {
		out.UPSPowers = over.UPSPowers
	}
	if o.HasPowerLinks {
		out.PowerLinks = over.PowerLinks
	}
	if o.HasExclude {
		out.PowerExcludeFromTotal = over.PowerExcludeFromTotal
	}
	return out
}

// Set overrides the named field with l's value of it (a nil value is stored as an empty one: "set here to
// nothing").
func (o *HostOption) Set(field string, l config.Links) {
	l = l.Clone()
	switch field {
	case config.FieldUPSPowers:
		if o.HasUPSPowers, o.Links.UPSPowers = true, l.UPSPowers; o.Links.UPSPowers == nil {
			o.Links.UPSPowers = map[string][]string{}
		}
	case config.FieldPowerLinks:
		if o.HasPowerLinks, o.Links.PowerLinks = true, l.PowerLinks; o.Links.PowerLinks == nil {
			o.Links.PowerLinks = map[string]string{}
		}
	case config.FieldExclude:
		if o.HasExclude, o.Links.PowerExcludeFromTotal = true, l.PowerExcludeFromTotal; o.Links.PowerExcludeFromTotal == nil {
			o.Links.PowerExcludeFromTotal = []string{}
		}
	}
}

// Reset takes the override of the named field away (config.yaml's value counts again). It reports whether
// there was one.
func (o *HostOption) Reset(field string) bool {
	had := o.Has(field)
	switch field {
	case config.FieldUPSPowers:
		o.HasUPSPowers, o.Links.UPSPowers = false, nil
	case config.FieldPowerLinks:
		o.HasPowerLinks, o.Links.PowerLinks = false, nil
	case config.FieldExclude:
		o.HasExclude, o.Links.PowerExcludeFromTotal = false, nil
	}
	return had
}

// HostOptions is the whole overlay. Rev counts the writes (it only ever grows): two answers with the same
// Rev show the same stored state.
type HostOptions struct {
	Rev    int
	Agents map[string]HostOption
}

// Clone returns a copy that shares nothing with o.
func (o HostOptions) Clone() HostOptions {
	out := HostOptions{Rev: o.Rev, Agents: make(map[string]HostOption, len(o.Agents))}
	for id, e := range o.Agents {
		e.Links = e.Links.Clone()
		out.Agents[id] = e
	}
	return out
}

// For returns the overrides of one agent (the zero value: none).
func (o HostOptions) For(agentID string) HostOption { return o.Agents[agentID] }

// Forget is what "Remove" of a host does to the overlay: the overrides of the host's own agent go, and so
// does every reference to one of the host's rows in the overrides of OTHER agents (a UPS that powers it, a
// plug that is linked to it). A host's id is made from its name, so without this the next machine added
// under the same name would be shown on a UPS, and with a plug's watts, that nobody linked to it. ids are the
// ids the host is shown under (its agent's and its node's). It reports what was taken out, one line each.
func (o *HostOptions) Forget(agentID string, ids ...string) []string {
	var gone []string
	if e, ok := o.Agents[agentID]; ok {
		delete(o.Agents, agentID)
		if !e.Empty() {
			gone = append(gone, "the overrides of "+agentID)
		}
	}
	ids = append(ids, agentID)
	for _, other := range slices.Sorted(maps.Keys(o.Agents)) {
		e := o.Agents[other]
		for _, name := range slices.Sorted(maps.Keys(e.Links.UPSPowers)) {
			rows := e.Links.UPSPowers[name]
			kept := slices.DeleteFunc(slices.Clone(rows), func(id string) bool { return slices.Contains(ids, id) })
			if len(kept) != len(rows) {
				e.Links.UPSPowers[name] = kept
				gone = append(gone, fmt.Sprintf("%s: UPS %s no longer powers it", other, name))
			}
		}
		for _, dev := range slices.Sorted(maps.Keys(e.Links.PowerLinks)) {
			if slices.Contains(ids, e.Links.PowerLinks[dev]) {
				delete(e.Links.PowerLinks, dev)
				gone = append(gone, fmt.Sprintf("%s: device %s is no longer linked to it", other, dev))
			}
		}
		o.Agents[other] = e
	}
	return gone
}

// hostOptions reads the overlay out of a state.json object. It never fails: ignored is one line for every
// thing that was left out, and config.yaml's value counts for each of them.
func hostOptions(d statefile.Data) (o HostOptions, ignored []string) {
	o = HostOptions{Agents: map[string]HostOption{}}
	raw, present := d[hostOptionsKey]
	if !present {
		return o, nil
	}
	say := func(format string, a ...any) { ignored = append(ignored, fmt.Sprintf(format, a...)) }
	top, ok := raw.(map[string]any)
	if !ok {
		say("%q is not the object Fairy writes (found %s): none of it is used", hostOptionsKey, kindOf(raw))
		return o, ignored
	}
	if v, has := top["rev"]; has {
		// the counter is only shown; one that is no whole number from 0 up reads as 0
		if f, isNum := v.(float64); isNum && f >= 0 && f <= 1<<52 && f == math.Trunc(f) {
			o.Rev = int(f)
		} else {
			say("%q: \"rev\" is not a whole number (found %s): it counts as 0", hostOptionsKey, kindOf(v))
		}
	}
	for _, k := range slices.Sorted(maps.Keys(top)) {
		if k != "rev" && k != "agents" {
			say("%q: the key %q is not one Fairy writes: it is not used", hostOptionsKey, clipKey(k))
		}
	}
	rawAgents, present := top["agents"]
	if !present {
		return o, ignored
	}
	agents, ok := rawAgents.(map[string]any)
	if !ok {
		say("%q: \"agents\" is not an object (found %s): none of it is used", hostOptionsKey, kindOf(rawAgents))
		return o, ignored
	}
	if len(agents) > MaxHostOptionAgents {
		say("%q has entries for %d agents, more than the %d a panel can have: none of it is used", hostOptionsKey, len(agents), MaxHostOptionAgents)
		return o, ignored
	}
	for _, id := range slices.Sorted(maps.Keys(agents)) {
		if !configutil.ValidID(id) {
			say("%q is not an id an agent can have: its entry is not used", clipKey(id))
			continue
		}
		entry, ok := agents[id].(map[string]any)
		if !ok {
			say("agent %s: its entry is not an object (found %s): it is not used", id, kindOf(agents[id]))
			continue
		}
		e := HostOption{}
		for _, k := range slices.Sorted(maps.Keys(entry)) {
			v := entry[k]
			var l config.Links
			shape := true
			switch k {
			case config.FieldUPSPowers:
				l.UPSPowers, shape = upsPowersOf(v)
			case config.FieldPowerLinks:
				l.PowerLinks, shape = textMapOf(v)
			case config.FieldExclude:
				l.PowerExcludeFromTotal, shape = textListOf(v)
			case "by":
				s, _ := v.(string)
				e.By = clipKey(s)
				continue
			case "at":
				s, _ := v.(string)
				e.At, _ = time.Parse(time.RFC3339, s)
				continue
			default:
				// url, token_env, power_control, ...: no such thing can be set from the panel, whatever the file says
				say("agent %s: %q cannot be set in the panel: it is not used", id, clipKey(k))
				continue
			}
			if !shape {
				say("agent %s: %s is not of the form Fairy writes (found %s): config.yaml's value counts", id, k, kindOf(v))
				continue
			}
			if p := config.LinkProblems(l); len(p) > 0 {
				say("agent %s: %s: config.yaml's value counts", id, p[0].Text)
				continue
			}
			e.Set(k, l)
		}
		if !e.Empty() {
			o.Agents[id] = e
		}
	}
	return o, ignored
}

// kindOf names the kind of a JSON value in a log line (never the value itself).
func kindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "a text"
	case float64:
		return "a number"
	case bool:
		return "true / false"
	case []any:
		return "a list"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("%T", v)
}

// clipKey is a name out of the file as a line quotes it: one line, at most 80 bytes.
func clipKey(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029 {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "?"))
	if len(s) > 80 {
		s = strings.ToValidUTF8(s[:80], "") + "…"
	}
	return s
}

func textListOf(v any) ([]string, bool) {
	l, ok := v.([]any)
	if !ok || len(l) > 4*config.MaxLinkRows { // far over every limit: not looked at entry by entry
		return nil, false
	}
	out := make([]string, 0, len(l))
	for _, x := range l {
		s, ok := x.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

func textMapOf(v any) (map[string]string, bool) {
	m, ok := v.(map[string]any)
	if !ok || len(m) > 4*config.MaxLinkDevices {
		return nil, false
	}
	out := make(map[string]string, len(m))
	for k, x := range m {
		s, ok := x.(string)
		if !ok {
			return nil, false
		}
		out[k] = s
	}
	return out, true
}

func upsPowersOf(v any) (map[string][]string, bool) {
	m, ok := v.(map[string]any)
	if !ok || len(m) > 4*config.MaxLinkUPS {
		return nil, false
	}
	out := make(map[string][]string, len(m))
	for k, x := range m {
		l, ok := textListOf(x)
		if !ok {
			return nil, false
		}
		out[k] = l
	}
	return out, true
}

// encode is the overlay as it is written to state.json.
func (o HostOptions) encode() map[string]any {
	agents := map[string]any{}
	for id, e := range o.Agents {
		if e.Empty() {
			continue
		}
		m := map[string]any{}
		if e.HasUPSPowers {
			ups := map[string]any{}
			for name, rows := range e.Links.UPSPowers {
				ups[name] = toAny(rows)
			}
			m[config.FieldUPSPowers] = ups
		}
		if e.HasPowerLinks {
			links := map[string]any{}
			for dev, to := range e.Links.PowerLinks {
				links[dev] = to
			}
			m[config.FieldPowerLinks] = links
		}
		if e.HasExclude {
			m[config.FieldExclude] = toAny(e.Links.PowerExcludeFromTotal)
		}
		if e.By != "" {
			m["by"] = e.By
		}
		if !e.At.IsZero() {
			m["at"] = e.At.UTC().Format(time.RFC3339)
		}
		agents[id] = m
	}
	return map[string]any{"rev": o.Rev, "agents": agents}
}

func toAny(l []string) []any {
	out := make([]any, len(l))
	for i, s := range l {
		out[i] = s
	}
	return out
}

// HostOptions returns the overlay as the panel uses it right now (none while state.json is unreadable: every
// agent then has config.yaml's values). What the file holds that is not used is said once per change of the
// file, as one WARN line each.
func (a *State) HostOptions() HostOptions {
	o, _ := a.HostOptionsRead()
	return o
}

// HostOptionsRead is HostOptions with the lines about what was left out (for the dialog and for `fairy admin
// config check`).
func (a *State) HostOptionsRead() (HostOptions, []string) {
	if a.state.Err() != nil {
		return HostOptions{Agents: map[string]HostOption{}}, nil
	}
	o, ignored := hostOptions(a.state.Get())
	if len(ignored) > 0 {
		a.hostOptionsWarn(ignored)
	}
	return o, ignored
}

// hostOptionsWarn logs the ignored lines once for each version of the file.
func (a *State) hostOptionsWarn(ignored []string) {
	a.shapeMu.Lock()
	defer a.shapeMu.Unlock()
	if v, logged := a.shapeLog[hostOptionsKey]; logged && v == a.state.Version {
		return
	}
	if a.shapeLog == nil {
		a.shapeLog = map[string]int{}
	}
	a.shapeLog[hostOptionsKey] = a.state.Version
	for _, line := range ignored {
		slog.Warn("state.json: an \"Edit host\" override is ignored: "+line, "path", a.StatePath, "key", hostOptionsKey)
	}
}

// EditHostOptions changes the overlay in one locked read-modify-write of state.json: fn gets what the file
// holds NOW and changes it. Two admins who save at the same moment are run one after the other, each on the
// state the other left: nothing is lost and nothing is half written; of two changes to the same thing the
// later one counts. fn's error stops the write and is returned as it is.
//
// by is who changes it; it and the time are put on every agent's entry that differs afterwards. after is the
// stored state as it is after the call, changed reports whether anything was written. Entries of the file
// that the reader leaves out are dropped by the same write (as SetHostModel does).
func (a *State) EditHostOptions(by string, fn func(o *HostOptions) error) (after HostOptions, changed bool, err error) {
	_, err = statefile.Update(a.StatePath, func(d statefile.Data) error {
		cur, _ := hostOptions(d)
		before := cur.Clone()
		if err := fn(&cur); err != nil {
			return err
		}
		now := time.Now().UTC().Truncate(time.Second)
		for id, e := range cur.Agents {
			if e.Empty() {
				delete(cur.Agents, id)
				continue
			}
			if !sameOption(before.Agents[id], e) {
				e.By, e.At = clipKey(by), now
				cur.Agents[id] = e
			}
		}
		same := len(before.Agents) == len(cur.Agents)
		for id, e := range cur.Agents {
			same = same && sameOption(before.Agents[id], e)
		}
		after = cur
		if same {
			after.Rev = before.Rev
			return errNoChange
		}
		cur.Rev = before.Rev + 1
		after = cur
		d[hostOptionsKey] = cur.encode()
		return nil
	})
	if errors.Is(err, errNoChange) {
		return after, false, nil
	}
	a.state.Refresh()
	if err != nil {
		return HostOptions{}, false, err
	}
	return after, true, nil
}

// sameOption reports whether two entries override the same fields with the same values (who and when aside).
func sameOption(a, b HostOption) bool {
	if a.HasUPSPowers != b.HasUPSPowers || a.HasPowerLinks != b.HasPowerLinks || a.HasExclude != b.HasExclude {
		return false
	}
	return SameLinks(a.Links, b.Links)
}

// SameLinks reports whether two sets of link fields say the same: the same UPSes with the same rows in the
// same order, the same devices linked to the same rows, the same devices left out of the total (in any
// order; nil and empty are the same).
func SameLinks(a, b config.Links) bool {
	if len(a.UPSPowers) != len(b.UPSPowers) || len(a.PowerLinks) != len(b.PowerLinks) {
		return false
	}
	for name, rows := range a.UPSPowers {
		other, ok := b.UPSPowers[name]
		if !ok || !slices.Equal(rows, other) {
			return false
		}
	}
	for dev, to := range a.PowerLinks {
		if other, ok := b.PowerLinks[dev]; !ok || other != to {
			return false
		}
	}
	return slices.Equal(slices.Sorted(slices.Values(a.PowerExcludeFromTotal)), slices.Sorted(slices.Values(b.PowerExcludeFromTotal)))
}
