package enrollment

import (
	"strings"
	"testing"
)

// Add host can give the new machine a BMC power-on relay (agent v0.4.2 bmc_relay); it becomes the host's
// systems: entry like Wake-on-LAN does.
func TestAddHostWithBMCPower(t *testing.T) {
	m := &Manager{}
	none := func(string) string { return "" }
	h, err := m.hostEntry(Request{Name: "SRV-TOWER", OS: "windows", BMCRelay: "srv-winpc-agent", BMCTarget: "tower-bmc"}, nil, none)
	if err != nil {
		t.Fatal(err)
	}
	o := h.Override()
	if o == nil || o.ID != "srv-tower" || o.Wol != nil || o.BMCPower == nil || o.BMCPower.Relay != "srv-winpc-agent" || o.BMCPower.Target != "tower-bmc" {
		t.Fatalf("%+v", o)
	}
	for _, r := range []Request{{BMCRelay: "x"}, {BMCTarget: "tower-bmc"}, {BMCRelay: "x", BMCTarget: "Bad Target"}} {
		r.Name, r.OS = "SRV-TOWER", "windows"
		if _, err := m.hostEntry(r, nil, none); err == nil || !strings.Contains(err.Error(), "BMC") {
			t.Errorf("%+v: %v", r, err)
		}
	}
	if h, _ := m.hostEntry(Request{Name: "plain", OS: "linux"}, nil, none); h.Override() != nil {
		t.Fatal("no wol, no bmc: no systems entry")
	}
}
