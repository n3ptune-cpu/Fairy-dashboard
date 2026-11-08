// Package demo: a realistic simulator for Proxmox, agents, power sources, UPSes and a Supermicro BMC.
//
// The simulator produces *raw* payloads (Proxmox cluster/resources JSON, agent /metrics JSON) and feeds them
// through the real normalisation, power-merge and API code. Authentication is bypassed with a fake user, so the
// server refuses to serve demo mode on anything but a loopback socket. NEVER expose a demo instance.
package demo

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	mrand "math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/config"
	"github.com/n3ptune-cpu/Fairy/internal/devices"
	"github.com/n3ptune-cpu/Fairy/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy/internal/model"
	"github.com/n3ptune-cpu/Fairy/internal/poller"
	"github.com/n3ptune-cpu/Fairy/internal/power"
	"github.com/n3ptune-cpu/Fairy/internal/redact"
	"github.com/n3ptune-cpu/Fairy/internal/ups"
)

const GiB = 1 << 30

var T0 = time.Now()

func since() float64 { return time.Since(T0).Seconds() }
func epoch() float64 { return float64(time.Now().UnixNano()) / 1e9 }

var User = model.User{Sub: "demo-neptune", Name: model.P("Neptune"), PreferredUsername: model.P("neptune"),
	Email: model.P("neptune@example.invalid"), Groups: []string{"viewers", "controllers"}, Auth: "demo"}

func uniform(a, b float64) float64 { return a + (b-a)*mrand.Float64() }

func r1(v float64) float64 { return math.Round(v*10) / 10 }
func r2(v float64) float64 { return math.Round(v*100) / 100 }

// wave: a smooth drifting value in [lo, hi] plus a little noise.
func wave(period, lo, hi, phase float64) float64 {
	x := (math.Sin(2*math.Pi*since()/period+phase) + 1) / 2
	v := lo + (hi-lo)*x
	return math.Max(lo*0.9, v*(1+uniform(-0.03, 0.03)))
}

// llmLoad: 0..1 'generating' factor for the llama.cpp box: long busy stretches with short idle dips.
func llmLoad() float64 {
	t := math.Mod(since(), 47)
	busy := 1.0
	if t >= 30 && t < 37 { // a 7 s idle gap every 47 s (between requests)
		busy = 0.04
	}
	return busy * uniform(0.93, 1.0)
}

// ---------------------------------------------------------------- simulated power plugs ("demo" type)
type demoPower struct {
	power.Defaults
	b                            power.Base
	baseW, swingW, period, volts float64
	profile                      string
}

func (d *demoPower) Info() power.Base         { return d.b }
func (d *demoPower) DefaultInterval() float64 { return 5 } // behaves like a Tuya plug
func (d *demoPower) FloorInterval() float64   { return 3 }
func (d *demoPower) Read(context.Context, time.Duration) ([]model.PowerReading, error) {
	var w float64
	if d.profile == "llm" {
		w = r1(d.baseW + d.swingW*llmLoad() + uniform(-8, 8))
	} else {
		w = r1(wave(d.period, d.baseW, d.baseW+d.swingW, float64(len(d.b.ID))))
	}
	v := r1(d.volts + uniform(-2, 2))
	a := math.Round(w/v*1000) / 1000
	e := math.Round((1234.5+since()*w/3.6e6)*1000) / 1000
	return []model.PowerReading{power.Reading(d.b, devices.Reading{Watts: &w, Voltage: &v, Current: &a, EnergyKWh: &e})}, nil
}

func num(raw map[string]any, k string, def float64) float64 {
	if v, ok := raw[k]; ok {
		switch x := v.(type) {
		case int:
			return float64(x)
		case float64:
			return x
		}
	}
	return def
}

const outagePeriod, outageStart, outageLen = 150.0, 90.0, 40.0 // Straw's UPS: 40 s on battery every 150 s

