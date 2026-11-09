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
	if sm.Cluster == nil || sm.Cluster.NodesOnline != 1 || sm.Cluster.GuestsTotal != 3 || sm.Counts.Total != len(p.Snapshot()) ||
		// both nodes (the laptop agent merged into one) and cyrene; only the online node adds usage
		sm.Totals.Hosts != 3 || sm.Totals.HostsOnline != 1 || sm.Totals.Cores != sm.Cluster.Cores || sm.Totals.MemTotal != sm.Cluster.MemTotal {
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

// Hosts added in the panel (Add host) are polled at once with their own tokens and forgotten when removed,
// without a restart; the real HTTP backend sends exactly their tokens.
func TestAddAndRemoveAgentAtRunTime(t *testing.T) {
	var auth atomic.Value
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(testutil.AgentMetrics())
	}))
	defer agent.Close()
	cfg := testutil.Config(t, []string{deadURL(t)}, deadURL(t), deadURL(t))
	p, err := New(cfg, config.Secrets{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p.PollOnce(ctx)
	a := config.Agent{ID: "nsx-furina", Name: "NSX-FURINA", URL: agent.URL, Managed: true,
		Token: "managed-read-token-xxxxxxxxxxxxxxxx", ControlToken: "managed-control-token-yyyyyyyyyyyy", PowerControl: true}
	o := &config.SystemOverride{ID: "nsx-furina", Wol: &config.Wol{MACs: []string{"aa:bb:cc:dd:ee:ff"}, Relay: "laptop"}}
	if err := p.AddAgent(a, o); err != nil {
		t.Fatal(err)
	}
	if err := p.AddAgent(a, nil); err == nil {
		t.Fatal("duplicate accepted")
	}
	p.PollOnce(ctx)
	if s, ok := snap(p)["nsx-furina"]; !ok || s.Status != "online" {
		t.Fatalf("managed agent not polled: %+v", s)
	}
	if auth.Load() != "Bearer managed-read-token-xxxxxxxxxxxxxxxx" {
		t.Fatal("wrong token sent")
	}
	if got, ok := p.AgentByID("nsx-furina"); !ok || !got.Managed || p.Override("nsx-furina") == nil {
		t.Fatal("agent / override not registered")
	}
	p.RemoveAgent("nsx-furina")
	p.PollOnce(ctx)
	if _, ok := snap(p)["nsx-furina"]; ok || p.Override("nsx-furina") != nil {
		t.Fatal("removed agent still shown")
	}
	for _, x := range p.Agents() {
		if x.ID == "nsx-furina" {
			t.Fatal("still in Agents()")
		}
	}
}

// After a shutdown from the panel the host stops answering: its pending action (and the Cancel button and the
// countdown banner) goes once the time has passed or the host is offline; a blip before the time keeps it.
func TestPendingPowerClearsWhenHostGoesAway(t *testing.T) {
	cfg := testutil.Config(t, []string{deadURL(t)}, deadURL(t), deadURL(t))
	p, err := New(cfg, config.Secrets{}, testutil.NewBackend())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seen := now.Add(-5 * time.Second)
	s := model.System{ID: "x", Status: "online", LastSeen: &seen,
		PowerPending: &model.PowerPending{Action: "shutdown", At: now.Add(30 * time.Second), DelayS: 60}}
	if d := p.degrade(s, now, "timeout"); d.Status != "stale" || d.PowerPending == nil {
		t.Fatal("a blip before the shutdown time keeps the pending action", d.Status, d.PowerPending)
	}
	s.PowerPending.At = now.Add(-time.Second)
	if d := p.degrade(s, now, "timeout"); d.PowerPending != nil {
		t.Fatal("after its time the shutdown is no longer pending")
	}
	old := now.Add(-time.Hour)
	s.LastSeen, s.PowerPending.At = &old, now.Add(time.Hour)
	if d := p.degrade(s, now, "timeout"); d.Status != "offline" || d.PowerPending != nil {
		t.Fatal("offline host keeps its pending action", d.Status)
	}
}

// The Overview's CPU / memory cards add up every visible host: nodes of all clusters and standalone agents, not
// agents inside a guest; offline hosts are counted as hosts but not towards usage.
func TestHostTotals(t *testing.T) {
	node := func(id, cluster, status string, cores int, cpu float64, used, total int64) model.System {
		return model.System{ID: id, Kind: "node", ClusterID: model.P(cluster), Status: status, Cores: model.P(cores),
			CPUPercent: model.P(cpu), MemUsed: model.P(used), MemTotal: model.P(total)}
	}
	agent := node("pc", "", "online", 8, 50, 4, 16)
	agent.Kind, agent.ClusterID = "agent", nil
	inVM := agent
	inVM.ID = "vm-agent"
	guest := node("qemu-100", "a", "online", 4, 100, 2, 8)
	guest.Kind = "qemu"
	systems := []model.System{node("node-a", "a", "online", 8, 10, 10, 32), node("node-b", "b", "online", 4, 40, 6, 16),
		node("node-c", "b", "offline", 16, 90, 30, 64), agent, inVM, guest}
	got := hostTotals(systems, map[string]bool{"vm-agent": true})
	// cores 8+4+8 = 20; busy 80+160+400 = 640 -> 32 %; memory 10+6+4 of 32+16+16
	if got.Hosts != 4 || got.HostsOnline != 3 || got.Cores != 20 || *got.CPUPercent != 32 || got.MemUsed != 20 || got.MemTotal != 64 {
		t.Fatalf("%+v", got)
	}
	if e := hostTotals(nil, nil); e.Hosts != 0 || e.CPUPercent != nil || e.MemTotal != 0 {
		t.Fatalf("empty: %+v", e)
	}
}

// Add host / Re-pair / Remove change the systems: overrides while snapshots and Proxmox polls read them. Under
// -race this used to report a data race on p.overrides (and could crash with "concurrent map read and map
// write").
func TestOverridesRaceWithAddRepairRemove(t *testing.T) {
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://cyrene")
	p, err := New(cfg, config.Secrets{}, testutil.NewBackend())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p.PollOnce(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 200 {
			id := fmt.Sprintf("host-%d", i%3)
			a := config.Agent{ID: id, Name: id, URL: "http://" + id, Managed: true, Token: "managed-read-token-xxxxxxxxxxxxxxxx"}
			o := &config.SystemOverride{ID: id, Wol: &config.Wol{MACs: []string{"aa:bb:cc:dd:ee:ff"}, Relay: "laptop"}}
			p.RemoveAgent(id) // Re-pair = Remove + Add
			if err := p.AddAgent(a, o); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for {
		select {
		case <-done:
			for i := range 3 {
				p.RemoveAgent(fmt.Sprintf("host-%d", i))
			}
			if len(p.SystemsConfig()) != len(cfg.Systems) {
				t.Fatalf("systems: entries left behind: %+v", p.SystemsConfig())
			}
			return
		default:
		}
		p.Bump()
		_ = p.Snapshot()
		p.PollOnce(ctx)
	}
}
