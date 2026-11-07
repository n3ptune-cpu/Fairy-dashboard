// Package normalise: pure functions turning Proxmox / agent payloads into the unified System schema.
package normalise

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nsxservices/fairy/internal/config"
	"github.com/nsxservices/fairy/internal/devices"
	"github.com/nsxservices/fairy/internal/jsonx"
	"github.com/nsxservices/fairy/internal/model"
)

// Round to n decimals (Python round semantics are banker's rounding; the half-way difference is irrelevant here).
func Round(v float64, n int) float64 {
	p := math.Pow(10, float64(n))
	return math.Round(v*p) / p
}

// RateTracker turns cumulative byte counters into bytes/s between two successful polls.
//
// hold_s: Proxmox only refreshes its counters every ~10 s (pvestatd), so polling it at 2 s sees the same numbers
// several times in a row. With holdS > 0 an unchanged sample keeps the last rate (instead of flashing 0 then
// spiking) and the next real change is divided by the full time since the last change.
type RateTracker struct {
	mu   sync.Mutex
	prev map[string]rateSample
	last map[string][2]*float64
}

type rateSample struct {
	ts     float64
	rx, tx *int64
}

func NewRateTracker() *RateTracker {
	return &RateTracker{prev: map[string]rateSample{}, last: map[string][2]*float64{}}
}

func eqp(a, b *int64) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

