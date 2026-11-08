package demo

import (
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/jsonx"
)

func TestCounterNeverDecreases(t *testing.T) {
	prev := int64(-1)
	for i := 0; i < 5000; i++ {
		rate := uniform(0, 9e6) // as jittery as wave() gets, and then some
		v := counterAt("test/rx", 5e9, rate, float64(i)*0.37)
		if v < prev {
			t.Fatalf("sample %d: counter went backwards %d -> %d", i, prev, v)
		}
		prev = v
	}
	if counterAt("test/neg", 10, -5, 1) != 10 || counterAt("test/neg", 10, -5, 2) != 10 {
		t.Fatal("a negative rate must not shrink the counter")
	}
}

// TestDemoNetworkCountersMonotonic samples the simulated agents and Proxmox guests many times: their
// cumulative network counters must never go down (otherwise the rate tracker sees a reset and the UI's
// Network row flickers).
func TestDemoNetworkCountersMonotonic(t *testing.T) {
	agents := []string{"nsx-cyrene", "neptune-laptop", "llm-box", "straw-pc"}
	last := map[string]int64{}
	check := func(key string, v int64) {
		if p, ok := last[key]; ok && v < p {
			t.Fatalf("%s went backwards: %d -> %d", key, p, v)
		}
		last[key] = v
	}
	for i := 0; i < 300; i++ {
		for _, id := range agents {
			m, err := agentMetrics(id)
			if err != nil {
				t.Fatal(err)
			}
			net := m["net"].(map[string]any)
			check(id+" rx", net["bytes_recv"].(int64))
			check(id+" tx", net["bytes_sent"].(int64))
		}
		for _, r := range append(proxmoxResources(), strawResources()...) {
			if jsonx.S(r, "status") != "running" || (jsonx.S(r, "type") != "qemu" && jsonx.S(r, "type") != "lxc") {
				continue
			}
			id := jsonx.S(r, "id") + "@" + jsonx.S(r, "node")
			check(id+" in", int64(r["netin"].(float64)))
			check(id+" out", int64(r["netout"].(float64)))
		}
		time.Sleep(time.Millisecond)
	}
}