func f0(v float64) string { return strconv.FormatFloat(v, 'f', 0, 64) }
func f1(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

// cyberpowerVars: CP1500EPFCLCD-style usbhid-ups variables (no ups.realpower, like many CyberPower units).
func cyberpowerVars() map[string]string {
	t := math.Mod(since(), outagePeriod)
	load := math.Round(wave(40, 22, 28, 0))
	onBatt := t >= outageStart && t < outageStart+outageLen
	charge := 100.0
	if onBatt {
		charge = 100 - 0.2*(t-outageStart)
	} else if t >= outageStart+outageLen {
		charge = math.Min(100, 92+0.4*(t-outageStart-outageLen))
	}
	status := "OL"
	if onBatt {
		status = "OB DISCHRG"
	} else if charge < 100 {
		status = "OL CHRG"
	}
	bv, in, out := 27.1, f1(wave(60, 229, 233, 0)), f1(wave(60, 229, 233, 0))
	if onBatt {
		bv, in, out = 23.6-(100-charge)*0.05, "0.0", "230.0"
	}
	v := map[string]string{"ups.status": status, "battery.charge": f0(charge),
		"battery.runtime": strconv.Itoa(int(2100 * charge / 100 * 25 / load)), "battery.voltage": f1(bv),
		"ups.load": f0(load), "ups.realpower.nominal": "900", "input.voltage": in, "output.voltage": out,
		"ups.model": "CP1500EPFCLCD", "ups.mfr": "CPS", "ups.serial": "CRBDEMO0000123",
		"battery.mfr.date": "2023/05/14", "ups.test.result": "Done and passed", "device.type": "ups",
		"driver.name": "usbhid-ups"}
	if onBatt {
		v["input.transfer.reason"] = "input voltage out of range"
	}
	return v
}

// rackUPS: backend-polled rack UPS that reports a measured ups.realpower (APC-style).
func rackUPS(context.Context, time.Duration) ([]map[string]any, error) {
	load := math.Round(wave(200, 28, 36, 0))
	return []map[string]any{ups.RowMap(devices.UPSFromVars("rack", map[string]string{
		"ups.status": "OL", "battery.charge": "100", "battery.runtime": strconv.Itoa(int(1900 * 30 / load)),
		"battery.voltage": "27.3", "ups.load": f0(load), "ups.realpower": f0(load * 10),
		"ups.realpower.nominal": "1000", "input.voltage": f1(wave(60, 229, 233, 0)), "output.voltage": "230.0",
		"input.frequency": "50.0", "ups.model": "Smart-UPS 1500", "ups.mfr": "American Power Conversion",
		"ups.serial": "AS1234DEMO", "battery.date": "2024-02-10", "ups.test.result": "No test initiated"}, ""))}, nil
}

var registerOnce sync.Once

func register() {
	registerOnce.Do(func() {
		power.Extra["demo"] = func(raw map[string]any) (power.Source, error) {
			b, err := power.NewBase(raw)
			if err != nil {
				return nil, err
			}
			p, _ := raw["profile"].(string)
			return &demoPower{b: b, baseW: num(raw, "base_w", 100), swingW: num(raw, "swing_w", 10),
				period: num(raw, "period_s", 90), volts: num(raw, "voltage", 230), profile: p}, nil
		}
		ups.Extra["demo"] = func(raw map[string]any) (*ups.Source, error) {
			id, _ := raw["id"].(string)
			name, _ := raw["name"].(string)
			p, err := ups.ParsePowers(id, raw["powers"])
			if err != nil {
				return nil, err
			}
			return &ups.Source{ID: id, Name: name, Powers: p, Read: rackUPS}, nil
		}
	})
}

// ---------------------------------------------------------------- config
const configYAML = `
public_url: %q
offline_after: 30
enable_docs: true
audit_log: %q
data_dir: %q
oidc: {issuer: "https://id.example.invalid", client_id: demo, control_groups: [controllers]}
proxmox:
  - {id: teyvat, id_prefix: "", cluster_name: NSX-TEYVAT, urls: ["https://nsx-kachina.invalid:8006"],
     token_id: "fairy@pve!panel", token_secret_env: DEMO_PVE_TOKEN,
     # separate token with only the FairyPower role (VM.PowerMgmt) -> guest start/shutdown/reboot/stop
     power_token_id: "fairy@pve!power", power_token_secret_env: DEMO_PVE_POWER_TOKEN}
  # Straw's standalone PVE host, reached over Tailscale node sharing with a read-only token he gave Neptune
  - {id: straw, cluster_name: ASUS-i7, standalone: true, group: Straw, owner: straw,
     urls: ["https://asus-i7.straws-tailnet.invalid:8006"], token_id: "fairy@pve!panel", token_secret_env: DEMO_STRAW_PVE_TOKEN}
agents:
  - {id: nsx-cyrene, name: NSX-CYRENE, url: "http://100.64.0.10:9101", token_env: DEMO_TOKEN, group: Neptune, owner: neptune,
     tags: [server, supermicro], dwservice_url: "https://www.dwservice.net/en/login.html?loginUser=demo-cyrene",
     fan_control: {vendor: supermicro, control_token_env: DEMO_CONTROL_TOKEN}, power_control: true}
  - {id: neptune-laptop, name: "ASUS Vivobook 16X", url: "http://100.64.0.11:9101", token_env: DEMO_TOKEN,
     group: Neptune, owner: neptune, tags: [laptop], power_control: true, control_token_env: DEMO_CONTROL_TOKEN,
     dwservice_url: "https://www.dwservice.net/en/login.html?loginUser=demo-laptop"}
  - {id: llm-box, name: llm-box, url: "http://100.64.0.12:9101", token_env: DEMO_TOKEN, group: Neptune, owner: neptune, tags: [llm]}
  # agent on the NSX-KACHINA node itself (matched by node name): adds board model, temps, power control
  - {id: kachina-agent, name: "NSX-KACHINA agent", url: "http://100.64.0.5:9101", token_env: DEMO_TOKEN,
     node: NSX-KACHINA, power_control: true, wol_relay: true, control_token_env: DEMO_CONTROL_TOKEN}
  - {id: straw-pc, name: "Straw's Ryzen PC", url: "http://100.64.0.20:9101", token_env: DEMO_TOKEN,
     group: Straw, owner: straw, tags: [friend, gaming], power_links: {desk-plug: straw-pc}}
  - {id: asus-i7-agent, name: "ASUS-i7 agent", url: "http://100.64.0.22:9101", token_env: DEMO_TOKEN,
     group: Straw, owner: straw, links_to: straw-node-asus-i7,
     ups_powers: {cyberpower: [straw-node-asus-i7, straw-pc]}, ups_names: {cyberpower: "Straw's CyberPower"},
     wol_relay: true, control_token_env: DEMO_CONTROL_TOKEN}
  - {id: straw-laptop, name: "Straw's laptop", url: "http://100.64.0.21:9101", token_env: DEMO_TOKEN,
     group: Straw, owner: straw, tags: [friend, laptop]}
systems:
  - {id: qemu-100, dwservice_url: "https://www.dwservice.net/en/login.html?loginUser=demo-win11"}
  - {id: lxc-101, tags: [games]}
  - {id: straw-laptop, wol: {mac: "a8:5e:45:12:34:56", relay: asus-i7-agent, broadcast: 192.168.1.255}}
  - {id: nsx-cyrene, wol: {mac: "3c:ec:ef:00:11:22", relay: kachina-agent, broadcast: 192.168.10.255}}
panel_hosts: [node-nsx-kachina, lxc-110]
ups:
  - {id: rack, type: demo, name: "Rack UPS · APC Smart-UPS 1500", powers: [node-nsx-fontaine, node-nsx-kachina, node-nsx-snezhnaya]}
power_sources:
  - {id: plug-cyrene, type: demo, name: "Tuya plug · NSX-CYRENE", system: nsx-cyrene, base_w: 95, swing_w: 140, period_s: 120}
  - {id: plug-llm-box, type: demo, name: "Tuya plug · llm-box", system: llm-box, base_w: 190, swing_w: 665, profile: llm}
  - {id: ups-rack, type: demo, name: "Rack UPS (NUT)", base_w: 210, swing_w: 60, period_s: 200}
`

func randTok() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Config builds the demo configuration (data under dir).
func Config(dir, publicURL string) (*config.Panel, error) {
	for k, v := range map[string]string{"DEMO_TOKEN": randTok(), "DEMO_CONTROL_TOKEN": randTok(),
		"DEMO_PVE_TOKEN": "demo-not-a-real-token", "DEMO_STRAW_PVE_TOKEN": "demo-not-a-real-token",
		"DEMO_PVE_POWER_TOKEN": "demo-not-a-real-token"} {
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
	return config.Parse([]byte(fmt.Sprintf(configYAML, publicURL, filepath.Join(dir, "demo-audit.log"), filepath.Join(dir, "data"))))
}

// ---------------------------------------------------------------- simulated BMC
type simBMC struct {
	mu     sync.Mutex
	mode   string
	duty   map[int]int
	events []map[string]any
}

var modeRPM = map[string]float64{"standard": 900, "full": 1900, "optimal": 420, "heavyio": 1200}

func (b *simBMC) status() map[string]any {
	rpm := func(zone int, base float64) float64 {
		target := modeRPM[b.mode]
		if d, ok := b.duty[zone]; ok && b.mode == "full" {
			target = float64(d) / 100 * 1900
		}
		v := math.Round(target*base*uniform(0.97, 1.03)/100) * 100
		if v == 0 {
			v = 100
		}
		return v
	}
	duty := map[string]any{}
	for k, v := range b.duty {
		duty[strconv.Itoa(k)] = v
	}
	ev := b.events
	if len(ev) > 10 {
		ev = ev[len(ev)-10:]
	}
	return map[string]any{"enabled": true, "vendor": "supermicro", "mode": b.mode, "error": nil,
		"overridden": b.mode != "optimal", "duty": duty, "min_duty": 30, "max_temp_c": 80.0, "events": ev,
		"fans": []any{map[string]any{"name": "CPU_FAN1", "rpm": rpm(0, 1.0)}, map[string]any{"name": "CPU_FAN2", "rpm": rpm(0, 0.95)},
			map[string]any{"name": "SYS_FAN3", "rpm": rpm(0, 1.1)}}}
}

func (b *simBMC) handle(path string, body map[string]any) (int, jsonx.M) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch path {
	case "/fan/status":
		return 200, b.status()
	case "/fan/mode":
		mode, _ := body["mode"].(string)
		if _, ok := modeRPM[mode]; !ok {
			return 400, jsonx.M{"error": "bad mode"}
		}
		b.mode, b.duty = mode, map[int]int{}
		b.events = append(b.events, map[string]any{"t": epoch(), "action": "mode=" + mode, "detail": "api"})
		return 200, jsonx.M{"mode": mode}
	case "/fan/duty":
		zone, _ := body["zone"].(int)
		pct, ok := body["percent"].(int)
		if b.mode != "full" {
			return 409, jsonx.M{"error": fmt.Sprintf("fan mode is %s; set mode 'full' first (duty is ignored otherwise)", b.mode)}
		}
		if !ok || pct < 30 || pct > 100 {
			return 400, jsonx.M{"error": "percent must be an integer between 30 and 100"}
		}
		b.duty[zone] = pct
		b.events = append(b.events, map[string]any{"t": epoch(), "action": fmt.Sprintf("duty zone %d=%d%%", zone, pct), "detail": "api"})
		return 200, jsonx.M{"zone": zone, "percent": pct}
	}
	return 404, jsonx.M{"error": "not found"}
}

// ---------------------------------------------------------------- Proxmox resources
func guest(vmid int, kind, name, node string, cores int, memGiB, diskGiB, cpuLo, cpuHi float64, status, tags string) jsonx.M {
	running := status == "running"
	t := since()
	v := float64(vmid)
	r := jsonx.M{"id": fmt.Sprintf("%s/%d", kind, vmid), "type": kind, "vmid": v, "name": name, "node": node,
		"status": status, "maxcpu": float64(cores), "maxmem": float64(int64(memGiB * GiB)),
		"disk": float64(int64(diskGiB * 0.42 * GiB)), "maxdisk": float64(int64(diskGiB * GiB)), "tags": tags, "template": 0.0,
		"cpu": 0.0, "mem": 0.0, "netin": 0.0, "netout": 0.0, "uptime": 0.0}
	if running {
		r["cpu"] = wave(60+float64(vmid%17), cpuLo, cpuHi, v)
		r["mem"] = float64(int64(wave(300, 0.35, 0.8, v) * memGiB * GiB))
		r["netin"] = float64(int64(v*1e6 + t*wave(45, 2e4, 4e6, v)))
		r["netout"] = float64(int64(v*5e5 + t*wave(50, 1e4, 1.5e6, v+1)))
		r["uptime"] = float64(int64(86400*3 + t))
	}
	return r
}

type nodeSpec struct {
	name   string
	cores  int
	mem    float64
	lo, hi float64
}

var mainNodes = []nodeSpec{{"NSX-FONTAINE", 16, 64, 0.18, 0.55}, {"NSX-KACHINA", 8, 32, 0.05, 0.2}, {"NSX-SNEZHNAYA", 12, 64, 0.1, 0.35}}

func proxmoxResources() []jsonx.M {
	t := since()
	var out []jsonx.M
	for i, n := range mainNodes {
		out = append(out, jsonx.M{"id": "node/" + n.name, "type": "node", "node": n.name, "status": "online",
			"cpu": wave(80, n.lo, n.hi, float64(i)), "maxcpu": float64(n.cores),
			"mem": float64(int64(wave(400, 0.4, 0.7, float64(i)) * n.mem * GiB)), "maxmem": n.mem * GiB,
			"disk": float64(38 * GiB), "maxdisk": float64(100 * GiB), "uptime": float64(int64(86400*12 + t))})
	}
	out = append(out,
		guest(100, "qemu", "win11-gaming", "NSX-FONTAINE", 8, 16, 256, 0.05, 0.6, "running", "windows;vm"),
		guest(101, "lxc", "minecraft", "NSX-FONTAINE", 4, 8, 32, 0.25, 0.85, "running", "games"),
		guest(102, "lxc", "jellyfin", "NSX-FONTAINE", 4, 4, 16, 0.02, 0.4, "running", "media"),
		guest(110, "lxc", "fairy", "NSX-KACHINA", 1, 0.5, 4, 0.01, 0.04, "running", "infra"),
		guest(111, "lxc", "pocket-id", "NSX-KACHINA", 1, 0.5, 4, 0.0, 0.02, "running", "infra;auth"),
		guest(120, "qemu", "truenas", "NSX-SNEZHNAYA", 4, 16, 32, 0.03, 0.15, "running", "storage"),
		guest(121, "qemu", "home-assistant", "NSX-SNEZHNAYA", 2, 4, 32, 0.02, 0.1, "running", "iot"),
		guest(122, "lxc", "backup-runner", "NSX-SNEZHNAYA", 2, 2, 8, 0, 0, "stopped", "backup"),
		jsonx.M{"id": "storage/NSX-KACHINA/local", "type": "storage", "node": "NSX-KACHINA", "status": "available"})
	names := []string{"nginx", "postgres", "redis", "grafana", "loki", "gitea", "vaultwarden", "immich", "paperless", "adguard",
		"uptime-kuma", "nextcloud", "syncthing", "frigate", "mqtt", "zigbee2mqtt", "minio", "authelia", "ntfy", "n8n"}
	extra, _ := strconv.Atoi(os.Getenv("FAIRY_DEMO_EXTRA")) // stress the UI with many guests
	for i := 0; i < extra; i++ {
		st := "running"
		if i%7 == 6 {
			st = "stopped"
		}
		out = append(out, guest(200+i, "lxc", fmt.Sprintf("%s-%d", names[i%len(names)], i/len(names)+1),
			mainNodes[i%3].name, 2, 2, 8, 0.01, 0.3, st, ""))
	}
	return out
}

func strawResources() []jsonx.M {
	t := since()
	return []jsonx.M{
		{"id": "node/ASUS-i7", "type": "node", "node": "ASUS-i7", "status": "online", "cpu": wave(55, 0.08, 0.3, 4),
			"maxcpu": 8.0, "mem": float64(int64(wave(250, 14, 19, 0) * GiB)), "maxmem": float64(32 * GiB),
			"disk": float64(61 * GiB), "maxdisk": float64(220 * GiB), "uptime": float64(int64(86400*2 + t))},
		guest(100, "lxc", "pihole", "ASUS-i7", 1, 1, 8, 0.01, 0.06, "running", "dns"),
		guest(101, "lxc", "plex", "ASUS-i7", 4, 4, 32, 0.05, 0.35, "running", "media"),
		guest(102, "qemu", "win10-steam", "ASUS-i7", 4, 8, 128, 0, 0, "stopped", "games"),
	}
}

var nodeCPU = map[string]string{"NSX-FONTAINE": "AMD Ryzen 9 5950X 16-Core Processor",
	"NSX-KACHINA": "Intel(R) Core(TM) i7-8700T CPU @ 2.40GHz", "NSX-SNEZHNAYA": "AMD Ryzen 7 5700G with Radeon Graphics",
	"ASUS-i7": "Intel(R) Core(TM) i7-4790K CPU @ 4.00GHz"}

// ---------------------------------------------------------------- backend
type guestKey struct {
	cluster string
	vmid    int
}

type guestChange struct {
	at     time.Time
	status string
}

// Backend implements poller.Backend with simulated sources. Power actions change the simulation: guests
// stop/start, a host that shuts down goes offline until it is woken (WoL), a reboot takes it offline ~25 s.
type Backend struct {
	cfg          *config.Panel
	bmc          *simBMC
	mu           sync.Mutex
	pending      map[string]map[string]any // agent id -> {action, at, delay_s, requested_by}
	downUntil    map[string]float64        // agent id -> epoch it comes back (+Inf = until woken)
	guestStatus  map[guestKey]string
	guestPending map[guestKey]guestChange
}

func NewBackend(cfg *config.Panel) *Backend {
	return &Backend{cfg: cfg, bmc: &simBMC{mode: "optimal", duty: map[int]int{}}, pending: map[string]map[string]any{},
		downUntil: map[string]float64{}, guestStatus: map[guestKey]string{}, guestPending: map[guestKey]guestChange{}}
}

func (b *Backend) FetchProxmox(_ context.Context, pve config.Proxmox, _ time.Duration) ([]jsonx.M, error) {
	rows := proxmoxResources()
	if pve.ID == "straw" {
		rows = strawResources()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	for k, c := range b.guestPending {
		if !now.Before(c.at) {
			b.guestStatus[k] = c.status
			delete(b.guestPending, k)
		}
	}
	for _, r := range rows {
		typ := jsonx.S(r, "type")
		v := jsonx.Num(r, "vmid")
		if v == nil || (typ != "qemu" && typ != "lxc") {
			continue
		}
		if st, ok := b.guestStatus[guestKey{pve.ID, int(*v)}]; ok {
			r["status"] = st
			if st == "stopped" {
				for _, k := range []string{"cpu", "mem", "uptime", "netin", "netout"} {
					r[k] = 0.0
				}
			}
		}
	}
	return rows, nil
}

func (b *Backend) FetchNodeStatus(_ context.Context, _ config.Proxmox, node string) (jsonx.M, error) {
	m, ok := nodeCPU[node]
	if !ok {
		m = "Unknown CPU"
	}
	return jsonx.M{"cpuinfo": map[string]any{"model": m, "cpus": 8.0, "sockets": 1.0}}, nil
}

func (b *Backend) FetchAgentMetrics(_ context.Context, a config.Agent, _ time.Duration) (jsonx.M, error) {
	b.mu.Lock()
	now := epoch()
	if p := b.pending[a.ID]; p != nil && now >= p["at"].(float64) { // the scheduled shutdown/reboot "happens"
		if p["action"] == "shutdown" {
			b.downUntil[a.ID] = math.Inf(1)
		} else {
			b.downUntil[a.ID] = now + 25
		}
		delete(b.pending, a.ID)
	}
	if b.downUntil[a.ID] > now {
		b.mu.Unlock()
		return nil, fmt.Errorf("simulated: host is powered off: %w", context.DeadlineExceeded)
	}
	delete(b.downUntil, a.ID)
	pend := b.pending[a.ID]
	b.mu.Unlock()
	m, err := agentMetrics(a.ID)
	if err != nil {
		return nil, err
	}
	feat := jsonx.Obj(m, "features")
	if a.PowerControl {
		feat["power_control"] = true
		var p any
		if pend != nil {
			p = pend
		}
		m["power_control"] = map[string]any{"enabled": true, "pending": p, "min_delay_s": 10.0}
	}
	feat["wol_relay"] = a.WolRelay
	// round-trip through JSON so the poller sees exactly what a real agent would send
	raw, _ := json.Marshal(m)
	var out jsonx.M
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

func (b *Backend) PVEPower(_ context.Context, clusterID, path string, _ url.Values) (any, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/") // nodes/{node}/{kind}/{vmid}/status/{action}
	if len(parts) == 6 {
		vmid, _ := strconv.Atoi(parts[3])
		action := parts[5]
		nw := "stopped"
		if action == "start" || action == "reboot" {
			nw = "running"
		}
		delay := map[string]int{"start": 3, "reboot": 6, "shutdown": 8, "stop": 1}[action]
		k := guestKey{clusterID, vmid}
		b.mu.Lock()
		if action == "reboot" {
			b.guestStatus[k] = "stopped"
		}
		b.guestPending[k] = guestChange{time.Now().Add(time.Duration(delay) * time.Second), nw}
		b.mu.Unlock()
		return "UPID:demo:" + action, nil
	}
	return "UPID:demo:node", nil
}

func (b *Backend) FetchProcesses(_ context.Context, a config.Agent, sid string, limit int, sortBy string) (model.ProcessList, error) {
	switch a.ID {
	case "straw-pc":
		return model.ProcessList{SystemID: sid, DisabledByHost: true, Sort: sortBy, Processes: []model.Process{}}, nil
	case "straw-laptop":
		return model.ProcessList{}, fmt.Errorf("simulated: %w", context.DeadlineExceeded)
	}
	return poller.DecodeProcessList(demoProcesses(a.ID, limit, sortBy), sid)
}

var reqClean = regexp.MustCompile(`[^A-Za-z0-9 ._@-]`)

func (b *Backend) AgentRequest(_ context.Context, a config.Agent, _ string, path string, body any) (int, jsonx.M, error) {
	bm := map[string]any{}
	if body != nil {
		raw, _ := json.Marshal(body)
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.UseNumber()
		_ = dec.Decode(&bm)
		for k, v := range bm { // integers as int (like JSON decoding on a real agent would validate)
			if n, ok := v.(json.Number); ok {
				if i, err := n.Int64(); err == nil {
					bm[k] = int(i)
				} else if f, err := n.Float64(); err == nil {
					bm[k] = f
				}
			}
		}
	}
	switch path {
	case "/power":
		return b.simPower(a, bm)
	case "/wol":
		mac, _ := bm["mac"].(string)
		for _, o := range b.cfg.Systems {
			if o.Wol != nil && o.Wol.MAC == mac {
				for _, ag := range b.cfg.Agents {
					if ag.ID == o.ID {
						b.mu.Lock()
						b.downUntil[o.ID] = epoch() + 20 // boots in ~20 s
						b.mu.Unlock()
					}
				}
			}
		}
		port := bm["port"]
		if port == nil {
			port = 9
		}
		return 200, jsonx.M{"sent": true, "mac": mac, "broadcast": bm["broadcast"], "port": port, "bytes": 102}, nil
	}
	st, out := b.bmc.handle(path, bm)
	return st, out, nil
}

func (b *Backend) simPower(a config.Agent, body map[string]any) (int, jsonx.M, error) {
	action, _ := body["action"].(string)
	if !a.PowerControl {
		return 404, jsonx.M{"error": "power control disabled on this host", "code": "power_control_disabled"}, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if action == "cancel" {
		delete(b.pending, a.ID)
		return 200, jsonx.M{"action": "cancel", "pending": nil}, nil
	}
	delay, _ := body["delay_s"].(int)
	if delay == 0 {
		delay = 60
	}
	used := delay
	if a.ID != "nsx-cyrene" && a.ID != "neptune-laptop" { // Windows vs Linux (whole minutes)
		used = (delay + 59) / 60 * 60
	}
	who := fmt.Sprint(body["requested_by"])
	who = reqClean.ReplaceAllString(who, "")
	if len(who) > 64 {
		who = who[:64]
	}
	if who == "" || body["requested_by"] == nil {
		who = "unknown"
	}
	p := map[string]any{"action": action, "at": epoch() + float64(used), "delay_s": used, "requested_by": who}
	b.pending[a.ID] = p
	return 200, jsonx.M{"action": action, "pending": p}, nil
}

// ---------------------------------------------------------------- agent metrics
func gpuRow(name string, uLo, uHi, vramGiB, pLo, pHi, tLo, tHi, phase float64) map[string]any {
	u := wave(70, uLo, uHi, phase)
	return map[string]any{"name": name, "vendor": "nvidia", "util_percent": r1(u),
		"vram_used": int64((0.15 + u/160) * vramGiB * GiB), "vram_total": int64(vramGiB * GiB),
		"temp_c": r1(wave(90, tLo, tHi, phase)), "power_w": r1(wave(70, pLo, pHi, phase))}
}

func temp(sensor, label string, c float64) map[string]any {
	return map[string]any{"sensor": sensor, "label": label, "current": c}
}

func agentMetrics(id string) (map[string]any, error) {
	t := since()
	m := map[string]any{"agent_version": "1.0.0", "timestamp": epoch(), "load_avg": nil,
		"features": map[string]any{"processes": true, "cmdline": true, "fan_control": false}, "power_sources": []any{}}
	set := func(kv map[string]any) {
		for k, v := range kv {
			m[k] = v
		}
	}
	feat := m["features"].(map[string]any)
	switch id {
	case "nsx-cyrene":
		pkg := r1(wave(100, 18, 70, 0))
		g := gpuRow("NVIDIA GeForce RTX 2080 SUPER", 3, 85, 8, 18, 210, 38, 72, 0)
		set(map[string]any{"hostname": "NSX-CYRENE", "uptime_s": int64(86400*6 + t),
			"platform": map[string]any{"system": "Windows", "pretty": "Windows 11 Enterprise 24H2"},
			"cpu":      map[string]any{"percent": r1(wave(100, 8, 55, 0)), "logical": 16},
			"memory":   map[string]any{"total": int64(64 * GiB), "used": int64(wave(500, 20, 34, 0) * GiB)},
			"disks": []any{map[string]any{"mount": `C:\`, "fstype": "NTFS", "total": int64(1000 * GiB), "used": int64(412 * GiB)},
				map[string]any{"mount": `D:\`, "fstype": "NTFS", "total": int64(4000 * GiB), "used": int64(2710 * GiB)}},
			"net": map[string]any{"bytes_recv": int64(5e9 + t*wave(40, 1e5, 9e6, 0)), "bytes_sent": int64(2e9 + t*wave(40, 5e4, 3e6, 0))},
			"temps": []any{temp("IPMI", "CPU Temp", r1(wave(100, 38, 66, 0))), temp("IPMI", "PCH Temp", r1(wave(300, 44, 50, 0))),
				temp("IPMI", "System Temp", r1(wave(300, 29, 33, 0))), temp("IPMI", "Peripheral Temp", r1(wave(300, 31, 36, 0))),
				temp("IPMI", "VcpuVRM Temp", r1(wave(100, 36, 52, 0)))},
			"fans": []any{}, "voltages": []any{map[string]any{"name": "IPMI 12V", "volts": 12.09}, map[string]any{"name": "IPMI 5VCC", "volts": 5.03},
				map[string]any{"name": "IPMI 3.3VCC", "volts": 3.31}, map[string]any{"name": "IPMI Vcpu", "volts": 0.91}},
			"gpus": []any{g},
			"power": map[string]any{"cpu_package_w": pkg, "gpu_w": g["power_w"], "battery_w": nil, "battery_state": nil,
				"bmc_w": nil, "bmc_note": "DCMI power reading unavailable (completion code 0xC1)",
				"sensors": []any{map[string]any{"name": "Intel Xeon E-2278G CPU Package", "watts": pkg}},
				"source":  []any{"lhm-http", "gpu"}},
			"hardware": map[string]any{"system_vendor": "System manufacturer", "system_model": "System Product Name",
				"board_vendor": "Supermicro", "board_model": "X11SCA-F", "bios_version": "1.8",
				"cpu_model": "Intel(R) Xeon(R) E-2278G CPU @ 3.40GHz"}})
		feat["fan_control"] = true
	case "neptune-laptop":
		bat := r1(wave(60, 11, 26, 0))
		g := gpuRow("NVIDIA GeForce RTX 3050 Laptop GPU", 0, 35, 4, 4, 38, 40, 63, 1.5)
		set(map[string]any{"hostname": "NEPTUNE-VIVOBOOK", "uptime_s": int64(3600*5 + t),
			"platform": map[string]any{"system": "Windows", "pretty": "Windows 11 Home 24H2"},
			"cpu":      map[string]any{"percent": r1(wave(50, 4, 35, 0)), "logical": 12},
			"memory":   map[string]any{"total": int64(16 * GiB), "used": int64(wave(300, 8, 12, 0) * GiB)},
			"disks":    []any{map[string]any{"mount": `C:\`, "fstype": "NTFS", "total": int64(512 * GiB), "used": int64(301 * GiB)}},
			"net":      map[string]any{"bytes_recv": int64(1e9 + t*wave(30, 2e4, 2e6, 0)), "bytes_sent": int64(3e8 + t*wave(30, 1e4, 4e5, 0))},
			"temps":    []any{temp("lhm", "CPU Package", r1(wave(60, 48, 74, 0)))},
			"gpus":     []any{g},
			"power": map[string]any{"cpu_package_w": r1(wave(60, 6, 28, 0)), "gpu_w": g["power_w"], "battery_w": bat,
				"battery_state": "discharging", "bmc_w": nil, "sensors": []any{}, "source": []any{"lhm-http", "wmi-battery", "gpu"}},
			"hardware": map[string]any{"system_vendor": "ASUSTeK COMPUTER INC.", "system_model": "Vivobook 16X K3605ZC",
				"board_vendor": "ASUSTeK COMPUTER INC.", "board_model": "K3605ZC", "bios_version": "K3605ZC.305",
				"cpu_model": "12th Gen Intel(R) Core(TM) i5-12500H"}})
	case "llm-box":
		load := llmLoad()
		gpu := func(i int) map[string]any {
			busy := load * uniform(0.91, 1.0)
			return map[string]any{"name": "NVIDIA GeForce RTX 3090", "vendor": "nvidia", "util_percent": r1(2 + 96*busy),
				"vram_used": int64((21.2 + 0.4*float64(i) + uniform(-0.05, 0.05)) * GiB), "vram_total": int64(24 * GiB),
				"temp_c": r1(48 + 23*busy + uniform(-1, 1)), "power_w": r1(32 + 300*busy + uniform(-6, 6))}
		}
		g0, g1 := gpu(0), gpu(1)
		pkg := r1(55 + 75*load + uniform(-4, 4))
		set(map[string]any{"hostname": "llm-box", "uptime_s": int64(86400*9 + t), "load_avg": []any{r2(5 + 4*load), 6.1, 5.8},
			"platform": map[string]any{"system": "Linux", "pretty": "Ubuntu 24.04.1 LTS"},
			"cpu":      map[string]any{"percent": r1(6 + 16*load + uniform(-2, 3)), "logical": 32, "physical": 16},
			"memory":   map[string]any{"total": int64(128 * GiB), "used": int64((38.5 + 2*load + uniform(-0.3, 0.3)) * GiB)},
			"disks": []any{map[string]any{"mount": "/", "fstype": "ext4", "total": int64(1000 * GiB), "used": int64(212 * GiB)},
				map[string]any{"mount": "/models", "fstype": "xfs", "total": int64(4000 * GiB), "used": int64(1460 * GiB)}},
			"net": map[string]any{"bytes_recv": int64(3e10 + t*(2e5+4e5*load)), "bytes_sent": int64(1e10 + t*(1e5+9e5*load))},
			"temps": []any{map[string]any{"sensor": "k10temp", "label": "Tctl", "current": r1(52 + 14*load), "high": 95.0},
				temp("nvme", "Composite", 44.9)},
			"fans": []any{map[string]any{"name": "nct6799 fan1", "rpm": int(900 + 500*load)}, map[string]any{"name": "nct6799 fan2", "rpm": int(1100 + 600*load)}},
			"gpus": []any{g0, g1},
			"power": map[string]any{"cpu_package_w": pkg, "gpu_w": r1(g0["power_w"].(float64) + g1["power_w"].(float64)), "battery_w": nil,
				"battery_state": nil, "bmc_w": nil, "sensors": []any{map[string]any{"name": "rapl package-0", "watts": pkg}},
				"source": []any{"rapl", "gpu"}},
			"hardware": map[string]any{"system_vendor": "To be filled by O.E.M.", "system_model": "To be filled by O.E.M.",
				"board_vendor": "Gigabyte Technology Co., Ltd.", "board_model": "X670E AORUS MASTER",
				"bios_version": "F31", "cpu_model": "AMD Ryzen 9 7950X 16-Core Processor"}})
	case "asus-i7-agent": // agent on Straw's standalone PVE host, relaying its local upsd
		row := ups.RowMap(devices.UPSFromVars("cyberpower", cyberpowerVars(), "CyberPower CP1500"))
		set(map[string]any{"hostname": nil, "uptime_s": int64(86400*2 + t), "platform": map[string]any{"system": "Linux", "pretty": "Proxmox VE 9"},
			"cpu":    map[string]any{"percent": r1(wave(55, 8, 30, 4)), "logical": 8},
			"memory": map[string]any{"total": int64(32 * GiB), "used": int64(wave(250, 14, 19, 0) * GiB)},
			"disks":  []any{}, "net": map[string]any{"bytes_recv": 0, "bytes_sent": 0},
			"temps": []any{temp("coretemp", "Package id 0", r1(wave(60, 41, 58, 0)))}, "gpus": []any{},
			"ups": []any{row},
			"hardware": map[string]any{"system_vendor": "ASUS", "system_model": "All Series", "board_vendor": "ASUSTeK COMPUTER INC.",
				"board_model": "Z97-A", "bios_version": "2801", "cpu_model": "Intel(R) Core(TM) i7-4790K CPU @ 4.00GHz"}})
		feat["processes"], feat["cmdline"] = false, false // Straw keeps --no-processes
	case "kachina-agent": // agent on the NSX-KACHINA Proxmox node
		set(map[string]any{"hostname": "NSX-KACHINA", "uptime_s": int64(86400*12 + t),
			"platform": map[string]any{"system": "Linux", "pretty": "Proxmox VE 9.0"},
			"cpu":      map[string]any{"percent": r1(wave(80, 5, 20, 1)), "logical": 8},
			"memory":   map[string]any{"total": int64(32 * GiB), "used": int64(wave(400, 13, 22, 0) * GiB)},
			"disks":    []any{}, "net": map[string]any{"bytes_recv": 0, "bytes_sent": 0},
			"temps": []any{temp("coretemp", "Package id 0", r1(wave(60, 38, 51, 0))), temp("nvme", "Composite", 41.9)},
			"fans":  []any{map[string]any{"name": "it8613 fan1", "rpm": int(wave(90, 1100, 1400, 0))}},
			"gpus":  []any{},
			"hardware": map[string]any{"system_vendor": "LENOVO", "system_model": "ThinkCentre M720q", "board_vendor": "LENOVO",
				"board_model": "3136", "bios_version": "M1UKT77A", "cpu_model": "Intel(R) Core(TM) i7-8700T CPU @ 2.40GHz"}})
	case "straw-pc":
		gw := r1(wave(45, 12, 250, 0))
		set(map[string]any{"hostname": nil, "uptime_s": int64(3600*30 + t), // owner set --no-hostname-details
			"platform": map[string]any{"system": "Windows", "pretty": "Windows"},
			"cpu":      map[string]any{"percent": r1(wave(45, 6, 70, 2)), "logical": 16},
			"memory":   map[string]any{"total": int64(32 * GiB), "used": int64(wave(250, 11, 22, 0) * GiB)},
			"disks":    []any{map[string]any{"mount": `C:\`, "fstype": "NTFS", "total": int64(2000 * GiB), "used": int64(1220 * GiB)}},
			"net":      map[string]any{"bytes_recv": int64(8e9 + t*wave(35, 5e4, 6e6, 0)), "bytes_sent": int64(1e9 + t*wave(35, 2e4, 9e5, 0))},
			"temps":    []any{temp("lhm", "Core (Tctl/Tdie)", r1(wave(70, 45, 82, 0)))},
			"gpus": []any{map[string]any{"name": "AMD Radeon RX 7800 XT", "vendor": "amd", "util_percent": r1(wave(45, 2, 97, 0)),
				"vram_used": int64(wave(45, 1.5, 11, 0) * GiB), "vram_total": int64(16 * GiB),
				"temp_c": r1(wave(45, 35, 74, 0)), "power_w": gw}},
			"power": map[string]any{"cpu_package_w": r1(wave(45, 28, 88, 2)), "gpu_w": gw, "battery_w": nil,
				"battery_state": nil, "bmc_w": nil, "sensors": []any{}, "source": []any{"lhm-http"}},
			"power_sources": []any{map[string]any{"id": "desk-plug", "name": "Straw's Tuya desk plug", "type": "tuya", "ok": true,
				"watts": r1(wave(45, 95, 420, 0)), "voltage": 229.8, "current": nil, "energy_kwh_total": nil,
				"system_hint": "self", "error": nil}}})
		feat["processes"], feat["cmdline"] = false, false
	default:
		return nil, fmt.Errorf("simulated: agent unreachable: %w", context.DeadlineExceeded)
	}
	return m, nil
}

const llamaCmd = "llama-server -m /models/qwen3-30b-a3b-q4_k_m.gguf -ngl 99 --split-mode layer -c 32768 " +
	"--host 100.64.0.12 --port 8080 --api-key sk-demo-7f3a9c2b11"

func sortRows(rows []map[string]any, sortBy string) {
	key := map[string]string{"cpu": "cpu_percent", "mem": "rss", "gpu": "gpu_mem"}[sortBy]
	val := func(r map[string]any) float64 {
		switch v := r[key].(type) {
		case float64:
			return v
		case int64:
			return float64(v)
		}
		return 0
	}
	sort.SliceStable(rows, func(i, j int) bool { return val(rows[i]) > val(rows[j]) })
}

func demoProcesses(id string, limit int, sortBy string) jsonx.M {
	t0 := float64(T0.Unix())
	var rows []map[string]any
	total := 0
	if id == "llm-box" {
		load, G := llmLoad(), float64(GiB)
		rows = []map[string]any{
			{"pid": 2417, "name": "llama-server", "username": "llama", "cpu_percent": r1(180 + 220*load), "rss": int64(19.6 * G),
				"gpu_mem": int64(42.6 * G), "gpu_mem_by_gpu": []any{int64(21.2 * G), int64(21.4 * G)}, "cmdline": redact.Cmdline(llamaCmd, 200)},
			{"pid": 3120, "name": "python3", "username": "neptune", "cpu_percent": r1(3 + 9*load), "rss": int64(1.1 * G),
				"gpu_mem": nil, "cmdline": "python3 /opt/bench/stream_chat.py --endpoint http://127.0.0.1:8080 --token=***"},
			{"pid": 1203, "name": "nvidia-persistenced", "username": "nvidia-persistenced", "cpu_percent": 0.0,
				"rss": int64(6 << 20), "gpu_mem": nil, "cmdline": "/usr/bin/nvidia-persistenced --user nvidia-persistenced"},
			{"pid": 988, "name": "tailscaled", "username": "root", "cpu_percent": 0.6, "rss": int64(48 << 20), "gpu_mem": nil,
				"cmdline": "/usr/sbin/tailscaled --state=/var/lib/tailscale/tailscaled.state"},
			{"pid": 1544, "name": "fairy-agent", "username": "fairy-agent", "cpu_percent": 0.4, "rss": int64(21 << 20),
				"gpu_mem": nil, "cmdline": "/usr/local/bin/fairy-agent run"},
			{"pid": 1, "name": "systemd", "username": "root", "cpu_percent": 0.0, "rss": int64(13 << 20), "gpu_mem": nil, "cmdline": "/sbin/init"},
		}
		for i, r := range rows {
			r["status"] = "running"
			r["mem_percent"] = r2(float64(r["rss"].(int64)) / (128 * G) * 100)
			r["create_time"] = t0 - 86400 + float64(i*60)
		}
		total = 312
	} else {
		type p struct {
			name, user        string
			cpu, rssMiB, gMiB float64
		}
		var names []p
		switch id {
		case "nsx-cyrene":
			names = []p{{"Plex Media Server.exe", `NSX-CYRENE\plex`, 12, 900, 0}, {"obs64.exe", `NSX-CYRENE\neptune`, 9, 620, 410},
				{"chrome.exe", `NSX-CYRENE\neptune`, 6, 1400, 120}, {"MsMpEng.exe", `NT AUTHORITY\SYSTEM`, 4, 310, 0},
				{"Ollama.exe", `NSX-CYRENE\neptune`, 3, 5200, 5600}, {"dwagsvc.exe", `NT AUTHORITY\SYSTEM`, 1.2, 45, 0},
				{"fairy-agent.exe", `NT AUTHORITY\SYSTEM`, 0.6, 24, 0}, {"svchost.exe", "", 0.4, 60, 0},
				{"System", "", 0.9, 2, 0}, {"LibreHardwareMonitor.exe", `NT AUTHORITY\SYSTEM`, 0.8, 160, 0}}
		case "neptune-laptop":
			names = []p{{"Code.exe", `NEPTUNE-VIVOBOOK\neptune`, 7, 900, 0}, {"firefox.exe", `NEPTUNE-VIVOBOOK\neptune`, 11, 1600, 90},
				{"Discord.exe", `NEPTUNE-VIVOBOOK\neptune`, 2, 420, 60}, {"fairy-agent.exe", `NT AUTHORITY\SYSTEM`, 0.5, 22, 0}}
		}
		cmd := map[string]string{"Ollama.exe": "ollama.exe serve --host 127.0.0.1 --api-key=***",
			"obs64.exe":       `"C:\Program Files\obs-studio\bin\64bit\obs64.exe" --startreplaybuffer`,
			"fairy-agent.exe": `"C:\Program Files\Fairy\fairy-agent.exe" run`}
		for i, x := range names {
			c, ok := cmd[x.name]
			if !ok {
				c = x.name
			}
			var user, gm any
			if x.user != "" {
				user = x.user
			}
			if x.gMiB > 0 {
				gm = int64(x.gMiB * (1 << 20))
			}
			rows = append(rows, map[string]any{"pid": 1000 + i*412, "name": x.name, "username": user, "status": "running",
				"cpu_percent": r1(x.cpu * uniform(0.6, 1.5)), "rss": int64(x.rssMiB * (1 << 20)),
				"mem_percent": r2(x.rssMiB / 655.36), "gpu_mem": gm, "create_time": t0 - 3600*float64(i+1), "cmdline": c})
		}
		total = 180 + len(rows)
	}
	sortRows(rows, sortBy)
	if limit < len(rows) {
		rows = rows[:limit]
	}
	list := make([]any, len(rows))
	for i, r := range rows {
		list[i] = r
	}
	raw, _ := json.Marshal(jsonx.M{"sampled_at": epoch(), "sort": sortBy, "total_processes": total, "processes": list})
	var out jsonx.M
	_ = json.Unmarshal(raw, &out)
	return out
}

// SeedAudit writes sample history for the demo's Audit page (only when the demo log is empty).
func SeedAudit(path string) error {
	if st, err := os.Stat(path); err == nil && st.Size() > 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	now := time.Now()
	type row struct {
		dt                  time.Duration
		actor               string
		sub                 any
		action, target      string
		params              map[string]any
		outcome, source, ip string
	}
	h := time.Hour
	rows := []row{
		{-48 * h, "root", nil, "token.create", "home-assistant", map[string]any{"id": "3fa1c2d9", "scope": "read", "owner": "system"}, "ok", "cli", "local"},
		{-48*h + time.Minute, "root", nil, "control.disable", "", map[string]any{"reason": "replacing PSU in NSX-CYRENE"}, "ok", "cli", "local"},
		{-48*h + 90*time.Minute, "root", nil, "control.enable", "", map[string]any{}, "ok", "cli", "local"},
		{-26 * h, "neptune@example.invalid", "demo-neptune", "auth.login", "", map[string]any{}, "ok", "ui", "203.0.113.24"},
		{-26*h + 30*time.Second, "neptune@example.invalid", "demo-neptune", "auth.step_up", "", map[string]any{}, "ok", "ui", "203.0.113.24"},
		{-26*h + 40*time.Second, "neptune@example.invalid", "demo-neptune", "fan.mode", "nsx-cyrene", map[string]any{"mode": "full"}, "ok", "ui", "203.0.113.24"},
		{-26*h + 70*time.Second, "neptune@example.invalid", "demo-neptune", "fan.duty", "nsx-cyrene", map[string]any{"zone": 0, "percent": 45}, "ok", "ui", "203.0.113.24"},
		{-20 * h, "straw@example.invalid", "demo-straw", "auth.login", "", map[string]any{}, "ok", "ui", "198.51.100.7"},
		{-20*h + 15*time.Second, "straw@example.invalid", "demo-straw", "auth.denied", "/api/systems/nsx-cyrene/power",
			map[string]any{"method": "POST"}, "403: not allowed to control", "ui", "198.51.100.7"},
		{-9 * h, "home-assistant (token, owner system)", "token:3fa1c2d9", "auth.denied", "/api/systems/llm-box/poll",
			map[string]any{"method": "PUT"}, "403: not allowed to control", "token:home-assistant", "100.64.0.31"},
		{-5 * h, "unknown@example.invalid", "x9", "auth.login", "", map[string]any{}, "denied: not in an allowed group", "ui", "192.0.2.50"},
		{-3 * h, "neptune@example.invalid", "demo-neptune", "settings.update", "", map[string]any{"background": "upload", "dim": 55}, "ok", "ui", "203.0.113.24"},
		{-2 * h, "neptune@example.invalid", "demo-neptune", "poll.set", "llm-box", map[string]any{"interval_s": 2}, "ok", "ui", "203.0.113.24"},
		{-90 * time.Minute, "neptune@example.invalid", "demo-neptune", "power.start", "lxc-122", map[string]any{"action": "start", "via": "proxmox-api"}, "ok", "ui", "203.0.113.24"},
		{-40 * time.Minute, "root", nil, "host.power_control.disable", "straw-pc", map[string]any{}, "ok", "cli", "local"},
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, r := range rows {
		b, _ := json.Marshal(map[string]any{"ts": now.Add(r.dt).UTC().Format("2006-01-02T15:04:05.000000-07:00"),
			"actor": r.actor, "sub": r.sub, "action": r.action, "target": r.target, "params": r.params,
			"outcome": r.outcome, "source": r.source, "ip": r.ip})
		if _, err := f.Write(append(b, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// Build returns the demo config, secrets and a poller wired to the simulator.
func Build(dir, publicURL string) (*config.Panel, config.Secrets, *poller.Poller, error) {
	register()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, config.Secrets{}, nil, err
	}
	cfg, err := Config(dir, publicURL)
	if err != nil {
		return nil, config.Secrets{}, nil, err
	}
	if err := SeedAudit(cfg.AuditLog); err != nil {
		return nil, config.Secrets{}, nil, err
	}
	secrets := config.Secrets{SessionSecret: randTok() + randTok(), InsecureCookies: true}
	p, err := poller.New(cfg, secrets, NewBackend(cfg))
	if err != nil {
		return nil, config.Secrets{}, nil, err
	}
	return cfg, secrets, p, nil
}
