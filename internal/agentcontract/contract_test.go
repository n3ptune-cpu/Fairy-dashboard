package agentcontract_test

// The panel contract test: the panel's real poller, normaliser, power/UPS managers and web API talk over HTTP
// to the real fairy-agent handler (with fake collectors, a fake BMC and fake devices). If this passes, the panel
// works with fairy-agent without any panel change.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/agenttest"
	agentapi "github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/collect"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/ipmi/fakebmc"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/release"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/release/releasetest"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/demo"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/web"
)

const (
	readTok = "contract-read-token-xxxxxxxxxxxxxxx"
	ctlTok  = "contract-control-token-yyyyyyyyyyyy"
)

// fakeUpsd answers NUT's LIST UPS / LIST VAR like upsd with one CyberPower UPS.
func fakeUpsd(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					switch strings.TrimSpace(line) {
					case "LIST UPS":
						fmt.Fprint(c, "BEGIN LIST UPS\nUPS cyberpower \"CyberPower CP1500\"\nEND LIST UPS\n")
					case "LIST VAR cyberpower":
						fmt.Fprint(c, "BEGIN LIST VAR cyberpower\nVAR cyberpower ups.status \"OL CHRG\"\n"+
							"VAR cyberpower battery.charge \"100\"\nVAR cyberpower ups.load \"20\"\n"+
							"VAR cyberpower ups.realpower.nominal \"900\"\nVAR cyberpower battery.runtime \"3000\"\nEND LIST VAR cyberpower\n")
					case "LOGOUT":
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

type contract struct {
	agentSrv *httptest.Server
	bmc      *fakebmc.BMC
	power    *agenttest.PowerExec
	helper   *agenttest.Helper // the Windows elevated helper (Fairy-agent v0.5.0), running at the start
	mu       sync.Mutex
	wolSent  []string
	ipmitool []string            // the BMC relay's ipmitool command lines (and whether the password came by environment)
	gh       *releasetest.Server // fake GitHub with a signed Fairy-agent release
	applied  atomic.Int32        // the agent started its installer
}

func startAgent(t *testing.T) *contract {
	t.Helper()
	plug := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"apower": 120.5, "voltage": 230.1, "current": 0.52, "aenergy": map[string]any{"total": 5000.0}})
	}))
	t.Cleanup(plug.Close)
	yaml := fmt.Sprintf(`
listen: 127.0.0.1:9101
ipmi: {enabled: true}
fan_control: {enabled: true}
power_control: {enabled: true}
wol: {enabled: true}
self_update: {enabled: true}
bmc_relay:
  enabled: true
  targets: [{id: sleepy-bmc, host: 192.168.1.50, username: fairy, password_env: CONTRACT_BMC_PASSWORD, cipher_suite: 17}]
control_rate_per_min: 60
power_sources:
  - {id: desk-plug, type: shelly, name: "Desk plug", url: %q, gen: "2", system_hint: self}
ups:
  - {id: local, host: 127.0.0.1, port: %d}
`, plug.URL, fakeUpsd(t))
	cfg, err := agent.Parse([]byte(yaml), filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	c := &contract{bmc: fakebmc.X11SCAF(), power: &agenttest.PowerExec{}, helper: agenttest.NewHelper()}
	t.Setenv("CONTRACT_BMC_PASSWORD", "hunter2")
	bmcRelay := agent.NewBMCRelay(cfg.BMCRelay, "")
	bmcRelay.FindIpmitool = func() (string, error) { return "/usr/bin/ipmitool", nil }
	bmcRelay.Run = func(_ context.Context, path string, args, env []string) ([]byte, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.ipmitool = append(c.ipmitool, fmt.Sprintf("%s %s env=%v", path, strings.Join(args, " "), env))
		return []byte("Chassis Power Control: Up/On\n"), nil
	}
	relay, err := agent.NewRelay(cfg.PowerSources, cfg.UPS)
	if err != nil {
		t.Fatal(err)
	}
	relay.PollOnce(context.Background())
	bmc := agent.NewBMC(c.bmc, 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go bmc.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for bmc.Readings() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	wol := agent.NewWOL(nil)
	wol.Send = func(addr string, pkt []byte) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.wolSent = append(c.wolSent, fmt.Sprintf("%s %d", addr, len(pkt)))
		return nil
	}
	c.gh = releasetest.New(t, release.AgentRepo)
	asset := release.AssetName(release.AgentProgram, "linux", "amd64")
	c.gh.Publish("build-42", release.AgentProgram, "42-1a2b3c4", map[string][]byte{asset: []byte("fairy-agent 42")}, nil)
	stage := t.TempDir()
	upd := agent.NewUpdater(agent.UpdaterOptions{Version: "contract", Key: c.gh.Pub, Client: c.gh.Client(),
		StageDir: filepath.Join(stage, "update"), ResultPath: filepath.Join(stage, "result.json"), Asset: asset,
		ResultWait: 100 * time.Millisecond, Apply: func(context.Context) error { c.applied.Add(1); return nil }})
	var a *agent.Agent
	d := agent.Deps{Collector: agenttest.NewCollector(), GPU: agenttest.GPU{}, BMC: bmc, Updater: upd,
		Fan:   agent.NewFan(c.bmc, bmc, cfg.FanControl, func() []collect.Temp { return a.LocalTemps() }),
		Power: agent.NewPower(c.power, cfg.PowerControl.MinDelayS), WOL: wol, Relay: relay, BMCRelay: bmcRelay, Version: "contract",
		Helper: c.helper, HelperSources: []string{agentapi.FeatureCoreTemp, agentapi.FeatureIPMI, agentapi.FeatureFans, agentapi.FeaturePower}}
	a = agent.New(cfg, agent.Tokens{Read: readTok, Control: ctlTok}, d)
	c.agentSrv = httptest.NewServer(a.Handler())
	t.Cleanup(c.agentSrv.Close)
	return c
}

