package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/power"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// padded returns v as JSON padded with spaces to exactly size bytes.
func padded(t *testing.T, v any, size int) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil || len(b) > size {
		t.Fatal("padded", len(b), err)
	}
	return append(b, bytes.Repeat([]byte(" "), size-len(b))...)
}

// The panel reads at most 1 MiB of an agent's answer (M1). A longer one is refused as "invalid response" (the
// host shows as offline with that error), it is not read to its end, and the limit itself is still accepted. The
// rows that fit into 1 MiB are bounded too: tens of thousands of relayed power sources leave 16.
func TestAgentAnswersAreSizeLimited(t *testing.T) {
	var body atomic.Value
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body.Load().([]byte))
	}))
	defer agent.Close()
	cfg := testutil.Config(t, []string{deadURL(t)}, agent.URL, deadURL(t))
	p, err := New(cfg, config.Secrets{SessionSecret: "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	body.Store(padded(t, testutil.AgentMetrics(), maxAgentBody)) // exactly the limit: fine
	p.PollOnce(ctx)
	if lap := snap(p)["laptop"]; lap.Status != "online" || *lap.Cores != 8 {
		t.Fatalf("%+v", lap)
	}
	if _, st, err := p.AgentRequest(ctx, cfg.Agents[0], "GET", "/fan/status", nil); err != nil || st["hostname"] != "laptop" {
		t.Fatal(st, err)
	}

	body.Store(padded(t, testutil.AgentMetrics(), maxAgentBody+1)) // one byte more: refused
	p.PollOnce(ctx)
	if lap := snap(p)["laptop"]; lap.Status == "online" || lap.Error == nil || *lap.Error != "invalid response" {
		t.Fatalf("%+v %v", lap.Status, lap.Error)
	}
	if _, _, err := p.AgentRequest(ctx, cfg.Agents[0], "GET", "/fan/status", nil); err == nil || err.Error() != "invalid response" {
		t.Fatal("oversize answer to a control request:", err)
	}
	if _, err := p.FetchProcesses(ctx, cfg.Agents[0], "laptop", 25, "cpu"); err == nil || err.Error() != "invalid response" {
		t.Fatal("oversize process list:", err)
	}
	if len(p.Power.Overview().Sources) != 1 { // the laptop's plug, now stale: an unreadable answer adds nothing
		t.Fatalf("%+v", p.Power.Overview().Sources)
	}

	// about 25,000 small rows fit into 1 MiB: 16 are kept
	m := testutil.AgentMetrics()
	rows := []any{}
	for i := range 25_000 {
		rows = append(rows, jsonx.M{"id": fmt.Sprintf("p%d", i), "ok": true, "watts": 1.0})
	}
	m["power_sources"] = rows
	b, _ := json.Marshal(m)
	if len(b) > maxAgentBody {
		t.Fatal("fixture too large", len(b))
	}
	body.Store(b)
	p.PollOnce(ctx)
	n := 0
	for _, s := range p.Power.Overview().Sources {
		if strings.HasPrefix(s.ID, "laptop/") {
			n++
		}
	}
	if n != power.MaxRemoteSources {
		t.Fatalf("%d relayed sources kept", n)
	}
	if hv := p.Power.HistoryView(""); len(hv.Points) == 0 || len(hv.Points[len(hv.Points)-1].Sources) > 2*power.MaxRemoteSources {
		t.Fatal("history sample not bounded")
	}
}

// The process list an agent answers with is bounded as well: rows, names, per-GPU lists.
func TestProcessListIsBounded(t *testing.T) {
	long, huge := strings.Repeat("x", 1<<10), strings.Repeat("y", 1<<18)
	rows := []any{}
	for i := range 5000 {
		rows = append(rows, jsonx.M{"pid": float64(i), "name": long, "username": long, "status": long, "cmdline": "sleep 60",
			"gpu_mem_by_gpu": make([]any, 0)})
	}
	gpus := make([]any, 1000)
	for i := range gpus {
		gpus[i] = 1.0
	}
	rows[0] = jsonx.M{"pid": 0.0, "name": huge, "username": huge, "status": huge, "cmdline": huge, "gpu_mem_by_gpu": gpus}
	d := jsonx.M{"processes": rows, "sort": long, "total_processes": 5000.0}
	pl, err := DecodeProcessList(d, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Processes) != maxProcesses || len(d["processes"].([]any)) != 5000 || *pl.TotalProcesses != 5000 {
		t.Fatal("rows", len(pl.Processes))
	}
	p0 := pl.Processes[0]
	if len(*p0.Name) > maxProcName+3 || len(*p0.Username) > maxProcName+3 || len(*p0.Status) > 40 || len(*p0.Cmdline) > maxProcCmdline+3 ||
		len(p0.GPUMemByGPU) != maxProcGPUs || len(pl.Sort) > 20 {
		t.Fatalf("%d %d %d %d %d", len(*p0.Name), len(*p0.Username), len(*p0.Status), len(*p0.Cmdline), len(p0.GPUMemByGPU))
	}
	if b, _ := json.Marshal(pl); len(b) > 1<<20 {
		t.Fatal("size", len(b))
	}
}

// HostileMetrics is a /metrics answer made of the audit's inputs (M2): numbers that are each possible but add up
// or scale to impossible ones, a date in the year 33658, negative sizes, 250 %.
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

// mustJSON fails the test when v cannot be written as JSON.
func mustJSON(t *testing.T, what string, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s cannot be written as JSON: %v", what, err)
	}
	return b
}

