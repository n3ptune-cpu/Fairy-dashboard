package adminstate

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/configutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// "Edit host", the machine itself: its name, its Wake-on-LAN details and its own two timings of "Update
// unpowered hosts", changed in the panel. They are kept next to the link decisions (hostoptions.go), in the
// same object of state.json and under the same rules:
//
//	"host_options": {"rev": 8, "agents": {...}, "hosts": {"<machine id>": {
//	    "name": "<display name>",
//	    "wol": {"macs": ["aa:bb:cc:dd:ee:ff"], "relay": "<agent id>", "port": 7 | 9}  or  {"off": true},
//	    "wake_update": {"off_after_s": 300..3600, "shutdown_delay_s": 10..3600},
//	    "by": "<who>", "at": "<RFC 3339>"}}}
//
// <machine id> is the id of the machine's agent, or the id of its row for a Proxmox node without an agent
// (poller.HostEdit finds an entry under every id the machine has). Each key is one DECISION laid over what
// the configuration says (config.yaml's entry, or the one "Add host" made):
//
//   - name: the machine is shown under this name;
//   - wol.macs: the wake packet goes to these network cards (instead of mac and macs);
//   - wol.relay: this agent sends it; wol.port: to this port;
//   - wol.off: the machine has no Wake-on-LAN, whatever the configuration says;
//   - wake_update.off_after_s, wake_update.shutdown_delay_s: the machine's own timing in a run (RunEdit),
//     instead of the one its systems: entry gives it in config.yaml, or of the panel's where it has none.
//
// What no decision is about follows the configuration. "Reset" takes a decision out. NOTHING else can be
// here: where a wake packet is sent to (broadcast) and the password that goes with it (password_env) are the
// configuration's alone, like an agent's address and tokens; a key with another name is not read (and is
// kept: a newer build wrote it).
//
// The site of a machine is not here: it is the assignment of the Users page (users.json).

// MaxHostName is how many characters a host's name may have.
const MaxHostName = 64

// MaxWolMACs is how many network cards one machine's Wake-on-LAN can name (config.yaml's limit for macs).
const MaxWolMACs = 8

// HostName checks a host's display name as a person typed it and returns it as it is stored (the rules of a
// custom model name: one line, no control or invisible characters, at most MaxHostName characters). "": no
// name.
//
// One more rule than a model name has, because a host's name is typed to confirm a power action and must not
// be taken for another host's: a word of it is written in ONE alphabet. A word is a run of letters: it ends
// at a space, a hyphen, a digit or any other sign. So "Сервер-NAS", "ПК-Dell" and "Büro ПК" are names (a
// Cyrillic word and a Latin word), and "Dоn" with a Cyrillic о in a Latin word is refused: nobody writes it
// by accident, and it reads as "Don". "μServer" with the Greek letter mu is refused by the same rule (one word,
// two alphabets), and the message names the word; the micro sign "µ" of a keyboard is no Greek letter and is
// taken.
//
// A word that is all of another alphabet next to a Latin one ("PC-А" with a Cyrillic А) is a name by this
// rule. Whether it is another host's name is not asked here: NameKey makes it the same name as "PC-A", and
// the API that knows the hosts refuses it then.
func HostName(raw string) (string, error) {
	name, err := labelText(raw, "name", MaxHostName)
	if err != nil {
		return "", err
	}
	for _, word := range strings.FieldsFunc(name, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.Is(unicode.M, r) }) {
		latin, other := false, false
		for _, r := range word {
			latin = latin || unicode.Is(unicode.Latin, r)
			other = other || unicode.In(r, unicode.Cyrillic, unicode.Greek)
		}
		if latin && other {
			return "", fmt.Errorf("name mixes Latin letters with Cyrillic or Greek ones in one word (%q): write the word in one alphabet", word)
		}
	}
	return name, nil
}

// nameLookalikes are the Cyrillic and Greek letters (in lower case) that are drawn like a Latin letter in one
// of their two cases, with that Latin letter. The standard library has no table of look-alikes; this one is
// the letters of the two alphabets a keyboard in Europe types, and it is used for one thing only: NameKey.
var nameLookalikes = map[rune]rune{
	'а': 'a', 'в': 'b', 'е': 'e', 'к': 'k', 'м': 'm', 'н': 'h', 'о': 'o', 'р': 'p', 'с': 'c', 'т': 't', 'у': 'y', 'х': 'x',
	'і': 'i', 'ј': 'j', 'ѕ': 's', 'ԁ': 'd', 'ԛ': 'q', 'ԝ': 'w',
	'α': 'a', 'β': 'b', 'ε': 'e', 'ζ': 'z', 'η': 'h', 'ι': 'i', 'κ': 'k', 'μ': 'm', 'ν': 'n', 'ο': 'o', 'ρ': 'p', 'τ': 't', 'υ': 'y', 'χ': 'x', 'ϲ': 'c',
}

