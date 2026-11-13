package poller

import (
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// A host added in the panel while it runs (Add host, Re-pair) with Wake-on-LAN, and config.yaml has its own
// timings for it ({id, wake_update}): the entry the panel makes stands in the hand-written one's place and
// keeps the timings, and they are still there after the host is removed and added again (a Re-pair). Until
// this the generated entry replaced the hand-written one in the poller's table, and the timings were gone
// until the next start.
func TestAHostAddedInThePanelKeepsItsOwnTimingsFromConfigYAML(t *testing.T) {
	cfg := testutil.SameNameConfig(t)
	cfg.Systems = append(cfg.Systems, config.SystemOverride{ID: "pc-new", WakeUpdate: &config.SystemWakeUpdate{OffAfterS: 600, ShutdownDelayS: 30}})
	p, err := New(cfg, config.Secrets{}, testutil.NewBackend())
	if err != nil {
		t.Fatal(err)
	}
	entries := func() (n int, o config.SystemOverride) {
		for _, x := range p.SystemsConfig() {
			if x.ID == "pc-new" {
				n, o = n+1, x
			}
		}
		return n, o
	}
	check := func(when string, wol bool) {
		t.Helper()
		n, o := entries()
		if n != 1 || (o.Wol != nil) != wol || o.WakeUpdate == nil || o.WakeUpdate.OffAfterS != 600 {
			t.Fatalf("%s: %d entries of pc-new in the configuration: %+v", when, n, o)
		}
		if off, delay := p.WakeUpdateOverride("pc-new"); off != 600 || delay != 30 {
			t.Fatalf("%s: its own timings: %d, %d", when, off, delay)
		}
		if po := p.PowerOverride("pc-new"); (po != nil && po.Wol != nil) != wol {
			t.Fatalf("%s: its wol: %+v", when, po)
		}
	}
	check("before it is added", false)
	agent := config.Agent{ID: "pc-new", Name: "PC", URL: "http://pc-new:9101", Managed: true, Token: "read-word", ControlToken: "control-word", PowerControl: true}
	gen := &config.SystemOverride{ID: "pc-new", Wol: &config.Wol{MACs: []string{"00:00:5e:00:53:07"}, Relay: "relay"}}
	for i := 0; i < 2; i++ { // added, removed (Re-pair takes the old entry out first), added again
		if err := p.AddAgent(agent, gen); err != nil {
			t.Fatal(err)
		}
		check("added", true)
		if gen.WakeUpdate != nil {
			t.Fatal("the caller's entry was written to")
		}
		p.RemoveAgent("pc-new")
		check("removed", false)
	}
	if err := p.Cfg.Validate(); err != nil {
		t.Fatalf("the configuration after it all: %v", err)
	}
}
