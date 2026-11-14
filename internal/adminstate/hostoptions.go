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
//	    "ups_powers": {"<UPS name>": {"<row id>": true | false}},
//	    "power_links": {"<device id>": "<row id>" | ""},
//	    "power_exclude_from_total": {"<device id>": true | false},
//	    "by": "<who>", "at": "<RFC 3339>"}}}
//
// Keyed by the id of the agent that READS the devices: that is where config.yaml has these fields. What is
// stored is not a copy of a field but single DECISIONS, each about one line of the dialog, laid over
// config.yaml's value of that line:
//
//   - ups_powers: this UPS powers this row (true), or does not (false), whatever config.yaml says about the two;
//   - power_links: this device is linked to this row, or to none (""), whatever config.yaml says about the device;
//   - power_exclude_from_total: this device is left out of the total (true), or counted (false).
//
// Everything no decision is about follows config.yaml, also when config.yaml is edited later: a change made
// for one machine does not freeze what config.yaml says about another. "Reset" takes one decision out. Only
// these three fields exist here: nothing that says where an agent is, which token it has or what the panel may
// do to a machine can be in the overlay, and a key with another name is not read.
//
// A key this build does not know (at the top, or in an agent's entry) is not used either, and it is kept: a
// newer build wrote it, and a save by this one writes it back as it was (HostOptions.unknown).
//
// The reader never fails. Whatever is not what Fairy writes is left out, config.yaml's value counts for it,
// and one WARN line says so (once): the panel always starts. `fairy check-config`, which a
// `fairy update` asks before it installs a version, does not read state.json at all.

// hostOptionsKey is the name of the object in state.json.
const hostOptionsKey = "host_options"

// MaxHostOptionAgents is how many agents may have overrides. A panel has far fewer agents; a longer list is
// not something Fairy wrote, and none of it is used.
const MaxHostOptionAgents = 256

// HostOption is what was changed about the devices one agent reads: the decisions, by field. A nil map and
// an empty one are the same: no decision.
type HostOption struct {
	UPS     map[string]map[string]bool // UPS name -> row id -> the UPS powers that row (true) or does not (false)
	Links   map[string]string          // device id -> the row it is linked to ("": to none)
	Exclude map[string]bool            // device id -> left out of the total (true) or counted (false)
	By      string                     // who changed it last (the audit log's name for them)
	At      time.Time
}

// Empty reports whether there is no decision.
func (o HostOption) Empty() bool { return o.Count() == 0 }

// Count is the number of decisions.
func (o HostOption) Count() int {
	n := len(o.Links) + len(o.Exclude)
	for _, rows := range o.UPS {
		n += len(rows)
	}
	return n
}

// Has reports whether there is a decision in the named field (config.Field...).
func (o HostOption) Has(field string) bool {
	switch field {
	case config.FieldUPSPowers:
		for _, rows := range o.UPS {
			if len(rows) > 0 {
				return true
			}
		}
	case config.FieldPowerLinks:
		return len(o.Links) > 0
	case config.FieldExclude:
		return len(o.Exclude) > 0
	}
	return false
}

// Clone returns a copy that shares nothing with o.
func (o HostOption) Clone() HostOption {
	out := HostOption{By: o.By, At: o.At}
	for name, rows := range o.UPS {
		if len(rows) == 0 {
			continue
		}
		if out.UPS == nil {
			out.UPS = map[string]map[string]bool{}
		}
		out.UPS[name] = maps.Clone(rows)
	}
	if len(o.Links) > 0 {
		out.Links = maps.Clone(o.Links)
	}
	if len(o.Exclude) > 0 {
		out.Exclude = maps.Clone(o.Exclude)
	}
	return out
}

// Apply returns the links that count for an agent whose entry says base: base, with every decision laid
// over it. What no decision is about is base's. The result shares nothing with base or o.
func (o HostOption) Apply(base config.Links) config.Links {
	out := base.Clone()
	for _, name := range slices.Sorted(maps.Keys(o.UPS)) {
		rows := o.UPS[name]
		for _, id := range slices.Sorted(maps.Keys(rows)) {
			switch has := slices.Contains(out.UPSPowers[name], id); {
			case rows[id] && !has:
				if out.UPSPowers == nil {
					out.UPSPowers = map[string][]string{}
				}
				out.UPSPowers[name] = append(out.UPSPowers[name], id)
			case !rows[id] && has:
				out.UPSPowers[name] = slices.DeleteFunc(out.UPSPowers[name], func(x string) bool { return x == id })
			}
		}
	}
	for dev, to := range o.Links {
		if to == "" {
			delete(out.PowerLinks, dev)
			continue
		}
		if out.PowerLinks == nil {
			out.PowerLinks = map[string]string{}
		}
		out.PowerLinks[dev] = to
	}
	for _, dev := range slices.Sorted(maps.Keys(o.Exclude)) {
		switch has := slices.Contains(out.PowerExcludeFromTotal, dev); {
		case o.Exclude[dev] && !has:
			out.PowerExcludeFromTotal = append(out.PowerExcludeFromTotal, dev)
		case !o.Exclude[dev] && has:
			out.PowerExcludeFromTotal = slices.DeleteFunc(out.PowerExcludeFromTotal, func(x string) bool { return x == dev })
		}
	}
	return out
}

