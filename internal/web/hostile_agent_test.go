package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// hostileMetrics is a /metrics answer made of the security audit's inputs (M2): numbers that are each possible
// but add up or scale to impossible ones, a date in the year 33658, negative sizes, 250 %.
func hostileMetrics() jsonx.M {
	big := 1e308
	m := testutil.AgentMetrics()
	m["uptime_s"] = -5.0
	m["cpu"] = jsonx.M{"percent": 250.0, "logical": big}
	m["memory"] = jsonx.M{"total": big, "used": -1.0}
	m["net"] = jsonx.M{"bytes_recv": big, "bytes_sent": big}
	m["disks"] = []any{jsonx.M{"mount": "/", "total": big, "used": big}, jsonx.M{"mount": "/b", "total": big, "used": big}}
	m["temps"] = []any{jsonx.M{"sensor": "x", "current": big, "high": big}}
	m["load_avg"] = []any{big, big, big}
	m["gpus"] = []any{jsonx.M{"name": "g", "vendor": "nvidia", "util_percent": big, "vram_used": big, "vram_total": big, "temp_c": big, "power_w": big}}
	m["power"] = jsonx.M{"cpu_package_w": big, "gpu_w": big, "battery_w": big, "bmc_w": big, "sensors": []any{jsonx.M{"name": "s", "watts": big}}}
	m["power_control"] = jsonx.M{"pending": jsonx.M{"action": "sleep", "at": 1e12}}
	m["power_sources"] = []any{jsonx.M{"id": "desk", "ok": true, "watts": big, "system_hint": "self"}, jsonx.M{"id": "tv", "ok": true, "watts": big}}
	m["ups"] = []any{jsonx.M{"name": "apc", "state": "online", "battery_charge": 250.0, "battery_runtime_s": -5.0, "load_percent": big, "watts": big}}
	return m
}

// One hostile agent among three hosts (M2): the system list, the summary, the power page, its history, the UPS
// page and the first frame of the live stream are still answered, as valid JSON, with every host in them.
func TestAPIAnswersWithOneHostileAgent(t *testing.T) {
	e := setup(t)
	c := e.cookie(controller(), 0)
	e.b.Mu.Lock()
	e.b.Metrics["cyrene"] = func() (jsonx.M, error) { m := testutil.AgentMetrics(); m["hostname"] = "cyrene"; return m, nil }
	e.b.Mu.Unlock()
	e.s.Poller.PollOnce(context.Background())
	want := len(e.do("GET", "/api/systems", nil, "Cookie", c).body["systems"].([]any))

	e.b.Mu.Lock()
	e.b.Metrics["laptop"] = func() (jsonx.M, error) { return hostileMetrics(), nil }
	e.b.PVE = func() ([]jsonx.M, error) {
		rows := testutil.PVEResources()
		rows[2]["cpu"], rows[2]["maxmem"] = 1.8e307, 1e308
		return rows, nil
	}
	e.b.Mu.Unlock()
	for range 3 {
		e.s.Poller.PollOnce(context.Background())
	}
	for _, path := range []string{"/api/systems", "/api/summary", "/api/power", "/api/power/history", "/api/ups", "/api/ups?history=true",
		"/api/systems/laptop", "/api/systems/cyrene", "/api/systems/qemu-100", "/api/systems/laptop/power"} {
		r := e.do("GET", path, nil, "Cookie", c)
		if r.code != 200 || !json.Valid(r.raw) || r.body == nil {
			t.Fatalf("%s: %d %s", path, r.code, r.raw)
		}
	}
	systems := e.do("GET", "/api/systems", nil, "Cookie", c).body["systems"].([]any)
	if len(systems) != want {
		t.Fatalf("%d systems, %d before: a host is hidden", len(systems), want)
	}
	seen := map[string]string{}
	for _, x := range systems {
		m := x.(map[string]any)
		seen[m["id"].(string)], _ = m["status"].(string)
	}
	if seen["laptop"] != "online" || seen["cyrene"] != "online" || seen["qemu-100"] != "online" {
		t.Fatalf("%v", seen)
	}
	if lap := e.do("GET", "/api/systems/laptop", nil, "Cookie", c).body; lap["cpu_percent"] != 100.0 || lap["mem_total"] != nil || lap["power_pending"] != nil {
		t.Fatalf("%v", lap)
	}

	// the first frame of the live stream: every event's data is valid JSON, and the systems event holds every host
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/api/stream", nil)
	req.Header.Set("Cookie", c)
	r, err := http.DefaultClient.Do(req)
	if err != nil || r.StatusCode != 200 {
		t.Fatal(err, r)
	}
	defer r.Body.Close()
	got := map[string]bool{}
	sc := bufio.NewScanner(r.Body)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	event := ""
	for sc.Scan() && !(got["systems"] && got["summary"] && got["ups"] && got["power"]) {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data := strings.TrimPrefix(line, "data: ")
			var m map[string]any
			if err := json.Unmarshal([]byte(data), &m); err != nil {
				t.Fatalf("stream event %q is not valid JSON: %v\n%.300s", event, err, data)
			}
			if event == "systems" {
				list, _ := m["systems"].([]any)
				if len(list) != want {
					t.Fatalf("stream: %d systems instead of %d", len(list), want)
				}
				for _, x := range list {
					if x == nil {
						t.Fatal("stream: an empty system entry")
					}
				}
			}
			got[event] = true
		}
	}
	if !(got["systems"] && got["summary"] && got["ups"] && got["power"]) {
		t.Fatalf("first frame incomplete: %v (%v)", got, sc.Err())
	}
}
