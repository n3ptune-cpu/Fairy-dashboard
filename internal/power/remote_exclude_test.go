package power

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// A plug at the INPUT of a UPS: the machines behind the UPS are already counted through the UPS, and the plug
// in front of it would count them a second time. It is left out of the total with the setting the panel has
// had since before build 14, power_exclude_from_total in the entry of the agent that relays it. It is still
// listed, with its watts and its link, and marked as not counted (include_in_total false: the Power page's
// "not in total"). A row of type homeassistant is a row like every other plug's here.
func TestAnExcludedHomeAssistantPlugIsListedAndNotCounted(t *testing.T) {
	pm := manager(t, nil, nil, nil)
	rows := []map[string]any{
		{"id": "ups-load", "name": "UPS load", "type": "nut", "ok": true, "watts": 300.0},
		{"id": "corner", "name": "Server corner plug", "type": "homeassistant", "ok": true, "watts": 345.0},
		{"id": "desk", "name": "Desk plug", "type": "homeassistant", "ok": true, "watts": 60.0, "system_hint": "self"}}
	pm.UpdateRemote("relay", rows, false, map[string]string{"corner": "rack-host"}, []string{"corner"})
	ov := pm.Overview()
	st := byID(ov)
	c := st["relay/corner"]
	if c.IncludeInTotal || c.Status != "ok" || c.Type != "homeassistant" || len(c.Readings) != 1 || *c.Readings[0].Watts != 345 || c.SystemID == nil || *c.SystemID != "rack-host" {
		t.Fatalf("the plug in front of the UPS: %+v", c)
	}
	if d := st["relay/desk"]; !d.IncludeInTotal || d.Type != "homeassistant" || *d.SystemID != "relay" {
		t.Fatalf("another plug: %+v", d)
	}
	if ov.TotalW == nil || *ov.TotalW != 360 {
		t.Fatalf("the total is %v, want the UPS's 300 W and the desk's 60 W", ov.TotalW)
	}
	if w := pm.LinkedWatts(); w["rack-host"] != 345 { // left out of the total, not of its machine's row
		t.Fatalf("the watts on the row it is linked to: %v", w)
	}
	// the history's total leaves it out as well, and has the plug's own line
	pm.SampleHistory()
	h := pm.HistoryView("")
	if n := len(h.Points); n != 1 || *h.Points[0].TotalW != 360 || *h.Points[0].Sources["relay/corner"] != 345 {
		t.Fatalf("the history: %+v", h.Points)
	}
	// without the setting it is counted
	pm.UpdateRemote("relay", rows, false, nil, nil)
	if ov := pm.Overview(); *ov.TotalW != 705 {
		t.Fatalf("without power_exclude_from_total: total %v", *ov.TotalW)
	}
}

// The device ids in power_links and power_exclude_from_total are the agent's own. An id the agent does not
// report (a typing mistake; the device was taken out of the agent's config) links nothing and leaves nothing
// out, so a plug that was meant to be left out is counted. The running panel says so once in its log, and
// again only when the list of such ids changes.
func TestDevicesThatAnAgentDoesNotReportAreSaidOnce(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)
	count := func() int { return strings.Count(logs.String(), "names devices the agent does not report") }
	pm := manager(t, nil, nil, nil)
	rows := []map[string]any{{"id": "corner", "ok": true, "watts": 345.0}, {"id": "desk", "ok": true, "watts": 60.0}}
	for i := 0; i < 3; i++ {
		pm.UpdateRemote("relay", rows, false, map[string]string{"desk": "pc-a", "dsek": "pc-a"}, []string{"corner", "corner-plug"})
	}
	if count() != 1 || !strings.Contains(logs.String(), `devices="corner-plug, dsek"`) || !strings.Contains(logs.String(), "agent=relay") {
		t.Fatalf("said %d times:\n%s", count(), logs.String())
	}
	pm.UpdateRemote("relay", nil, true, map[string]string{"dsek": "pc-a"}, nil) // the agent cannot be reached: nothing is judged
	pm.UpdateRemote("relay", rows, false, map[string]string{"desk": "pc-a"}, []string{"corner", "corner-plug"})
	if count() != 2 || !strings.Contains(logs.String(), `devices=corner-plug`) {
		t.Fatalf("after one of the two was put right, said %d times:\n%s", count(), logs.String())
	}
	pm.UpdateRemote("relay", rows, false, map[string]string{"desk": "pc-a"}, []string{"corner"})
	pm.UpdateRemote("relay", rows, false, map[string]string{"desk": "pc-a"}, []string{"corner"})
	if count() != 2 {
		t.Fatalf("with every id put right, said %d times:\n%s", count(), logs.String())
	}
	pm.UpdateRemote("other", rows, false, nil, nil) // an agent with nothing set
	if count() != 2 {
		t.Fatalf("an agent with no links: said %d times", count())
	}
}
