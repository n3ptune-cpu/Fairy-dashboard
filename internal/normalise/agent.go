package normalise

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

func pending(pc jsonx.M) *model.PowerPending {
	p := jsonx.Obj(pc, "pending")
	if p == nil {
		return nil
	}
	a := jsonx.S(p, "action")
	at := jsonx.Num(p, "at")
	if (a != "shutdown" && a != "reboot" && a != "sleep") || at == nil {
		return nil
	}
	d := 0
	if v := jsonx.Num(p, "delay_s"); v != nil {
		d = int(*v)
	}
	return &model.PowerPending{Action: a, At: EpochTime(*at), DelayS: d,
		RequestedBy: jsonx.Str(p, "requested_by")}
}

func strOr(m jsonx.M, k, def string) string {
	v, ok := m[k]
	if !ok || v == nil {
		return def
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// NormaliseAgent converts an agent /metrics payload. Malformed entries in the lists (a disk without sizes, a
// sensor without a reading) are skipped rather than failing the whole host.
func NormaliseAgent(cfg config.Agent, m jsonx.M, now time.Time, ts float64, rates *RateTracker) model.System {
	cpu, mem, nett, plat := jsonx.Obj(m, "cpu"), jsonx.Obj(m, "memory"), jsonx.Obj(m, "net"), jsonx.Obj(m, "platform")
	disks, used, total := agentDisks(m)
	rx, tx := rates.Update(cfg.ID, ts, jsonx.Int(nett, "bytes_recv"), jsonx.Int(nett, "bytes_sent"), 0)
	features := jsonx.Obj(m, "features")
	hw := jsonx.Obj(m, "hardware")
	name := cfg.Name
	if name == "" {
		name = jsonx.S(m, "hostname")
	}
	if name == "" {
		name = cfg.ID
	}
	s := model.System{ID: cfg.ID, Name: name, Kind: "agent", Source: "agent", Status: "online",
		CPUPercent: jsonx.Num(cpu, "percent"), Cores: jsonx.IntP(cpu, "logical"),
		MemUsed: jsonx.Int(mem, "used"), MemTotal: jsonx.Int(mem, "total"),
		Disks: disks, NetRxBps: rx, NetTxBps: tx, UptimeS: jsonx.Int(m, "uptime_s"),
		OS: jsonx.Str(plat, "pretty"), Platform: jsonx.Str(plat, "system"), LoadAvg: agentLoadAvg(m),
		Temps: agentTemps(m), Fans: agentFans(m), Voltages: agentVoltages(m), GPUs: agentGPUs(m),
		ComponentPower: agentComponentPower(m), Battery: agentBattery(m),
		ProcessesAvailable: jsonx.Bool(features, "processes", true),
		FanControl:         cfg.FanControl != nil && jsonx.Truthy(features["fan_control"]),
		Hardware:           HardwareDisplay(hw), CPUModel: jsonx.Str(hw, "cpu_model"),
		PowerPending: pending(jsonx.Obj(m, "power_control")),
		Group:        cfg.Group, Owner: cfg.Owner, Tags: slices.Clone(cfg.Tags), LastSeen: model.P(now),
		PollMode: "default"}
	if len(disks) > 0 {
		s.DiskUsed, s.DiskTotal = &used, &total
	}
	if br := jsonx.Obj(m, "bmc_relay"); br != nil {
		s.BMCRelayTargets = []string{}
		for _, t := range jsonx.Objs(br["targets"]) {
			if id := jsonx.S(t, "id"); id != "" {
				s.BMCRelayTargets = append(s.BMCRelayTargets, id)
			}
		}
	}
	if pc := jsonx.Obj(m, "power_control"); pc != nil {
		if v, ok := pc["sleep"].(bool); ok {
			s.Sleep = model.P(v)
			if !v {
				s.SleepNote = jsonx.Str(pc, "sleep_note")
			}
		}
	}
	if cfg.DWServiceURL != "" {
		s.DWServiceURL = model.P(cfg.DWServiceURL)
	}
	s.Helper, s.ServiceAccount = agentHelper(m), agentServiceAccount(m) // Helper.Paused: the poller, ApplyHelperPause
	model.Normalize(&s)
	return s
}

// agentDisks returns the mounted filesystems and their summed used / total bytes.
func agentDisks(m jsonx.M) (disks []model.Disk, used, total int64) {
	disks = []model.Disk{}
	for _, d := range jsonx.Objs(m["disks"]) {
		mount, ok := d["mount"].(string)
		u, t := jsonx.Int(d, "used"), jsonx.Int(d, "total")
		if !ok || u == nil || t == nil {
			continue
		}
		disks = append(disks, model.Disk{Mount: mount, FSType: jsonx.Str(d, "fstype"), Used: *u, Total: *t})
		used += *u
		total += *t
	}
	return disks, used, total
}

func agentTemps(m jsonx.M) []model.Temperature {
	temps := []model.Temperature{}
	for _, t := range jsonx.Objs(m["temps"]) {
		cur := jsonx.Num(t, "current")
		if cur == nil {
			continue
		}
		temps = append(temps, model.Temperature{Sensor: strOr(t, "sensor", "?"), Label: jsonx.Str(t, "label"),
			CurrentC: *cur, HighC: jsonx.Num(t, "high"), CriticalC: jsonx.Num(t, "critical")})
	}
	return temps
}

// gpuSource keeps the sources the UI knows ("windows"; "amd": Windows' counters plus AMD's ADL for temperature
// and watts, Fairy-agent v0.4.2); anything else is dropped.
func gpuSource(s string) string {
	if s == "windows" || s == "amd" {
		return s
	}
	return ""
}

// agentGPUs maps the vendor to nvidia / amd / intel / other.
func agentGPUs(m jsonx.M) []model.Gpu {
	gpus := []model.Gpu{}
	for _, g := range jsonx.Objs(m["gpus"]) {
		vendor := jsonx.S(g, "vendor")
		if vendor != "nvidia" && vendor != "amd" && vendor != "intel" {
			vendor = "other"
		}
		name := jsonx.S(g, "name")
		if name == "" {
			name = "GPU"
		}
		gpus = append(gpus, model.Gpu{Name: name, Vendor: vendor, UtilPercent: jsonx.Num(g, "util_percent"),
			VRAMUsed: jsonx.Int(g, "vram_used"), VRAMTotal: jsonx.Int(g, "vram_total"), TempC: jsonx.Num(g, "temp_c"),
			PowerW: jsonx.Num(g, "power_w"), Source: gpuSource(jsonx.S(g, "source"))})
	}
	return gpus
}

// agentBattery is the optional `battery` block (agents before it, and machines without a battery, send none).
// Out-of-range values are dropped rather than shown.
func agentBattery(m jsonx.M) *model.Battery {
	b := jsonx.Obj(m, "battery")
	if b == nil {
		return nil
	}
	out := &model.Battery{State: "unknown"}
	switch st := jsonx.S(b, "state"); st {
	case "charging", "discharging", "full", "idle":
		out.State = st
	}
	if p := jsonx.Num(b, "percent"); p != nil && *p >= 0 && *p <= 100 {
		out.Percent = p
	}
	if v, ok := b["ac_online"].(bool); ok {
		out.ACOnline = &v
	}
	if w := jsonx.Num(b, "power_w"); w != nil && math.Abs(*w) < 1000 {
		out.PowerW = w
	}
	if t := jsonx.Num(b, "time_remaining_s"); t != nil && *t >= 0 && *t < 30*86400 {
		out.TimeRemainingS = model.P(int64(*t))
	}
	if h := jsonx.Num(b, "health_pct"); h != nil && *h > 0 && *h <= 200 {
		out.HealthPct = h
	}
	return out
}

// agentComponentPower is the `power` block (CPU package, GPU, battery, BMC watts); nil when the agent sent none.
func agentComponentPower(m jsonx.M) *model.ComponentPower {
	p := jsonx.Obj(m, "power")
	if len(p) == 0 {
		return nil
	}
	power := &model.ComponentPower{CPUPackageW: jsonx.Num(p, "cpu_package_w"), GPUW: jsonx.Num(p, "gpu_w"),
		BatteryW: jsonx.Num(p, "battery_w"), BatteryState: jsonx.Str(p, "battery_state"),
		BMCW: jsonx.Num(p, "bmc_w"), BMCNote: jsonx.Str(p, "bmc_note")}
	for _, s := range jsonx.Objs(p["sensors"]) {
		if w := jsonx.Num(s, "watts"); w != nil {
			power.Sensors = append(power.Sensors, model.PowerSensor{Name: strOr(s, "name", "?"), Watts: *w})
		}
	}
	for _, s := range jsonx.List(p, "source") {
		if str, ok := s.(string); ok {
			power.Source = append(power.Source, str)
		}
	}
	return power
}

func agentFans(m jsonx.M) []model.Fan {
	fans := []model.Fan{}
	for _, f := range jsonx.Objs(m["fans"]) {
		if r := jsonx.Num(f, "rpm"); r != nil {
			fans = append(fans, model.Fan{Name: strOr(f, "name", "?"), RPM: *r})
		}
	}
	return fans
}

func agentVoltages(m jsonx.M) []model.Voltage {
	volts := []model.Voltage{}
	for _, v := range jsonx.Objs(m["voltages"]) {
		if x := jsonx.Num(v, "volts"); x != nil {
			volts = append(volts, model.Voltage{Name: strOr(v, "name", "?"), Volts: *x})
		}
	}
	return volts
}

// agentLoadAvg is nil (JSON null) when the agent sends no load average, not an empty list.
func agentLoadAvg(m jsonx.M) []float64 {
	l := jsonx.List(m, "load_avg")
	if l == nil {
		return nil
	}
	load := []float64{}
	for _, v := range l {
		if f := jsonx.NumV(v); f != nil {
			load = append(load, *f)
		}
	}
	return load
}

// EpochTime converts Unix seconds with a fraction (as agents send them) to a UTC time.
func EpochTime(sec float64) time.Time {
	whole, frac := math.Modf(sec)
	return time.Unix(int64(whole), int64(frac*1e9)).UTC()
}
