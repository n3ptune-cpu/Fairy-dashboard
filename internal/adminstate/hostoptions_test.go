package adminstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
)

// "Edit host": the overlay of link overrides in state.json ("host_options").

func writeState(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readOptions(t *testing.T, dir string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	d := map[string]any{}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("state.json is not JSON: %v\n%s", err, b)
	}
	o, _ := d[hostOptionsKey].(map[string]any)
	return o
}

// A decision is about one line (this UPS and this row; this device) and is laid over what config.yaml says
// about that line. Everything no decision is about is config.yaml's, whatever config.yaml says, now or later.
func TestHostOptionApply(t *testing.T) {
	base := config.Links{UPSPowers: map[string][]string{"rack": {"pc-a", "pc-c"}}, PowerLinks: map[string]string{"plug": "pc-a", "other": "pc-c"}, PowerExcludeFromTotal: []string{"corner"}}
	var o HostOption
	if got := o.Apply(base); !SameLinks(got, base) || !o.Empty() || o.Count() != 0 {
		t.Fatalf("no decision: %+v", got)
	}
	o.SetUPS("desk", "pc-b", true)  // a UPS config.yaml does not name
	o.SetUPS("rack", "pc-a", false) // a row config.yaml has on the UPS
	o.SetUPS("rack", "pc-b", true)
	got := o.Apply(base)
	if !slices.Equal(got.UPSPowers["desk"], []string{"pc-b"}) || !slices.Equal(got.UPSPowers["rack"], []string{"pc-c", "pc-b"}) || len(got.UPSPowers) != 2 ||
		got.PowerLinks["plug"] != "pc-a" || len(got.PowerLinks) != 2 || !slices.Equal(got.PowerExcludeFromTotal, []string{"corner"}) {
		t.Fatalf("three decisions about UPSes: %+v", got)
	}
	if !o.Has(config.FieldUPSPowers) || o.Has(config.FieldPowerLinks) || o.Has(config.FieldExclude) || o.Has("url") || o.Empty() || o.Count() != 3 {
		t.Fatalf("Has: %+v", o)
	}
	// the result shares nothing with the entry or the decisions
	got.UPSPowers["rack"][0], got.PowerLinks["plug"], got.PowerExcludeFromTotal[0] = "changed", "changed", "changed"
	if !slices.Equal(base.UPSPowers["rack"], []string{"pc-a", "pc-c"}) || base.PowerLinks["plug"] != "pc-a" || base.PowerExcludeFromTotal[0] != "corner" || !o.UPS["rack"]["pc-b"] {
		t.Fatal("Apply shares with its inputs")
	}
	// config.yaml is edited afterwards: what no decision is about follows it, what a decision is about does not
	later := config.Links{UPSPowers: map[string][]string{"rack": {"pc-a", "pc-d"}, "desk": {"pc-d"}}}
	if got := o.Apply(later); !slices.Equal(got.UPSPowers["rack"], []string{"pc-d", "pc-b"}) || !slices.Equal(got.UPSPowers["desk"], []string{"pc-d", "pc-b"}) {
		t.Fatalf("after config.yaml changed: %+v", got)
	}
	// a decision that says what config.yaml says changes nothing (and adds no row twice)
	o.SetUPS("rack", "pc-c", true)
	o.SetUPS("rack", "nobody", false)
	if got := o.Apply(base); !slices.Equal(got.UPSPowers["rack"], []string{"pc-c", "pc-b"}) {
		t.Fatalf("decisions that say the same: %+v", got)
	}
	// a link: to another row, or to none; the device no decision is about keeps config.yaml's
	o.SetLink("plug", "pc-b")
	o.SetLink("other", "")
	o.SetLink("new", "pc-b")
	o.SetLink("never", "")
	if got := o.Apply(base); got.PowerLinks["plug"] != "pc-b" || got.PowerLinks["new"] != "pc-b" || len(got.PowerLinks) != 2 {
		t.Fatalf("links: %+v", got.PowerLinks)
	}
	if got := (HostOption{Links: map[string]string{"x": "pc"}}).Apply(config.Links{}); got.PowerLinks["x"] != "pc" {
		t.Fatalf("a link over an entry without links: %+v", got)
	}
	// the total: left out, or counted
	o.SetExclude("corner", false)
	o.SetExclude("plug", true)
	o.SetExclude("plug", true)
	if got := o.Apply(base); !slices.Equal(got.PowerExcludeFromTotal, []string{"plug"}) {
		t.Fatalf("the total: %+v", got.PowerExcludeFromTotal)
	}
	// taken out one by one: config.yaml's word about each line counts again
	if !o.DropLink("plug") || o.DropLink("plug") || o.DropLink("nope") || !o.DropExclude("corner") || o.DropExclude("corner") ||
		!o.DropUPS("rack", "pc-a") || o.DropUPS("rack", "pc-a") || o.DropUPS("attic", "pc-a") {
		t.Fatal("Drop... reports whether there was a decision")
	}
	if got := o.Apply(base); got.PowerLinks["plug"] != "pc-a" || !slices.Equal(got.PowerExcludeFromTotal, []string{"corner", "plug"}) || !slices.Equal(got.UPSPowers["rack"], []string{"pc-a", "pc-c", "pc-b"}) {
		t.Fatalf("after three decisions were taken out: %+v", got)
	}
	for _, id := range []string{"pc-b", "pc-c", "nobody"} {
		o.DropUPS("rack", id)
	}
	o.DropUPS("desk", "pc-b")
	for _, dev := range []string{"other", "new", "never"} {
		o.DropLink(dev)
	}
	o.DropExclude("plug")
	if !o.Empty() || len(o.UPS) != 0 || !SameLinks(o.Apply(base), base) {
		t.Fatalf("every decision taken out is no change: %+v", o)
	}
	// Named is what the decisions name, for the one check of the link fields; Same and Clone
	o.SetUPS("rack", "pc-b", true)
	o.SetUPS("rack", "pc-a", false)
	o.SetLink("plug", "")
	o.SetExclude("corner", false)
	n := o.Named()
	if !slices.Equal(n.UPSPowers["rack"], []string{"pc-a", "pc-b"}) || n.PowerLinks["plug"] != "" || len(n.PowerLinks) != 1 || !slices.Equal(n.PowerExcludeFromTotal, []string{"corner"}) || len(config.LinkProblems(n)) != 0 {
		t.Fatalf("Named: %+v", n)
	}
	c := o.Clone()
	if !c.Same(o) || !o.Same(c) {
		t.Fatal("a copy is not the same")
	}
	c.SetUPS("rack", "pc-b", false)
	if c.Same(o) || !o.UPS["rack"]["pc-b"] {
		t.Fatal("a copy shares with what it was made from, or Same does not see a decision turned round")
	}
	o.UPS["attic"] = map[string]bool{} // a UPS no decision is left about says nothing
	if !o.Clone().Same(o) || len(o.Clone().UPS) != 1 || o.Count() != 4 {
		t.Fatalf("an empty UPS counts: %+v", o)
	}
}