// SetUPS decides that the UPS powers the row (on) or does not, whatever config.yaml says.
func (o *HostOption) SetUPS(name, row string, on bool) {
	if o.UPS == nil {
		o.UPS = map[string]map[string]bool{}
	}
	if o.UPS[name] == nil {
		o.UPS[name] = map[string]bool{}
	}
	o.UPS[name][row] = on
}

// DropUPS takes the decision about the UPS and the row out (config.yaml counts for the two again). It
// reports whether there was one.
func (o *HostOption) DropUPS(name, row string) bool {
	_, had := o.UPS[name][row]
	delete(o.UPS[name], row)
	if len(o.UPS[name]) == 0 {
		delete(o.UPS, name)
	}
	return had
}

// SetLink decides that the device is linked to the row to ("": to none), whatever config.yaml says.
func (o *HostOption) SetLink(dev, to string) {
	if o.Links == nil {
		o.Links = map[string]string{}
	}
	o.Links[dev] = to
}

// DropLink takes the decision about the device's link out. It reports whether there was one.
func (o *HostOption) DropLink(dev string) bool {
	_, had := o.Links[dev]
	delete(o.Links, dev)
	return had
}

// SetExclude decides that the device is left out of the total (out) or counted, whatever config.yaml says.
func (o *HostOption) SetExclude(dev string, out bool) {
	if o.Exclude == nil {
		o.Exclude = map[string]bool{}
	}
	o.Exclude[dev] = out
}

// DropExclude takes the decision about the device's place in the total out. It reports whether there was one.
func (o *HostOption) DropExclude(dev string) bool {
	_, had := o.Exclude[dev]
	delete(o.Exclude, dev)
	return had
}

// Named is every UPS, row and device the decisions name, in the form of an agent's link fields: for the one
// check of those fields (config.LinkProblems), which then checks the names, the ids and how many there are.
func (o HostOption) Named() config.Links {
	var l config.Links
	for name, rows := range o.UPS {
		if l.UPSPowers == nil {
			l.UPSPowers = map[string][]string{}
		}
		l.UPSPowers[name] = slices.Sorted(maps.Keys(rows))
	}
	if len(o.Links) > 0 {
		l.PowerLinks = maps.Clone(o.Links)
	}
	l.PowerExcludeFromTotal = slices.Sorted(maps.Keys(o.Exclude))
	return l
}

// HostOptions is the whole overlay. Rev counts the writes (it only ever grows): two answers with the same
// Rev show the same stored state.
type HostOptions struct {
	Rev    int
	Agents map[string]HostOption
	// unknown is what the file holds under keys this build does not know: at the top of "host_options" (top),
	// and in the entries of agents (agents: agent id -> key -> value; also of an agent that has nothing else).
	// None of it is ever used. It is carried from the read to the write as it is, never looked into and never
	// changed, so that a save by this build does not drop what a newer build wrote.
	unknown struct {
		top    map[string]any
		agents map[string]map[string]any
	}
}

// Clone returns a copy that shares nothing with o (the unknown keys, which nothing changes, are the same).
func (o HostOptions) Clone() HostOptions {
	out := HostOptions{Rev: o.Rev, Agents: make(map[string]HostOption, len(o.Agents)), unknown: o.unknown}
	out.unknown.agents = maps.Clone(o.unknown.agents)
	for id, e := range o.Agents {
		out.Agents[id] = e.Clone()
	}
	return out
}

// Drop takes the entry of an agent out whole: its changes, and what this build does not know in it. For an
// id that is no agent's any more, and for a host that is removed: all of the entry was about that machine.
func (o *HostOptions) Drop(agentID string) {
	delete(o.Agents, agentID)
	delete(o.unknown.agents, agentID)
}