// NameKey is the form of a name under which two names count as ONE name: what a person reading a row, or
// typing a name to confirm, cannot tell apart. Two hosts must not have names with the same key.
//
// Folded: upper and lower case; spaces at the ends and runs of spaces (every kind of space); every dash and
// hyphen (the hyphen-minus, U+2010 to U+2015, the minus sign, the small and the full-width ones) to "-"; the
// full-width forms of ASCII to ASCII; and the Cyrillic and Greek letters that are drawn like a Latin letter
// to that letter (nameLookalikes), so that "РС-А" in Cyrillic is "PC-A".
//
// NOT folded, on purpose: an accent ("PC-Á" is another name than "PC-A": it is another word in the languages
// that write it) and a dot or any other sign at the end ("PC-A." is another name). Both can be told apart on
// the page, and typing one for the other does not confirm.
func NameKey(name string) string {
	var b strings.Builder
	for _, r := range strings.Join(strings.FieldsFunc(name, unicode.IsSpace), " ") {
		if r >= 0xff01 && r <= 0xff5e { // full-width ASCII
			r -= 0xfee0
		}
		switch {
		case unicode.Is(unicode.Pd, r), r == 0x2212, r == 0x2043, r == 0x02d7:
			r = '-'
		default:
			r = unicode.ToLower(unicode.ToUpper(r)) // both ways: a letter whose lower case has two forms is one
			if l, ok := nameLookalikes[r]; ok {
				r = l
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}

var wolMACRe = regexp.MustCompile(`^[0-9a-f]{2}(:[0-9a-f]{2}){5}$`)

// WolMACs checks the MAC addresses of a Wake-on-LAN decision and returns them as they are stored: lower
// case with ":" (aa-bb-.. and AA:BB:.. are taken), 1 to MaxWolMACs of them, each once, none of them the
// address of no network card: all zero, or a group address (the lowest bit of the first byte is set:
// multicast, and the broadcast address ff:ff:ff:ff:ff:ff). A network card's own address never has that bit,
// so a wake packet "for" such an address wakes nothing, and nothing good is meant by one.
func WolMACs(list []string) ([]string, error) {
	if len(list) == 0 {
		return nil, errors.New("Wake-on-LAN needs at least one MAC address")
	}
	if len(list) > MaxWolMACs {
		return nil, fmt.Errorf("at most %d MAC addresses", MaxWolMACs)
	}
	out := make([]string, 0, len(list))
	for _, raw := range list {
		m := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(raw), "-", ":"))
		if !wolMACRe.MatchString(m) {
			return nil, errors.New("a MAC address must look like aa:bb:cc:dd:ee:ff")
		}
		if first, _ := strconv.ParseUint(m[:2], 16, 8); m == "00:00:00:00:00:00" || first&1 == 1 {
			return nil, errors.New(m + " is not the address of a network card")
		}
		if slices.Contains(out, m) {
			return nil, errors.New("a MAC address is there twice")
		}
		out = append(out, m)
	}
	return out, nil
}

// WolEdit is the decisions about one machine's Wake-on-LAN. A zero field is no decision.
type WolEdit struct {
	MACs  []string // the network cards a wake packet goes to
	Relay string   // the agent that sends it
	Port  int      // 7 or 9
	Off   bool     // no Wake-on-LAN for this machine (then nothing else is set)
}

// Empty reports whether there is no decision.
func (w WolEdit) Empty() bool { return len(w.MACs) == 0 && w.Relay == "" && w.Port == 0 && !w.Off }

// Count is the number of decisions.
func (w WolEdit) Count() int {
	n := 0
	for _, set := range []bool{len(w.MACs) > 0, w.Relay != "", w.Port != 0, w.Off} {
		if set {
			n++
		}
	}
	return n
}

// Clone returns a copy that shares nothing with w.
func (w WolEdit) Clone() WolEdit {
	w.MACs = slices.Clone(w.MACs)
	return w
}

// Same reports whether two hold the same decisions.
func (w WolEdit) Same(b WolEdit) bool {
	return slices.Equal(w.MACs, b.MACs) && w.Relay == b.Relay && w.Port == b.Port && w.Off == b.Off
}

// Check reports what is wrong with the decisions by themselves (nil: nothing): the form of each, and that
// "off" stands alone. Whether the relay is an agent that may send wake packets is the panel's to know
// (poller.WolOf).
func (w WolEdit) Check() error {
	if w.Off {
		if len(w.MACs) > 0 || w.Relay != "" || w.Port != 0 {
			return errors.New("\"off\" stands alone")
		}
		return nil
	}
	if len(w.MACs) > 0 {
		macs, err := WolMACs(w.MACs)
		if err != nil {
			return err
		}
		if !slices.Equal(macs, w.MACs) {
			return errors.New("a MAC address is not written as aa:bb:cc:dd:ee:ff in lower case")
		}
	}
	if w.Relay != "" && !configutil.ValidID(w.Relay) {
		return errors.New("the relay is not an id an agent can have")
	}
	if w.Port != 0 && w.Port != 7 && w.Port != 9 {
		return errors.New("the port must be 7 or 9")
	}
	return nil
}

// WolRelayFixed says why the relay of a machine's Wake-on-LAN cannot be changed in the panel ("": it can).
// config.yaml can give a wake packet a target address (broadcast) and a password (password_env). Both were
// written for the relay that stands next to them: another relay would send a packet to that address, or
// that password, into a network they were not meant for. Neither can be changed from the browser, so the
// relay of such an entry stays what config.yaml says.
func WolRelayFixed(base *config.Wol) string {
	switch {
	case base == nil:
		return ""
	case base.Broadcast != "" && base.PasswordEnv != "":
		return "config.yaml sets a target address and a password for this machine's wake packet: they belong to the relay written there"
	case base.Broadcast != "":
		return "config.yaml sets a target address for this machine's wake packet: it belongs to the relay written there"
	case base.PasswordEnv != "":
		return "config.yaml sets a password for this machine's wake packet: it belongs to the relay written there"
	}
	return ""
}

// Apply returns the Wake-on-LAN that counts for a machine whose configuration says base (nil: it has none):
// base with the decisions laid over it; nil when the machine then has none ("off"). unused says why the
// decisions cannot be used ("": they are): the result is then base, as if nothing were decided. They are
// used whole or not at all, so that a packet is never sent to half of what somebody decided.
func (w WolEdit) Apply(base *config.Wol) (out *config.Wol, unused string) {
	keep := func(why string) (*config.Wol, string) {
		if base == nil {
			return nil, why
		}
		b := *base
		b.MACs = slices.Clone(base.MACs)
		return &b, why
	}
	if w.Empty() {
		return keep("")
	}
	if err := w.Check(); err != nil {
		return keep(err.Error())
	}
	if w.Off {
		return nil, ""
	}
	if w.Relay != "" && base != nil && w.Relay != base.Relay {
		if why := WolRelayFixed(base); why != "" {
			return keep(why)
		}
	}
	e := config.Wol{}
	if base != nil {
		e = *base
		e.MACs = slices.Clone(base.MACs)
	}
	if len(w.MACs) > 0 {
		e.MAC, e.MACs = "", slices.Clone(w.MACs)
	}
	if w.Relay != "" {
		e.Relay = w.Relay
	}
	if w.Port != 0 {
		e.Port = w.Port
	}
	switch {
	case len(e.AllMACs()) == 0:
		return keep("no MAC address is set for it")
	case e.Relay == "":
		return keep("no relay is set for it")
	}
	return &e, ""
}

// RunEdit is the decisions about one machine's own timings of "Update unpowered hosts": what the wake_update:
// block of its systems: entry sets in config.yaml (config.MachineRunTimings has the two keys and their
// ranges, which are the panel's). 0: no decision. A value outside its range is never a decision: the reader
// leaves it out, and Check refuses it before it is written.
type RunEdit struct {
	OffAfterS      int // the machine counts as off after this long without an answer
	ShutdownDelayS int // the countdown of the shutdown a run asks it for
}

// Empty reports whether there is no decision.
func (r RunEdit) Empty() bool { return r.OffAfterS == 0 && r.ShutdownDelayS == 0 }

// Value is the decision about one of the two keys (0: none, or no such key).
func (r RunEdit) Value(key string) int {
	switch key {
	case config.KeyOffAfter:
		return r.OffAfterS
	case config.KeyShutdownDelay:
		return r.ShutdownDelayS
	}
	return 0
}

// Set decides v for one of the two keys (0: no decision; a key that is none of them: nothing).
func (r *RunEdit) Set(key string, v int) {
	switch key {
	case config.KeyOffAfter:
		r.OffAfterS = v
	case config.KeyShutdownDelay:
		r.ShutdownDelayS = v
	}
}

// Count is the number of decisions.
func (r RunEdit) Count() int {
	n := 0
	for _, t := range config.MachineRunTimings() {
		if r.Value(t.Key) != 0 {
			n++
		}
	}
	return n
}

// Check reports what is wrong with the decisions (nil: nothing): each is a value its key can have.
func (r RunEdit) Check() error {
	for _, t := range config.MachineRunTimings() {
		if v := r.Value(t.Key); v != 0 && !t.In(v) {
			return fmt.Errorf("%s must be %d to %d", t.Key, t.Min, t.Max)
		}
	}
	return nil
}

// HostEdit is what was changed about one machine itself. A zero field is no decision.
type HostEdit struct {
	Name string   // the name it is shown under
	Wol  *WolEdit // its Wake-on-LAN (nil: nothing decided)
	Run  RunEdit  // its own timings of "Update unpowered hosts"
	By   string   // who changed it last (the audit log's name for them)
	At   time.Time
}

// Empty reports whether there is no decision.
func (e HostEdit) Empty() bool { return e.Count() == 0 }

// Count is the number of decisions.
func (e HostEdit) Count() int {
	n := 0
	if e.Name != "" {
		n++
	}
	if e.Wol != nil {
		n += e.Wol.Count()
	}
	return n + e.Run.Count()
}

// Clone returns a copy that shares nothing with e (a Wake-on-LAN without a decision is none).
func (e HostEdit) Clone() HostEdit {
	if e.Wol != nil {
		if e.Wol.Empty() {
			e.Wol = nil
		} else {
			w := e.Wol.Clone()
			e.Wol = &w
		}
	}
	return e
}

// Same reports whether two entries hold the same decisions (who and when aside).
func (e HostEdit) Same(b HostEdit) bool {
	x, y := e.Clone(), b.Clone()
	if x.Name != y.Name || x.Run != y.Run || (x.Wol == nil) != (y.Wol == nil) {
		return false
	}
	return x.Wol == nil || x.Wol.Same(*y.Wol)
}

func (e HostEdit) encode() map[string]any {
	m := map[string]any{}
	if e.Name != "" {
		m["name"] = e.Name
	}
	if e.Wol != nil && !e.Wol.Empty() {
		w := map[string]any{}
		if e.Wol.Off {
			w["off"] = true
		}
		if len(e.Wol.MACs) > 0 {
			macs := make([]any, len(e.Wol.MACs))
			for i, x := range e.Wol.MACs {
				macs[i] = x
			}
			w["macs"] = macs
		}
		if e.Wol.Relay != "" {
			w["relay"] = e.Wol.Relay
		}
		if e.Wol.Port != 0 {
			w["port"] = e.Wol.Port
		}
		m["wol"] = w
	}
	if !e.Run.Empty() {
		r := map[string]any{}
		for _, t := range config.MachineRunTimings() {
			if v := e.Run.Value(t.Key); v != 0 {
				r[t.Key] = v
			}
		}
		m[hostRunKey] = r
	}
	if e.By != "" {
		m["by"] = e.By
	}
	if !e.At.IsZero() {
		m["at"] = e.At.UTC().Format(time.RFC3339)
	}
	return m
}

// wolEditOf reads a "wol" object as Fairy writes it. why says what is not that form ("": it is).
func wolEditOf(v any) (w WolEdit, why string) {
	m, ok := v.(map[string]any)
	if !ok {
		return w, "it is not an object (found " + kindOf(v) + ")"
	}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		switch x := m[k]; k {
		case "off":
			if b, isBool := x.(bool); !isBool || !b {
				return WolEdit{}, "\"off\" is not true"
			}
			w.Off = true
		case "macs":
			list, isList := x.([]any)
			if !isList || len(list) == 0 || len(list) > MaxWolMACs {
				return WolEdit{}, fmt.Sprintf("\"macs\" is not a list of 1 to %d addresses", MaxWolMACs)
			}
			for _, e := range list {
				s, isText := e.(string)
				if !isText {
					return WolEdit{}, "\"macs\" holds something that is not a text"
				}
				w.MACs = append(w.MACs, s)
			}
		case "relay":
			s, isText := x.(string)
			if !isText || s == "" {
				return WolEdit{}, "\"relay\" is not an agent's id"
			}
			w.Relay = s
		case "port":
			f, isNum := x.(float64)
			if !isNum || (f != 7 && f != 9) {
				return WolEdit{}, "\"port\" is not 7 or 9"
			}
			w.Port = int(f)
		default:
			// broadcast, password_env, ...: no such thing can be set from the panel, whatever the file says
			return WolEdit{}, fmt.Sprintf("%q cannot be set in the panel", clipKey(k))
		}
	}
	if w.Empty() {
		return WolEdit{}, "it decides nothing"
	}
	if err := w.Check(); err != nil {
		return WolEdit{}, err.Error()
	}
	return w, ""
}