func panelConfig(t *testing.T, agentURL string) *config.Panel {
	t.Setenv("AGENT_TOKEN_SRV_TOWER", readTok)
	t.Setenv("AGENT_CONTROL_TOKEN_SRV_TOWER", ctlTok)
	t.Setenv("AGENT_TOKEN_SLEEPY", readTok)
	dir := t.TempDir()
	y := fmt.Sprintf(`
public_url: https://panel.example.com
request_timeout: 2
control_rate_per_min: 30
data_dir: %q
audit_log: %q
oidc: {issuer: "https://id.example.com", client_id: cid, allowed_groups: [homelab], control_groups: [admins]}
agents:
  - id: srv-tower
    name: SRV-TOWER
    url: %q
    token_env: AGENT_TOKEN_SRV_TOWER
    group: Alice
    fan_control: {vendor: supermicro, control_token_env: AGENT_CONTROL_TOKEN_SRV_TOWER}
    power_control: true
    wol_relay: true
    self_update: true
    ups_powers: {cyberpower: [srv-tower]}
  - {id: sleepy, url: "http://127.0.0.1:1", token_env: AGENT_TOKEN_SLEEPY}
systems:
  - {id: sleepy, wol: {mac: "00:00:5e:00:53:04", macs: ["00-00-5E-00-53-04", "00:00:5e:00:53:05"], relay: srv-tower, broadcast: 192.168.1.255},
     bmc_power: {relay: srv-tower, target: sleepy-bmc}}
`, filepath.Join(dir, "data"), filepath.Join(dir, "audit.log"), agentURL)
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

type api struct {
	t   *testing.T
	srv *httptest.Server
	tok string
}

func (a api) do(method, path string, body any) (int, map[string]any) {
	a.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, a.srv.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+a.tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(raw) > 0 && raw[0] == '{' {
		_ = json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, out
}

func TestPanelContract(t *testing.T) {
	c := startAgent(t)
	cfg := panelConfig(t, c.agentSrv.URL)
	p, err := poller.New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	p.Clock = func() time.Time { return now }
	ctx := context.Background()
	p.PollOnce(ctx)
	time.Sleep(300 * time.Millisecond) // the agent computes /metrics at most every 250 ms
	now = now.Add(2 * time.Second)
	p.PollOnce(ctx)

	var s model.System
	for _, x := range p.Snapshot() {
		if x.ID == "srv-tower" {
			s = x
		}
	}
	// --- /metrics through the panel's normaliser
	if s.Status != "online" || s.Name != "SRV-TOWER" || s.Kind != "agent" || s.Error != nil {
		t.Fatalf("%+v", s)
	}
	if *s.CPUPercent != 23.5 || *s.Cores != 16 || *s.MemTotal != 64<<30 || *s.MemUsed != 20<<30 || len(s.Disks) != 2 ||
		*s.DiskTotal != 5000<<30 || *s.UptimeS != 86400*6 {
		t.Fatalf("cpu/mem/disk %+v", s)
	}
	if s.NetRxBps == nil || *s.NetRxBps != 500000 || *s.NetTxBps != 250000 {
		t.Fatal("network rate from the cumulative counters", s.NetRxBps, s.NetTxBps)
	}
	if model.Deref(s.OS) != "Windows 11 Enterprise 24H2" || model.Deref(s.Platform) != "Windows" || s.LoadAvg != nil {
		t.Fatal(s.OS, s.Platform, s.LoadAvg)
	}
	if s.Hardware == nil || model.Deref(s.Hardware.Display) != "Supermicro X11SCA-F" || model.Deref(s.CPUModel) == "" {
		t.Fatalf("hardware %+v", s.Hardware)
	}
	temps := map[string]float64{}
	for _, x := range s.Temps {
		temps[x.Sensor+"/"+model.Deref(x.Label)] = x.CurrentC
	}
	if temps["IPMI/CPU Temp"] != 45 || temps["acpi/ACPI thermal zone TZ00"] != 27.8 {
		t.Fatal(temps)
	}
	if len(s.Fans) != 2 || s.Fans[0].Name != "IPMI FAN1" || s.Fans[0].RPM != 1400 || len(s.Voltages) != 1 || s.Voltages[0].Volts != 12.12 {
		t.Fatal(s.Fans, s.Voltages)
	}
	if len(s.GPUs) != 1 || s.GPUs[0].Name != "NVIDIA GeForce RTX 2080 SUPER" || s.GPUs[0].Vendor != "nvidia" ||
		*s.GPUs[0].PowerW != 87.5 || *s.GPUs[0].VRAMTotal != 8<<30 || *s.GPUs[0].TempC != 61 {
		t.Fatalf("gpu %+v", s.GPUs)
	}
	if s.ComponentPower == nil || *s.ComponentPower.GPUW != 87.5 || s.ComponentPower.CPUPackageW != nil {
		t.Fatalf("power %+v", s.ComponentPower)
	}
	if !s.ProcessesAvailable || !s.FanControl || s.PowerPending != nil {
		t.Fatal("features", s.ProcessesAvailable, s.FanControl)
	}

	// --- devices relayed by the agent
	ov := p.Power.Overview()
	var plug *model.PowerSourceState
	for i := range ov.Sources {
		if ov.Sources[i].ID == "srv-tower/desk-plug" {
			plug = &ov.Sources[i]
		}
	}
	if plug == nil || plug.Status != "ok" || *plug.Readings[0].Watts != 120.5 || model.Deref(plug.Readings[0].SystemID) != "srv-tower" ||
		model.Deref(plug.Via) != "srv-tower" {
		t.Fatalf("relayed plug %+v", ov.Sources)
	}
	upsOv, _ := json.Marshal(p.UPS.Overview(false))
	if !strings.Contains(string(upsOv), `"srv-tower/cyberpower"`) || !strings.Contains(string(upsOv), `"online"`) {
		t.Fatal("relayed UPS", string(upsOv))
	}

	// --- the web API on top (processes, fan, power, Wake-on-LAN), as the UI uses it
	srv := web.New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, p, nil, "test")
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	tok, _, err := srv.Tokens.Create("contract", "system", "control", "test", 0)
	if err != nil {
		t.Fatal(err)
	}
	a := api{t, hs, tok}

	st, pl := a.do("GET", "/api/systems/srv-tower/processes?limit=3&sort=cpu", nil)
	procs, _ := pl["processes"].([]any)
	if st != 200 || pl["disabled_by_host"] != false || pl["total_processes"] != 212.0 || len(procs) != 3 {
		t.Fatal(st, pl)
	}
	if first := procs[0].(map[string]any); first["name"] != "Ollama.exe" || first["gpu_mem"] != float64(5<<30) {
		t.Fatal(first)
	}
	if third := procs[2].(map[string]any); third["cmdline"] != "backup.exe --password=*** --target s3" {
		t.Fatal(third)
	}

	st, fs := a.do("GET", "/api/systems/srv-tower/fan", nil)
	if st != 200 || fs["enabled"] != true || fs["mode"] != "optimal" || fs["vendor"] != "supermicro" || fs["min_duty"] != 30.0 ||
		fs["max_temp_c"] != 80.0 || len(fs["fans"].([]any)) != 2 {
		t.Fatal(st, fs)
	}
	if st, b := a.do("POST", "/api/systems/srv-tower/fan/duty", map[string]any{"zone": 0, "percent": 50}); st != 409 ||
		!strings.Contains(fmt.Sprint(b["detail"]), "full") {
		t.Fatal("agent's 409 passes through", st, b)
	}
	if st, b := a.do("POST", "/api/systems/srv-tower/fan/mode", map[string]any{"mode": "full"}); st != 200 || b["ok"] != true {
		t.Fatal(st, b)
	}
	if st, b := a.do("POST", "/api/systems/srv-tower/fan/duty", map[string]any{"zone": 0, "percent": 45}); st != 200 {
		t.Fatal(st, b)
	}
	if mode, duty := c.bmc.State(); mode != 1 || duty[0] != 45 {
		t.Fatal(mode, duty)
	}
	_, fs = a.do("GET", "/api/systems/srv-tower/fan", nil)
	if fs["overridden"] != true || fs["duty"].(map[string]any)["0"] != 45.0 || len(fs["events"].([]any)) != 2 ||
		fs["auto_mode"] != "optimal" || len(fs["unsupported_modes"].([]any)) != 0 {
		t.Fatal(fs)
	}
	// a board without "optimal" (Enginetech EG320W-G20): the BMC's refusal reaches the user as 422, not as a
	// 502 that Cloudflare would replace, and the panel learns to grey the mode out
	c.bmc.RefuseMode(2, 0xCC)
	if st, b := a.do("POST", "/api/systems/srv-tower/fan/mode", map[string]any{"mode": "optimal"}); st != 422 ||
		!strings.Contains(fmt.Sprint(b["detail"]), "BMC completion code 0xCC") {
		t.Fatal("refused mode", st, b)
	}
	_, fs = a.do("GET", "/api/systems/srv-tower/fan", nil)
	if um := fs["unsupported_modes"].([]any); len(um) != 1 || um[0] != "optimal" || fs["mode"] != "full" {
		t.Fatal(fs)
	}
	c.bmc.RefuseMode(0, 0xC3) // a BMC failure (not "no such mode"): 424 with the agent's message
	if st, b := a.do("POST", "/api/systems/srv-tower/fan/mode", map[string]any{"mode": "standard"}); st != 424 ||
		!strings.Contains(fmt.Sprint(b["detail"]), "0xC3") {
		t.Fatal("BMC failure", st, b)
	}
	c.bmc.RefuseMode(0, 0)

	st, caps := a.do("GET", "/api/systems/srv-tower/power", nil)
	if st != 200 || !strings.Contains(fmt.Sprint(caps["actions"]), "via:agent") {
		t.Fatal(st, caps)
	}
	st, res := a.do("POST", "/api/systems/srv-tower/power", map[string]any{"action": "shutdown", "delay_s": 300, "confirm": "SRV-TOWER"})
	if st != 200 || res["via"] != "agent" || res["pending"].(map[string]any)["action"] != "shutdown" {
		t.Fatal(st, res)
	}
	p.PollOnce(ctx)
	for _, x := range p.Snapshot() {
		if x.ID == "srv-tower" && (x.PowerPending == nil || x.PowerPending.Action != "shutdown" || x.PowerPending.DelayS != 300 ||
			model.Deref(x.PowerPending.RequestedBy) == "") {
			t.Fatalf("pending shutdown in /metrics: %+v", x.PowerPending)
		}
	}
	if st, res := a.do("POST", "/api/systems/srv-tower/power", map[string]any{"action": "cancel"}); st != 200 || res["pending"] != nil {
		t.Fatal(st, res)
	}
	if strings.Join(c.power.Scheduled, ",") != "shutdown@5m0s" || c.power.Cancels != 1 {
		t.Fatal(c.power.Scheduled, c.power.Cancels)
	}
	// sleep (agent v0.4.2): reported in /metrics, offered, carried out through the agent, cancelled
	st, res = a.do("POST", "/api/systems/srv-tower/power", map[string]any{"action": "sleep", "delay_s": 30, "confirm": "srv-tower"})
	if st != 200 || res["via"] != "agent" || res["pending"].(map[string]any)["action"] != "sleep" {
		t.Fatal(st, res)
	}
	if st, _ := a.do("POST", "/api/systems/srv-tower/power", map[string]any{"action": "cancel"}); st != 200 ||
		strings.Join(c.power.Scheduled, ",") != "shutdown@5m0s,sleep@30s" {
		t.Fatal(st, c.power.Scheduled)
	}

	if st, res := a.do("POST", "/api/systems/sleepy/power", map[string]any{"action": "wake"}); st != 200 || res["via"] != "wol:srv-tower" {
		t.Fatal(st, res)
	}
	c.mu.Lock()
	sent := strings.Join(c.wolSent, ",")
	c.mu.Unlock()
	if sent != "192.168.1.255:9 102,192.168.1.255:9 102" { // both network cards; the duplicate MAC once
		t.Fatal(sent)
	}
	// BMC power-on (agent v0.4.2 bmc_relay): offered while sleepy is off, typed name, through srv-tower's relay
	st, caps = a.do("GET", "/api/systems/sleepy/power", nil)
	var bmcCap map[string]any
	for _, x := range caps["actions"].([]any) {
		if m := x.(map[string]any); m["action"] == "bmc_on" {
			bmcCap = m
		}
	}
	if st != 200 || bmcCap == nil || bmcCap["available"] != true || bmcCap["confirm"] != "typed" || bmcCap["via_label"] != "SRV-TOWER" {
		t.Fatal(st, caps)
	}
	if st, _ := a.do("POST", "/api/systems/sleepy/power", map[string]any{"action": "bmc_on"}); st != 400 {
		t.Fatal("typed name", st)
	}
	if st, res := a.do("POST", "/api/systems/sleepy/power", map[string]any{"action": "bmc_on", "confirm": "sleepy"}); st != 200 || res["via"] != "bmc:srv-tower" {
		t.Fatal(st, res)
	}
	c.mu.Lock()
	ipmi := strings.Join(c.ipmitool, "\n")
	c.mu.Unlock()
	if ipmi != "/usr/bin/ipmitool -I lanplus -H 192.168.1.50 -U fairy -E -C 17 chassis power on env=[IPMI_PASSWORD=hunter2]" {
		t.Fatal(ipmi)
	}

	// --- self-update: the Updates page shows the agent's version and state; Update reaches POST /update with
	// the control token; the agent checks the signed release on (fake) GitHub, stages it and starts its installer.
	st, page := a.do("GET", "/api/admin/updates", nil)
	row := func(page map[string]any) map[string]any {
		for _, x := range page["agents"].([]any) {
			if x.(map[string]any)["id"] == "srv-tower" {
				return x.(map[string]any)
			}
		}
		return nil
	}
	if r := row(page); st != 200 || r["version"] != "contract" || r["agent_enabled"] != true || r["can_update"] != true ||
		r["self_update"].(map[string]any)["state"] != "idle" {
		t.Fatal(st, page)
	}
	if st, res := a.do("POST", "/api/admin/updates/srv-tower", map[string]any{}); st != 200 || res["ok"] != true ||
		res["result"].(map[string]any)["state"] != "downloading" {
		t.Fatal(st, res)
	}
	deadline := time.Now().Add(10 * time.Second)
	for c.applied.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if c.applied.Load() != 1 {
		t.Fatal("the agent did not start its installer")
	}
	time.Sleep(300 * time.Millisecond)
	now = now.Add(2 * time.Second)
	p.PollOnce(ctx)
	_, page = a.do("GET", "/api/admin/updates", nil)
	if r := row(page); r["self_update"].(map[string]any)["target"] != "42-1a2b3c4" {
		t.Fatal(page)
	}
	if st, res := a.do("POST", "/api/admin/updates/sleepy", map[string]any{}); st != 404 {
		t.Fatal("update of an agent without self_update:", st, res)
	}
}

// A read-only agent (the default config): processes off is shown as "disabled by host", the panel's fan and
// power calls get the agent's "disabled" answers, never a wrong-token error.
func TestPanelContractReadOnlyAgent(t *testing.T) {
	cfg, err := agent.Parse([]byte("processes: {enabled: false}\n"), filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	a := agent.New(cfg, agent.Tokens{Read: readTok}, agent.Deps{Collector: agenttest.NewCollector(), Version: "ro"})
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	pcfg := panelConfig(t, srv.URL)
	p, err := poller.New(pcfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	ag, _ := p.AgentByID("srv-tower")
	pl, err := p.FetchProcesses(context.Background(), ag, "srv-tower", 25, "cpu")
	if err != nil || !pl.DisabledByHost {
		t.Fatal(pl, err)
	}
	for _, x := range p.Snapshot() {
		if x.ID == "srv-tower" && (x.Status != "online" || x.ProcessesAvailable || x.FanControl) {
			t.Fatalf("%+v", x)
		}
	}
	st, body, err := p.AgentRequest(context.Background(), ag, "POST", "/power", map[string]any{"action": "shutdown", "delay_s": 60})
	if err != nil || st != 404 || body["code"] != "power_control_disabled" {
		t.Fatal(st, body, err)
	}
	st, body, _ = p.AgentRequest(context.Background(), ag, "GET", "/fan/status", nil)
	if st != 404 {
		t.Fatal(st, body)
	}
	// no elevated helper (Linux, or Windows without it): the agent's answer the panel maps to "not installed"
	st, body, _ = p.AgentRequest(context.Background(), ag, "POST", agentapi.PathHelperStop, map[string]any{})
	if st != 409 || body["error"] != agentapi.ErrHelperNotInstalled.Error() {
		t.Fatal(st, body)
	}
}

// The Windows elevated helper (Fairy-agent v0.5.0) end to end: the real agent handler with agenttest's fake
// helper, the panel's poller and web API. Stop through the panel, readings and power actions wait for it,
// Start again.
func TestPanelContractHelper(t *testing.T) {
	c := startAgent(t)
	cfg := panelConfig(t, c.agentSrv.URL)
	p, err := poller.New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sys := func() model.System {
		time.Sleep(300 * time.Millisecond) // the agent computes /metrics at most every 250 ms
		p.PollOnce(ctx)
		for _, x := range p.Snapshot() {
			if x.ID == "srv-tower" {
				return x
			}
		}
		t.Fatal("srv-tower missing")
		return model.System{}
	}
	s := sys()
	if h := s.Helper; h == nil || h.State != agentapi.HelperRunning || !h.Needed || h.Version != "0.5.0" || !h.Watchdog ||
		fmt.Sprint(h.Features) != "[coretemp ipmi fans power]" || len(h.Paused) != 0 || s.PowerHelperReason != "" {
		t.Fatalf("running: %+v", s.Helper)
	}
	srv := web.New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, p, nil, "test")
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	tok, _, err := srv.Tokens.Create("contract", "system", "control", "test", 0)
	if err != nil {
		t.Fatal(err)
	}
	a := api{t, hs, tok}
	// Helper stop / start need a browser sign-in: an API token is refused there (it cannot "confirm it's you").
	if st, b := a.do("POST", "/api/systems/srv-tower/helper/stop", map[string]any{"confirm": "srv-tower"}); st != 403 || c.helper.Stops != 0 {
		t.Fatal("an API token must not stop the helper", st, b)
	}
	admin := newBrowser(t, cfg, p)
	if st, b := admin.do("POST", "/api/systems/srv-tower/helper/stop", map[string]any{"confirm": "srv-tower"}); st != 200 || b["ok"] != true {
		t.Fatal("stop", st, b)
	}
	if c.helper.Stops != 1 {
		t.Fatal("the agent's helper was not stopped", c.helper.Stops)
	}
	s = sys()
	// while it ran this host had BMC sensors and fan control (no Core Temp on the fake PC): those are paused
	if h := s.Helper; h.State != agentapi.HelperStopped || fmt.Sprint(h.Paused) != "[ipmi fans]" || s.PowerHelperReason != agentapi.ReasonHelperStopped ||
		!s.FanControl {
		t.Fatalf("stopped: %+v power %q", s.Helper, s.PowerHelperReason)
	}
	st, caps := a.do("GET", "/api/systems/srv-tower/power", nil)
	for _, x := range caps["actions"].([]any) {
		if m := x.(map[string]any); m["action"] == "shutdown" && (m["available"] != false || !strings.Contains(fmt.Sprint(m["reason"]), "start the helper first")) {
			t.Fatal("power while stopped", st, m)
		}
	}
	if st, b := a.do("GET", "/api/systems/srv-tower/fan", nil); st != 200 || b["available"] != false {
		t.Fatal("fan status while stopped", st, b)
	}
	if st, b := admin.do("POST", "/api/systems/srv-tower/helper/start", map[string]any{"confirm": "SRV-TOWER"}); st != 200 || b["state"] != agentapi.HelperRunning {
		t.Fatal("start", st, b)
	}
	if s := sys(); c.helper.Starts != 1 || s.Helper.State != agentapi.HelperRunning || len(s.Helper.Paused) != 0 || s.PowerHelperReason != "" {
		t.Fatalf("restarted: %+v", s.Helper)
	}
}

// browser is a signed-in admin in a browser, for the control actions an API token may not run (helper stop and
// start): a demo-mode panel on the same poller, whose simulated Pocket ID sign-in confirms the action the panel
// kept. do answers with the action's own status and body when it ran at once; when the panel first asked to
// "confirm it's you", it confirms and answers with the kept action's result ({"ok", "message"} and its status).
type browser struct {
	t   *testing.T
	srv *httptest.Server
	c   *http.Client
}

func newBrowser(t *testing.T, cfg *config.Panel, p *poller.Poller) browser {
	u := demo.User
	ws := web.New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48), InsecureCookies: true}, p, &u, "test")
	if _, _, err := ws.Access.Seen(u.Sub, model.Deref(u.Email), model.Deref(u.Name), "admin", "contract test"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(ws.Handler())
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return browser{t, srv, &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (b browser) call(method, path string, body any) (int, map[string]any) {
	b.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, b.srv.URL+path, rd)
	req.Header.Set("X-Requested-With", "fairy")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(raw) > 0 && raw[0] == '{' {
		_ = json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, out
}

func (b browser) do(method, path string, body any) (int, map[string]any) {
	b.t.Helper()
	st, out := b.call(method, path, body)
	d, _ := out["detail"].(map[string]any)
	if st != 401 || d["code"] != "step_up_required" {
		return st, out
	}
	id, _ := d["pending_id"].(string)
	// "Continue" in the "Confirm it's you" dialog (demo mode: the sign-in is simulated and the action runs)
	if st, out := b.call("POST", "/api/me/step-up", map[string]any{"pending_id": id, "next": "/"}); st != 200 {
		b.t.Fatal("step-up", st, out)
	}
	_, me := b.call("GET", "/api/me", nil)
	res, _ := me["pending_result"].(map[string]any)
	if res == nil {
		b.t.Fatal("the confirmed action did not run: no pending_result in /api/me", me)
	}
	code, _ := res["status"].(float64)
	return int(code), res
}

// keys returns the key set of a JSON object.
func keys(m map[string]any) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}

// The demo's simulated agents (fairy serve --demo) send only what fairy-agent sends: every key of every demo
// payload, and of its features / power / power_control blocks, exists in the real agent's /metrics.
func TestDemoAgentsMatchTheRealAgent(t *testing.T) {
	c := startAgent(t)
	fetch := func() map[string]any {
		req, _ := http.NewRequest("GET", c.agentSrv.URL+"/metrics", nil)
		req.Header.Set("Authorization", "Bearer "+readTok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return m
	}
	real := fetch()
	// and with the Windows helper stopped: the "unavailable" list and power_control's available / reason
	if _, err := c.helper.Stop(context.Background(), "contract"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // the agent computes /metrics at most every 250 ms
	stopped := fetch()
	for k, v := range stopped {
		if _, ok := real[k]; !ok {
			real[k] = v
		}
	}
	for k, v := range stopped["power_control"].(map[string]any) {
		real["power_control"].(map[string]any)[k] = v
	}
	if stopped["unavailable"] == nil || real["helper"] == nil {
		t.Fatal("the fake agent has no helper", stopped)
	}
	real["hostname"] = "x"  // the fake has every optional block except load_avg (Linux only) and battery
	var batt map[string]any // the battery block's keys (the fake PC is a desktop)
	bb, _ := json.Marshal(collect.BatteryStatus{})
	_ = json.Unmarshal(bb, &batt)
	real["battery"] = batt
	if pc, ok := real["power_control"].(map[string]any); ok {
		pc["sleep_note"] = "" // only sent when the machine cannot sleep (the fake one can)
	}
	sub := func(name string, demo, real map[string]any) {
		rk := keys(real)
		for k := range demo {
			if !rk[k] && k != "load_avg" {
				t.Errorf("%s: demo sends %q, fairy-agent does not", name, k)
			}
		}
	}
	dcfg, err := demo.Config(t.TempDir(), "http://127.0.0.1:8090")
	if err != nil {
		t.Fatal(err)
	}
	b := demo.NewBackend(dcfg)
	seen := 0
	for _, ag := range dcfg.Agents {
		m, err := b.FetchAgentMetrics(context.Background(), ag, time.Second)
		if err != nil {
			continue // simulated offline host
		}
		seen++
		sub(ag.ID, m, real)
		for _, blk := range []string{"features", "power", "power_control", "platform", "cpu", "memory", "net", "hardware", "battery", "helper"} {
			if d, ok := m[blk].(map[string]any); ok {
				r, _ := real[blk].(map[string]any)
				sub(ag.ID+"."+blk, d, r)
			}
		}
		for _, u := range jsonx.Objs(m["unavailable"]) {
			sub(ag.ID+".unavailable[]", u, jsonx.Objs(real["unavailable"])[0])
		}
		if src, ok := m["power"].(map[string]any); ok {
			for _, s := range src["source"].([]any) {
				if s != "gpu" && s != "rapl" && s != "battery" {
					t.Errorf("%s: power source %v is not one fairy-agent has", ag.ID, s)
				}
			}
		}
	}
	if seen < 5 {
		t.Fatal("demo agents online:", seen)
	}
}