// The reader never fails and never takes more than the three link fields. Whatever is not what Fairy writes
// is left out with one line; the rest is used.
func TestHostOptionsReaderIsTolerant(t *testing.T) {
	var seventeen, many65, rows33 []string
	for i := 0; i < 65; i++ {
		if i < 17 {
			seventeen = append(seventeen, fmt.Sprintf(`"d%d": true`, i))
		}
		if i < 33 {
			rows33 = append(rows33, fmt.Sprintf(`"pc-%d": true`, i))
		}
		many65 = append(many65, fmt.Sprintf(`"d%d": false`, i))
	}
	many := map[string]any{}
	for i := 0; i <= MaxHostOptionAgents; i++ {
		many[fmt.Sprintf("a%d", i)] = map[string]any{"power_exclude_from_total": map[string]any{"x": true}}
	}
	manyJSON, _ := json.Marshal(map[string]any{"agents": many})
	for name, c := range map[string]struct {
		overlay string
		ignored []string // a part of each line, in order
		agents  string   // the agents with overrides and their fields: "nas:ups_powers+power_links ..."
		rev     int
	}{
		"what Fairy writes": {`{"rev": 7, "agents": {"nas": {"ups_powers": {"rack": {"pc-a": true, "lab-node-pve-a": false}}, "power_links": {"pc-plug": "pc-a", "old": ""}, "power_exclude_from_total": {"corner": true, "pc-plug": false}, "by": "nora", "at": "2026-11-13T23:40:00Z"}}}`,
			nil, "nas:ups_powers+power_links+power_exclude_from_total", 7},
		"fields with no decision say nothing": {`{"agents": {"nas": {"ups_powers": {}, "power_exclude_from_total": {}}, "pc": {"ups_powers": {"rack": {}}, "power_links": {"a": "pc"}}}}`, nil, "pc:power_links", 0},
		"an entry with nothing in it":         {`{"agents": {"nas": {"by": "nora"}}}`, nil, "", 0},
		"a text":                              {`"x"`, []string{"is not the object Fairy writes (found a text)"}, "", 0},
		"a list":                              {`[]`, []string{"found a list"}, "", 0},
		"null":                                {`null`, []string{"found null"}, "", 0},
		"a number":                            {`7`, []string{"found a number"}, "", 0},
		"true":                                {`true`, []string{"found true / false"}, "", 0},
		"agents is a text":                    {`{"rev": 2, "agents": "all"}`, []string{`"agents" is not an object (found a text)`}, "", 2},
		"rev is a text":                       {`{"rev": "7", "agents": {"nas": {"power_exclude_from_total": {"x": true}}}}`, []string{`"rev" is not a whole number`}, "nas:power_exclude_from_total", 0},
		"rev is negative":                     {`{"rev": -1}`, []string{`"rev" is not a whole number`}, "", 0},
		"rev is a fraction":                   {`{"rev": 1.5}`, []string{`"rev" is not a whole number`}, "", 0},
		"rev is huge":                         {`{"rev": 1e300}`, []string{`"rev" is not a whole number`}, "", 0},
		"another key at the top":              {`{"rev": 1, "hosts": {}, "agents": {}}`, []string{`the key "hosts" is not one Fairy writes`}, "", 1},
		"an id no agent can have":             {`{"agents": {"NAS": {"power_exclude_from_total": {"x": true}}, "": {}, "a/b": {}, "ok": {"power_exclude_from_total": {"x": true}}}}`, []string{`"" is not an id`, `"NAS" is not an id`, `"a/b" is not an id`}, "ok:power_exclude_from_total", 0},
		"an entry that is no object":          {`{"agents": {"nas": ["ups_powers"], "pc": null}}`, []string{"agent nas: its entry is not an object (found a list)", "agent pc: its entry is not an object (found null)"}, "", 0},
		"fields of the wrong form":            {`{"agents": {"nas": {"ups_powers": {"rack": "pc-a"}, "power_links": ["a"], "power_exclude_from_total": "a"}}}`, []string{"power_exclude_from_total is not of the form", "power_links is not of the form", "ups_powers is not of the form"}, "", 0},
		"lists, as config.yaml has them":      {`{"agents": {"nas": {"ups_powers": {"rack": ["pc-a"]}, "power_exclude_from_total": ["a"], "power_links": {"a": "pc"}}}}`, []string{"power_exclude_from_total is not of the form", "ups_powers is not of the form"}, "nas:power_links", 0},
		"a decision that is no yes or no":     {`{"agents": {"nas": {"power_exclude_from_total": {"a": "yes"}, "ups_powers": {"rack": {"pc": null}}, "power_links": {"a": {}}}}}`, []string{"power_exclude_from_total is not of the form", "power_links is not of the form", "ups_powers is not of the form"}, "", 0},
		"seventeen devices":                   {`{"agents": {"nas": {"power_exclude_from_total": {` + strings.Join(seventeen, ",") + `}, "power_links": {"plug": "pc"}}}}`, []string{"power_exclude_from_total: 17 devices"}, "nas:power_links", 0},
		"decisions far over every limit":      {`{"agents": {"nas": {"power_exclude_from_total": {` + strings.Join(many65, ",") + `}}}}`, []string{"power_exclude_from_total is not of the form"}, "", 0},
		"thirty-three rows for one UPS":       {`{"agents": {"nas": {"ups_powers": {"rack": {` + strings.Join(rows33, ",") + `}}, "power_exclude_from_total": {"x": false}}}}`, []string{"ups_powers: UPS rack powers 33 rows"}, "nas:power_exclude_from_total", 0},
		"ids no row or device can have":       {`{"agents": {"nas": {"ups_powers": {"rack": {"PC A": true}}, "power_links": {"Bad_ID": "pc"}, "power_exclude_from_total": {"ok": true}}}}`, []string{"power_links:", "ups_powers:"}, "nas:power_exclude_from_total", 0},
		"a link to an id no row can have":     {`{"agents": {"nas": {"power_links": {"plug": "No Row"}}}}`, []string{"power_links: device plug"}, "", 0},
		"a UPS name no UPS can have":          {`{"agents": {"nas": {"ups_powers": {"rack ups": {"pc": true}}}}}`, []string{"is not a name a UPS can have"}, "", 0},
		"what cannot be set here": {`{"agents": {"nas": {"url": "http://198.51.100.7:9101", "token_env": "FAIRY_SESSION_SECRET", "control_token_env": "X", "verify_tls": false, "node": "pve", "links_to": "node-x", "fan_control": {"vendor": "supermicro"}, "power_control": true, "self_update": true, "wol_relay": true, "owner": "eve", "site": "Home", "name": "x", "id": "other", "wake_update": {"off_after_s": 300}, "power_exclude_from_total": {"corner": true}}}}`,
			[]string{`"control_token_env" cannot be set`, `"fan_control" cannot`, `"id" cannot`, `"links_to" cannot`, `"name" cannot`, `"node" cannot`, `"owner" cannot`, `"power_control" cannot`, `"self_update" cannot`, `"site" cannot`, `"token_env" cannot`, `"url" cannot`, `"verify_tls" cannot`, `"wake_update" cannot`, `"wol_relay" cannot`}, "nas:power_exclude_from_total", 0},
		"more agents than a panel has": {string(manyJSON), []string{"more than the 256 a panel can have"}, "", 0},
		"a name that is not text":      {`{"agents": {"nas": {"power_exclude_from_total": {"x": true}, "by": 7, "at": "yesterday"}}}`, nil, "nas:power_exclude_from_total", 0},
	} {
		d := statefile.Data{}
		if err := json.Unmarshal([]byte(`{"host_options": `+c.overlay+`, "wake_update_hosts": ["pc-a"]}`), &d); err != nil {
			t.Fatalf("%s: the fixture is not JSON: %v", name, err)
		}
		o, ignored := hostOptions(d)
		var got []string
		for _, id := range slices.Sorted(func(yield func(string) bool) {
			for id := range o.Agents {
				if !yield(id) {
					return
				}
			}
		}) {
			var f []string
			for _, field := range config.LinkFields {
				if o.Agents[id].Has(field) {
					f = append(f, field)
				}
			}
			got = append(got, id+":"+strings.Join(f, "+"))
		}
		if strings.Join(got, " ") != c.agents || o.Rev != c.rev {
			t.Errorf("%s: agents %q rev %d, want %q rev %d", name, strings.Join(got, " "), o.Rev, c.agents, c.rev)
		}
		if len(ignored) != len(c.ignored) {
			t.Errorf("%s: %d lines, want %d: %q", name, len(ignored), len(c.ignored), ignored)
			continue
		}
		for i, want := range c.ignored {
			if !strings.Contains(ignored[i], want) || strings.ContainsAny(ignored[i], "\n\r") || len(ignored[i]) > 400 {
				t.Errorf("%s: line %d is %q, want it to say %q", name, i, ignored[i], want)
			}
		}
		// a line never holds a value of the file (an address, the name of a secret's variable): only keys and kinds
		for _, line := range ignored {
			if strings.Contains(line, "198.51.100.7") || strings.Contains(line, "FAIRY_SESSION_SECRET") {
				t.Errorf("%s: a line holds a value from the file: %q", name, line)
			}
		}
		// what is read passes the one check of the link fields, and is read back the same once it was written
		for id, e := range o.Agents {
			if p := config.LinkProblems(e.Named()); len(p) != 0 || e.Empty() {
				t.Errorf("%s: %s was taken with %v", name, id, p)
			}
			enc, _ := json.Marshal(map[string]any{hostOptionsKey: o.encode()})
			back := statefile.Data{}
			_ = json.Unmarshal(enc, &back)
			o2, ig := hostOptions(back)
			if !o2.Agents[id].Same(e) || o2.Rev != o.Rev {
				t.Errorf("%s: %s is not read back as it was written: %s", name, id, enc)
			}
			for _, line := range ig { // what is written back and still ignored is what is under an unknown key, and nothing else
				if !strings.Contains(line, "(and left in the file as it is)") {
					t.Errorf("%s: written back, and ignored at the next read: %q", name, line)
				}
			}
		}
		// the overrides that are read hold the three link fields and nothing else: what is under another key is
		// kept apart (to be written back as it was), and is no part of what anything is given to use
		for id, e := range o.Agents {
			enc, _ := json.Marshal(e.encode())
			for _, k := range []string{"url", "token_env", "power_control", "wol_relay", "node", "owner", "site", "wake_update"} {
				if strings.Contains(string(enc), `"`+k+`"`) {
					t.Errorf("%s: what is read for %s holds %q: %s", name, id, k, enc)
				}
			}
		}
	}
	// no key at all: nothing, and nothing to say
	if o, ignored := hostOptions(statefile.Data{}); len(o.Agents) != 0 || o.Rev != 0 || ignored != nil {
		t.Fatalf("no key: %+v %v", o, ignored)
	}
}

