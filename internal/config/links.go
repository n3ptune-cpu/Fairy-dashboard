package config

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/configutil"
)

// Links are the three link fields of an agent's entry: what the devices that agent reads are attached to.
// They say what is SHOWN where (the UPS chip of a row, the watts of a plug on a row, the total) and with that
// who sees a UPS or a plug. No power action of a machine, no run and no wake reads them. One thing does since
// build 17: the switching of a plug's socket (internal/web, plug.go). The only socket of a plug takes the
// plug's link (power_links), and the panel then asks before it cuts the power of that machine while it runs,
// and locks the socket when the machine is the one that reads the plugs, Home Assistant's or the panel's.
// These are guards against a mistake; what holds against the panel is the agent's own config (feeds_self).
//
//	ups_powers                {<UPS name on the agent>: [<row id>, ...]}
//	power_links               {<device id on the agent>: <row id>}
//	power_exclude_from_total  [<device id on the agent>, ...]
//
// They are written in config.yaml and can be changed, line by line, from the panel ("Edit host": state.json,
// adminstate.HostOptions holds a decision for each changed line, laid over config.yaml's). One check,
// LinkProblems, is used for both.
type Links struct {
	UPSPowers             map[string][]string
	PowerLinks            map[string]string
	PowerExcludeFromTotal []string
}

// Links returns the link fields of the entry as it is written (config.yaml, or none for a host added in the
// panel).
func (a Agent) Links() Links {
	return Links{UPSPowers: a.UPSPowers, PowerLinks: a.PowerLinks, PowerExcludeFromTotal: a.PowerExcludeFromTotal}
}

// The names of the link fields, as config.yaml and state.json spell them.
const (
	FieldUPSPowers  = "ups_powers"
	FieldPowerLinks = "power_links"
	FieldExclude    = "power_exclude_from_total"
)

// LinkFields is every link field, in the order they are listed.
var LinkFields = []string{FieldUPSPowers, FieldPowerLinks, FieldExclude}

// Limits of the link fields. An agent relays at most MaxLinkDevices plugs and MaxLinkUPS UPSes (the limits of
// power.MaxRemoteSources and ups.MaxAgentUPS; a test holds them together), so a longer list names devices
// that cannot exist. MaxLinkRows is how many rows one UPS may be said to power.
const (
	MaxLinkDevices = 16
	MaxLinkUPS     = 16
	MaxLinkRows    = 32
	maxRowID       = 128
)

var (
	// linkUPSNameRe is what the panel takes as the name of a UPS an agent relays (ups.Manager.UpdateFromAgent).
	linkUPSNameRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)
	// linkRowIDRe is the form every row id has: an agent's id, or the id the panel makes for a node or a guest
	// (<id_prefix>node-<name>, <id_prefix>qemu-<vmid>), which can be longer than an agent's id.
	linkRowIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

// ValidUPSName reports whether s can be the name of a UPS an agent relays.
func ValidUPSName(s string) bool { return linkUPSNameRe.MatchString(s) }

// ValidRowID reports whether s has the form of a row's id (it says nothing about whether such a row exists).
func ValidRowID(s string) bool { return len(s) <= maxRowID && linkRowIDRe.MatchString(s) }

// LinkProblem is one thing LinkProblems found. Kind is what it is about: "count" (a list longer than its
// limit), "device" (an id no device can have), "ups" (a name no UPS can have), "row" (an id no row can have)
// or "twice" (an entry that is there twice).
type LinkProblem struct {
	Field string
	Kind  string
	Text  string
}

// LinkProblems is the one check of an agent's link fields. It is used three times:
//
//   - by the panel's API before an override is stored ("Edit host"): any problem refuses the change;
//   - by the reader of the overrides in state.json: a field with a problem is left out, with one WARN line,
//     and config.yaml's value counts (the panel always starts);
//   - by `fairy admin config check` for the fields of config.yaml, as notes. config.yaml is never refused
//     for them: a file that loads today keeps loading (the loader does not call this).
//
// The result is in a fixed order (by field, then by name). nil: nothing to say.
func LinkProblems(l Links) []LinkProblem {
	var out []LinkProblem
	add := func(field, kind, format string, a ...any) {
		out = append(out, LinkProblem{Field: field, Kind: kind, Text: field + ": " + fmt.Sprintf(format, a...)})
	}
	if n := len(l.UPSPowers); n > MaxLinkUPS {
		add(FieldUPSPowers, "count", "%d UPSes, and an agent relays at most %d", n, MaxLinkUPS)
	}
	for _, name := range slices.Sorted(maps.Keys(l.UPSPowers)) {
		rows := l.UPSPowers[name]
		if !ValidUPSName(name) {
			add(FieldUPSPowers, "ups", "%q is not a name a UPS can have (letters, digits and _ . : -, at most 64)", clipID(name))
		}
		if len(rows) > MaxLinkRows {
			add(FieldUPSPowers, "count", "UPS %s powers %d rows, at most %d are taken", clipID(name), len(rows), MaxLinkRows)
		}
		seen := map[string]bool{}
		for _, id := range rows {
			switch {
			case !ValidRowID(id):
				add(FieldUPSPowers, "row", "UPS %s: %q is not an id a row can have", clipID(name), clipID(id))
			case seen[id]:
				add(FieldUPSPowers, "twice", "UPS %s names %s twice", clipID(name), id)
			}
			seen[id] = true
		}
	}
	if n := len(l.PowerLinks); n > MaxLinkDevices {
		add(FieldPowerLinks, "count", "%d devices, and an agent relays at most %d", n, MaxLinkDevices)
	}
	for _, dev := range slices.Sorted(maps.Keys(l.PowerLinks)) {
		if !configutil.ValidID(dev) {
			add(FieldPowerLinks, "device", "%q is not an id a device can have (lowercase letters, digits and dashes)", clipID(dev))
		}
		// "" is "no link" in config.yaml; anything else must have the form of a row's id
		if to := l.PowerLinks[dev]; to != "" && !ValidRowID(to) {
			add(FieldPowerLinks, "row", "device %s: %q is not an id a row can have", clipID(dev), clipID(to))
		}
	}
	if n := len(l.PowerExcludeFromTotal); n > MaxLinkDevices {
		add(FieldExclude, "count", "%d devices, and an agent relays at most %d", n, MaxLinkDevices)
	}
	seen := map[string]bool{}
	for _, dev := range slices.Sorted(slices.Values(l.PowerExcludeFromTotal)) {
		switch {
		case !configutil.ValidID(dev):
			add(FieldExclude, "device", "%q is not an id a device can have (lowercase letters, digits and dashes)", clipID(dev))
		case seen[dev]:
			add(FieldExclude, "twice", "%s is there twice", dev)
		}
		seen[dev] = true
	}
	return out
}

