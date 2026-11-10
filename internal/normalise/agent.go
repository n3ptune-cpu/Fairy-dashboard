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
	// A time the page cannot show (before 1970, after 2286, not a number) is no pending action: the whole block
	// is dropped. A time outside the years 0 to 9999 cannot even be written as JSON.
	if (a != "shutdown" && a != "reboot" && a != "sleep") || at == nil || !(*at > 0 && *at < MaxUnixTime) {
		return nil
	}
	d := 0
	if v := num(p, "delay_s", 0, 1e9); v != nil {
		d = int(*v)
	}
	return &model.PowerPending{Action: a, At: EpochTime(*at), DelayS: d,
		RequestedBy: clipP(jsonx.Str(p, "requested_by"), maxName)}
}

func strOr(m jsonx.M, k, def string) string {
	v, ok := m[k]
	if !ok || v == nil {
		return def
	}
	if s, ok := v.(string); ok {
		return ClipText(s, maxName)
	}
	return ClipText(fmt.Sprint(v), maxName)
}

// NormaliseAgent converts an agent /metrics payload. Malformed entries in the lists (a disk without sizes, a
// sensor without a reading) are skipped rather than failing the whole host; lists and texts are bounded (see
// limits.go), so one agent cannot make its entry arbitrarily large.
func NormaliseAgent(cfg config.Agent, m jsonx.M, now time.Time, ts float64, rates *RateTracker) model.System {
	cpu, mem, nett, plat := jsonx.Obj(m, "cpu"), jsonx.Obj(m, "memory"), jsonx.Obj(m, "net"), jsonx.Obj(m, "platform")
	disks, used, total := agentDisks(m)
	rx, tx := rates.Update(cfg.ID, ts, counter(nett, "bytes_recv"), counter(nett, "bytes_sent"), 0)
	features := jsonx.Obj(m, "features")
	hw := jsonx.Obj(m, "hardware")
	name := cfg.Name
	if name == "" {
		name = ClipText(jsonx.S(m, "hostname"), maxLong)
	}
	if name == "" {
		name = cfg.ID
	}
	s := model.System{ID: cfg.ID, Name: name, Kind: "agent", Source: "agent", Status: "online",
		CPUPercent: percent(cpu, "percent"), Cores: cores(cpu, "logical"),
		MemUsed: size(mem, "used"), MemTotal: size(mem, "total"),
		Disks: disks, NetRxBps: rx, NetTxBps: tx, UptimeS: whole(m, "uptime_s", 0, MaxUnixTime),
		OS: clipP(jsonx.Str(plat, "pretty"), maxLong), Platform: clipP(jsonx.Str(plat, "system"), maxShort), LoadAvg: agentLoadAvg(m),
		Temps: agentTemps(m), Fans: agentFans(m), Voltages: agentVoltages(m), GPUs: agentGPUs(m),
		ComponentPower: agentComponentPower(m), Battery: agentBattery(m),
		ProcessesAvailable: jsonx.Bool(features, "processes", true),
		FanControl:         cfg.FanControl != nil && jsonx.Truthy(features["fan_control"]),
		Hardware:           HardwareDisplay(hw), CPUModel: clipP(jsonx.Str(hw, "cpu_model"), maxLong),
		PowerPending: pending(jsonx.Obj(m, "power_control")),
		Group:        cfg.Group, Owner: cfg.Owner, Tags: slices.Clone(cfg.Tags), LastSeen: model.P(now),
		PollMode: "default"}
	if len(disks) > 0 {
		s.DiskUsed, s.DiskTotal = &used, &total
	}
	if br := jsonx.Obj(m, "bmc_relay"); br != nil {
		s.BMCRelayTargets = []string{}
		for _, t := range jsonx.Objs(br["targets"]) {
			if len(s.BMCRelayTargets) == MaxRelayTargets {
				break
			}
			if id := jsonx.S(t, "id"); id != "" && len(id) <= maxShort {
				s.BMCRelayTargets = append(s.BMCRelayTargets, id)
			}
		}
	}
	if pc := jsonx.Obj(m, "power_control"); pc != nil {
		if v, ok := pc["sleep"].(bool); ok {
			s.Sleep = model.P(v)
			if !v {
				s.SleepNote = clipP(jsonx.Str(pc, "sleep_note"), maxLong)
			}
		}
	}
	if cfg.DWServiceURL != "" {
		s.DWServiceURL = model.P(cfg.DWServiceURL)
	}
	s.Helper = agentHelper(m) // Helper.Paused: the poller, ApplyHelperPause
	if s.Helper != nil {
		s.PowerHelperReason = powerHelperReason(m)
	}
	model.Normalize(&s)
	return s
}