// What is stored is read back as it was stored, by this State and by another one on the same folder (a
// restart), and the three fields are kept apart from "not set".
func TestHostOptionsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)
	if o := a.HostOptions(); len(o.Agents) != 0 || o.Rev != 0 {
		t.Fatalf("a new folder: %+v", o)
	}
	want := HostOption{UPS: map[string]map[string]bool{"rack": {"lab-b-agent": true, "lab-node-pve-a": false}, "APC_1.5:b": {"pc": true}},
		Links: map[string]string{"pc-plug": "lab-b-agent", "old-plug": ""}, Exclude: map[string]bool{"corner-plug": true, "a": false}}
	after, changed, err := a.EditHostOptions("nora", func(o *HostOptions) error {
		o.Agents["nas-agent"] = want.Clone()
		e2 := o.Agents["pc"]
		e2.SetExclude("x", false) // "counted, whatever config.yaml says" is a decision too
		o.Agents["pc"] = e2
		o.Agents["empty"] = HostOption{}                                             // an entry with no decision is not stored
		o.Agents["hollow"] = HostOption{UPS: map[string]map[string]bool{"rack": {}}} // nor one with a UPS nothing is decided about
		return nil
	})
	if err != nil || !changed || after.Rev != 1 || len(after.Agents) != 2 {
		t.Fatalf("the first write: %+v %v %v", after, changed, err)
	}
	for _, st := range []*State{a, New(dir)} {
		o, ignored := st.HostOptionsRead()
		e := o.For("nas-agent")
		if len(ignored) != 0 || o.Rev != 1 || len(o.Agents) != 2 || !e.Same(want) || e.Count() != 7 ||
			e.By != "nora" || time.Since(e.At) > time.Minute || time.Since(e.At) < -time.Minute {
			t.Fatalf("read back: %+v %v", o, ignored)
		}
		if pc := o.For("pc"); !pc.Has(config.FieldExclude) || pc.Has(config.FieldPowerLinks) || pc.Exclude["x"] != false || len(pc.Exclude) != 1 {
			t.Fatalf("\"counted\" read back: %+v", pc)
		}
		if !o.For("nobody").Empty() {
			t.Fatal("an agent without an entry has overrides")
		}
	}
	// on the disk: the documented shape
	raw := readOptions(t, dir)
	nas := raw["agents"].(map[string]any)["nas-agent"].(map[string]any)
	if raw["rev"] != 1.0 || nas["by"] != "nora" || fmt.Sprint(nas["power_links"]) != "map[old-plug: pc-plug:lab-b-agent]" ||
		fmt.Sprint(nas["ups_powers"]) != "map[APC_1.5:b:map[pc:true] rack:map[lab-b-agent:true lab-node-pve-a:false]]" || fmt.Sprint(nas["power_exclude_from_total"]) != "map[a:false corner-plug:true]" {
		t.Fatalf("state.json: %v", raw)
	}
	// the same again writes nothing: the rev, the file and who changed it stay
	before, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	after, changed, err = a.EditHostOptions("someone else", func(o *HostOptions) error {
		e := o.Agents["nas-agent"]
		e.SetLink("pc-plug", "lab-b-agent")
		e.SetUPS("rack", "lab-node-pve-a", false)
		o.Agents["nas-agent"] = e
		return nil
	})
	now, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil || changed || after.Rev != 1 || !bytes.Equal(before, now) || after.For("nas-agent").By != "nora" {
		t.Fatalf("a change that changes nothing: changed %v rev %d err %v", changed, after.Rev, err)
	}
	// a change to one agent stamps that agent, not the other
	after, changed, _ = a.EditHostOptions("bea", func(o *HostOptions) error {
		e := o.Agents["pc"]
		e.SetExclude("x", true) // the same device, the other way round: a change
		o.Agents["pc"] = e
		return nil
	})
	if !changed || after.Rev != 2 || after.For("pc").By != "bea" || after.For("nas-agent").By != "nora" {
		t.Fatalf("the second change: %+v", after)
	}
	// an error of fn stops the write, and is the caller's error
	stop := errors.New("refused")
	if _, changed, err := a.EditHostOptions("bea", func(o *HostOptions) error { delete(o.Agents, "pc"); return stop }); !errors.Is(err, stop) || changed {
		t.Fatalf("an error of fn: %v %v", changed, err)
	}
	if o := a.HostOptions(); o.Rev != 2 || len(o.Agents) != 2 {
		t.Fatalf("after a refused change: %+v", o)
	}
	// the other keys of state.json are not touched by any of it
	if err := a.SetWakeUpdate("pc-a", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.EditHostOptions("bea", func(o *HostOptions) error { delete(o.Agents, "pc"); return nil }); err != nil {
		t.Fatal(err)
	}
	if !a.WakeUpdateEnabled("pc-a") || a.HostOptions().Rev != 3 || len(a.HostOptions().Agents) != 1 {
		t.Fatalf("the other keys: wake switch %v, options %+v", a.WakeUpdateEnabled("pc-a"), a.HostOptions())
	}
	// the last override taken out: the key stays with its counter (the rev never goes back)
	after, _, _ = a.EditHostOptions("bea", func(o *HostOptions) error { delete(o.Agents, "nas-agent"); return nil })
	if raw := readOptions(t, dir); after.Rev != 4 || raw["rev"] != 4.0 || len(raw["agents"].(map[string]any)) != 0 {
		t.Fatalf("no override left: %v", raw)
	}
}