// clipID is an id or name as a problem's sentence quotes it: at most 80 bytes of it. What is quoted comes from
// a file a person edited (or from state.json), so it can be anything; %q makes it one line.
func clipID(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

// Clone returns a copy of l that shares nothing with it. A field that is nil stays nil, and one that is
// empty stays empty (not nil): for an override the two differ ("not set here" and "set here to nothing").
func (l Links) Clone() Links {
	out := Links{}
	if l.UPSPowers != nil {
		out.UPSPowers = make(map[string][]string, len(l.UPSPowers))
		for k, v := range l.UPSPowers {
			out.UPSPowers[k] = append([]string{}, v...)
		}
	}
	if l.PowerLinks != nil {
		out.PowerLinks = make(map[string]string, len(l.PowerLinks))
		for k, v := range l.PowerLinks {
			out.PowerLinks[k] = v
		}
	}
	if l.PowerExcludeFromTotal != nil {
		out.PowerExcludeFromTotal = append([]string{}, l.PowerExcludeFromTotal...)
	}
	return out
}

// SocketLinkName splits the name a socket has in socket_links, "<device id>/<socket id>", into the id of the
// plug on the agent and the id of the socket; ok is false for anything that cannot be such a name (the forms
// are the agent's own: a plug with sockets has an id of at most 40 characters, a socket one of at most 16).
func SocketLinkName(name string) (dev, socket string, ok bool) {
	dev, socket, found := strings.Cut(name, "/")
	if !found || api.CheckSocketSource(dev) != nil || !configutil.ValidID(dev) || api.CheckSocketID(socket) != nil {
		return "", "", false
	}
	return dev, socket, true
}

// MaxSocketLinks is how many sockets an agent's entry can link: every socket of every plug it can relay.
const MaxSocketLinks = MaxLinkDevices * 8

// SocketLinkProblems is the check of an agent's socket_links and home_assistant_system for what can never
// work, whatever the machines are: a name that is not "<device id>/<socket id>", a row id that no row can
// have, a socket linked to nothing (the line is then left out instead), too many lines. Unlike the three link
// fields (LinkProblems), these are refused when config.yaml is loaded: they are new with build 17, so no file
// that loads today has them, and a line that cannot work would leave a socket without the panel's question
// "this machine is still running" and without its locks, with nothing on the page to say so. Whether a row
// or a socket with such an id exists is not known here (`fairy admin config check` has notes; the running
// panel says in its log when the agent reports no such socket).
func SocketLinkProblems(a Agent) []string {
	var out []string
	if n := len(a.SocketLinks); n > MaxSocketLinks {
		out = append(out, fmt.Sprintf("socket_links: %d sockets, and an agent relays at most %d", n, MaxSocketLinks))
	}
	for _, name := range slices.Sorted(maps.Keys(a.SocketLinks)) {
		if _, _, ok := SocketLinkName(name); !ok {
			out = append(out, fmt.Sprintf("socket_links: %q is not \"<device id>/<socket id>\" (the ids the agent's own config gives the plug and its socket: "+
				"lowercase letters, digits and dashes)", clipID(name)))
			continue
		}
		switch to := a.SocketLinks[name]; {
		case to == "":
			out = append(out, fmt.Sprintf("socket_links: %s is linked to nothing: name the row of the machine that hangs on it, or leave the line out", name))
		case !ValidRowID(to):
			out = append(out, fmt.Sprintf("socket_links: %s: %q is not an id a row can have", name, clipID(to)))
		}
	}
	if h := a.HomeAssistantSystem; h != "" && !ValidRowID(h) {
		out = append(out, fmt.Sprintf("home_assistant_system: %q is not an id a row can have", clipID(h)))
	}
	return out
}
