package poller

import (
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/scheduler"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// A poll override is written into state.json by the scheduler, not by the panel's admin state, which keeps
// the view of that file. The view has the override when SetPoll returns: it is told of the write, and does not
// find it at its next look (up to a second later, and by telling the new file from the old one).
func TestAPollOverrideIsInThePanelsViewOfStateJSONAtOnce(t *testing.T) {
	p, err := New(testutil.SameNameConfig(t), config.Secrets{}, testutil.NewBackend())
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := p.Admin.StateData(); d["poll_overrides"] != nil { // the view has looked at the file just now
		t.Fatalf("the set-up: %v", d["poll_overrides"])
	}
	if err := p.SetPoll("pc-a", &scheduler.Override{Seconds: 30}); err != nil {
		t.Fatal(err)
	}
	d, _ := p.Admin.StateData()
	if po, _ := d["poll_overrides"].(map[string]any); po["pc-a"] != 30.0 {
		t.Errorf("after SetPoll the panel's view of state.json says %v", d["poll_overrides"])
	}
}
