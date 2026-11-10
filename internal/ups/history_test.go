package ups

import (
	"fmt"
	"testing"
	"time"
)

// An agent that renames its UPSes on every poll does not leave one history behind per name for ever (M1): the
// history of a UPS that is gone is kept for a while (it may come back), but only for a bounded number of them.
func TestHistoryOfRenamedUPSesIsBounded(t *testing.T) {
	m, err := NewManager(nil, 5, time.Second, 60, 3600, "")
	if err != nil {
		t.Fatal(err)
	}
	for round := range 200 {
		rows := make([]map[string]any, 0, MaxAgentUPS)
		for i := range MaxAgentUPS {
			rows = append(rows, map[string]any{"name": fmt.Sprintf("u%d-%d", round, i), "state": "online", "battery_charge": 100.0})
		}
		m.UpdateFromAgent("evil", rows, false, nil, nil)
		m.SampleHistory()
	}
	if n := len(m.Overview(true).History); n > MaxAgentUPS+maxOrphanHistories {
		t.Fatalf("%d histories kept", n)
	}
	// a UPS that disappears for a moment (its agent restarted) keeps its chart
	good := []map[string]any{{"name": "apc", "state": "online", "battery_charge": 90.0}}
	m.UpdateFromAgent("nas", good, false, nil, nil)
	m.SampleHistory()
	m.UpdateFromAgent("nas", nil, false, nil, nil)
	m.SampleHistory()
	m.UpdateFromAgent("nas", good, false, nil, nil)
	m.SampleHistory()
	if h := m.Overview(true).History["nas/apc"]; len(h) != 2 {
		t.Fatalf("%d samples", len(h))
	}
	// ... until its newest sample is older than the history length
	m.UpdateFromAgent("nas", nil, false, nil, nil)
	m.mu.Lock()
	m.pruneHistory(time.Now().Add(2 * time.Hour))
	_, kept := m.history["nas/apc"]
	m.mu.Unlock()
	if kept {
		t.Fatal("history of a UPS gone for longer than the history length was kept")
	}
}