// One hostile agent among three hosts, and one hostile Proxmox row (M2): everything the API serves from the
// poller can still be written, no host is hidden, and the other hosts are exactly as without it.
func TestOneHostileAgentAmongThree(t *testing.T) {
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	b := testutil.NewBackend()
	b.Metrics["tower"] = func() (jsonx.M, error) { m := testutil.AgentMetrics(); m["hostname"] = "tower"; return m, nil }
	p, err := New(cfg, config.Secrets{}, b)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p.PollOnce(ctx)
	before := snap(p)
	wantTower := string(mustJSON(t, "tower", stable(before["tower"])))

	b.Mu.Lock()
	b.Metrics["laptop"] = func() (jsonx.M, error) { return hostileMetrics(), nil }
	b.PVE = func() ([]jsonx.M, error) {
		rows := testutil.PVEResources()
		rows[2]["cpu"], rows[2]["maxmem"], rows[2]["netin"] = 1.8e307, 1e308, 1e308
		return append(rows, jsonx.M{"type": "qemu", "vmid": -5.0, "node": "SRV-PVEA", "status": "running", "cpu": 1e308}), nil
	}
	b.Mu.Unlock()
	for range 3 {
		p.PollOnce(ctx)
	}
	list := p.Snapshot()
	mustJSON(t, "system list", model.SystemList{Systems: list})
	js, order := p.SnapshotJSON()
	if len(order) != len(list) || len(order) != len(before) {
		t.Fatalf("%d systems, %d before", len(order), len(before))
	}
	for _, id := range order {
		if len(js[id]) == 0 || !json.Valid(js[id]) {
			t.Fatalf("%s: stream JSON %q", id, js[id])
		}
	}
	mustJSON(t, "summary", p.Summary())
	mustJSON(t, "power", p.Power.Overview())
	mustJSON(t, "power history", p.Power.HistoryView(""))
	mustJSON(t, "ups", p.UPS.Overview(true))
	mustJSON(t, "health", p.healthReport())

	after := snap(p)
	lap := after["laptop"]
	if lap.Status != "online" || *lap.CPUPercent != 100 || lap.Cores != nil || lap.MemTotal != nil || lap.MemUsed != nil ||
		lap.DiskTotal != nil || lap.UptimeS != nil || len(lap.Temps) != 0 || len(lap.LoadAvg) != 0 || lap.PowerPending != nil ||
		*lap.PowerW != 80 || lap.NetRxBps != nil { // 80 W: its plug's last good reading, kept while the row is stale
		t.Fatalf("the hostile host: %+v", lap)
	}
	if got := string(mustJSON(t, "tower", stable(after["tower"]))); got != wantTower {
		t.Fatalf("another host changed:\n%s\n%s", got, wantTower)
	}
	if vm := after["qemu-100"]; vm.Status != "online" || vm.CPUPercent != nil || vm.MemTotal != nil || vm.NetRxBps != nil {
		t.Fatalf("the hostile Proxmox row: %+v", vm)
	}
	sm := p.Summary()
	if sm.Totals.Hosts != 4 || sm.Totals.HostsOnline != 3 || sm.Totals.CPUPercent == nil || *sm.Totals.CPUPercent < 0 || *sm.Totals.CPUPercent > 100 ||
		sm.Totals.MemTotal < 0 || sm.Counts.Total != len(list) {
		t.Fatalf("%+v", sm.Totals)
	}
	ov := p.Power.Overview()
	for _, st := range ov.Sources {
		if strings.HasPrefix(st.ID, "laptop/") && (st.Status == "ok" || st.Error == nil) {
			t.Fatalf("%+v", st)
		}
	}
	if ov.TotalW == nil || *ov.TotalW != 160 { // tower's plug (80 W) and the laptop's last good reading (80 W, stale)
		t.Fatalf("total %v", ov.TotalW)
	}
	if u := p.UPS.State("laptop/apc"); u == nil || u.Status != "online" {
		t.Fatalf("%+v", u)
	}
	// the agent is normal again: so is its host
	b.Mu.Lock()
	b.Metrics["laptop"] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
	b.Mu.Unlock()
	p.PollOnce(ctx)
	if lap := snap(p)["laptop"]; lap.Status != "online" || *lap.Cores != 8 || *lap.PowerW != 80 {
		t.Fatalf("%+v", lap)
	}
}

