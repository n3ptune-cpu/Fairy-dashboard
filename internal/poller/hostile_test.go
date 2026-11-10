package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
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