// For returns the overrides of one agent (the zero value: none).
func (o HostOptions) For(agentID string) HostOption { return o.Agents[agentID] }

// Forget is what "Remove" of a host does to the overlay: the overrides of the host's own agent go, and so
// does every decision about one of the host's rows in the overrides of OTHER agents (a UPS that powers it or
// not, a plug that is linked to it). A host's id is made from its name, so without this the next machine
// added under the same name would be shown on a UPS, and with a plug's watts, that nobody linked to it. ids
// are the rows that go away with the host, besides its agent's own. It reports what was taken out, one line
// each.
func (o *HostOptions) Forget(agentID string, ids ...string) []string {
	var gone []string
	if e := o.Agents[agentID]; !e.Empty() {
		gone = append(gone, "the overrides of "+agentID)
	}
	o.Drop(agentID)
	ids = append(ids, agentID)
	for _, other := range slices.Sorted(maps.Keys(o.Agents)) {
		e := o.Agents[other].Clone()
		for _, name := range slices.Sorted(maps.Keys(e.UPS)) {
			powered, changed := false, false
			for _, id := range ids {
				on := e.UPS[name][id]
				if e.DropUPS(name, id) {
					powered, changed = powered || on, true
				}
			}
			switch {
			case powered:
				gone = append(gone, fmt.Sprintf("%s: UPS %s no longer powers it", other, name))
			case changed: // "does not power it, whatever config.yaml says": nothing that shows goes, but it was about the host
				gone = append(gone, fmt.Sprintf("%s: what was changed about UPS %s and it", other, name))
			}
		}
		for _, dev := range slices.Sorted(maps.Keys(e.Links)) {
			if slices.Contains(ids, e.Links[dev]) {
				e.DropLink(dev)
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
			say("%q: the key %q is not one Fairy writes: it is not used (and left in the file as it is)", hostOptionsKey, clipKey(k))
			if o.unknown.top == nil {
				o.unknown.top = map[string]any{}
			}
			o.unknown.top[k] = top[k]
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
			var part HostOption
			shape := true
			switch k {
			case config.FieldUPSPowers:
				part.UPS, shape = upsDecisionsOf(v)
			case config.FieldPowerLinks:
				part.Links, shape = textMapOf(v)
			case config.FieldExclude:
				part.Exclude, shape = boolMapOf(v, 4*config.MaxLinkDevices)
			case "by":
				s, _ := v.(string)
				e.By = clipKey(s)
				continue
			case "at":
				s, _ := v.(string)
				e.At, _ = time.Parse(time.RFC3339, s)
				continue
			default:
				// url, token_env, power_control, ...: no such thing can be set from the panel, whatever the file says.
				// Nothing reads it; it stays in the file (a newer build may have written it).
				say("agent %s: %q cannot be set in the panel: it is not used (and left in the file as it is)", id, clipKey(k))
				if o.unknown.agents == nil {
					o.unknown.agents = map[string]map[string]any{}
				}
				if o.unknown.agents[id] == nil {
					o.unknown.agents[id] = map[string]any{}
				}
				o.unknown.agents[id][k] = v
				continue
			}
			if !shape {
				say("agent %s: %s is not of the form Fairy writes (found %s): config.yaml's value counts", id, k, kindOf(v))
				continue
			}
			if p := config.LinkProblems(part.Named()); len(p) > 0 {
				say("agent %s: %s: config.yaml's value counts", id, p[0].Text)
				continue
			}
			switch k {
			case config.FieldUPSPowers:
				e.UPS = part.UPS
			case config.FieldPowerLinks:
				e.Links = part.Links
			case config.FieldExclude:
				e.Exclude = part.Exclude
			}
		}
		if e = e.Clone(); !e.Empty() {
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

func textMapOf(v any) (map[string]string, bool) {
	m, ok := v.(map[string]any)
	if !ok || len(m) > 4*config.MaxLinkDevices { // far over every limit: not looked at entry by entry
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

func boolMapOf(v any, limit int) (map[string]bool, bool) {
	m, ok := v.(map[string]any)
	if !ok || len(m) > limit {
		return nil, false
	}
	out := make(map[string]bool, len(m))
	for k, x := range m {
		b, ok := x.(bool)
		if !ok {
			return nil, false
		}
		out[k] = b
	}
	return out, true
}

func upsDecisionsOf(v any) (map[string]map[string]bool, bool) {
	m, ok := v.(map[string]any)
	if !ok || len(m) > 4*config.MaxLinkUPS {
		return nil, false
	}
	out := make(map[string]map[string]bool, len(m))
	for k, x := range m {
		rows, ok := boolMapOf(x, 4*config.MaxLinkRows)
		if !ok {
			return nil, false
		}
		out[k] = rows
	}
	return out, true
}

// encode is the overlay as it is written to state.json: what this build knows, and next to it, untouched,
// what it read under keys it does not know.
func (o HostOptions) encode() map[string]any {
	agents := map[string]any{}
	for id, e := range o.Agents {
		if e.Empty() {
			continue
		}
		agents[id] = e.encode()
	}
	for id, keys := range o.unknown.agents {
		entry, _ := agents[id].(map[string]any)
		if entry == nil {
			entry = map[string]any{}
			agents[id] = entry
		}
		for k, v := range keys { // never one of the keys encode writes: the reader takes those as its own
			entry[k] = v
		}
	}
	out := map[string]any{}
	for k, v := range o.unknown.top {
		out[k] = v
	}
	out["rev"], out["agents"] = o.Rev, agents
	return out
}

func (e HostOption) encode() map[string]any {
	m := map[string]any{}
	if e.Has(config.FieldUPSPowers) {
		ups := map[string]any{}
		for name, rows := range e.UPS {
			if len(rows) > 0 {
				ups[name] = boolsToAny(rows)
			}
		}
		m[config.FieldUPSPowers] = ups
	}
	if len(e.Links) > 0 {
		links := map[string]any{}
		for dev, to := range e.Links {
			links[dev] = to
		}
		m[config.FieldPowerLinks] = links
	}
	if len(e.Exclude) > 0 {
		m[config.FieldExclude] = boolsToAny(e.Exclude)
	}
	if e.By != "" {
		m["by"] = e.By
	}
	if !e.At.IsZero() {
		m["at"] = e.At.UTC().Format(time.RFC3339)
	}
	return m
}

func boolsToAny(m map[string]bool) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
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

// hostOptionsWarn logs each ignored line once. A line is said again only after the panel was started again:
// what is under a key this build does not know stays in the file through every save (and every other write
// to state.json), and would otherwise be said again each time.
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
	if len(a.hostOptionsSaid) > 1024 { // a file that changes its broken parts for ever: the memory of it stays small
		a.hostOptionsSaid = nil
	}
	for _, line := range ignored {
		if a.hostOptionsSaid[line] {
			continue
		}
		if a.hostOptionsSaid == nil {
			a.hostOptionsSaid = map[string]bool{}
		}
		a.hostOptionsSaid[line] = true
		slog.Warn("state.json: an \"Edit host\" override is ignored: "+line, "path", a.StatePath, "key", hostOptionsKey)
	}
}

// EditHostOptions changes the overlay in one locked read-modify-write of state.json: fn gets what the file
// holds NOW and changes it. Two admins who save at the same moment are run one after the other, each on the
// state the other left: nothing is lost and nothing is half written; of two changes to the same thing the
// later one counts. fn's error stops the write and is returned as it is.
//
// by is who changes it; it and the time are put on every agent's entry that differs afterwards. after is the
// stored state as it is after the call, changed reports whether anything was written. What the reader leaves
// out because it is not of the form Fairy writes (an entry that is no object, a field of another form) is
// dropped by the same write, as SetHostModel does; what is under a key this build does not know is written
// back as it was.
func (a *State) EditHostOptions(by string, fn func(o *HostOptions) error) (after HostOptions, changed bool, err error) {
	_, err = statefile.Update(a.StatePath, func(d statefile.Data) error {
		cur, _ := hostOptions(d)
		before := cur.Clone()
		if err := fn(&cur); err != nil {
			return err
		}
		now := time.Now().UTC().Truncate(time.Second)
		for id, e := range cur.Agents {
			if e = e.Clone(); e.Empty() { // Clone: without the UPSes no decision is left about
				delete(cur.Agents, id)
				continue
			}
			if !sameOption(before.Agents[id], e) {
				e.By, e.At = clipKey(by), now
			}
			cur.Agents[id] = e
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

// Same reports whether two entries hold the same decisions (who and when aside).
func (o HostOption) Same(b HostOption) bool {
	a, b := o.Clone(), b.Clone() // Clone: a UPS no decision is left about does not count
	if len(a.UPS) != len(b.UPS) {
		return false
	}
	for name, rows := range a.UPS {
		if !maps.Equal(rows, b.UPS[name]) {
			return false
		}
	}
	return maps.Equal(a.Links, b.Links) && maps.Equal(a.Exclude, b.Exclude)
}

func sameOption(a, b HostOption) bool { return a.Same(b) }

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
