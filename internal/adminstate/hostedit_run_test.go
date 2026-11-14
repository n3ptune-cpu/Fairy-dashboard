package adminstate

import (
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// "Edit host", a machine's own run timings in state.json ("host_options", "hosts", "<id>", "wake_update").

// What is written is read back; a machine with nothing but a timing has an entry; the same again writes
// nothing; a decision that goes takes its key with it, and the last one the whole entry.
func TestHostRunTimingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeState(t, dir, `{"host_options": {"rev": 2, "agents": {}}}`)
	a := New(dir)
	set := func(by string, id string, e HostEdit) (HostOptions, bool) {
		t.Helper()
		after, changed, err := a.EditHostOptions(by, func(o *HostOptions) error { o.SetHost(id, e); return nil })
		if err != nil {
			t.Fatal(err)
		}
		return after, changed
	}
	if after, changed := set("admin", "pc-a", HostEdit{Run: RunEdit{OffAfterS: 600}}); !changed || after.Rev != 3 || after.Host("pc-a").Count() != 1 {
		t.Fatalf("one timing: %+v changed %v", after.Hosts, changed)
	}
	hosts, _ := readOptions(t, dir)["hosts"].(map[string]any)
	pc, _ := hosts["pc-a"].(map[string]any)
	run, _ := pc["wake_update"].(map[string]any)
	if len(hosts) != 1 || len(pc) != 3 || len(run) != 1 || run["off_after_s"] != 600.0 || pc["by"] != "admin" || pc["at"] == nil {
		t.Fatalf("stored: %v", hosts)
	}
	got := New(dir).HostOptions().Host("pc-a")
	if got.Run != (RunEdit{OffAfterS: 600}) || got.By != "admin" || got.Name != "" || got.Wol != nil || got.Empty() {
		t.Fatalf("read back: %+v", got)
	}
	if _, changed := set("other", "pc-a", HostEdit{Run: RunEdit{OffAfterS: 600}}); changed {
		t.Fatal("the same timing again counts as a change")
	}
	// the second timing, next to a name: one entry, stamped by who changed it
	if after, changed := set("other", "pc-a", HostEdit{Name: "Den PC", Run: RunEdit{OffAfterS: 600, ShutdownDelayS: 30}}); !changed || after.Host("pc-a").Count() != 3 {
		t.Fatalf("two timings and a name: %+v", after.Hosts)
	}
	hosts, _ = readOptions(t, dir)["hosts"].(map[string]any)
	pc, _ = hosts["pc-a"].(map[string]any)
	run, _ = pc["wake_update"].(map[string]any)
	if len(run) != 2 || run["shutdown_delay_s"] != 30.0 || pc["name"] != "Den PC" || pc["by"] != "other" {
		t.Fatalf("stored: %v", hosts)
	}
	// one decision goes: its key goes, the other stays
	set("other", "pc-a", HostEdit{Name: "Den PC", Run: RunEdit{ShutdownDelayS: 30}})
	hosts, _ = readOptions(t, dir)["hosts"].(map[string]any)
	pc, _ = hosts["pc-a"].(map[string]any)
	run, _ = pc["wake_update"].(map[string]any)
	if _, has := run["off_after_s"]; has || len(run) != 1 {
		t.Fatalf("after the first decision went: %v", pc)
	}
	// both go: no "wake_update" is written, the name stays
	set("other", "pc-a", HostEdit{Name: "Den PC"})
	hosts, _ = readOptions(t, dir)["hosts"].(map[string]any)
	pc, _ = hosts["pc-a"].(map[string]any)
	if _, has := pc["wake_update"]; has || pc["name"] != "Den PC" {
		t.Fatalf("no timing is decided and the key is written: %v", pc)
	}
	set("other", "pc-a", HostEdit{Run: RunEdit{ShutdownDelayS: 10}})
	set("other", "pc-a", HostEdit{})
	if _, has := readOptions(t, dir)["hosts"]; has {
		t.Fatalf("no decision is left and \"hosts\" is still written: %v", readOptions(t, dir))
	}
}

