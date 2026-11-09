package demo

import (
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
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

// The demo's NSX-CYRENE has no "optimal" fan mode, like the real board: refused with the agent's 422, listed.
func TestDemoBMCRefusesOptimal(t *testing.T) {
	b := newSimBMC()
	if st, out := b.handle("/fan/mode", map[string]any{"mode": "optimal"}); st != 422 || out["code"] != "fan_mode_unsupported" {
		t.Fatal(st, out)
	}
	if st, _ := b.handle("/fan/mode", map[string]any{"mode": "full"}); st != 200 {
		t.Fatal(st)
	}
	_, s := b.handle("/fan/status", nil)
	if s["mode"] != "full" || s["overridden"] != true || s["auto_mode"] != "standard" || len(s["unsupported_modes"].([]string)) != 1 {
		t.Fatal(s)
	}
}

// The demo's Windows agents: NSX-CYRENE's elevated helper runs, the laptop's is stopped; Stop / Start work.
func TestDemoHelperStopStart(t *testing.T) {
	b := NewBackend(nil)
	metrics := func(id string) jsonx.M {
		m, err := agentMetrics(id)
		if err != nil {
			t.Fatal(err)
		}
		b.mu.Lock()
		b.applyHelper(id, m)
		b.mu.Unlock()
		return m
	}
	state := func(m jsonx.M) string { return jsonx.S(jsonx.Obj(m, "helper"), "state") }
	hasIPMI := func(m jsonx.M) bool {
		for _, x := range jsonx.Objs(m["temps"]) {
			if x["sensor"] == "IPMI" || x["sensor"] == "coretemp" {
				return true
			}
		}
		return false
	}
	if m := metrics("nsx-cyrene"); state(m) != "running" || !hasIPMI(m) || m["unavailable"] != nil {
		t.Fatal("cyrene", m["helper"])
	}
	if m := metrics("neptune-laptop"); state(m) != "stopped" || hasIPMI(m) || jsonx.S(jsonx.Obj(m, "unavailable"), "coretemp") != "helper_stopped" {
		t.Fatal("laptop", m["helper"], m["unavailable"])
	}
	if m := metrics("straw-pc"); state(m) != "not_installed" || jsonx.Bool(jsonx.Obj(m, "helper"), "needed", true) {
		t.Fatal("straw-pc", m["helper"])
	}
	if st, out := b.simHelperControl("nsx-cyrene", "stop"); st != 200 || out["state"] != "stopped" {
		t.Fatal(st, out)
	}
	m := metrics("nsx-cyrene")
	if state(m) != "stopped" || hasIPMI(m) || len(jsonx.Objs(m["fans"])) != 0 || jsonx.Obj(m, "features")["fan_control"] != false {
		t.Fatal("stopped cyrene still reads the BMC", m["temps"], m["fans"])
	}
	if st, out, _ := b.AgentRequest(t.Context(), config.Agent{ID: "nsx-cyrene"}, "POST", "/fan/mode", map[string]any{"mode": "full"}); st != 409 || out["error"] != "helper_stopped" {
		t.Fatal("fan write while stopped", st, out)
	}
	if st, out := b.simHelperControl("nsx-cyrene", "start"); st != 200 || out["state"] != "starting" {
		t.Fatal(st, out)
	}
	b.mu.Lock()
	b.helpers["nsx-cyrene"].readyAt = time.Now()
	b.mu.Unlock()
	if m := metrics("nsx-cyrene"); state(m) != "running" || !hasIPMI(m) {
		t.Fatal("restarted", m["helper"])
	}
	if st, out := b.simHelperControl("straw-pc", "start"); st != 409 || out["error"] != "not_installed" {
		t.Fatal(st, out)
	}
}