func (r *RateTracker) Update(key string, ts float64, rx, tx *int64, holdS float64) (*float64, *float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev, ok := r.prev[key]
	if ok && holdS > 0 && eqp(rx, prev.rx) && eqp(tx, prev.tx) && ts-prev.ts < holdS {
		l := r.last[key]
		return l[0], l[1]
	}
	r.prev[key] = rateSample{ts, rx, tx}
	if !ok || ts <= prev.ts {
		return nil, nil
	}
	dt := ts - prev.ts
	rate := func(cur, old *int64) *float64 {
		if cur == nil || old == nil || *cur < *old {
			return nil
		}
		v := Round(float64(*cur-*old)/dt, 1)
		return &v
	}
	out := [2]*float64{rate(rx, prev.rx), rate(tx, prev.tx)}
	r.last[key] = out
	return out[0], out[1]
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func Slug(v string) string {
	return strings.Trim(slugRe.ReplaceAllString(strings.ToLower(v), "-"), "-")
}

var tagSplit = regexp.MustCompile(`[;,\s]+`)

func splitTags(raw string) []string {
	out := []string{}
	for _, t := range tagSplit.Split(raw, -1) {
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// Describe returns a short, secret-free reason string (never includes headers or URLs).
func Describe(err error) string {
	if err == nil {
		return ""
	}
	var de *devices.Error
	if errors.As(err, &de) {
		return de.Msg
	}
	var he *devices.HTTPStatusError
	if errors.As(err, &he) {
		return fmt.Sprintf("http %d", he.Code)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	msg := err.Error()
	if strings.Contains(msg, "certificate") || strings.Contains(msg, "x509") {
		return "tls verification failed"
	}
	var se *json.SyntaxError
	var ue *json.UnmarshalTypeError
	if errors.As(err, &se) || errors.As(err, &ue) || errors.Is(err, strconv.ErrSyntax) {
		return "invalid response"
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return "connection failed"
	}
	if strings.Contains(msg, "connection refused") || strings.Contains(msg, "no route") || strings.Contains(msg, "EOF") {
		return "connection failed"
	}
	return "error"
}

func pct(v any) *float64 {
	f := jsonx.NumV(v)
	if f == nil {
		return nil
	}
	return model.P(Round(*f*100, 1))
}

// ProxmoxOpts: prefix keeps ids unique across clusters (e.g. straw-qemu-100 vs qemu-100).
type ProxmoxOpts struct {
	IncludeTemplates bool
	Prefix           string
	ClusterID        *string
	Cluster          *string
	Group, Owner     *string
}

var guestStatus = map[string]string{"running": "online", "stopped": "stopped", "paused": "stopped", "suspended": "stopped"}

func NormaliseProxmox(resources []jsonx.M, now time.Time, ts float64, rates *RateTracker, o ProxmoxOpts) []model.System {
	out := []model.System{}
	for _, r := range resources {
		rtype := jsonx.S(r, "type")
		if rtype != "node" && rtype != "qemu" && rtype != "lxc" {
			continue
		}
		if jsonx.Truthy(r["template"]) && !o.IncludeTemplates {
			continue
		}
		statusRaw := jsonx.S(r, "status")
		var sid, name, status string
		var parent *string
		var vmid *int
		if rtype == "node" {
			node := jsonx.S(r, "node")
			if node == "" {
				node = strings.TrimPrefix(jsonx.S(r, "id"), "node/")
			}
			status = "offline"
			if statusRaw == "online" {
				status = "online"
			}
			sid, name = o.Prefix+"node-"+Slug(node), node
		} else {
			vf := jsonx.Num(r, "vmid")
			if vf == nil {
				if s := jsonx.S(r, "vmid"); s != "" {
					if n, err := strconv.Atoi(s); err == nil {
						f := float64(n)
						vf = &f
					}
				}
			}
			if vf == nil {
				continue
			}
			v := int(*vf)
			vmid = &v
			var ok bool
			if status, ok = guestStatus[statusRaw]; !ok {
				status = "offline" // "unknown" when its node is down
			}
			sid = fmt.Sprintf("%s%s-%d", o.Prefix, rtype, v)
			name = jsonx.S(r, "name")
			if name == "" {
				name = fmt.Sprintf("%s %d", rtype, v)
			}
			parent = jsonx.Str(r, "node")
		}
		live := status == "online"
		var rx, tx *float64
		if live {
			rx, tx = rates.Update(sid, ts, jsonx.Int(r, "netin"), jsonx.Int(r, "netout"), 15)
		}
		s := model.System{ID: sid, Name: name, Kind: rtype, Source: "proxmox", Parent: parent, VMID: vmid,
			Status: status, ClusterID: o.ClusterID, Cluster: o.Cluster, Group: o.Group, Owner: o.Owner,
			MemTotal: jsonx.Int(r, "maxmem"), DiskUsed: jsonx.Int(r, "disk"), DiskTotal: jsonx.Int(r, "maxdisk"),
			NetRxBps: rx, NetTxBps: tx, Tags: splitTags(jsonx.S(r, "tags")), PollMode: "default"}
		if live {
			s.CPUPercent = pct(r["cpu"])
			s.MemUsed = jsonx.Int(r, "mem")
			s.UptimeS = jsonx.Int(r, "uptime")
		}
		if c := jsonx.IntP(r, "maxcpu"); c != nil && *c != 0 {
			s.Cores = c
		}
		if status == "online" || status == "stopped" {
			s.LastSeen = model.P(now)
		}
		model.Normalize(&s)
		out = append(out, s)
	}
	return out
}

// Placeholders are SMBIOS strings that board vendors leave unfilled (compared case-insensitively, trimmed).
var Placeholders = map[string]bool{"": true, "system product name": true, "system manufacturer": true,
	"to be filled by o.e.m.": true, "to be filled by oem": true, "default string": true, "o.e.m.": true, "oem": true,
	"not applicable": true, "none": true, "n/a": true, "na": true, "unknown": true, "system version": true,
	"type1productconfigid": true, "not specified": true, "base board product name": true,
	"base board manufacturer": true, "all series": true}

var vendorShort = map[string]string{"asustek computer inc.": "ASUS", "asustek computer inc": "ASUS",
	"supermicro": "Supermicro", "super micro computer, inc.": "Supermicro", "micro-star international co., ltd.": "MSI",
	"gigabyte technology co., ltd.": "Gigabyte", "asrock": "ASRock", "dell inc.": "Dell", "lenovo": "Lenovo",
	"hp": "HP", "hewlett-packard": "HP"}

func realStr(v *string) *string {
	if v == nil {
		return nil
	}
	s := strings.TrimSpace(*v)
	if Placeholders[strings.ToLower(s)] {
		return nil
	}
	return &s
}

func trimmed(m jsonx.M, k string) *string {
	v, ok := m[k]
	if !ok || v == nil {
		return nil
	}
	var s string
	switch x := v.(type) {
	case string:
		s = x
	default:
		s = fmt.Sprint(x)
	}
	s = strings.TrimSpace(s)
	return &s
}

// HardwareDisplay: system vendor + model, falling back to the mainboard when the model is a placeholder
// ("System Product Name", "To be filled by O.E.M." ...), e.g. a Supermicro X11SCA-F in a custom build.
func HardwareDisplay(hw jsonx.M) *model.Hardware {
	if len(hw) == 0 {
		return nil
	}
	h := &model.Hardware{SystemVendor: trimmed(hw, "system_vendor"), SystemModel: trimmed(hw, "system_model"),
		BoardVendor: trimmed(hw, "board_vendor"), BoardModel: trimmed(hw, "board_model"),
		BIOSVersion: trimmed(hw, "bios_version")}
	mdl, board := realStr(h.SystemModel), realStr(h.BoardModel)
	var vendor *string
	if mdl != nil {
		vendor = realStr(h.SystemVendor)
	} else if board != nil {
		vendor, mdl = realStr(h.BoardVendor), board
	}
	if mdl != nil {
		short := ""
		if vendor != nil {
			short = *vendor
			if v, ok := vendorShort[strings.ToLower(*vendor)]; ok {
				short = v
			}
		}
		d := *mdl
		if short != "" && !strings.HasPrefix(strings.ToLower(d), strings.ToLower(short)) {
			d = short + " " + d
		}
		h.Display = &d
	}
	return h
}

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
	sec, frac := math.Modf(*at)
	return &model.PowerPending{Action: a, At: time.Unix(int64(sec), int64(frac*1e9)).UTC(), DelayS: d,
		RequestedBy: jsonx.Str(p, "requested_by")}
}

func objs(l []any) []jsonx.M {
	out := make([]jsonx.M, 0, len(l))
	for _, v := range l {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
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
	for _, d := range objs(jsonx.List(m, "disks")) {
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
	for _, t := range objs(jsonx.List(m, "temps")) {
		cur := jsonx.Num(t, "current")
		if cur == nil {
			continue
		}
		temps = append(temps, model.Temperature{Sensor: strOr(t, "sensor", "?"), Label: jsonx.Str(t, "label"),
			CurrentC: *cur, HighC: jsonx.Num(t, "high"), CriticalC: jsonx.Num(t, "critical")})
	}
	gpus := []model.Gpu{}
	for _, g := range objs(jsonx.List(m, "gpus")) {
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
		for _, s := range objs(jsonx.List(p, "sensors")) {
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
	for _, f := range objs(jsonx.List(m, "fans")) {
		if r := jsonx.Num(f, "rpm"); r != nil {
			fans = append(fans, model.Fan{Name: strOr(f, "name", "?"), RPM: *r})
		}
	}
	volts := []model.Voltage{}
	for _, v := range objs(jsonx.List(m, "voltages")) {
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

func or(a, b *string) *string {
	if a != nil && *a != "" {
		return a
	}
	return b
}

// ApplyOverride merges a systems: entry (name, tags, group, owner, dwservice_url).
func ApplyOverride(s model.System, o *config.SystemOverride) model.System {
	if o == nil {
		return s
	}
	if o.Name != "" {
		s.Name = o.Name
	}
	tags := slices.Clone(s.Tags)
	for _, t := range o.Tags {
		if !slices.Contains(tags, t) {
			tags = append(tags, t)
		}
	}
	if tags == nil {
		tags = []string{}
	}
	s.Tags = tags
	s.Group = or(o.Group, s.Group)
	s.Owner = or(o.Owner, s.Owner)
	if o.DWServiceURL != "" {
		s.DWServiceURL = model.P(o.DWServiceURL)
	}
	return s
}