// cores is m[k] as a number of processor cores (0 to MaxCores), or nil.
func cores(m jsonx.M, k string) *int {
	c := whole(m, k, 0, MaxCores)
	if c == nil {
		return nil
	}
	v := int(*c)
	return &v
}

// agentDisks returns the mounted filesystems and their summed used / total bytes. A disk with a negative or
// absurd size is skipped; with at most MaxDisks sizes of at most MaxBytes the sums cannot overflow.
func agentDisks(m jsonx.M) (disks []model.Disk, used, total int64) {
	disks = []model.Disk{}
	for _, d := range jsonx.Objs(m["disks"]) {
		mount, ok := d["mount"].(string)
		u, t := size(d, "used"), size(d, "total")
		if !ok || u == nil || t == nil {
			continue
		}
		if len(disks) == MaxDisks {
			break
		}
		disks = append(disks, model.Disk{Mount: ClipText(mount, maxMount), FSType: clipP(jsonx.Str(d, "fstype"), maxShort), Used: *u, Total: *t})
		used = AddBytes(used, *u)
		total = AddBytes(total, *t)
	}
	return disks, used, total
}

func agentTemps(m jsonx.M) []model.Temperature {
	temps := []model.Temperature{}
	for _, t := range jsonx.Objs(m["temps"]) {
		cur := reading(t, "current")
		if cur == nil {
			continue
		}
		if len(temps) == MaxTemps {
			break
		}
		temps = append(temps, model.Temperature{Sensor: strOr(t, "sensor", "?"), Label: clipP(jsonx.Str(t, "label"), maxName),
			CurrentC: *cur, HighC: reading(t, "high"), CriticalC: reading(t, "critical")})
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
		if len(gpus) == MaxGPUs {
			break
		}
		vendor := jsonx.S(g, "vendor")
		if vendor != "nvidia" && vendor != "amd" && vendor != "intel" {
			vendor = "other"
		}
		name := ClipText(jsonx.S(g, "name"), maxName)
		if name == "" {
			name = "GPU"
		}
		gpus = append(gpus, model.Gpu{Name: name, Vendor: vendor, UtilPercent: percent(g, "util_percent"),
			VRAMUsed: size(g, "vram_used"), VRAMTotal: size(g, "vram_total"), TempC: reading(g, "temp_c"),
			PowerW: reading(g, "power_w"), Source: gpuSource(jsonx.S(g, "source"))})
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
	power := &model.ComponentPower{CPUPackageW: reading(p, "cpu_package_w"), GPUW: reading(p, "gpu_w"),
		BatteryW: reading(p, "battery_w"), BatteryState: clipP(jsonx.Str(p, "battery_state"), maxShort),
		BMCW: reading(p, "bmc_w"), BMCNote: clipP(jsonx.Str(p, "bmc_note"), maxLong)}
	for _, s := range jsonx.Objs(p["sensors"]) {
		if len(power.Sensors) == MaxPowerSensors {
			break
		}
		if w := reading(s, "watts"); w != nil {
			power.Sensors = append(power.Sensors, model.PowerSensor{Name: strOr(s, "name", "?"), Watts: *w})
		}
	}
	for _, s := range jsonx.List(p, "source") {
		if len(power.Source) == maxSourceNames {
			break
		}
		if str, ok := s.(string); ok {
			power.Source = append(power.Source, ClipText(str, maxShort))
		}
	}
	return power
}

func agentFans(m jsonx.M) []model.Fan {
	fans := []model.Fan{}
	for _, f := range jsonx.Objs(m["fans"]) {
		if len(fans) == MaxFans {
			break
		}
		if r := reading(f, "rpm"); r != nil {
			fans = append(fans, model.Fan{Name: strOr(f, "name", "?"), RPM: *r})
		}
	}
	return fans
}

func agentVoltages(m jsonx.M) []model.Voltage {
	volts := []model.Voltage{}
	for _, v := range jsonx.Objs(m["voltages"]) {
		if len(volts) == MaxVoltages {
			break
		}
		if x := reading(v, "volts"); x != nil {
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
		if len(load) == MaxLoadAvg {
			break
		}
		if f := Finite(jsonx.NumV(v), 0, MaxReading); f != nil {
			load = append(load, *f)
		}
	}
	return load
}

// EpochTime converts Unix seconds with a fraction (as agents send them) to a UTC time. The result can always be
// written as JSON: seconds outside 0 to MaxUnixTime (the years 1970 to 2286) are moved to the nearer end, and
// "not a number" is 0. Callers that can drop such a value should do so first (see pending).
func EpochTime(sec float64) time.Time {
	sec = math.Min(math.Max(sec, 0), MaxUnixTime) // math.Max(NaN, 0) is NaN
	if math.IsNaN(sec) {
		sec = 0
	}
	whole, frac := math.Modf(sec)
	return time.Unix(int64(whole), int64(frac*1e9)).UTC()
}
