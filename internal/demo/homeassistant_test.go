package demo

import (
	"context"
	"math"
	"testing"
)

// `fairy serve --demo` has two plugs that an agent reads through Home Assistant (rows of type homeassistant,
// as a Fairy-agent of v0.8.0 relays them), so that the pages can be looked at without the hardware: the plug
// of a Proxmox node, linked with the id of the node's AGENT and shown on the node's row, and a plug in front
// of a UPS, which has its own line on the Power page and is in no total.
func TestDemoHasPlugsReadThroughHomeAssistant(t *testing.T) {
	_, _, p, err := Build(t.TempDir(), "http://127.0.0.1:8090") // the demo as fairy serve --demo makes it
	if err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	ov := p.Power.Overview()
	var counted float64
	found := map[string]bool{}
	for _, s := range ov.Sources {
		var watts float64
		for _, r := range s.Readings {
			if r.Watts != nil {
				watts += *r.Watts
			}
		}
		if s.IncludeInTotal && s.Status != "unreachable" {
			counted += watts
		}
		switch s.ID {
		case "mini-i7-agent/mini-plug":
			found[s.ID] = true
			if s.Type != "homeassistant" || !s.IncludeInTotal || s.SystemID == nil || *s.SystemID != "bob-node-mini-i7" || watts <= 0 {
				t.Errorf("the node's plug: %+v", s)
			}
			var row *float64
			for _, x := range p.Snapshot() {
				if x.ID == "bob-node-mini-i7" {
					row = x.PowerW
				}
				if x.ID == "mini-i7-agent" {
					t.Error("the fixture: the agent has a row of its own")
				}
			}
			if row == nil || *row != watts {
				t.Errorf("the node's row has %v W of wall power, the plug reads %v", row, watts)
			}
		case "mini-i7-agent/corner-plug":
			found[s.ID] = true
			if s.Type != "homeassistant" || s.IncludeInTotal || s.Status != "ok" || watts <= 0 {
				t.Errorf("the plug in front of the UPS: %+v", s)
			}
		}
	}
	if len(found) != 2 {
		t.Fatalf("the demo's Home Assistant plugs: %v", found)
	}
	if ov.TotalW == nil || math.Abs(*ov.TotalW-counted) > 0.11 {
		t.Errorf("the total is %v, the counted sources add up to %.1f", ov.TotalW, counted)
	}
}