// hostRunKey is the key of a machine's own run timings in its entry, as config.yaml names the block.
const hostRunKey = "wake_update"

// runEditOf reads a "wake_update" object as Fairy writes it. It never fails: say is told each key whose
// value is not a whole number of its range (the configuration's value counts for that key, the other key is
// read all the same: the two are two decisions). unknown is what is under a key that is none of the two: it
// is not used, and kept (a newer build may have written it).
func runEditOf(id string, v any, say func(format string, a ...any)) (r RunEdit, unknown map[string]any) {
	m, ok := v.(map[string]any)
	if !ok {
		say("host %s: %q is not an object (found %s): the configuration's run timings count", id, hostRunKey, kindOf(v))
		return RunEdit{}, nil
	}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		t, known := config.RunTimingOf(k)
		if !known || !slices.Contains(config.MachineRunTimings(), t) {
			say("host %s: %s.%q cannot be set in the panel: it is not used (and left in the file as it is)", id, hostRunKey, clipKey(k))
			if unknown == nil {
				unknown = map[string]any{}
			}
			unknown[k] = m[k]
			continue
		}
		f, isNum := m[k].(float64)
		if !isNum || f != math.Trunc(f) || f < float64(t.Min) || f > float64(t.Max) {
			say("host %s: %s.%s is not a whole number from %d to %d: the configuration's value counts", id, hostRunKey, k, t.Min, t.Max)
			continue
		}
		r.Set(k, int(f))
	}
	return r, unknown
}