// stable is s without the fields that change from poll to poll (the node's CPU model arrives in the background).
func stable(s model.System) model.System {
	s.LastSeen, s.UpdatedAt, s.NetRxBps, s.NetTxBps, s.CPUModel = nil, nil, nil, nil, nil
	return s
}

// The last line of defence (M2): should a system still hold something JSON cannot carry, that one system shows
// as an "error" entry with a reason. The list, the stream and the summary keep working, the other systems are
// untouched, the machine stays visible, and it is back as soon as its data is.
func TestSystemThatCannotBeWrittenShowsAsError(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)

	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	b := testutil.NewBackend()
	p, err := New(cfg, config.Secrets{}, b)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p.PollOnce(ctx)
	good := snap(p)
	poison := func(change func(s *model.System)) {
		p.mu.Lock()
		s := p.systems["laptop"]
		change(&s)
		p.systems["laptop"] = s
		p.version++
		p.mu.Unlock()
	}
	for name, change := range map[string]func(s *model.System){
		"NaN":      func(s *model.System) { s.CPUPercent = model.P(math.NaN()) },
		"infinity": func(s *model.System) { s.Temps = []model.Temperature{{Sensor: "x", CurrentC: math.Inf(1)}} },
		"date": func(s *model.System) {
			s.PowerPending = &model.PowerPending{Action: "sleep", At: time.Unix(1e12, 0).UTC()}
			s.LastSeen = model.P(time.Unix(1e12, 0).UTC())
		},
	} {
		poison(change)
		list := p.Snapshot()
		mustJSON(t, name+": system list", model.SystemList{Systems: list})
		js, order := p.SnapshotJSON()
		if len(order) != len(good) {
			t.Fatalf("%s: %d systems instead of %d: a system is hidden", name, len(order), len(good))
		}
		for _, id := range order {
			if len(js[id]) == 0 || !json.Valid(js[id]) {
				t.Fatalf("%s: %s: stream JSON %q", name, id, js[id])
			}
		}
		mustJSON(t, name+": summary", p.Summary())
		now := snap(p)
		e := now["laptop"]
		if e.Status != StatusError || e.Error == nil || !strings.Contains(*e.Error, "cannot show") || e.Name != "laptop" || e.Kind != "agent" ||
			e.Source != "agent" || *e.Group != "Alice" || e.CPUPercent != nil || e.PowerPending != nil || e.PollMode != "default" ||
			len(e.PowerActions) == 0 || *e.DWServiceURL == "" {
			t.Fatalf("%s: %+v", name, e)
		}
		// a complete System: every list the page reads is a list, every key is there
		var m map[string]any
		_ = json.Unmarshal(js["laptop"], &m)
		for _, k := range []string{"disks", "temps", "fans", "voltages", "gpus", "ups", "tags", "power_actions"} {
			if _, ok := m[k].([]any); !ok {
				t.Fatalf("%s: %q is %v in the error entry", name, k, m[k])
			}
		}
		var ok map[string]any
		_ = json.Unmarshal(js["tower"], &ok)
		for k := range ok {
			if _, has := m[k]; !has {
				t.Fatalf("%s: the error entry has no %q", name, k)
			}
		}
		if m["status"] != "error" || m["id"] != "laptop" {
			t.Fatalf("%s: %s", name, js["laptop"])
		}
		for id, s := range now {
			if id != "laptop" && string(mustJSON(t, id, stable(s))) != string(mustJSON(t, id, stable(good[id]))) {
				t.Fatalf("%s: %s changed", name, id)
			}
		}
		if sm := p.Summary(); sm.Counts.Total != len(good) || sm.Counts.Online != 2 { // the node, the VM; not the error entry
			t.Fatalf("%s: %+v", name, sm.Counts)
		}
		p.Snapshot() // read again without a change: not logged again
		p.Bump()
		p.Snapshot()
		p.PollOnce(ctx) // the agent's next (good) answer replaces the entry
		if s := snap(p)["laptop"]; s.Status != "online" || *s.Cores != 8 {
			t.Fatalf("%s: not back: %+v", name, s)
		}
	}
	if n := strings.Count(logs.String(), "cannot be written as JSON"); n != 3 {
		t.Fatalf("logged %d times:\n%s", n, logs.String())
	}
}