// What the reader leaves out is said once, however often the panel asks and however often the file is written,
// and the next write drops from the file what is not of the form Fairy writes. (What is under a key this
// build does not know is kept: TestHostOptionsKeysOfANewerBuildSurviveASaveAndAReset.) A state.json that
// cannot be read gives no overrides (config.yaml counts).
func TestHostOptionsIgnoredIsSaidOnceAndDroppedByTheNextWrite(t *testing.T) {
	var logs bytes.Buffer
	var mu sync.Mutex
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return logs.Write(p) }), nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	warns := func() int {
		mu.Lock()
		defer mu.Unlock()
		return strings.Count(logs.String(), "level=WARN")
	}
	dir := t.TempDir()
	writeState(t, dir, `{"host_options": {"rev": 3, "agents": {"nas": {"url": "http://x", "power_links": 7, "ups_powers": {"rack": {"pc": true}}}, "BAD": {}}}}`)
	a := New(dir)
	for i := 0; i < 50; i++ {
		o, ignored := a.HostOptionsRead()
		if len(ignored) != 3 || !o.For("nas").Has(config.FieldUPSPowers) || o.For("nas").Has(config.FieldPowerLinks) || len(o.Agents) != 1 {
			t.Fatalf("read: %+v %q", o, ignored)
		}
		_ = a.HostOptions()
	}
	if n := warns(); n != 3 {
		t.Fatalf("%d WARN lines for three ignored things asked 100 times, want 3:\n%s", n, logs.String())
	}
	mu.Lock()
	said := logs.String()
	mu.Unlock()
	if !strings.Contains(said, `an \"Edit host\" override is ignored`) || !strings.Contains(said, "state.json") || strings.Contains(said, "http://x") {
		t.Fatalf("the WARN lines: %s", said)
	}
	// the next write keeps what was good and what is under an unknown key, and drops what is broken
	if _, _, err := a.EditHostOptions("nora", func(o *HostOptions) error {
		e := o.Agents["nas"]
		e.SetExclude("corner", true)
		o.Agents["nas"] = e
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if _, ignored := a.HostOptionsRead(); len(ignored) != 1 || !strings.Contains(ignored[0], `"url" cannot be set in the panel`) || !strings.Contains(string(raw), `"url": "http://x"`) ||
		strings.Contains(string(raw), "BAD") || strings.Contains(string(raw), `"power_links"`) || !strings.Contains(string(raw), `"rack"`) || a.HostOptions().Rev != 4 {
		t.Fatalf("after the next write: %s", raw)
	}
	for i := 0; i < 3; i++ { // more writes, to this key and to another one of the file
		if err := a.SetWakeUpdate(fmt.Sprintf("pc-%d", i), true); err != nil {
			t.Fatal(err)
		}
		if _, _, err := a.EditHostOptions("nora", func(o *HostOptions) error {
			e := o.Agents["nas"]
			e.SetExclude("corner", i%2 == 0)
			o.Agents["nas"] = e
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		_ = a.HostOptions()
	}
	if n := warns(); n != 3 {
		t.Fatalf("%d WARN lines after the file was written again and again, want still 3 (the key that stays is said once):\n%s", n, logs.String())
	}
	// unreadable: no overrides, no error for the caller (the start of the panel refuses such a file itself)
	writeState(t, dir, `{"host_options": {"agents": {"nas": {"ups_powers": {"rack": {"pc": true}}}}}`) // cut off
	b := New(dir)
	if o, ignored := b.HostOptionsRead(); len(o.Agents) != 0 || ignored != nil {
		t.Fatalf("a state.json that is not JSON: %+v %v", o, ignored)
	}
	if _, _, err := b.EditHostOptions("nora", func(o *HostOptions) error { return nil }); err == nil {
		t.Fatal("a write to a state.json that is not JSON did not fail")
	}
	if _, _, err := ReadHostOptions(dir); err == nil {
		t.Fatal("ReadHostOptions of a state.json that is not JSON gave no error")
	}
	// ReadHostOptions (the CLI's reader) gives the same as the panel's, and logs nothing
	writeState(t, dir, `{"host_options": {"rev": 3, "agents": {"nas": {"url": "http://x", "ups_powers": {"rack": {"pc": true}}}}}}`)
	n := warns()
	o, ignored, err := ReadHostOptions(dir)
	if err != nil || len(ignored) != 1 || !o.For("nas").UPS["rack"]["pc"] || o.Rev != 3 || warns() != n {
		t.Fatalf("ReadHostOptions: %+v %q %v", o, ignored, err)
	}
	if o, ignored, err := ReadHostOptions(t.TempDir()); err != nil || len(o.Agents) != 0 || ignored != nil {
		t.Fatalf("ReadHostOptions of a folder without state.json: %+v %v %v", o, ignored, err)
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// However much of the file is not what Fairy writes, what is said about it is short: the first twenty things,
// one line each, and how many more there are. The count is the whole number (the dialog says it); the log gets
// the twenty lines and the one about the rest, once.
func TestHostOptionsIgnoredLinesAreCapped(t *testing.T) {
	var logs bytes.Buffer
	var mu sync.Mutex
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return logs.Write(p) }), nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	var keys, agents []string
	for i := 0; i < 300; i++ {
		keys = append(keys, fmt.Sprintf(`"k%03d": %d`, i, i))
	}
	for i := 0; i < 200; i++ {
		agents = append(agents, fmt.Sprintf(`"Bad Agent %03d": {}`, i))
	}
	dir := t.TempDir()
	writeState(t, dir, `{"host_options": {"agents": {"nas": {"power_links": {"plug": "pc"}, `+strings.Join(keys, ",")+`}, `+strings.Join(agents, ",")+`}}}`)
	a := New(dir)
	for i := 0; i < 5; i++ {
		o, ignored := a.HostOptionsRead()
		if len(ignored) != MaxIgnoredLines+1 || o.Ignored != 500 || ignored[MaxIgnoredLines] != "and 480 more that are ignored in the same way" || o.For("nas").Links["plug"] != "pc" {
			t.Fatalf("500 ignored things: %d lines, count %d, last line %q", len(ignored), o.Ignored, ignored[len(ignored)-1])
		}
		if !strings.Contains(ignored[0], `"Bad Agent 000" is not an id an agent can have`) || !strings.Contains(ignored[19], `"Bad Agent 019"`) {
			t.Fatalf("the first twenty are the first twenty: %q ... %q", ignored[0], ignored[19])
		}
	}
	mu.Lock()
	said := logs.String()
	mu.Unlock()
	if n := strings.Count(said, "level=WARN"); n != MaxIgnoredLines+1 || strings.Count(said, "and 480 more that are ignored in the same way") != 1 {
		t.Fatalf("%d WARN lines for 500 ignored things, want %d:\n%s", n, MaxIgnoredLines+1, said)
	}
	// the CLI's reader gives the same lines; a file with a few things says each of them and nothing about more
	if o, ignored, err := ReadHostOptions(dir); err != nil || len(ignored) != MaxIgnoredLines+1 || o.Ignored != 500 {
		t.Fatalf("ReadHostOptions: %d lines, count %d, %v", len(ignored), o.Ignored, err)
	}
	writeState(t, dir, `{"host_options": {"agents": {"nas": {`+strings.Join(keys[:MaxIgnoredLines], ",")+`}}}}`)
	if o, ignored, _ := ReadHostOptions(dir); len(ignored) != MaxIgnoredLines || o.Ignored != MaxIgnoredLines || strings.Contains(ignored[MaxIgnoredLines-1], "more") {
		t.Fatalf("exactly twenty: %d lines, count %d", len(ignored), o.Ignored)
	}
	if o, ignored, _ := ReadHostOptions(t.TempDir()); o.Ignored != 0 || ignored != nil {
		t.Fatalf("nothing ignored: %d %v", o.Ignored, ignored)
	}
}

// Keys under "host_options" that this build does not know (a newer build wrote them, and the panel was rolled
// back) are not used, and they are not lost either: a save and a Reset write them back as they were, at the
// top of the key and in every agent's entry, also in an entry that has nothing else. Before, the first Edit
// host save after a rollback silently dropped them for every agent.
func TestHostOptionsKeysOfANewerBuildSurviveASaveAndAReset(t *testing.T) {
	dir := t.TempDir()
	const top = `"format": 2, "notes": {"a": [1, 2.5, null, true, "x", {"deep": []}], "b": {"c": "d"}}`
	const nas = `"wake_update": {"off_after_s": 600, "shutdown_delay_s": 120}, "label": "Rack room", "n": 12345678`
	const pc = `"wake_update": {"off_after_s": 300}, "names": ["a", "b"]`
	writeState(t, dir, `{"host_models": {"x": "y"}, "host_options": {"rev": 4, `+top+`, "agents": {
		"nas": {"ups_powers": {"rack": {"pc": true}}, `+nas+`, "by": "nora", "at": "2026-11-13T23:40:00Z"},
		"pc": {`+pc+`},
		"tower": {"power_links": {"plug": "pc"}}}}}`)
	canon := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	want := func(obj string) string {
		var v map[string]any
		if err := json.Unmarshal([]byte("{"+obj+"}"), &v); err != nil {
			t.Fatal(err)
		}
		return canon(v)
	}
	// unknown is the keys of m that are none of known, as they are in the file now
	unknown := func(m map[string]any, known ...string) string {
		out := map[string]any{}
		for k, v := range m {
			if !slices.Contains(known, k) {
				out[k] = v
			}
		}
		return canon(out)
	}
	check := func(step string, rev float64, agentsWant string) {
		t.Helper()
		raw := readOptions(t, dir)
		agents, _ := raw["agents"].(map[string]any)
		nasNow, _ := agents["nas"].(map[string]any)
		pcNow, _ := agents["pc"].(map[string]any)
		if got := unknown(raw, "rev", "agents"); got != want(top) {
			t.Errorf("%s: the unknown keys at the top:\n%s\nwant\n%s", step, got, want(top))
		}
		if got := unknown(nasNow, "ups_powers", "power_links", "power_exclude_from_total", "by", "at"); got != want(nas) {
			t.Errorf("%s: the unknown keys of nas:\n%s\nwant\n%s", step, got, want(nas))
		}
		if got := canon(pcNow); got != want(pc) {
			t.Errorf("%s: the entry of pc, which has unknown keys only:\n%s\nwant\n%s", step, got, want(pc))
		}
		if got := strings.Join(slices.Sorted(maps.Keys(agents)), " "); raw["rev"] != rev || got != agentsWant {
			t.Errorf("%s: rev %v, entries %q; want rev %v, entries %q", step, raw["rev"], got, rev, agentsWant)
		}
		// they are still not used, and still said
		o, ignored := New(dir).HostOptionsRead()
		if _, has := o.Agents["pc"]; has || len(ignored) != 7 {
			t.Errorf("%s: read: agents %v, ignored %q", step, slices.Sorted(maps.Keys(o.Agents)), ignored)
		}
	}
	a := New(dir)
	check("as written by the newer build", 4, "nas pc tower")
	// an Edit host save for another agent
	if _, changed, err := a.EditHostOptions("bea", func(o *HostOptions) error {
		e := o.Agents["tower"]
		e.SetExclude("plug", true)
		o.Agents["tower"] = e
		return nil
	}); err != nil || !changed {
		t.Fatal(changed, err)
	}
	check("after a save", 5, "nas pc tower")
	// a Reset of the one thing this build knows in the entry of nas: the entry stays, with what it does not know
	after, _, err := a.EditHostOptions("bea", func(o *HostOptions) error {
		e := o.Agents["nas"]
		e.DropUPS("rack", "pc")
		o.Agents["nas"] = e
		return nil
	})
	if _, has := after.Agents["nas"]; err != nil || has {
		t.Fatalf("after the Reset nas has no change this build knows: %+v %v", after.Agents, err)
	}
	check("after a Reset", 6, "nas pc tower")
	if raw := readOptions(t, dir)["agents"].(map[string]any)["nas"].(map[string]any); raw["ups_powers"] != nil || raw["by"] != nil {
		t.Errorf("the entry of nas after the Reset: %v", raw)
	}
	// an entry without unknown keys goes when its last change goes, as before
	if _, _, err := a.EditHostOptions("bea", func(o *HostOptions) error { delete(o.Agents, "tower"); return nil }); err != nil {
		t.Fatal(err)
	}
	check("after the last change of tower was taken out", 7, "nas pc")
	// a change for an agent that had unknown keys only: both are in its entry
	if _, _, err := a.EditHostOptions("bea", func(o *HostOptions) error {
		o.Agents["pc"] = HostOption{Links: map[string]string{"own": "tower"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if raw := readOptions(t, dir)["agents"].(map[string]any)["pc"].(map[string]any); unknown(raw, "power_links", "by", "at") != want(pc) || fmt.Sprint(raw["power_links"]) != "map[own:tower]" || raw["by"] != "bea" {
		t.Errorf("the entry of pc with a change of this build next to the unknown keys: %v", raw)
	}
	// Remove of a host forgets its entry whole, what this build does not know in it too (it was about that machine)
	if _, _, err := a.EditHostOptions("bea", func(o *HostOptions) error { o.Forget("pc"); return nil }); err != nil {
		t.Fatal(err)
	}
	raw := readOptions(t, dir)
	if agents := raw["agents"].(map[string]any); agents["pc"] != nil || unknown(agents["nas"].(map[string]any)) != want(nas) || unknown(raw, "rev", "agents") != want(top) {
		t.Errorf("after Forget of pc: %v", raw)
	}
	// the rest of state.json was never touched
	if d, _ := statefile.Read(filepath.Join(dir, "state.json")); fmt.Sprint(d["host_models"]) != "map[x:y]" {
		t.Errorf("host_models: %v", d["host_models"])
	}
}

// "by" is a name out of a file: whatever is there, what is read of it is one short line (the dialog shows it
// as a tooltip, `fairy admin config check` prints it), and what is not a text or a time reads as none.
func TestHostOptionsWhoAndWhenAreReadAsOneShortLine(t *testing.T) {
	long := strings.Repeat("n", 5000)
	for name, c := range map[string]struct{ by, want string }{
		"long":          {`"` + long + `"`, long[:80] + "…"},
		"line breaks":   {`"nora\r\nlevel=ERROR msg=made-up\u2028x\u0000y\u007f"`, "nora  level=ERROR msg=made-up x y "},
		"a short one":   {`"nora@example.com (unverified)"`, "nora@example.com (unverified)"},
		"not a text":    {`{"name": "nora"}`, ""},
		"cut in a rune": {`"` + strings.Repeat("é", 60) + `"`, strings.Repeat("é", 40) + "…"},
	} {
		d := statefile.Data{}
		if err := json.Unmarshal([]byte(`{"host_options": {"agents": {"nas": {"power_links": {"plug": "pc"}, "by": `+c.by+`, "at": "2026-11-13T23:40:00Z"}}}}`), &d); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		o, ignored := hostOptions(d)
		if e := o.For("nas"); e.By != c.want || len(ignored) != 0 || e.At.IsZero() || e.Links["plug"] != "pc" {
			t.Errorf("%s: by %q (%d bytes), want %q; ignored %q", name, e.By, len(e.By), c.want, ignored)
		}
	}
	// the writer's name is cut the same way before it is stored
	dir := t.TempDir()
	after, _, err := New(dir).EditHostOptions(long+"\nx", func(o *HostOptions) error {
		o.Agents["nas"] = HostOption{Links: map[string]string{"plug": "pc"}}
		return nil
	})
	if err != nil || after.For("nas").By != long[:80]+"…" || readOptions(t, dir)["agents"].(map[string]any)["nas"].(map[string]any)["by"] != long[:80]+"…" {
		t.Fatalf("written: %q %v", after.For("nas").By, err)
	}
}

// Many writers at once, each changing its own thing in the entry of the same agent: every change is there
// afterwards, the counter counted every write, and the file is whole after each of them. (The lock is the
// file's: two processes, the panel and a CLI, are held apart by it too.)
func TestHostOptionsWritersAtOnce(t *testing.T) {
	dir := t.TempDir()
	const n = 24
	var wg sync.WaitGroup
	revs := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := New(dir) // each its own view of the file, as two processes have
			after, changed, err := st.EditHostOptions(fmt.Sprintf("admin%d", i), func(o *HostOptions) error {
				e := o.Agents["nas"]
				e.SetLink(fmt.Sprintf("plug%d", i%16), fmt.Sprintf("pc%d", i))
				o.Agents["nas"] = e
				if i%3 == 0 {
					o.Agents[fmt.Sprintf("agent%d", i)] = HostOption{Exclude: map[string]bool{"x": true}}
				}
				return nil
			})
			if err != nil || !changed {
				t.Errorf("writer %d: %v %v", i, changed, err)
			}
			revs[i] = after.Rev
			// the answer is the state this write left: it holds this writer's own change
			if after.For("nas").Links[fmt.Sprintf("plug%d", i%16)] != fmt.Sprintf("pc%d", i) {
				t.Errorf("writer %d: its answer does not hold its change", i)
			}
		}()
	}
	wg.Wait()
	slices.Sort(revs)
	for i, r := range revs {
		if r != i+1 {
			t.Fatalf("the revs of %d writes at once: %v", n, revs)
		}
	}
	o, ignored := New(dir).HostOptionsRead()
	if len(ignored) != 0 || o.Rev != n || len(o.For("nas").Links) != 16 || len(o.Agents) != 1+n/3 {
		t.Fatalf("after %d writes: rev %d, %d links, %d agents, ignored %v", n, o.Rev, len(o.For("nas").Links), len(o.Agents), ignored)
	}
	if raw := readOptions(t, dir); raw["rev"] != float64(n) {
		t.Fatalf("state.json: rev %v", raw["rev"])
	}
}

// Forget is what Remove of a host does: its own overrides, and every decision about one of its ids in the
// overrides of the others. Nothing else is touched.
func TestHostOptionsForget(t *testing.T) {
	mk := func() HostOptions {
		o := HostOptions{Rev: 5, Agents: map[string]HostOption{}}
		o.Agents["nas"] = HostOption{
			UPS:     map[string]map[string]bool{"rack": {"pc": true, "node-pc": true, "keep": true}, "desk": {"keep": true}, "attic": {"pc": false}},
			Links:   map[string]string{"a": "pc", "b": "node-pc", "c": "keep", "pc": "keep", "d": ""},
			Exclude: map[string]bool{"pc": true, "a": true}, // device ids: a device called like the host is no link to it
		}
		o.Agents["pc"] = HostOption{Exclude: map[string]bool{"own": true}}
		o.Agents["other"] = HostOption{UPS: map[string]map[string]bool{"ups": {"keep": true}}}
		o.Agents["bare"] = HostOption{Links: map[string]string{"x": "pc"}}
		return o
	}
	o := mk()
	gone := o.Forget("pc", "node-pc")
	want := []string{"the overrides of pc", "bare: device x is no longer linked to it", "nas: what was changed about UPS attic and it", "nas: UPS rack no longer powers it",
		"nas: device a is no longer linked to it", "nas: device b is no longer linked to it"}
	if !slices.Equal(gone, want) {
		t.Fatalf("gone:\n%q\nwant\n%q", gone, want)
	}
	nas := o.Agents["nas"]
	if _, has := o.Agents["pc"]; has || fmt.Sprint(nas.UPS) != "map[desk:map[keep:true] rack:map[keep:true]]" || fmt.Sprint(nas.Links) != "map[c:keep d: pc:keep]" || fmt.Sprint(nas.Exclude) != "map[a:true pc:true]" {
		t.Fatalf("after Forget: %+v", o.Agents)
	}
	if !o.Agents["other"].Same(mk().Agents["other"]) || !o.Agents["bare"].Empty() {
		t.Fatalf("the others: %+v", o.Agents)
	}
	// nothing about the host: nothing is said and nothing changes
	o2 := mk()
	if gone := o2.Forget("stranger", "node-stranger"); gone != nil || len(o2.Agents) != 4 || !o2.Agents["nas"].Same(mk().Agents["nas"]) {
		t.Fatalf("a host nobody links to: %q", gone)
	}
	// the agent's own id counts as one of its rows, also when no other id is given; a row that is not named stays
	o3 := mk()
	if gone := o3.Forget("pc"); len(gone) != 5 || fmt.Sprint(o3.Agents["nas"].UPS["rack"]) != "map[keep:true node-pc:true]" || o3.Agents["nas"].Links["b"] != "node-pc" {
		t.Fatalf("Forget by the agent's id alone: %q %+v", gone, o3.Agents["nas"])
	}
	// what Forget changed is what is written: the entries of others are copies, not the caller's maps
	o4 := mk()
	keep := o4.Clone()
	o4.Forget("stranger")
	if !o4.Agents["nas"].Same(keep.Agents["nas"]) {
		t.Fatal("Forget of a stranger changed an entry")
	}
}