// The reader never fails and never takes a number its key cannot have: the configuration's value counts for
// that key, and the other key is read all the same. A file cannot lower a floor.
func TestHostRunTimingsReaderIsTolerant(t *testing.T) {
	read := func(run string) (HostEdit, []string) {
		dir := t.TempDir()
		writeState(t, dir, `{"host_options": {"rev": 1, "hosts": {"pc-a": {"name": "Den PC", "wake_update": `+run+`}}}}`)
		o, ignored := New(dir).HostOptionsRead()
		return o.Host("pc-a"), ignored
	}
	if e, ignored := read(`{"off_after_s": 300, "shutdown_delay_s": 3600}`); len(ignored) != 0 || e.Run != (RunEdit{OffAfterS: 300, ShutdownDelayS: 3600}) || e.Count() != 3 {
		t.Fatalf("the two ends of the ranges: %+v %q", e, ignored)
	}
	for what, c := range map[string]struct {
		run, line string
		want      RunEdit
	}{
		"below the floor of off_after_s":    {`{"off_after_s": 299, "shutdown_delay_s": 30}`, `wake_update.off_after_s is not a whole number from 300 to 3600`, RunEdit{ShutdownDelayS: 30}},
		"a minute":                          {`{"off_after_s": 60}`, `wake_update.off_after_s is not a whole number from 300 to 3600`, RunEdit{}},
		"zero":                              {`{"off_after_s": 0, "shutdown_delay_s": 0}`, `is not a whole number from`, RunEdit{}},
		"negative":                          {`{"off_after_s": -900}`, `wake_update.off_after_s is not a whole number`, RunEdit{}},
		"above the range":                   {`{"off_after_s": 3601}`, `wake_update.off_after_s is not a whole number`, RunEdit{}},
		"a fraction":                        {`{"off_after_s": 300.5}`, `wake_update.off_after_s is not a whole number`, RunEdit{}},
		"a text":                            {`{"off_after_s": "600"}`, `wake_update.off_after_s is not a whole number`, RunEdit{}},
		"a huge number":                     {`{"off_after_s": 1e300, "shutdown_delay_s": 30}`, `wake_update.off_after_s is not a whole number`, RunEdit{ShutdownDelayS: 30}},
		"below the floor of the countdown":  {`{"off_after_s": 600, "shutdown_delay_s": 9}`, `wake_update.shutdown_delay_s is not a whole number from 10 to 3600`, RunEdit{OffAfterS: 600}},
		"a countdown above the range":       {`{"shutdown_delay_s": 3601}`, `wake_update.shutdown_delay_s is not a whole number`, RunEdit{}},
		"null":                              {`{"off_after_s": null}`, `wake_update.off_after_s is not a whole number`, RunEdit{}},
		"a list":                            {`[600, 30]`, `"wake_update" is not an object (found a list)`, RunEdit{}},
		"a number":                          {`600`, `"wake_update" is not an object`, RunEdit{}},
		"a timing of the panel, not of one": {`{"online_timeout_s": 60, "off_after_s": 600}`, `wake_update."online_timeout_s" cannot be set in the panel`, RunEdit{OffAfterS: 600}},
		"anything else":                     {`{"token_env": "X", "shutdown_delay_s": 30}`, `wake_update."token_env" cannot be set in the panel`, RunEdit{ShutdownDelayS: 30}},
	} {
		e, ignored := read(c.run)
		if e.Run != c.want || e.Name != "Den PC" {
			t.Errorf("%s: read %+v, want %+v (and the name)", what, e.Run, c.want)
		}
		if len(ignored) == 0 || !strings.Contains(strings.Join(ignored, "\n"), c.line) || !strings.Contains(ignored[0], "host pc-a") {
			t.Errorf("%s: the lines %q lack %q", what, ignored, c.line)
		}
	}
}

// Nobody's code can write a timing outside its range: the locked write refuses it and nothing is written.
func TestHostRunTimingsWriterRefusesWhatIsOutsideTheRange(t *testing.T) {
	dir := t.TempDir()
	writeState(t, dir, `{"host_options": {"rev": 5, "hosts": {"pc-a": {"wake_update": {"off_after_s": 600}}}}}`)
	a := New(dir)
	for _, bad := range []RunEdit{{OffAfterS: 299}, {OffAfterS: 60}, {OffAfterS: -1}, {OffAfterS: 3601}, {ShutdownDelayS: 9}, {ShutdownDelayS: 3601}, {OffAfterS: 600, ShutdownDelayS: 1}} {
		if bad.Check() == nil {
			t.Errorf("Check takes %+v", bad)
		}
		_, changed, err := a.EditHostOptions("admin", func(o *HostOptions) error { o.SetHost("pc-b", HostEdit{Run: bad}); return nil })
		if err == nil || changed || !strings.Contains(err.Error(), "host pc-b") {
			t.Errorf("%+v was written: changed %v, error %v", bad, changed, err)
		}
	}
	o := New(dir).HostOptions()
	if o.Rev != 5 || len(o.Hosts) != 1 || o.Host("pc-a").Run.OffAfterS != 600 {
		t.Fatalf("after the refused writes: %+v", o)
	}
	for _, t2 := range config.MachineRunTimings() { // the ends of each range are taken
		for _, v := range []int{t2.Min, t2.Max} {
			var r RunEdit
			if r.Set(t2.Key, v); r.Check() != nil || r.Value(t2.Key) != v || r.Count() != 1 {
				t.Errorf("%s %d: %+v %v", t2.Key, v, r, r.Check())
			}
		}
	}
	var r RunEdit
	if r.Set("online_timeout_s", 60); !r.Empty() || r.Value("online_timeout_s") != 0 {
		t.Errorf("a key that is none of the two is taken: %+v", r)
	}
}

