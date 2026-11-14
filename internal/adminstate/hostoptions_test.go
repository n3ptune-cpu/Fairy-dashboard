package adminstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

// An override replaces config.yaml's value of its field, whole, and only of its field; without one the
// entry's value counts. "Set to nothing" is an override too.
func TestHostOptionApply(t *testing.T) {
	base := config.Links{UPSPowers: map[string][]string{"rack": {"pc-a"}}, PowerLinks: map[string]string{"plug": "pc-a"}, PowerExcludeFromTotal: []string{"corner"}}
	var o HostOption
	if got := o.Apply(base); !SameLinks(got, base) || !o.Empty() {
		t.Fatalf("no override: %+v", got)
	}
	o.Set(config.FieldUPSPowers, config.Links{UPSPowers: map[string][]string{"desk": {"pc-b"}}, PowerLinks: map[string]string{"x": "y"}})
	got := o.Apply(base)
	if len(got.UPSPowers) != 1 || !slices.Equal(got.UPSPowers["desk"], []string{"pc-b"}) || got.PowerLinks["plug"] != "pc-a" || len(got.PowerLinks) != 1 || !slices.Equal(got.PowerExcludeFromTotal, []string{"corner"}) {
		t.Fatalf("ups_powers overridden: %+v", got)
	}
	if !o.Has(config.FieldUPSPowers) || o.Has(config.FieldPowerLinks) || o.Has(config.FieldExclude) || o.Has("url") || o.Empty() {
		t.Fatalf("Has: %+v", o)
	}
	// the result shares nothing with the entry or the override
	got.UPSPowers["desk"][0], got.PowerLinks["plug"], got.PowerExcludeFromTotal[0] = "changed", "changed", "changed"
	if o.Links.UPSPowers["desk"][0] != "pc-b" || base.PowerLinks["plug"] != "pc-a" || base.PowerExcludeFromTotal[0] != "corner" {
		t.Fatal("Apply shares with its inputs")
	}
	// set to nothing: an override that says "none", not "as config.yaml"
	o.Set(config.FieldPowerLinks, config.Links{})
	o.Set(config.FieldExclude, config.Links{})
	if got := o.Apply(base); len(got.PowerLinks) != 0 || len(got.PowerExcludeFromTotal) != 0 || got.PowerLinks == nil || got.PowerExcludeFromTotal == nil {
		t.Fatalf("set to nothing: %+v", got)
	}
	// Reset: the field is config.yaml's again
	if !o.Reset(config.FieldPowerLinks) || o.Reset(config.FieldPowerLinks) || o.Reset("url") {
		t.Fatal("Reset reports whether there was an override")
	}
	if got := o.Apply(base); got.PowerLinks["plug"] != "pc-a" || len(got.PowerExcludeFromTotal) != 0 || len(got.UPSPowers["desk"]) != 1 {
		t.Fatalf("after Reset of power_links: %+v", got)
	}
	o.Reset(config.FieldUPSPowers)
	o.Reset(config.FieldExclude)
	if !o.Empty() || !SameLinks(o.Apply(base), base) {
		t.Fatal("every field reset is no override")
	}
	// a field name that is none of the three sets nothing
	o.Set("url", config.Links{PowerLinks: map[string]string{"a": "b"}})
	if !o.Empty() {
		t.Fatal("Set took a field that is no link field")
	}
}

