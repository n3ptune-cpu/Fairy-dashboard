// Package testutil has shared fixtures for the poller and web tests.
package testutil

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// ReadToken and ControlToken are the agent tokens Config puts into the environment.
const (
	ReadToken    = "read-token-aaaaaaaaaaaaaaaaaaaa"
	ControlToken = "control-token-bbbbbbbbbbbbbbbbbb"
)

// Config returns the test panel config (like the prototype's conftest); proxmox/agent URLs may be overridden.
func Config(t *testing.T, pveURLs []string, laptopURL, cyreneURL string) *config.Panel {
	t.Helper()
	t.Setenv("TOK_LAPTOP", ReadToken)
	t.Setenv("TOK_CYRENE", ReadToken+"x")
	t.Setenv("CTL_CYRENE", ControlToken)
	t.Setenv("PVE_SECRET", "pve-secret-uuid")
	dir := t.TempDir()
	q := func(s string) string { return fmt.Sprintf("%q", s) }
	urls := ""
	for i, u := range pveURLs {
		if i > 0 {
			urls += ", "
		}
		urls += q(u)
	}
	y := `
public_url: https://panel.example.com
request_timeout: 1
offline_after: 60
enable_docs: true
audit_log: ` + q(dir+"/audit.log") + `
data_dir: ` + q(dir+"/data") + `
oidc: {issuer: "https://id.example.com", client_id: cid, allowed_groups: [viewers], control_groups: [controllers]}
proxmox: {urls: [` + urls + `], token_id: "fairy@pve!panel", token_secret_env: PVE_SECRET, verify_tls: false,
          power_token_id: "fairy@pve!power", power_token_secret_env: PVE_SECRET}
agents:
  - {id: laptop, url: ` + q(laptopURL) + `, token_env: TOK_LAPTOP, group: Neptune, power_control: true, self_update: true,
     control_token_env: CTL_CYRENE, dwservice_url: "https://www.dwservice.net/en/login.html?loginUser=abc"}
  - {id: cyrene, url: ` + q(cyreneURL) + `, token_env: TOK_CYRENE,
     fan_control: {vendor: supermicro, control_token_env: CTL_CYRENE}}
systems:
  - {id: qemu-100, dwservice_url: "https://www.dwservice.net/en/login.html?loginUser=vm"}
`
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// PVEResources is a cluster/resources fixture.
func PVEResources() []jsonx.M {
	return []jsonx.M{
		{"id": "node/NSX-KACHINA", "type": "node", "node": "NSX-KACHINA", "status": "online", "cpu": 0.125,
			"maxcpu": 8.0, "mem": float64(8 << 30), "maxmem": float64(32 << 30), "disk": float64(10 << 30), "maxdisk": float64(100 << 30), "uptime": 1000.0},
		{"id": "node/NSX-SNEZHNAYA", "type": "node", "node": "NSX-SNEZHNAYA", "status": "offline", "maxcpu": 12.0, "maxmem": float64(64 << 30)},
		{"id": "qemu/100", "type": "qemu", "vmid": 100.0, "name": "win11", "node": "NSX-KACHINA", "status": "running",
			"cpu": 0.5, "maxcpu": 4.0, "mem": float64(4 << 30), "maxmem": float64(8 << 30), "disk": 0.0, "maxdisk": float64(64 << 30),
			"netin": 1000.0, "netout": 500.0, "uptime": 50.0, "tags": "windows;gaming", "template": 0.0},
		{"id": "lxc/101", "type": "lxc", "vmid": 101.0, "name": "minecraft", "node": "NSX-KACHINA", "status": "stopped",
			"cpu": 0.0, "maxcpu": 2.0, "mem": 0.0, "maxmem": float64(1 << 31), "netin": 0.0, "netout": 0.0},
		{"id": "qemu/120", "type": "qemu", "vmid": 120.0, "name": "truenas", "node": "NSX-SNEZHNAYA", "status": "unknown"},
	}
}

// AgentMetrics is an agent /metrics fixture.
func AgentMetrics() jsonx.M {
	return jsonx.M{"hostname": "laptop", "platform": jsonx.M{"system": "Linux", "pretty": "Ubuntu 24.04.1 LTS"}, "uptime_s": 3600.0,
		"cpu": jsonx.M{"percent": 12.5, "logical": 8.0}, "memory": jsonx.M{"total": float64(16 << 30), "used": float64(6 << 30)},
		"disks": []any{jsonx.M{"mount": "/", "fstype": "ext4", "total": float64(500 << 30), "used": float64(100 << 30)},
			jsonx.M{"mount": "/home", "fstype": "ext4", "total": float64(1000 << 30), "used": float64(300 << 30)}},
		"net": jsonx.M{"bytes_recv": 10000.0, "bytes_sent": 5000.0}, "load_avg": []any{0.5, 0.4, 0.3},
		"temps": []any{jsonx.M{"sensor": "coretemp", "label": "Package id 0", "current": 55.0, "high": 90.0, "critical": 100.0}},
		"fans":  []any{jsonx.M{"name": "thinkpad fan1", "rpm": 2400.0}},
		"gpus": []any{jsonx.M{"name": "NVIDIA GeForce RTX 3050 Laptop GPU", "vendor": "nvidia", "util_percent": 7.0,
			"vram_used": float64(300 << 20), "vram_total": float64(4 << 30), "temp_c": 45.0, "power_w": 6.5}},
		"power": jsonx.M{"cpu_package_w": 9.5, "gpu_w": 6.5, "battery_w": 15.2, "battery_state": "discharging",
			"bmc_w": nil, "sensors": []any{jsonx.M{"name": "rapl package-0", "watts": 9.5}}, "source": []any{"rapl", "battery"}},
		"power_sources": []any{jsonx.M{"id": "desk", "name": "Desk plug", "type": "tuya", "ok": true, "watts": 80.0,
			"voltage": 230.0, "current": nil, "energy_kwh_total": nil, "system_hint": "self"}},
		"features": jsonx.M{"processes": true, "cmdline": true, "fan_control": false}}
}

// Backend is a scriptable poller.Backend.
type Backend struct {
	Mu        sync.Mutex
	PVE       func() ([]jsonx.M, error)
	Metrics   map[string]func() (jsonx.M, error)
	Requests  []string
	PVECalls  []string
	FanStatus jsonx.M
	// Reply, when set and returning a status (or an error), answers AgentRequest instead of the defaults.
	Reply func(agentID, path string) (int, jsonx.M, error)
	// ProcErr makes FetchProcesses fail.
	ProcErr error
}

// NewBackend returns a fake backend serving PVEResources and AgentMetrics.
func NewBackend() *Backend {
	return &Backend{PVE: func() ([]jsonx.M, error) { return PVEResources(), nil },
		Metrics: map[string]func() (jsonx.M, error){
			"laptop": func() (jsonx.M, error) { return AgentMetrics(), nil },
			"cyrene": func() (jsonx.M, error) { return nil, context.DeadlineExceeded },
		},
		FanStatus: jsonx.M{"enabled": true, "vendor": "supermicro", "mode": "optimal", "duty": jsonx.M{}, "fans": []any{}, "events": []any{}}}
}

// FetchProxmox returns the fixture rows (or the configured error).
func (b *Backend) FetchProxmox(context.Context, config.Proxmox, time.Duration) ([]jsonx.M, error) {
	b.Mu.Lock()
	defer b.Mu.Unlock()
	return b.PVE()
}

// FetchNodeStatus returns a fixed CPU model.
func (b *Backend) FetchNodeStatus(context.Context, config.Proxmox, string) (jsonx.M, error) {
	return jsonx.M{"cpuinfo": jsonx.M{"model": "Intel(R) Core(TM) i7-8700T CPU @ 2.40GHz"}}, nil
}

// FetchAgentMetrics returns the fixture payload (or the configured error).
func (b *Backend) FetchAgentMetrics(_ context.Context, a config.Agent, _ time.Duration) (jsonx.M, error) {
	b.Mu.Lock()
	defer b.Mu.Unlock()
	if f := b.Metrics[a.ID]; f != nil {
		return f()
	}
	return nil, devices.Errf("connection failed")
}

// PVEPower records the call.
func (b *Backend) PVEPower(_ context.Context, cluster, path string, data url.Values) (any, error) {
	b.Mu.Lock()
	defer b.Mu.Unlock()
	b.PVECalls = append(b.PVECalls, cluster+" "+path+" "+data.Encode())
	return "UPID:test", nil
}

// FetchProcesses returns an empty list (or ProcErr).
func (b *Backend) FetchProcesses(_ context.Context, _ config.Agent, sid string, _ int, s string) (model.ProcessList, error) {
	if b.ProcErr != nil {
		return model.ProcessList{}, b.ProcErr
	}
	return model.ProcessList{SystemID: sid, Sort: s, Processes: []model.Process{}}, nil
}

// AgentRequest records the call and answers with the configured reply.
func (b *Backend) AgentRequest(_ context.Context, a config.Agent, method, path string, body any) (int, jsonx.M, error) {
	b.Mu.Lock()
	defer b.Mu.Unlock()
	b.Requests = append(b.Requests, fmt.Sprintf("%s %s %s %v", a.ID, method, path, body))
	if b.Reply != nil {
		if st, data, err := b.Reply(a.ID, path); st != 0 || err != nil {
			return st, data, err
		}
	}
	switch path {
	case "/fan/status":
		return 200, b.FanStatus, nil
	case "/update":
		return 202, jsonx.M{"state": "downloading", "current": "41-aaaaaaa"}, nil
	case "/power":
		return 200, jsonx.M{"action": "shutdown", "pending": jsonx.M{"action": "shutdown", "at": 1.9e9, "delay_s": 120.0, "requested_by": "Neptune"}}, nil
	}
	return 200, jsonx.M{"ok": true}, nil
}

// HelperMetrics is the /metrics of a Windows agent with the elevated helper (Fairy-agent v0.5.0) in the given
// state: while it runs, Core Temp and BMC (IPMI) readings and fan control; otherwise none of them, marked
// "helper_stopped". features lists what the helper does on that host.
func HelperMetrics(state string, features ...string) jsonx.M {
	m := AgentMetrics()
	m["platform"] = jsonx.M{"system": "Windows", "pretty": "Windows 11 Pro 24H2"}
	m["load_avg"] = nil
	feat := make([]any, len(features))
	for i, f := range features {
		feat[i] = f
	}
	m["helper"] = jsonx.M{"state": state, "needed": true, "version": "0.5.0", "since": "2026-11-09T10:00:00Z",
		"watchdog": state == "running", "features": feat, "error": ""}
	m["service_account"] = `NT SERVICE\fairy-agent`
	m["temps"] = []any{jsonx.M{"sensor": "acpi", "label": "ACPI thermal zone", "current": 48.0}}
	m["fans"], m["voltages"] = []any{}, []any{}
	if state == "running" {
		m["temps"] = append(m["temps"].([]any), jsonx.M{"sensor": "coretemp", "label": "CPU Package", "current": 61.0},
			jsonx.M{"sensor": "IPMI", "label": "CPU Temp", "current": 55.0})
		m["fans"] = []any{jsonx.M{"name": "IPMI FAN1", "rpm": 900.0}}
		m["voltages"] = []any{jsonx.M{"name": "IPMI 12V", "volts": 12.1}}
		m["features"].(jsonx.M)["fan_control"] = true
		return m
	}
	un := jsonx.M{}
	for _, f := range features {
		if f == "coretemp" || f == "ipmi" || f == "fans" {
			un[f] = "helper_stopped"
		}
	}
	m["unavailable"] = un
	return m
}
