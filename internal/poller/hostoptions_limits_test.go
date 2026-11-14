package poller

import (
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/power"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/ups"
)

// The limits of the link fields (config.LinkProblems) are the limits of what an agent can relay: a list that
// is longer names devices that cannot exist. They are written in two packages that do not know each other;
// this holds them together.
func TestLinkLimitsAreTheManagersLimits(t *testing.T) {
	if config.MaxLinkDevices != power.MaxRemoteSources {
		t.Errorf("config.MaxLinkDevices is %d, power.MaxRemoteSources %d", config.MaxLinkDevices, power.MaxRemoteSources)
	}
	if config.MaxLinkUPS != ups.MaxAgentUPS {
		t.Errorf("config.MaxLinkUPS is %d, ups.MaxAgentUPS %d", config.MaxLinkUPS, ups.MaxAgentUPS)
	}
	// a UPS name the check takes is a name the UPS manager takes, and the other way round
	m, err := ups.NewManager(nil, 60, time.Second, 60, 3600, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"rack", "APC_1.5:b-2", "", "a b", "a/b", "räck", "x\n", string(make([]byte, 65)), "u23456789012345678901234567890123456789012345678901234567890123"} {
		m.UpdateFromAgent("nas", []map[string]any{{"name": name, "state": "online"}}, false, nil, nil)
		taken := m.State("nas/"+name) != nil
		if taken != config.ValidUPSName(name) {
			t.Errorf("%q: the UPS manager takes it: %v, config.ValidUPSName: %v", name, taken, config.ValidUPSName(name))
		}
	}
}
