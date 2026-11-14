package adminstate

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// "Edit host", the machine itself: its name and its Wake-on-LAN details in state.json ("host_options",
// "hosts").

// A name is one line a person can read: the rules of a custom model name, at most 64 characters.
func TestHostName(t *testing.T) {
	for raw, want := range map[string]string{"PC-A": "PC-A", "  Gaming   PC ": "Gaming PC", "": "", "   ": "", "Büro Ærø 2": "Büro Ærø 2", strings.Repeat("n", 64): strings.Repeat("n", 64)} {
		if got, err := HostName(raw); err != nil || got != want {
			t.Errorf("HostName(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for what, raw := range map[string]string{"two lines": "pc\na", "a tab": "pc\ta", "an escape": "pc\x1b[31m", "a direction override": "pc‮evil",
		"a zero-width space": "pc​a", "65 characters": strings.Repeat("n", 65), "not UTF-8": "pc\xff", "only marks": "́́"} {
		if got, err := HostName(raw); err == nil {
			t.Errorf("%s: taken as %q", what, got)
		} else if !strings.HasPrefix(err.Error(), "name ") {
			t.Errorf("%s: the sentence is not about a name: %v", what, err)
		}
	}
}

// The MAC addresses of a decision: 1 to 8 addresses of network cards, each once, stored in one spelling.
func TestWolMACs(t *testing.T) {
	got, err := WolMACs([]string{"AA-BB-CC-DD-EE-01", " aa:bb:cc:dd:ee:02 "})
	if err != nil || !slices.Equal(got, []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"}) {
		t.Fatalf("two addresses in other spellings: %v, %v", got, err)
	}
	eight := []string{}
	for i := range 9 {
		eight = append(eight, "aa:bb:cc:dd:ee:0"+string(rune('0'+i)))
	}
	if _, err := WolMACs(eight[:8]); err != nil {
		t.Errorf("eight addresses: %v", err)
	}
	for what, list := range map[string][]string{"none": {}, "nine": eight, "the same twice": {"aa:bb:cc:dd:ee:01", "AA-BB-CC-DD-EE-01"},
		"too short": {"aa:bb:cc:dd:ee"}, "no separators": {"aabbccddee01"}, "not hex": {"aa:bb:cc:dd:ee:zz"}, "an address": {"10.0.0.255"},
		"a name": {"pc-a"}, "empty": {""}, "all zero": {"00:00:00:00:00:00"}, "all ff": {"FF:FF:FF:FF:FF:FF"}, "with a port": {"aa:bb:cc:dd:ee:01:9"}} {
		if got, err := WolMACs(list); err == nil {
			t.Errorf("%s: taken as %v", what, got)
		}
	}
}

// The decisions are laid over the configuration's Wake-on-LAN, each over its own part. The target address
// and the password are never part of a decision: they stay what the configuration says.
func TestWolEditApply(t *testing.T) {
	base := &config.Wol{MAC: "aa:bb:cc:dd:ee:01", MACs: []string{"aa:bb:cc:dd:ee:02"}, Relay: "relay-a", Port: 9}
	if w, unused := (WolEdit{}).Apply(base); unused != "" || w == base || w.Relay != "relay-a" || len(w.AllMACs()) != 2 {
		t.Fatalf("no decision: %+v %q", w, unused)
	}
	if w, unused := (WolEdit{}).Apply(nil); w != nil || unused != "" {
		t.Fatalf("no decision, no Wake-on-LAN: %+v %q", w, unused)
	}
	w, unused := WolEdit{MACs: []string{"aa:bb:cc:dd:ee:09"}}.Apply(base)
	if unused != "" || !slices.Equal(w.AllMACs(), []string{"aa:bb:cc:dd:ee:09"}) || w.Relay != "relay-a" || w.Port != 9 {
		t.Fatalf("other MACs: %+v %q", w, unused)
	}
	if len(base.AllMACs()) != 2 || base.MAC == "" {
		t.Fatal("Apply changed the configuration's entry")
	}
	w, unused = WolEdit{Relay: "relay-b", Port: 7}.Apply(base)
	if unused != "" || w.Relay != "relay-b" || w.Port != 7 || len(w.AllMACs()) != 2 {
		t.Fatalf("another relay and port: %+v %q", w, unused)
	}
	if w, unused := (WolEdit{Off: true}).Apply(base); w != nil || unused != "" {
		t.Fatalf("off: %+v %q", w, unused)
	}
	// a machine without Wake-on-LAN gets one with MACs and a relay; with one of the two it has none, and says why
	w, unused = WolEdit{MACs: []string{"aa:bb:cc:dd:ee:09"}, Relay: "relay-a"}.Apply(nil)
	if unused != "" || w == nil || w.Relay != "relay-a" || w.Broadcast != "" || w.PasswordEnv != "" {
		t.Fatalf("a new Wake-on-LAN: %+v %q", w, unused)
	}
	if w, unused := (WolEdit{MACs: []string{"aa:bb:cc:dd:ee:09"}}).Apply(nil); w != nil || !strings.Contains(unused, "no relay") {
		t.Fatalf("MACs alone: %+v %q", w, unused)
	}
	if w, unused := (WolEdit{Relay: "relay-a"}).Apply(nil); w != nil || !strings.Contains(unused, "no MAC") {
		t.Fatalf("a relay alone: %+v %q", w, unused)
	}
	// With a target address or a password in config.yaml the relay stays config.yaml's: the decisions are
	// then not used at all, and the configuration's entry counts as it is. Other decisions are used.
	for what, fixed := range map[string]*config.Wol{
		"a target address": {MAC: "aa:bb:cc:dd:ee:01", Relay: "relay-a", Broadcast: "10.0.0.255"},
		"a password":       {MAC: "aa:bb:cc:dd:ee:01", Relay: "relay-a", PasswordEnv: "WOL_PASSWORD"},
	} {
		if WolRelayFixed(fixed) == "" {
			t.Errorf("%s: the relay is not fixed", what)
		}
		w, unused := WolEdit{Relay: "relay-b", MACs: []string{"aa:bb:cc:dd:ee:09"}}.Apply(fixed)
		if unused == "" || w.Relay != "relay-a" || !slices.Equal(w.AllMACs(), []string{"aa:bb:cc:dd:ee:01"}) || w.Broadcast != fixed.Broadcast || w.PasswordEnv != fixed.PasswordEnv {
			t.Errorf("%s, another relay: %+v %q", what, w, unused)
		}
		w, unused = WolEdit{MACs: []string{"aa:bb:cc:dd:ee:09"}, Port: 7}.Apply(fixed)
		if unused != "" || w.Relay != "relay-a" || !slices.Equal(w.AllMACs(), []string{"aa:bb:cc:dd:ee:09"}) || w.Broadcast != fixed.Broadcast || w.PasswordEnv != fixed.PasswordEnv {
			t.Errorf("%s, other MACs: %+v %q", what, w, unused)
		}
		if w, unused := (WolEdit{Relay: "relay-a"}).Apply(fixed); unused != "" || w.Relay != "relay-a" { // the relay it has: no change
			t.Errorf("%s, the same relay: %+v %q", what, w, unused)
		}
	}
	if WolRelayFixed(base) != "" || WolRelayFixed(nil) != "" {
		t.Error("the relay of an entry without a target address or password is fixed")
	}
	// a decision that is not of the form Fairy writes is not used either
	for what, e := range map[string]WolEdit{"off with MACs": {Off: true, MACs: []string{"aa:bb:cc:dd:ee:09"}}, "port 8": {Port: 8}, "a relay that is no id": {Relay: "Relay A"},
		"an upper-case MAC": {MACs: []string{"AA:BB:CC:DD:EE:09"}}, "a broadcast as a MAC": {MACs: []string{"ff:ff:ff:ff:ff:ff"}}} {
		if e.Check() == nil {
			t.Errorf("%s passes the check", what)
		}
		if w, unused := e.Apply(base); unused == "" || w == nil || w.Relay != "relay-a" || len(w.AllMACs()) != 2 {
			t.Errorf("%s: %+v %q", what, w, unused)
		}
	}
}

// The reader never fails, and never takes more than Fairy writes: a name that is no name, a "wol" with a
// key that cannot be set in the panel (a target address, a password, a token), or of another form, is left
// out with a line, and the configuration's value counts.
func TestHostEditsReaderIsTolerant(t *testing.T) {
	read := func(hosts string) (HostOptions, []string) {
		dir := t.TempDir()
		writeState(t, dir, `{"host_options": {"rev": 3, "hosts": `+hosts+`}}`)
		return New(dir).HostOptionsRead()
	}
	o, ignored := read(`{"pc-a": {"name": "Gaming PC", "wol": {"macs": ["aa:bb:cc:dd:ee:01"], "relay": "relay-a", "port": 7}, "by": "admin", "at": "2026-11-14T10:00:00Z"},
		"lab-node-pve-a": {"wol": {"off": true}}}`)
	a, n := o.Host("pc-a"), o.Host("lab-node-pve-a")
	if len(ignored) != 0 || o.Rev != 3 || a.Name != "Gaming PC" || a.Wol == nil || !slices.Equal(a.Wol.MACs, []string{"aa:bb:cc:dd:ee:01"}) || a.Wol.Relay != "relay-a" || a.Wol.Port != 7 ||
		a.By != "admin" || a.At.IsZero() || a.Count() != 4 || n.Wol == nil || !n.Wol.Off || n.Count() != 1 || len(o.Hosts) != 2 {
		t.Fatalf("what Fairy writes: %+v %+v %v", a, n, ignored)
	}
	for what, c := range map[string]struct{ hosts, line string }{
		"hosts is a list":            {`["pc-a"]`, `"hosts" is not an object`},
		"an id no host can have":     {`{"PC A": {"name": "x"}}`, `is not an id a host can have`},
		"an entry that is no object": {`{"pc-a": "Gaming PC"}`, `its entry is not an object`},
		"a name with a line break":   {`{"pc-a": {"name": "a\nb"}}`, `"name" is not a name`},
		"a name that is a number":    {`{"pc-a": {"name": 7}}`, `"name" is not a name`},
		"a name with spaces around":  {`{"pc-a": {"name": " a "}}`, `"name" is not a name`},
		"a target address":           {`{"pc-a": {"wol": {"macs": ["aa:bb:cc:dd:ee:01"], "broadcast": "10.0.0.255"}}}`, `"broadcast" cannot be set in the panel`},
		"a password variable":        {`{"pc-a": {"wol": {"relay": "relay-a", "password_env": "FAIRY_SESSION_SECRET"}}}`, `"password_env" cannot be set in the panel`},
		"a password":                 {`{"pc-a": {"wol": {"relay": "relay-a", "password": "aa:bb:cc:dd:ee:ff"}}}`, `"password" cannot be set in the panel`},
		"port 80":                    {`{"pc-a": {"wol": {"port": 80}}}`, `"port" is not 7 or 9`},
		"a port as a text":           {`{"pc-a": {"wol": {"port": "9"}}}`, `"port" is not 7 or 9`},
		"an upper-case MAC":          {`{"pc-a": {"wol": {"macs": ["AA:BB:CC:DD:EE:01"]}}}`, `not written as aa:bb:cc:dd:ee:ff`},
		"a MAC that is none":         {`{"pc-a": {"wol": {"macs": ["10.0.0.1"]}}}`, `must look like`},
		"nine MACs":                  {`{"pc-a": {"wol": {"macs": ["aa:bb:cc:dd:ee:01","aa:bb:cc:dd:ee:02","aa:bb:cc:dd:ee:03","aa:bb:cc:dd:ee:04","aa:bb:cc:dd:ee:05","aa:bb:cc:dd:ee:06","aa:bb:cc:dd:ee:07","aa:bb:cc:dd:ee:08","aa:bb:cc:dd:ee:09"]}}}`, `"macs" is not a list of 1 to 8`},
		"macs is a text":             {`{"pc-a": {"wol": {"macs": "aa:bb:cc:dd:ee:01"}}}`, `"macs" is not a list`},
		"a relay that is no id":      {`{"pc-a": {"wol": {"relay": "http://10.0.0.1"}}}`, `the relay is not an id`},
		"off is false":               {`{"pc-a": {"wol": {"off": false}}}`, `"off" is not true`},
		"off with a relay":           {`{"pc-a": {"wol": {"off": true, "relay": "relay-a"}}}`, `"off" stands alone`},
		"an empty wol":               {`{"pc-a": {"wol": {}}}`, `it decides nothing`},
		"wol is a list":              {`{"pc-a": {"wol": ["aa:bb:cc:dd:ee:01"]}}`, `it is not an object`},
	} {
		o, ignored := read(c.hosts)
		if len(o.Hosts) != 0 || len(ignored) != 1 || !strings.Contains(ignored[0], c.line) || o.Ignored != 1 {
			t.Errorf("%s: hosts %+v, lines %q (want one with %q)", what, o.Hosts, ignored, c.line)
		}
	}
	// one bad key does not take the entry's other decisions with it
	o, ignored = read(`{"pc-a": {"name": "Gaming PC", "wol": {"broadcast": "10.0.0.255"}}}`)
	if o.Host("pc-a").Name != "Gaming PC" || o.Host("pc-a").Wol != nil || len(ignored) != 1 {
		t.Errorf("a good name next to a bad wol: %+v %q", o.Host("pc-a"), ignored)
	}
	// more hosts than a panel can have: none of it
	many := map[string]any{}
	for i := range MaxHostOptionAgents + 1 {
		many[fmt.Sprintf("pc-%d", i)] = map[string]any{"name": "x"}
	}
	b, _ := json.Marshal(many)
	if o, ignored := read(string(b)); len(o.Hosts) != 0 || len(ignored) != 1 || !strings.Contains(ignored[0], "more than the 256") {
		t.Errorf("%d hosts: %d read, %q", len(many), len(o.Hosts), ignored)
	}
	delete(many, "pc-0")
	b, _ = json.Marshal(many)
	if o, ignored := read(string(b)); len(o.Hosts) != MaxHostOptionAgents || len(ignored) != 0 {
		t.Errorf("%d hosts: %d read, %q", len(many), len(o.Hosts), ignored)
	}
}

// What is written is what is read, a save keeps the link decisions next to it, who and when are put on the
// entry that changed and on no other, and a file that never had "hosts" gets no such key from a change to
// links.
func TestHostEditsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeState(t, dir, `{"host_models": {"pc-a": "Tower"}, "host_options": {"rev": 4, "agents": {"nas": {"power_exclude_from_total": {"desk": true}, "by": "old", "at": "2026-11-01T00:00:00Z"}}}}`)
	a := New(dir)
	after, changed, err := a.EditHostOptions("admin", func(o *HostOptions) error {
		e := o.Agents["nas"]
		e.SetExclude("lamp", true)
		o.Agents["nas"] = e
		return nil
	})
	if err != nil || !changed || after.Rev != 5 {
		t.Fatal(err, changed, after.Rev)
	}
	if _, has := readOptions(t, dir)["hosts"]; has {
		t.Fatal("a change to links wrote \"hosts\"")
	}
	after, changed, err = a.EditHostOptions("admin", func(o *HostOptions) error {
		o.SetHost("pc-a", HostEdit{Name: "Gaming PC", Wol: &WolEdit{MACs: []string{"aa:bb:cc:dd:ee:01"}, Relay: "relay-a"}})
		o.SetHost("pc-b", HostEdit{Wol: &WolEdit{}}) // nothing decided: no entry
		return nil
	})
	if err != nil || !changed || after.Rev != 6 || len(after.Hosts) != 1 {
		t.Fatal(err, changed, after.Rev, after.Hosts)
	}
	stored, _ := readOptions(t, dir)["hosts"].(map[string]any)
	pc, _ := stored["pc-a"].(map[string]any)
	wol, _ := pc["wol"].(map[string]any)
	if len(stored) != 1 || pc["name"] != "Gaming PC" || pc["by"] != "admin" || pc["at"] == nil || wol["relay"] != "relay-a" || len(wol) != 2 {
		t.Fatalf("stored: %v", stored)
	}
	o := New(dir).HostOptions()
	if got := o.Host("pc-a"); !got.Same(HostEdit{Name: "Gaming PC", Wol: &WolEdit{MACs: []string{"aa:bb:cc:dd:ee:01"}, Relay: "relay-a"}}) || got.By != "admin" {
		t.Fatalf("read back: %+v", got)
	}
	if nas := o.For("nas"); nas.By != "admin" || nas.Count() != 2 {
		t.Fatalf("the link decisions next to it: %+v", nas)
	}
	// the same again: nothing is written
	if _, changed, err := a.EditHostOptions("other", func(o *HostOptions) error {
		o.SetHost("pc-a", HostEdit{Name: "Gaming PC", Wol: &WolEdit{MACs: []string{"aa:bb:cc:dd:ee:01"}, Relay: "relay-a"}})
		return nil
	}); err != nil || changed {
		t.Fatal("the same decisions again count as a change", err, changed)
	}
	// a change to pc-a's name: its entry is stamped, the links' entry is not
	if _, _, err := a.EditHostOptions("other", func(o *HostOptions) error {
		e := o.Host("pc-a")
		e.Name = "Den PC"
		o.SetHost("pc-a", e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	o = New(dir).HostOptions()
	if o.Host("pc-a").By != "other" || o.For("nas").By != "admin" || o.Host("pc-a").Wol == nil {
		t.Fatalf("who: host %q, links %q", o.Host("pc-a").By, o.For("nas").By)
	}
	// the last decision goes: the key goes with it
	if _, _, err := a.EditHostOptions("other", func(o *HostOptions) error { o.SetHost("pc-a", HostEdit{}); return nil }); err != nil {
		t.Fatal(err)
	}
	if _, has := readOptions(t, dir)["hosts"]; has {
		t.Fatalf("no decision is left and \"hosts\" is still written: %v", readOptions(t, dir))
	}
}

// A state.json as build 15 wrote it (no "hosts") is read as it always was, and a key of a newer build inside
// a host's entry is not used and survives every save, also one that changes that host.
func TestHostEditsBuild15FileAndNewerKeys(t *testing.T) {
	dir := t.TempDir()
	b15 := `{"wake_update_hosts": ["pc-a"], "host_options": {"rev": 7, "agents": {"nas": {"ups_powers": {"rack": {"pc-a": true}}, "power_links": {"plug": ""},
		"power_exclude_from_total": {"desk": true}, "by": "admin", "at": "2026-11-14T05:00:00Z"}}}}`
	writeState(t, dir, b15)
	o, ignored := New(dir).HostOptionsRead()
	if len(ignored) != 0 || o.Rev != 7 || len(o.Hosts) != 0 || o.For("nas").Count() != 3 {
		t.Fatalf("a build 15 file: %+v %q", o, ignored)
	}
	writeState(t, dir, `{"host_options": {"rev": 7, "agents": {}, "hosts": {"pc-a": {"name": "Gaming PC", "run": {"off_after_s": 600}}, "pc-b": {"run": {"off_after_s": 300}}}}}`)
	a := New(dir)
	o, ignored = a.HostOptionsRead()
	if len(ignored) != 2 || o.Host("pc-a").Name != "Gaming PC" || len(o.Hosts) != 1 {
		t.Fatalf("a newer build's key: %+v %q", o.Hosts, ignored)
	}
	for _, l := range ignored {
		if !strings.Contains(l, `"run" cannot be set in the panel: it is not used (and left in the file as it is)`) {
			t.Errorf("line: %q", l)
		}
	}
	if _, _, err := a.EditHostOptions("admin", func(o *HostOptions) error {
		o.SetHost("pc-a", HostEdit{Wol: &WolEdit{Off: true}}) // its name goes, its Wake-on-LAN is off
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	hosts, _ := readOptions(t, dir)["hosts"].(map[string]any)
	pa, _ := hosts["pc-a"].(map[string]any)
	pb, _ := hosts["pc-b"].(map[string]any)
	if pa["run"] == nil || pb["run"] == nil || pa["name"] != nil || pa["wol"] == nil {
		t.Fatalf("after a save: %v", hosts)
	}
	// DropHost takes the entry whole, the unknown key with it
	if _, changed, err := a.EditHostOptions("admin", func(o *HostOptions) error { o.DropHost("pc-b"); return nil }); err != nil || !changed {
		t.Fatal(err, changed)
	}
	hosts, _ = readOptions(t, dir)["hosts"].(map[string]any)
	if _, has := hosts["pc-b"]; has || len(hosts) != 1 {
		t.Fatalf("after DropHost: %v", hosts)
	}
}

// Remove of a host: what was changed about the host itself goes, under every id it had, and so does a relay
// decision of another host that names it. The other host's other decisions stay.
func TestHostEditsForget(t *testing.T) {
	o := HostOptions{Agents: map[string]HostOption{}, Hosts: map[string]HostEdit{
		"pc-a":           {Name: "Gaming PC", Wol: &WolEdit{MACs: []string{"aa:bb:cc:dd:ee:01"}}},
		"lab-node-pve-a": {Name: "Rack"},
		"pc-b":           {Name: "Den", Wol: &WolEdit{Relay: "pc-a", Port: 7}},
		"pc-c":           {Wol: &WolEdit{Relay: "pc-a"}},
		"pc-d":           {Wol: &WolEdit{Relay: "relay-a"}},
	}}
	gone := o.Forget("pc-a", "lab-node-pve-a")
	if len(gone) != 4 {
		t.Fatalf("lines: %q", gone)
	}
	if _, has := o.Hosts["pc-a"]; has {
		t.Error("the host's own entry stayed")
	}
	if _, has := o.Hosts["lab-node-pve-a"]; has {
		t.Error("the entry under its row's id stayed")
	}
	if b := o.Host("pc-b"); b.Name != "Den" || b.Wol == nil || b.Wol.Relay != "" || b.Wol.Port != 7 {
		t.Errorf("pc-b: %+v %+v", b, b.Wol)
	}
	if _, has := o.Hosts["pc-c"]; has {
		t.Error("pc-c had only the relay: its entry is empty and stayed")
	}
	if d := o.Host("pc-d"); d.Wol == nil || d.Wol.Relay != "relay-a" {
		t.Errorf("pc-d names another relay and was changed: %+v", d)
	}
}