// readHosts reads "hosts" of the overlay into o. It never fails: say is told each thing that is left out
// (the configuration's value counts for it).
func (o *HostOptions) readHosts(raw any, say func(format string, a ...any)) {
	hosts, ok := raw.(map[string]any)
	if !ok {
		say("%q: \"hosts\" is not an object (found %s): none of it is used", hostOptionsKey, kindOf(raw))
		return
	}
	if len(hosts) > MaxHostOptionAgents {
		say("%q has entries for %d hosts, more than the %d a panel can have: none of them is used", hostOptionsKey, len(hosts), MaxHostOptionAgents)
		return
	}
	for _, id := range slices.Sorted(maps.Keys(hosts)) {
		if !config.ValidRowID(id) {
			say("%q is not an id a host can have: its entry is not used", clipKey(id))
			continue
		}
		entry, ok := hosts[id].(map[string]any)
		if !ok {
			say("host %s: its entry is not an object (found %s): it is not used", id, kindOf(hosts[id]))
			continue
		}
		e := HostEdit{}
		for _, k := range slices.Sorted(maps.Keys(entry)) {
			switch v := entry[k]; k {
			case "name":
				s, _ := v.(string)
				if name, err := HostName(s); err != nil || name == "" || name != s {
					say("host %s: \"name\" is not a name of the form Fairy writes: the configuration's name counts", id)
					continue
				}
				e.Name = s
			case "wol":
				w, why := wolEditOf(v)
				if why != "" {
					say("host %s: \"wol\": %s: the configuration's Wake-on-LAN counts", id, why)
					continue
				}
				e.Wol = &w
			case hostRunKey:
				var rest map[string]any
				if e.Run, rest = runEditOf(id, v, say); rest != nil {
					if o.unknown.hostRun == nil {
						o.unknown.hostRun = map[string]map[string]any{}
					}
					o.unknown.hostRun[id] = rest
				}
			case "by":
				s, _ := v.(string)
				e.By = clipKey(s)
			case "at":
				s, _ := v.(string)
				e.At, _ = time.Parse(time.RFC3339, s)
			default:
				// url, token_env, power_control, ...: no such thing can be set from the panel, whatever the file says.
				// Nothing reads it; it stays in the file (a newer build may have written it).
				say("host %s: %q cannot be set in the panel: it is not used (and left in the file as it is)", id, clipKey(k))
				if o.unknown.hosts == nil {
					o.unknown.hosts = map[string]map[string]any{}
				}
				if o.unknown.hosts[id] == nil {
					o.unknown.hosts[id] = map[string]any{}
				}
				o.unknown.hosts[id][k] = v
			}
		}
		if !e.Empty() {
			o.Hosts[id] = e
		}
	}
}

