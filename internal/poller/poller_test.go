package poller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

func snap(p *Poller) map[string]model.System {
	out := map[string]model.System{}
	for _, s := range p.Snapshot() {
		out[s.ID] = s
	}
	return out
}

func deadURL(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := "http://" + ln.Addr().String()
	ln.Close()
	return u
}

// Real HTTP backend: Proxmox failover, the exact auth headers, rates, stale/offline, deleted guests, http errors.
func TestPollRatesAuthFailoverAndOffline(t *testing.T) {
	var pveAuth, agentAuth atomic.Value
	pveData := atomic.Value{}
	pveData.Store(testutil.PVEResources())
	pveFail := atomic.Bool{}
	pve := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pveAuth.Store(r.Header.Get("Authorization"))
		if pveFail.Load() {
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": pveData.Load()})
	}))
	defer pve.Close()
	metrics := atomic.Value{}
	metrics.Store(testutil.AgentMetrics())
	agentDown := atomic.Bool{}
	laptop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agentAuth.Store(r.Header.Get("Authorization"))
		if agentDown.Load() {
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			c.Close()
			return
		}
		_ = json.NewEncoder(w).Encode(metrics.Load())
	}))
	defer laptop.Close()
	cfg := testutil.Config(t, []string{deadURL(t), pve.URL}, laptop.URL, deadURL(t))
	p, err := New(cfg, config.Secrets{SessionSecret: "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_000_000, 0)
	p.Clock = func() time.Time { return now }
	ctx := context.Background()
	p.PollOnce(ctx)

	if pveAuth.Load() != "PVEAPIToken=fairy@pve!panel=pve-secret-uuid" || agentAuth.Load() != "Bearer "+testutil.ReadToken {
		t.Fatal("auth headers", pveAuth.Load(), agentAuth.Load())
	}
	s := snap(p)
	if *s["qemu-100"].DWServiceURL != "https://www.dwservice.net/en/login.html?loginUser=vm" { // override applied
		t.Fatal("override")
	}
	lap := s["laptop"]
	if lap.Status != "online" || *lap.OS != "Ubuntu 24.04.1 LTS" || *lap.Cores != 8 || *lap.DiskTotal != 1500<<30 ||
		lap.Temps[0].CurrentC != 55 || *lap.GPUs[0].VRAMTotal != 4<<30 || lap.Fans[0].RPM != 2400 ||
		*lap.ComponentPower.BatteryState != "discharging" || lap.NetRxBps != nil || *lap.Group != "Neptune" {
		t.Fatalf("%+v", lap)
	}
	if s["cyrene"].Status != "offline" || *s["cyrene"].Error != "connection failed" { // never seen -> offline placeholder
		t.Fatalf("%+v", s["cyrene"])
	}
	// second poll 5 s later: counters grew -> rates; deleted guests disappear
	now = now.Add(5 * time.Second)
	m := testutil.AgentMetrics()
	m["net"] = jsonx.M{"bytes_recv": 15000.0, "bytes_sent": 6000.0}
	metrics.Store(m)
	res := testutil.PVEResources()
	res[2]["netin"], res[2]["netout"] = 6000.0, 1500.0
	pveData.Store(res[:3])
	p.PollOnce(ctx)
	s = snap(p)
	if *s["laptop"].NetRxBps != 1000 || *s["laptop"].NetTxBps != 200 || *s["qemu-100"].NetRxBps != 1000 || *s["qemu-100"].NetTxBps != 200 {
		t.Fatal("rates")
	}
	if _, ok := s["lxc-101"]; ok {
		t.Fatal("deleted guest kept")
	}
	// agent fails shortly after success -> stale (keeps values), much later -> offline
	now = now.Add(10 * time.Second)
	agentDown.Store(true)
	p.PollOnce(ctx)
	lap = snap(p)["laptop"]
	if lap.Status != "stale" || lap.LastSeen == nil || *lap.MemUsed != 6<<30 {
		t.Fatalf("%+v", lap)
	}
	now = now.Add(120 * time.Second)
	p.PollOnce(ctx)
	if snap(p)["laptop"].Status != "offline" {
		t.Fatal("offline")
	}
	// proxmox down entirely -> its systems degrade, the source reports the error
	pveFail.Store(true)
	p.PollOnce(ctx)
	var found bool
	for _, src := range p.Summary().Sources {
		if src.Name == "proxmox:pve" && src.LastError != nil && *src.LastError == "http 500" {
			found = true
		}
	}
	if st := snap(p)["qemu-100"].Status; !found || (st != "stale" && st != "offline") {
		t.Fatalf("%+v", p.Summary().Sources)
	}
}

func TestAgentPowerMergeNodeEnrichmentAndPause(t *testing.T) {
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://cyrene")
	cfg.Agents[0].Node = "NSX-KACHINA" // the laptop agent "runs on" the node: it enriches it
	b := testutil.NewBackend()
	p, err := New(cfg, config.Secrets{}, b)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p.PollOnce(ctx)
	p.PollOnce(ctx) // node cpuinfo is fetched in the background after the first poll
	time.Sleep(50 * time.Millisecond)
	p.PollOnce(ctx)
	s := snap(p)
	if _, ok := s["laptop"]; ok {
		t.Fatal("agent should be merged into its node")
	}
	n := s["node-nsx-kachina"]
	if len(n.Temps) != 1 || n.ComponentPower == nil || !n.ProcessesAvailable ||
		*n.CPUModel != "Intel(R) Core(TM) i7-8700T CPU @ 2.40GHz" {
		t.Fatalf("%+v", n)
	}
	if len(n.PowerActions) != 2 || len(s["qemu-100"].PowerActions) != 4 {
		t.Fatal(n.PowerActions, s["qemu-100"].PowerActions)
	}
	ov := p.Power.Overview()
	if *ov.TotalW != 80 || *ov.Sources[0].Via != "laptop" {
		t.Fatalf("%+v", ov)
	}
	// pause: frozen values + status "paused"; poll status reflects the override
	if err := p.SetPoll("qemu-100", nil); err != nil {
		t.Fatal(err)
	}
	ps := p.PollStatus("node-nsx-kachina")
	if ps.Mode != "default" || len(ps.Jobs) != 2 {
		t.Fatalf("%+v", ps)
	}
	b.Metrics["laptop"] = func() (jsonx.M, error) { return nil, context.DeadlineExceeded }
	p.PollOnce(ctx)
	if p.Power.Overview().Sources[0].Status != "stale" {
		t.Fatal("agent devices must go stale, not stay ok")
	}
	sm := p.Summary()
	if sm.Cluster == nil || sm.Cluster.NodesOnline != 1 || sm.Cluster.GuestsTotal != 3 || sm.Counts.Total != len(p.Snapshot()) {
		t.Fatalf("%+v", sm)
	}
}

// The panel redacts secrets in process command lines itself, whatever the agent sent.
func TestProcessCmdlinesAreRedacted(t *testing.T) {
	pl, err := DecodeProcessList(jsonx.M{"processes": []any{
		jsonx.M{"pid": 1.0, "cmdline": "backup --password=hunter2 --host db"},
		jsonx.M{"pid": 2.0, "cmdline": nil},
	}}, "straw-pc")
	if err != nil {
		t.Fatal(err)
	}
	if c := *pl.Processes[0].Cmdline; strings.Contains(c, "hunter2") || !strings.Contains(c, "--host db") {
		t.Fatal(c)
	}
	if pl.Processes[1].Cmdline != nil {
		t.Fatal("nil cmdline")
	}
}

// Proxmox answering with an error is 424 (its message must survive Cloudflare); not reaching it is 502.
func TestPVEPowerErrorStatus(t *testing.T) {
	status := atomic.Int32{}
	pve := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer pve.Close()
	for _, tc := range []struct {
		url  string
		code int32
		want int
		msg  string
	}{{pve.URL, 403, 424, "power token"}, {pve.URL, 500, 424, "http 500"}, {deadURL(t), 0, 502, "proxmox:"}} {
		status.Store(tc.code)
		p, err := New(testutil.Config(t, []string{tc.url}, deadURL(t), deadURL(t)), config.Secrets{SessionSecret: "x"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.PVEPower(context.Background(), "pve", "/nodes/n1/status", nil)
		var ae *apierr.Error
		if !errors.As(err, &ae) || ae.Status != tc.want || !strings.Contains(fmt.Sprint(ae.Detail), tc.msg) {
			t.Fatal(tc.code, err)
		}
	}
}