// A key of a newer build inside "wake_update" is not used and survives every save, also one that changes
// that machine's timings; Remove of the machine takes it out with the rest.
func TestHostRunTimingsNewerKeysAreKept(t *testing.T) {
	dir := t.TempDir()
	writeState(t, dir, `{"host_options": {"rev": 1, "hosts": {
		"pc-a": {"wake_update": {"off_after_s": 600, "wake_wait_s": 90}},
		"pc-b": {"wake_update": {"wake_wait_s": 45}}}}}`)
	a := New(dir)
	o, ignored := a.HostOptionsRead()
	if len(ignored) != 2 || o.Host("pc-a").Run != (RunEdit{OffAfterS: 600}) || len(o.Hosts) != 1 {
		t.Fatalf("read: %+v %q", o.Hosts, ignored)
	}
	if _, changed, err := a.EditHostOptions("admin", func(o *HostOptions) error {
		o.SetHost("pc-a", HostEdit{Run: RunEdit{ShutdownDelayS: 30}}) // its off_after_s goes, a countdown comes
		return nil
	}); err != nil || !changed {
		t.Fatal(err, changed)
	}
	wait := func(id string) (any, map[string]any) {
		hosts, _ := readOptions(t, dir)["hosts"].(map[string]any)
		e, _ := hosts[id].(map[string]any)
		run, _ := e["wake_update"].(map[string]any)
		return run["wake_wait_s"], run
	}
	if w, run := wait("pc-a"); w != 90.0 || run["shutdown_delay_s"] != 30.0 || len(run) != 2 {
		t.Fatalf("pc-a after a save: %v", run)
	}
	if w, run := wait("pc-b"); w != 45.0 || len(run) != 1 {
		t.Fatalf("pc-b after a save: %v", run)
	}
	// the machine's last own decision goes: what this build does not know stays where it was
	if _, _, err := a.EditHostOptions("admin", func(o *HostOptions) error { o.SetHost("pc-a", HostEdit{}); return nil }); err != nil {
		t.Fatal(err)
	}
	if w, run := wait("pc-a"); w != 90.0 || len(run) != 1 {
		t.Fatalf("pc-a without a decision of this build: %v", run)
	}
	if _, changed, err := a.EditHostOptions("admin", func(o *HostOptions) error { o.Forget("pc-b"); return nil }); err != nil || !changed {
		t.Fatal(err, changed)
	}
	hosts, _ := readOptions(t, dir)["hosts"].(map[string]any)
	if _, has := hosts["pc-b"]; has || len(hosts) != 1 {
		t.Fatalf("after Remove of pc-b: %v", hosts)
	}
}

// Remove of a host that sends other machines' wake packets takes those machines' Wake-on-LAN details out
// whole, and NOTHING else of them: their own run timings stay, next to their name. Remove of a machine takes
// its own run timings with it.
func TestHostRunTimingsSurviveTheRemoveOfARelay(t *testing.T) {
	dir := t.TempDir()
	writeState(t, dir, `{"host_options": {"rev": 1, "hosts": {
		"relay-a": {"name": "Relay", "wake_update": {"off_after_s": 1800}},
		"pc-a": {"name": "Den PC", "wol": {"macs": ["aa:bb:cc:dd:ee:01"], "relay": "relay-a", "port": 7}, "wake_update": {"off_after_s": 600, "shutdown_delay_s": 30}},
		"pc-b": {"wol": {"relay": "relay-a"}, "wake_update": {"shutdown_delay_s": 45}},
		"pc-c": {"wol": {"relay": "relay-a"}}}}}`)
	a := New(dir)
	var gone []string
	if _, changed, err := a.EditHostOptions("admin", func(o *HostOptions) error { gone = o.Forget("relay-a"); return nil }); err != nil || !changed {
		t.Fatal(err, changed)
	}
	o := New(dir).HostOptions()
	if e := o.Host("pc-a"); e.Wol != nil || e.Name != "Den PC" || e.Run != (RunEdit{OffAfterS: 600, ShutdownDelayS: 30}) {
		t.Errorf("pc-a: %+v (Wake-on-LAN %+v)", e, e.Wol)
	}
	if e := o.Host("pc-b"); e.Wol != nil || e.Run != (RunEdit{ShutdownDelayS: 45}) {
		t.Errorf("pc-b had its relay and a countdown; left: %+v (Wake-on-LAN %+v)", e, e.Wol)
	}
	if _, has := o.Hosts["pc-c"]; has {
		t.Error("pc-c had only the relay: its entry stayed")
	}
	if _, has := o.Hosts["relay-a"]; has {
		t.Error("the removed host's own entry stayed, with its run timings")
	}
	if len(gone) != 4 || !strings.Contains(gone[0], "what was changed about relay-a itself (name, Wake-on-LAN, run timings)") {
		t.Errorf("lines: %q", gone)
	}
	hosts, _ := readOptions(t, dir)["hosts"].(map[string]any)
	pa, _ := hosts["pc-a"].(map[string]any)
	if _, has := pa["wol"]; has || pa["wake_update"] == nil || len(hosts) != 2 {
		t.Errorf("stored: %v", hosts)
	}
}
