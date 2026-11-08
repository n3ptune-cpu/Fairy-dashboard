package normalise

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/config"
	"github.com/n3ptune-cpu/Fairy/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy/internal/model"
)

func pending(pc jsonx.M) *model.PowerPending {
	p := jsonx.Obj(pc, "pending")
	if p == nil {
		return nil
	}
	a := jsonx.S(p, "action")
	at := jsonx.Num(p, "at")
	if (a != "shutdown" && a != "reboot") || at == nil {
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

// NormaliseAgent converts an agent /metrics payload.
func NormaliseAgent(cfg config.Agent, m jsonx.M, now time.Time, ts float64, rates *RateTracker) model.System {
	cpu, mem, nett, plat := jsonx.Obj(m, "cpu"), jsonx.Obj(m, "memory"), jsonx.Obj(m, "net"), jsonx.Obj(m, "platform")
	disks := []model.Disk{}
	var used, total int64
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
	rx, tx := rates.Update(cfg.ID, ts, jsonx.Int(nett, "bytes_recv"), jsonx.Int(nett, "bytes_sent"), 0)
	temps := []model.Temperature{}
	for _, t := range jsonx.Objs(m["temps"]) {
		cur := jsonx.Num(t, "current")
		if cur == nil {
			continue
		}
		temps = append(temps, model.Temperature{Sensor: strOr(t, "sensor", "?"), Label: jsonx.Str(t, "label"),
			CurrentC: *cur, HighC: jsonx.Num(t, "high"), CriticalC: jsonx.Num(t, "critical")})
	}
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
			PowerW: jsonx.Num(g, "power_w")})
	}
	var power *model.ComponentPower
	if p := jsonx.Obj(m, "power"); len(p) > 0 {
		power = &model.ComponentPower{CPUPackageW: jsonx.Num(p, "cpu_package_w"), GPUW: jsonx.Num(p, "gpu_w"),
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
	}
	fans := []model.Fan{}
	for _, f := range jsonx.Objs(m["fans"]) {
		if r := jsonx.Num(f, "rpm"); r != nil {
			fans = append(fans, model.Fan{Name: strOr(f, "name", "?"), RPM: *r})
		}
	}
	volts := []model.Voltage{}
	for _, v := range jsonx.Objs(m["voltages"]) {
		if x := jsonx.Num(v, "volts"); x != nil {
			volts = append(volts, model.Voltage{Name: strOr(v, "name", "?"), Volts: *x})
		}
	}
	var load []float64
	if l := jsonx.List(m, "load_avg"); l != nil {
		load = []float64{}
		for _, v := range l {
			if f := jsonx.NumV(v); f != nil {
				load = append(load, *f)
			}
		}
	}
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
		OS: jsonx.Str(plat, "pretty"), Platform: jsonx.Str(plat, "system"), LoadAvg: load,
		Temps: temps, Fans: fans, Voltages: volts, GPUs: gpus, ComponentPower: power,
		ProcessesAvailable: jsonx.Bool(features, "processes", true),
		FanControl:         cfg.FanControl != nil && jsonx.Truthy(features["fan_control"]),
		Hardware:           HardwareDisplay(hw), CPUModel: jsonx.Str(hw, "cpu_model"),
		PowerPending: pending(jsonx.Obj(m, "power_control")),
		Group:        cfg.Group, Owner: cfg.Owner, Tags: slices.Clone(cfg.Tags), LastSeen: model.P(now),
		PollMode: "default"}
	if len(disks) > 0 {
		s.DiskUsed, s.DiskTotal = &used, &total
	}
	if cfg.DWServiceURL != "" {
		s.DWServiceURL = model.P(cfg.DWServiceURL)
	}
	model.Normalize(&s)
	return s
}

// EpochTime converts Unix seconds with a fraction (as agents send them) to a UTC time.
func EpochTime(sec float64) time.Time {
	whole, frac := math.Modf(sec)
	return time.Unix(int64(whole), int64(frac*1e9)).UTC()
}