// encodeHosts is "hosts" as it is written (nil: nothing to write): what this build knows, and next to it,
// untouched, what it read under keys it does not know.
func (o HostOptions) encodeHosts() map[string]any {
	hosts := map[string]any{}
	for id, e := range o.Hosts {
		if !e.Empty() {
			hosts[id] = e.encode()
		}
	}
	for id, keys := range o.unknown.hosts {
		entry, _ := hosts[id].(map[string]any)
		if entry == nil {
			entry = map[string]any{}
			hosts[id] = entry
		}
		for k, v := range keys {
			entry[k] = v
		}
	}
	for id, keys := range o.unknown.hostRun { // ... and in a machine's "wake_update"
		entry, _ := hosts[id].(map[string]any)
		if entry == nil {
			entry = map[string]any{}
			hosts[id] = entry
		}
		run, _ := entry[hostRunKey].(map[string]any)
		if run == nil {
			run = map[string]any{}
			entry[hostRunKey] = run
		}
		for k, v := range keys {
			run[k] = v
		}
	}
	if len(hosts) == 0 {
		return nil
	}
	return hosts
}

// Host returns what was changed about the machine kept under id (the zero value: nothing).
func (o HostOptions) Host(id string) HostEdit { return o.Hosts[id] }

// SetHost stores e for the machine kept under id; an e without a decision takes the entry out.
func (o *HostOptions) SetHost(id string, e HostEdit) {
	if e = e.Clone(); e.Empty() {
		delete(o.Hosts, id)
		return
	}
	if o.Hosts == nil {
		o.Hosts = map[string]HostEdit{}
	}
	o.Hosts[id] = e
}

