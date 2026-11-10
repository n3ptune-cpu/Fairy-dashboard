package ups

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// L7, the audit's second flood: the limit of 10 events per UPS was avoided by giving the UPSes new names on
// every poll (896 notifications in 30 seconds). Events are now also counted per source, so an agent or a UPS
// server gets a fixed number of them in 10 minutes, whatever its UPSes are called; another source's UPS is not
// affected, and the first event that gets through again says how many were not shown.
func TestRenamedUPSesDoNotAvoidTheEventLimit(t *testing.T) {
	m, err := NewManager(nil, 5, time.Second, 60, 3600, "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	m.OnEvent = func(e model.UpsEvent) { got = append(got, e.UpsID) }
	for round := range 200 { // 16 new UPSes per poll, each "on battery": 3200 would-be events
		rows := make([]map[string]any, 0, MaxAgentUPS)
		for i := range MaxAgentUPS {
			rows = append(rows, map[string]any{"name": fmt.Sprintf("u%d-%d", round, i), "state": "on_battery"})
		}
		m.UpdateFromAgent("evil", rows, false, nil, nil)
	}
	if n := len(m.EventsSince(0)); n != maxSourceEvents || len(got) != maxSourceEvents {
		t.Fatalf("%d events, %d notifications", n, len(got))
	}
	// a UPS of another source still gets its events
	m.UpdateFromAgent("nas", []map[string]any{{"name": "apc", "state": "online"}}, false, nil, nil)
	m.UpdateFromAgent("nas", []map[string]any{{"name": "apc", "state": "low_battery"}}, false, nil, nil)
	if k := kindsOf(m, "nas/apc"); k != "on_battery low_battery" {
		t.Fatalf("another source's events: %q", k)
	}
	// when the window is over, the next event of the flooding source says what was not shown
	m.sourceLimiter.Window = time.Nanosecond
	m.UpdateFromAgent("evil", []map[string]any{{"name": "last", "state": "on_battery"}}, false, nil, nil)
	evs := m.EventsSince(0)
	if last := evs[len(evs)-1]; last.UpsID != "evil/last" || !strings.Contains(last.Message, "3160 earlier UPS events from the same source not shown") {
		t.Fatalf("%+v", last)
	}
	if sourceOf("rack/apc") != "rack" || sourceOf("odd") != "odd" {
		t.Fatal("sourceOf")
	}
}
