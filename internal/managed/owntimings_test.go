package managed

import (
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// A machine added with "Add host" with Wake-on-LAN has a systems: entry the panel makes (its wol:). Its own
// timings for "Update unpowered hosts" can only be written in config.yaml, as a second entry with its id, and
// that made the panel skip the host at its start: "systems: in config.yaml already has an entry". The machines
// a run wakes are the machines with Wake-on-LAN, so the timings could not be set for the hosts they are for.
//
// A hand-written entry that holds nothing but wake_update: is put together with the generated one. Anything
// else in it is refused as before, with the same words.
func TestAddHostWithWakeOnLANTakesItsOwnTimingsFromConfigYAML(t *testing.T) {
	setup := func(t *testing.T, withWol bool, hand *config.SystemOverride) (*config.Panel, *Store) {
		t.Helper()
		cfg := testutil.Config(t, []string{"https://pve.example:8006"}, "http://127.0.0.1:1", "http://127.0.0.1:2")
		s := NewStore(cfg.DataDir)
		if err := s.Put(Host{ID: "relay-box", Name: "relay-box", URL: "http://relay-box:9101", OS: "linux", Options: Options{WolRelay: true}}, Secret{Token: tokA + "r", ControlToken: tokB + "r"}); err != nil {
			t.Fatal(err)
		}
		h := Host{ID: "srv-winpc", Name: "SRV-WINPC", URL: "http://srv-winpc:9101", OS: "windows", Options: Options{PowerControl: true}, AddedAt: time.Now().UTC()}
		if withWol {
			h.Wol = &config.Wol{MACs: []string{"00:00:5e:00:53:01"}, Relay: "relay-box"}
		}
		if err := s.Put(h, Secret{Token: tokA, ControlToken: tokB}); err != nil {
			t.Fatal(err)
		}
		if hand != nil {
			cfg.Systems = append(cfg.Systems, *hand)
		}
		return cfg, s
	}
	entryOf := func(cfg *config.Panel, id string) (out []config.SystemOverride) {
		for _, o := range cfg.Systems {
			if o.ID == id {
				out = append(out, o)
			}
		}
		return out
	}
	timings := &config.SystemWakeUpdate{OffAfterS: 600, ShutdownDelayS: 30}
	for _, withWol := range []bool{true, false} {
		cfg, s := setup(t, withWol, &config.SystemOverride{ID: "srv-winpc", WakeUpdate: timings})
		added, skipped, err := s.Merge(cfg)
		if err != nil || len(skipped) != 0 || len(added) != 2 {
			t.Fatalf("wol %v: added %d, skipped %q, err %v", withWol, len(added), skipped, err)
		}
		got := entryOf(cfg, "srv-winpc")
		if len(got) != 1 || got[0].WakeUpdate == nil || *got[0].WakeUpdate != *timings || (got[0].Wol != nil) != withWol {
			t.Fatalf("wol %v: the entries of srv-winpc after the merge: %+v", withWol, got)
		}
		if err := cfg.Validate(); err != nil { // one entry per id: the merged configuration is one the panel accepts
			t.Fatalf("wol %v: the merged configuration: %v", withWol, err)
		}
		for _, e := range added {
			if e.Agent.ID == "srv-winpc" && withWol && (e.Override == nil || e.Override.Wol == nil || e.Override.WakeUpdate == nil || e.Override.WakeUpdate.OffAfterS != 600) {
				t.Errorf("the entry handed to the poller: %+v", e.Override)
			}
		}
	}
	// without a hand-written entry: as before, and no block appears from nowhere
	cfg, s := setup(t, true, nil)
	if _, skipped, err := s.Merge(cfg); err != nil || len(skipped) != 0 {
		t.Fatalf("no hand-written entry: %q %v", skipped, err)
	}
	if got := entryOf(cfg, "srv-winpc"); len(got) != 1 || got[0].Wol == nil || got[0].WakeUpdate != nil {
		t.Fatalf("no hand-written entry: %+v", got)
	}
	// anything else next to the timings, or without them: refused as before, the host is skipped, config.yaml's entry stays
	for name, hand := range map[string]config.SystemOverride{
		"a name too":        {ID: "srv-winpc", Name: "Win", WakeUpdate: timings},
		"a wol: of its own": {ID: "srv-winpc", Wol: &config.Wol{MACs: []string{"00:00:5e:00:53:02"}, Relay: "relay-box"}, WakeUpdate: timings},
		"a name only":       {ID: "srv-winpc", Name: "Win"},
		"a site too":        {ID: "srv-winpc", Labels: config.Labels{Site: "home"}, WakeUpdate: timings},
	} {
		cfg, s := setup(t, true, &hand)
		added, skipped, err := s.Merge(cfg)
		if err != nil || len(added) != 1 || len(skipped) != 1 || !strings.Contains(skipped[0], "managed host srv-winpc skipped: systems: in config.yaml already has an entry for srv-winpc") {
			t.Errorf("%s: added %d, skipped %q, err %v", name, len(added), skipped, err)
		}
		if got := entryOf(cfg, "srv-winpc"); len(got) != 1 || got[0].Name != hand.Name {
			t.Errorf("%s: config.yaml's entry after the merge: %+v", name, got)
		}
	}
}