// DropHost takes the entry of a machine out whole: its decisions, and what this build does not know in it.
func (o *HostOptions) DropHost(id string) {
	delete(o.Hosts, id)
	delete(o.unknown.hosts, id)
	delete(o.unknown.hostRun, id)
}

// forgetHosts is the part of Forget about the machines themselves: the decisions kept under one of ids go,
// and so do, in the decisions of OTHER machines, the Wake-on-LAN details of a machine whose relay is one of
// ids. They go WHOLE: "these MAC addresses through that relay" was one decision, and with the relay alone
// taken out it would become "these MAC addresses through config.yaml's relay", a packet nobody decided on.
// config.yaml's own Wake-on-LAN counts for that machine again. (The next machine added under the removed id
// must not become the relay of a wake packet nobody gave it either.) Nothing else of such a machine goes: its
// name and its own run timings have nothing to do with who sends its wake packet. It reports what was taken
// out, and says each machine that lost its Wake-on-LAN details in the log.
func (o *HostOptions) forgetHosts(ids []string) (gone []string) {
	for _, id := range ids {
		if e, ok := o.Hosts[id]; ok && !e.Empty() {
			gone = append(gone, "what was changed about "+id+" itself (name, Wake-on-LAN, run timings)")
		}
		o.DropHost(id)
	}
	for _, other := range o.RelayUsers(ids...) {
		e := o.Hosts[other].Clone()
		relay := e.Wol.Relay
		e.Wol = nil
		o.SetHost(other, e)
		gone = append(gone, fmt.Sprintf("%s: the Wake-on-LAN details changed for it (they named %s as the relay): config.yaml's count for it again", other, relay))
		slog.Warn("Edit host: a host's Wake-on-LAN details changed in the panel are taken out, because their relay is removed: config.yaml's count for it again",
			"host", other, "relay", relay)
	}
	return gone
}

// RelayUsers is the machines (the ids their decisions are kept under, sorted) whose Wake-on-LAN decisions
// name one of ids as the relay.
func (o HostOptions) RelayUsers(ids ...string) []string {
	var out []string
	for _, key := range slices.Sorted(maps.Keys(o.Hosts)) {
		if e := o.Hosts[key]; e.Wol != nil && e.Wol.Relay != "" && slices.Contains(ids, e.Wol.Relay) && !slices.Contains(ids, key) {
			out = append(out, key)
		}
	}
	return out
}