// L10: a Proxmox API (or whatever answers in its place) that answers with a redirect. The panel used to follow
// it, up to 10 steps, also to plain http, and sent the API token along when the host name stayed the same. Now
// the redirect is not followed: no second request, no token anywhere else, the cluster shows the error, and a
// power action answered with a redirect is an error, not "sent".
func TestProxmoxRedirectsAreNotFollowed(t *testing.T) {
	var elsewhere atomic.Int32
	var leaked atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // plain http, same host name
		elsewhere.Add(1)
		leaked.Store(r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]any{"data": testutil.PVEResources()})
	}))
	defer other.Close()
	var code, asked atomic.Int32
	code.Store(302)
	pve := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		http.Redirect(w, r, other.URL+r.URL.Path, int(code.Load()))
	}))
	defer pve.Close()
	cfg := testutil.Config(t, []string{pve.URL}, deadURL(t), deadURL(t))
	p, err := New(cfg, config.Secrets{SessionSecret: "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, c := range []int32{301, 302, 303, 307, 308} {
		code.Store(c)
		p.PollOnce(ctx)
		var src *model.SourceStatus
		for _, s := range p.Summary().Sources {
			if s.Name == "proxmox:pve" {
				src = &s
			}
		}
		if src == nil || src.OK || src.LastError == nil || *src.LastError != fmt.Sprintf("http %d", c) {
			t.Fatalf("%d: %+v", c, src)
		}
		if _, err := p.PVEPower(ctx, "pve", "/nodes/n1/qemu/100/status/shutdown", nil); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("http %d", c)) {
			t.Fatalf("%d: a power action answered with a redirect: %v", c, err)
		}
	}
	if _, ok := snap(p)["qemu-100"]; ok {
		t.Fatal("the data behind the redirect was used")
	}
	if elsewhere.Load() != 0 {
		t.Fatalf("%d requests followed a redirect, with Authorization %q", elsewhere.Load(), leaked.Load())
	}
	if asked.Load() != 10 {
		t.Fatalf("%d requests to Proxmox", asked.Load())
	}
}

// An agent that answers /metrics with a redirect is not followed either (that was already so). The host shows
// as failing with the status ("http 302"), whatever the redirect's body holds.
func TestAgentRedirectIsAnError(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		_ = json.NewEncoder(w).Encode(testutil.AgentMetrics())
	}))
	defer other.Close()
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	defer agent.Close()
	cfg := testutil.Config(t, []string{deadURL(t)}, agent.URL, deadURL(t))
	p, err := New(cfg, config.Secrets{SessionSecret: "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	if lap := snap(p)["laptop"]; lap.Status != "offline" || lap.Error == nil || *lap.Error != "http 302" || elsewhere.Load() != 0 {
		t.Fatalf("%+v, %d requests followed the redirect", lap, elsewhere.Load())
	}
}