// The reader never fails and never takes more than the three link fields. Whatever is not what Fairy writes
// is left out with one line; the rest is used.
func TestHostOptionsReaderIsTolerant(t *testing.T) {
	var seventeen []string
	for i := 0; i < 17; i++ {
		seventeen = append(seventeen, fmt.Sprintf(`"d%d"`, i))
	}
	many := map[string]any{}
	for i := 0; i <= MaxHostOptionAgents; i++ {
		many[fmt.Sprintf("a%d", i)] = map[string]any{"power_exclude_from_total": []any{"x"}}
	}
	manyJSON, _ := json.Marshal(map[string]any{"agents": many})
	for name, c := range map[string]struct {
		overlay string
		ignored []string // a part of each line, in order
		agents  string   // the agents with overrides and their fields: "nas:ups_powers+power_links ..."
		rev     int
	}{
		"what Fairy writes": {`{"rev": 7, "agents": {"nas": {"ups_powers": {"rack": ["pc-a", "lab-node-pve-a"]}, "power_links": {"pc-plug": "pc-a"}, "power_exclude_from_total": ["corner"], "by": "nora", "at": "2026-11-13T23:40:00Z"}}}`,
			nil, "nas:ups_powers+power_links+power_exclude_from_total", 7},
		"empty overrides are overrides": {`{"agents": {"nas": {"ups_powers": {}, "power_exclude_from_total": []}}}`, nil, "nas:ups_powers+power_exclude_from_total", 0},
		"an entry with nothing in it":   {`{"agents": {"nas": {"by": "nora"}}}`, nil, "", 0},
		"a text":                        {`"x"`, []string{"is not the object Fairy writes (found a text)"}, "", 0},
		"a list":                        {`[]`, []string{"found a list"}, "", 0},
		"null":                          {`null`, []string{"found null"}, "", 0},
		"a number":                      {`7`, []string{"found a number"}, "", 0},
		"true":                          {`true`, []string{"found true / false"}, "", 0},
		"agents is a text":              {`{"rev": 2, "agents": "all"}`, []string{`"agents" is not an object (found a text)`}, "", 2},
		"rev is a text":                 {`{"rev": "7", "agents": {"nas": {"power_exclude_from_total": ["x"]}}}`, []string{`"rev" is not a whole number`}, "nas:power_exclude_from_total", 0},
		"rev is negative":               {`{"rev": -1}`, []string{`"rev" is not a whole number`}, "", 0},
		"rev is a fraction":             {`{"rev": 1.5}`, []string{`"rev" is not a whole number`}, "", 0},
		"rev is huge":                   {`{"rev": 1e300}`, []string{`"rev" is not a whole number`}, "", 0},
		"another key at the top":        {`{"rev": 1, "hosts": {}, "agents": {}}`, []string{`the key "hosts" is not one Fairy writes`}, "", 1},
		"an id no agent can have":       {`{"agents": {"NAS": {"power_exclude_from_total": ["x"]}, "": {}, "a/b": {}, "ok": {"power_exclude_from_total": ["x"]}}}`, []string{`"" is not an id`, `"NAS" is not an id`, `"a/b" is not an id`}, "ok:power_exclude_from_total", 0},
		"an entry that is no object":    {`{"agents": {"nas": ["ups_powers"], "pc": null}}`, []string{"agent nas: its entry is not an object (found a list)", "agent pc: its entry is not an object (found null)"}, "", 0},
		"fields of the wrong form":      {`{"agents": {"nas": {"ups_powers": {"rack": "pc-a"}, "power_links": ["a"], "power_exclude_from_total": {"a": true}}}}`, []string{"power_exclude_from_total is not of the form", "power_links is not of the form", "ups_powers is not of the form"}, "", 0},
		"a list with a number in it":    {`{"agents": {"nas": {"power_exclude_from_total": ["a", 7], "ups_powers": {"rack": ["pc", null]}, "power_links": {"a": {}}}}}`, []string{"power_exclude_from_total is not of the form", "power_links is not of the form", "ups_powers is not of the form"}, "", 0},
		"seventeen devices":             {`{"agents": {"nas": {"power_exclude_from_total": [` + strings.Join(seventeen, ",") + `], "power_links": {"plug": "pc"}}}}`, []string{"power_exclude_from_total: 17 devices"}, "nas:power_links", 0},
		"a list far over every limit":   {`{"agents": {"nas": {"power_exclude_from_total": [` + strings.Repeat(`"x",`, 500) + `"x"]}}}`, []string{"power_exclude_from_total is not of the form"}, "", 0},
		"ids no row or device can have": {`{"agents": {"nas": {"ups_powers": {"rack": ["PC A"]}, "power_links": {"Bad_ID": "pc"}, "power_exclude_from_total": ["ok"]}}}`, []string{"power_links:", "ups_powers:"}, "nas:power_exclude_from_total", 0},
		"a UPS name no UPS can have":    {`{"agents": {"nas": {"ups_powers": {"rack ups": ["pc"]}}}}`, []string{"is not a name a UPS can have"}, "", 0},
		"what cannot be set here": {`{"agents": {"nas": {"url": "http://198.51.100.7:9101", "token_env": "FAIRY_SESSION_SECRET", "control_token_env": "X", "verify_tls": false, "node": "pve", "links_to": "node-x", "fan_control": {"vendor": "supermicro"}, "power_control": true, "self_update": true, "wol_relay": true, "owner": "eve", "site": "Home", "name": "x", "id": "other", "wake_update": {"off_after_s": 300}, "power_exclude_from_total": ["corner"]}}}`,
			[]string{`"control_token_env" cannot be set`, `"fan_control" cannot`, `"id" cannot`, `"links_to" cannot`, `"name" cannot`, `"node" cannot`, `"owner" cannot`, `"power_control" cannot`, `"self_update" cannot`, `"site" cannot`, `"token_env" cannot`, `"url" cannot`, `"verify_tls" cannot`, `"wake_update" cannot`, `"wol_relay" cannot`}, "nas:power_exclude_from_total", 0},
		"more agents than a panel has": {string(manyJSON), []string{"more than the 256 a panel can have"}, "", 0},
		"a name that is not text":      {`{"agents": {"nas": {"power_exclude_from_total": ["x"], "by": 7, "at": "yesterday"}}}`, nil, "nas:power_exclude_from_total", 0},
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
		// the overrides that are read hold the three link fields and nothing else
		for id, e := range o.Agents {
			enc, _ := json.Marshal(HostOptions{Agents: map[string]HostOption{id: e}}.encode())
			for _, k := range []string{"url", "token_env", "power_control", "wol_relay", "node", "owner", "site", "wake_update"} {
				if strings.Contains(string(enc), `"`+k+`"`) {
					t.Errorf("%s: what is written back holds %q: %s", name, k, enc)
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
	want := config.Links{UPSPowers: map[string][]string{"rack": {"lab-b-agent", "lab-node-pve-a"}, "APC_1.5:b": {"pc"}},
		PowerLinks: map[string]string{"pc-plug": "lab-b-agent"}, PowerExcludeFromTotal: []string{"corner-plug", "a"}}
	after, changed, err := a.EditHostOptions("nora", func(o *HostOptions) error {
		e := o.Agents["nas-agent"]
		for _, f := range config.LinkFields {
			e.Set(f, want)
		}
		o.Agents["nas-agent"] = e
		e2 := o.Agents["pc"]
		e2.Set(config.FieldExclude, config.Links{}) // set to nothing
		o.Agents["pc"] = e2
		o.Agents["empty"] = HostOption{} // an entry with no field is not stored
		return nil
	})
	if err != nil || !changed || after.Rev != 1 || len(after.Agents) != 2 {
		t.Fatalf("the first write: %+v %v %v", after, changed, err)
	}
	for _, st := range []*State{a, New(dir)} {
		o, ignored := st.HostOptionsRead()
		e := o.For("nas-agent")
		if len(ignored) != 0 || o.Rev != 1 || len(o.Agents) != 2 || !SameLinks(e.Links, want) || !e.HasUPSPowers || !e.HasPowerLinks || !e.HasExclude ||
			e.By != "nora" || time.Since(e.At) > time.Minute || time.Since(e.At) < -time.Minute {
			t.Fatalf("read back: %+v %v", o, ignored)
		}
		if pc := o.For("pc"); !pc.HasExclude || pc.HasPowerLinks || pc.Links.PowerExcludeFromTotal == nil || len(pc.Links.PowerExcludeFromTotal) != 0 {
			t.Fatalf("\"set to nothing\" read back: %+v", pc)
		}
		if !o.For("nobody").Empty() {
			t.Fatal("an agent without an entry has overrides")
		}
	}
	// on the disk: the documented shape
	raw := readOptions(t, dir)
	nas := raw["agents"].(map[string]any)["nas-agent"].(map[string]any)
	if raw["rev"] != 1.0 || nas["by"] != "nora" || nas["power_links"].(map[string]any)["pc-plug"] != "lab-b-agent" || len(nas["ups_powers"].(map[string]any)["rack"].([]any)) != 2 {
		t.Fatalf("state.json: %v", raw)
	}
	// the same again writes nothing: the rev, the file and who changed it stay
	before, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	after, changed, err = a.EditHostOptions("someone else", func(o *HostOptions) error {
		e := o.Agents["nas-agent"]
		e.Set(config.FieldPowerLinks, want)
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
		e.Set(config.FieldExclude, config.Links{PowerExcludeFromTotal: []string{"x"}})
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

// What the reader leaves out is said once per change of the file, however often the panel asks, and the next
// write drops it from the file. A state.json that cannot be read gives no overrides (config.yaml counts).
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
	writeState(t, dir, `{"host_options": {"rev": 3, "agents": {"nas": {"url": "http://x", "power_links": 7, "ups_powers": {"rack": ["pc"]}}, "BAD": {}}}}`)
	a := New(dir)
	for i := 0; i < 50; i++ {
		o, ignored := a.HostOptionsRead()
		if len(ignored) != 3 || !o.For("nas").HasUPSPowers || o.For("nas").HasPowerLinks || len(o.Agents) != 1 {
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
	// the next write keeps what was good and drops the rest
	if _, _, err := a.EditHostOptions("nora", func(o *HostOptions) error {
		e := o.Agents["nas"]
		e.Set(config.FieldExclude, config.Links{PowerExcludeFromTotal: []string{"corner"}})
		o.Agents["nas"] = e
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if _, ignored := a.HostOptionsRead(); len(ignored) != 0 || strings.Contains(string(raw), "http://x") || strings.Contains(string(raw), "BAD") || !strings.Contains(string(raw), `"rack"`) || a.HostOptions().Rev != 4 {
		t.Fatalf("after the next write: %s", raw)
	}
	if n := warns(); n != 3 {
		t.Fatalf("%d WARN lines after the file was put right, want still 3", n)
	}
	// unreadable: no overrides, no error for the caller (the start of the panel refuses such a file itself)
	writeState(t, dir, `{"host_options": {"agents": {"nas": {"ups_powers": {"rack": ["pc"]}}}}`) // cut off
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
	writeState(t, dir, `{"host_options": {"rev": 3, "agents": {"nas": {"url": "http://x", "ups_powers": {"rack": ["pc"]}}}}}`)
	n := warns()
	o, ignored, err := ReadHostOptions(dir)
	if err != nil || len(ignored) != 1 || !o.For("nas").HasUPSPowers || o.Rev != 3 || warns() != n {
		t.Fatalf("ReadHostOptions: %+v %q %v", o, ignored, err)
	}
	if o, ignored, err := ReadHostOptions(t.TempDir()); err != nil || len(o.Agents) != 0 || ignored != nil {
		t.Fatalf("ReadHostOptions of a folder without state.json: %+v %v %v", o, ignored, err)
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

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
				l := e.Links.Clone()
				if l.PowerLinks == nil {
					l.PowerLinks = map[string]string{}
				}
				l.PowerLinks[fmt.Sprintf("plug%d", i%16)] = fmt.Sprintf("pc%d", i)
				e.Set(config.FieldPowerLinks, l)
				o.Agents["nas"] = e
				if i%3 == 0 {
					o.Agents[fmt.Sprintf("agent%d", i)] = HostOption{HasExclude: true, Links: config.Links{PowerExcludeFromTotal: []string{"x"}}}
				}
				return nil
			})
			if err != nil || !changed {
				t.Errorf("writer %d: %v %v", i, changed, err)
			}
			revs[i] = after.Rev
			// the answer is the state this write left: it holds this writer's own change
			if after.For("nas").Links.PowerLinks[fmt.Sprintf("plug%d", i%16)] != fmt.Sprintf("pc%d", i) {
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
	if len(ignored) != 0 || o.Rev != n || len(o.For("nas").Links.PowerLinks) != 16 || len(o.Agents) != 1+n/3 {
		t.Fatalf("after %d writes: rev %d, %d links, %d agents, ignored %v", n, o.Rev, len(o.For("nas").Links.PowerLinks), len(o.Agents), ignored)
	}
	if raw := readOptions(t, dir); raw["rev"] != float64(n) {
		t.Fatalf("state.json: rev %v", raw["rev"])
	}
}

// Forget is what Remove of a host does: its own overrides, and every link to one of its ids in the overrides
// of the others. Nothing else is touched.
func TestHostOptionsForget(t *testing.T) {
	mk := func() HostOptions {
		o := HostOptions{Rev: 5, Agents: map[string]HostOption{}}
		var nas, pc, other, bare HostOption
		nas.Set(config.FieldUPSPowers, config.Links{UPSPowers: map[string][]string{"rack": {"pc", "node-pc", "keep"}, "desk": {"keep"}, "attic": {"pc"}}})
		nas.Set(config.FieldPowerLinks, config.Links{PowerLinks: map[string]string{"a": "pc", "b": "node-pc", "c": "keep", "pc": "keep"}})
		nas.Set(config.FieldExclude, config.Links{PowerExcludeFromTotal: []string{"pc", "a"}}) // device ids: a device called like the host is no link to it
		pc.Set(config.FieldExclude, config.Links{PowerExcludeFromTotal: []string{"own"}})
		other.Set(config.FieldUPSPowers, config.Links{UPSPowers: map[string][]string{"ups": {"keep"}}})
		bare.Set(config.FieldPowerLinks, config.Links{PowerLinks: map[string]string{"x": "pc"}})
		o.Agents["nas"], o.Agents["pc"], o.Agents["other"], o.Agents["bare"] = nas, pc, other, bare
		return o
	}
	o := mk()
	gone := o.Forget("pc", "node-pc")
	want := []string{"the overrides of pc", "bare: device x is no longer linked to it", "nas: UPS attic no longer powers it", "nas: UPS rack no longer powers it",
		"nas: device a is no longer linked to it", "nas: device b is no longer linked to it"}
	if !slices.Equal(gone, want) {
		t.Fatalf("gone:\n%q\nwant\n%q", gone, want)
	}
	nas := o.Agents["nas"]
	if _, has := o.Agents["pc"]; has || !slices.Equal(nas.Links.UPSPowers["rack"], []string{"keep"}) || len(nas.Links.UPSPowers["attic"]) != 0 || !slices.Equal(nas.Links.UPSPowers["desk"], []string{"keep"}) ||
		len(nas.Links.PowerLinks) != 2 || nas.Links.PowerLinks["c"] != "keep" || nas.Links.PowerLinks["pc"] != "keep" || !slices.Equal(nas.Links.PowerExcludeFromTotal, []string{"pc", "a"}) {
		t.Fatalf("after Forget: %+v", o.Agents)
	}
	if !sameOption(o.Agents["other"], mk().Agents["other"]) || !o.Agents["bare"].HasPowerLinks || len(o.Agents["bare"].Links.PowerLinks) != 0 {
		t.Fatalf("the others: %+v", o.Agents)
	}
	// nothing about the host: nothing is said and nothing changes
	o2 := mk()
	if gone := o2.Forget("stranger", "node-stranger"); gone != nil || len(o2.Agents) != 4 || !sameOption(o2.Agents["nas"], mk().Agents["nas"]) {
		t.Fatalf("a host nobody links to: %q", gone)
	}
	// the agent's own id counts as one of its rows, also when no other id is given
	o3 := mk()
	if gone := o3.Forget("pc"); len(gone) != 5 || slices.Contains(o3.Agents["nas"].Links.UPSPowers["rack"], "pc") || !slices.Contains(o3.Agents["nas"].Links.UPSPowers["rack"], "node-pc") {
		t.Fatalf("Forget by the agent's id alone: %q %+v", gone, o3.Agents["nas"])
	}
}
