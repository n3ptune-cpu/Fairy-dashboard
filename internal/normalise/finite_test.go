package normalise

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

func f(v float64) *float64 { return &v }

func TestFiniteClampAndRound(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	for _, v := range []float64{nan, inf, -inf, 1e308, -1e308, 101, -0.001} {
		if InRange(v, 0, 100) || Finite(f(v), 0, 100) != nil {
			t.Errorf("%v accepted as 0-100", v)
		}
	}
	for _, v := range []float64{0, 100, 12.5} {
		if p := Finite(f(v), 0, 100); p == nil || *p != v {
			t.Errorf("%v refused", v)
		}
	}
	if Finite(nil, 0, 1) != nil || Clamp(nil, 0, 1) != nil || Reading(nil) != nil {
		t.Error("nil must stay nil")
	}
	for _, tc := range []struct{ in, want float64 }{{250, 100}, {-3, 0}, {1e308, 100}, {-1e308, 0}, {42.5, 42.5}} {
		if p := Clamp(f(tc.in), 0, 100); p == nil || *p != tc.want {
			t.Errorf("Clamp(%v) = %v", tc.in, p)
		}
	}
	for _, v := range []float64{nan, inf, -inf} {
		if Clamp(f(v), 0, 100) != nil {
			t.Errorf("Clamp(%v) must be nil", v)
		}
	}
	if Reading(f(1e9)) == nil || Reading(f(-1e9)) == nil || Reading(f(1.0000001e9)) != nil || Reading(f(nan)) != nil {
		t.Error("Reading")
	}
	// rounding never makes an infinity out of a number
	for _, v := range []float64{1e308, -1e308, math.MaxFloat64, 1.7976931348623157e307} {
		if r := Round(v, 1); math.IsInf(r, 0) || math.IsNaN(r) || r != v {
			t.Errorf("Round(%v) = %v", v, r)
		}
	}
	if Round(12.345, 1) != 12.3 || Round(-0.05, 1) != -0.1 || Round(80, 1) != 80 {
		t.Error("Round changed for normal numbers")
	}
	if AddBytes(math.MaxInt64, 1) != math.MaxInt64 || AddBytes(math.MinInt64, -1) != math.MinInt64 || AddBytes(2, 3) != 5 || AddBytes(-2, 3) != 1 {
		t.Error("AddBytes")
	}
}

// A time an agent reports can always be written as JSON (M2, rerunB-36).
func TestEpochTimeIsAlwaysEncodable(t *testing.T) {
	for _, sec := range []float64{1e12, 253402300800, -1e12, -62135596801, math.NaN(), math.Inf(1), math.Inf(-1), 1e308, 0, 1.7e9, 1e10} {
		at := EpochTime(sec)
		if _, err := json.Marshal(at); err != nil {
			t.Errorf("EpochTime(%v): %v", sec, err)
		}
		if y := at.Year(); y < 1970 || y > 2286 {
			t.Errorf("EpochTime(%v) is in the year %d", sec, y)
		}
	}
	if !EpochTime(1.7e9 + 0.5).Equal(time.Unix(1_700_000_000, 5e8)) {
		t.Error("EpochTime changed for normal times")
	}
}

// encodes fails the test when s cannot be written as JSON, and returns it decoded again.
func encodes(t *testing.T, what string, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s cannot be written as JSON: %v", what, err)
	}
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

// Impossible numbers and dates from one agent (M2): each of them is dropped or limited where it enters, the
// rest of the host's entry stays, and the entry can always be written.
func TestHostileAgentNumbers(t *testing.T) {
	a := config.Agent{ID: "evil"}
	norm := func(m jsonx.M) model.System {
		s := NormaliseAgent(a, m, time.Now(), 5, NewRateTracker())
		encodes(t, "agent entry", s)
		return s
	}
	// the audit's input: a pending action in the year 33658 fails the whole system list
	for _, at := range []float64{1e12, 253402300800, 1e10, 0, -1, -1e12, 1e308} {
		s := norm(jsonx.M{"power_control": jsonx.M{"pending": jsonx.M{"action": "sleep", "at": at, "delay_s": 1e300}}})
		if s.PowerPending != nil {
			t.Errorf("pending.at %v kept: %+v", at, s.PowerPending)
		}
	}
	if s := norm(jsonx.M{"power_control": jsonx.M{"pending": jsonx.M{"action": "reboot", "at": 1.9e9, "delay_s": 1e300}}}); s.PowerPending == nil ||
		s.PowerPending.DelayS != 0 || s.PowerPending.At.Unix() != 1_900_000_000 {
		t.Errorf("%+v", s.PowerPending)
	}
	big := 1e308
	m := jsonx.M{"uptime_s": -5.0,
		"cpu": jsonx.M{"percent": 250.0, "logical": big}, "memory": jsonx.M{"total": big, "used": -1.0},
		"net": jsonx.M{"bytes_recv": big, "bytes_sent": -4.0},
		"disks": []any{jsonx.M{"mount": "/", "total": big, "used": 1.0}, jsonx.M{"mount": "/neg", "total": 5.0, "used": -1.0},
			jsonx.M{"mount": "/ok", "total": 100.0, "used": 40.0}},
		"temps": []any{jsonx.M{"sensor": "a", "current": big}, jsonx.M{"sensor": "b", "current": 45.0, "high": big, "critical": -big}},
		"fans":  []any{jsonx.M{"name": "f", "rpm": big}, jsonx.M{"name": "g", "rpm": 1200.0}}, "voltages": []any{jsonx.M{"name": "v", "volts": -big}, jsonx.M{"name": "-12V", "volts": -11.9}},
		"gpus":     []any{jsonx.M{"name": "g", "vendor": "nvidia", "util_percent": 250.0, "vram_used": -1.0, "vram_total": big, "temp_c": big, "power_w": -big}},
		"load_avg": []any{big, -1.0, 0.5},
		"power":    jsonx.M{"cpu_package_w": big, "gpu_w": -big, "battery_w": 12.0, "bmc_w": big, "sensors": []any{jsonx.M{"name": "s", "watts": big}, jsonx.M{"name": "ok", "watts": 9.5}}},
		"battery":  jsonx.M{"percent": big, "power_w": big, "time_remaining_s": big, "health_pct": big}}
	s := norm(m)
	if *s.CPUPercent != 100 || s.Cores != nil || s.MemTotal != nil || s.MemUsed != nil || s.UptimeS != nil {
		t.Errorf("cpu / memory / uptime: %v %v %v %v %v", *s.CPUPercent, s.Cores, s.MemTotal, s.MemUsed, s.UptimeS)
	}
	if len(s.Disks) != 1 || s.Disks[0].Mount != "/ok" || *s.DiskTotal != 100 || *s.DiskUsed != 40 {
		t.Errorf("disks %+v", s.Disks)
	}
	if len(s.Temps) != 1 || s.Temps[0].Sensor != "b" || s.Temps[0].HighC != nil || s.Temps[0].CriticalC != nil {
		t.Errorf("temps %+v", s.Temps)
	}
	if len(s.Fans) != 1 || s.Fans[0].RPM != 1200 || len(s.Voltages) != 1 || s.Voltages[0].Volts != -11.9 {
		t.Errorf("fans %+v voltages %+v", s.Fans, s.Voltages)
	}
	if g := s.GPUs[0]; *g.UtilPercent != 100 || g.VRAMUsed != nil || g.VRAMTotal != nil || g.TempC != nil || g.PowerW != nil {
		t.Errorf("gpu %+v", g)
	}
	if len(s.LoadAvg) != 1 || s.LoadAvg[0] != 0.5 {
		t.Errorf("load %v", s.LoadAvg)
	}
	if p := s.ComponentPower; p.CPUPackageW != nil || p.GPUW != nil || *p.BatteryW != 12 || p.BMCW != nil || len(p.Sensors) != 1 || p.Sensors[0].Watts != 9.5 {
		t.Errorf("power %+v", p)
	}
	if b := s.Battery; b.Percent != nil || b.PowerW != nil || b.TimeRemainingS != nil || b.HealthPct != nil {
		t.Errorf("battery %+v", b)
	}
	// byte counters: two possible counters and a tiny time step do not make an impossible rate
	r := NewRateTracker()
	r.Update("x", 1, i64(0), i64(0), 0)
	if rx, tx := r.Update("x", 1+1e-12, i64(MaxCounter), i64(1000), 0); rx != nil || tx == nil {
		t.Errorf("rates %v %v", rx, tx)
	}
	// 64 disks of the largest size: the sums stay sizes
	disks := []any{}
	for range 500 {
		disks = append(disks, jsonx.M{"mount": "/d", "total": float64(MaxBytes), "used": float64(MaxBytes)})
	}
	if s := norm(jsonx.M{"disks": disks}); *s.DiskTotal != MaxDisks*MaxBytes || *s.DiskUsed <= 0 {
		t.Errorf("disk sum %d", *s.DiskTotal)
	}
}

// What a normal agent sends is not touched by the checks (the owner updates machines one at a time: a new panel
// must show old agents exactly as before).
func TestNormalAgentNumbersAreUnchanged(t *testing.T) {
	m := jsonx.M{"uptime_s": 3600.9, "cpu": jsonx.M{"percent": 12.5, "logical": 8.0}, "memory": jsonx.M{"total": float64(16 << 30), "used": float64(6 << 30)},
		"net":   jsonx.M{"bytes_recv": 15000.0, "bytes_sent": 6000.0},
		"temps": []any{jsonx.M{"sensor": "coretemp", "current": -3.5, "high": 90.0, "critical": 100.0}},
		"fans":  []any{jsonx.M{"name": "fan1", "rpm": 0.0}}, "voltages": []any{jsonx.M{"name": "-12V", "volts": -12.1}},
		"gpus":          []any{jsonx.M{"name": "g", "vendor": "amd", "util_percent": 0.0, "vram_used": float64(300 << 20), "vram_total": float64(4 << 30), "temp_c": 45.0, "power_w": 6.5}},
		"load_avg":      []any{0.5, 0.4, 0.3},
		"power":         jsonx.M{"cpu_package_w": 9.5, "gpu_w": 6.5, "battery_w": -15.2, "bmc_w": 180.0},
		"power_control": jsonx.M{"pending": jsonx.M{"action": "shutdown", "at": 1.7e9, "delay_s": 120.0}}}
	rates := NewRateTracker()
	rates.Update("a", 0, i64(10000), i64(5000), 0)
	s := NormaliseAgent(config.Agent{ID: "a"}, m, time.Now(), 5, rates)
	if *s.CPUPercent != 12.5 || *s.Cores != 8 || *s.MemTotal != 16<<30 || *s.MemUsed != 6<<30 || *s.UptimeS != 3600 ||
		*s.NetRxBps != 1000 || *s.NetTxBps != 200 || s.Temps[0].CurrentC != -3.5 || *s.Temps[0].HighC != 90 || s.Fans[0].RPM != 0 ||
		s.Voltages[0].Volts != -12.1 || *s.GPUs[0].UtilPercent != 0 || *s.GPUs[0].VRAMTotal != 4<<30 || *s.GPUs[0].PowerW != 6.5 ||
		len(s.LoadAvg) != 3 || *s.ComponentPower.BatteryW != -15.2 || *s.ComponentPower.BMCW != 180 ||
		s.PowerPending.DelayS != 120 || s.PowerPending.At.Unix() != 1_700_000_000 {
		t.Fatalf("%+v", s)
	}
}

// Impossible numbers from the Proxmox API (M2, rerunB-39 and rerunB-40): a processor value that overflows when
// it becomes a percentage, negative and absurd sizes, ids Proxmox itself would not accept.
func TestHostileProxmoxNumbers(t *testing.T) {
	big := 1e308
	rows := []jsonx.M{
		{"type": "node", "node": "n1", "status": "online", "cpu": 1.8e307, "maxcpu": big, "mem": -1.0, "maxmem": big, "disk": -1.0, "maxdisk": big, "uptime": -1.0},
		{"type": "node", "node": "n2", "status": "online", "cpu": 2.5, "maxcpu": 8.0, "mem": 4.0, "maxmem": 8.0, "uptime": 60.0},
		{"type": "qemu", "vmid": 100.0, "node": "n1", "status": "running", "cpu": -0.5, "netin": big, "netout": -1.0},
		{"type": "qemu", "vmid": -5.0, "node": "n1", "status": "running"},
		{"type": "qemu", "vmid": big, "node": "n1", "status": "running"},
		{"type": "lxc", "vmid": 0.0, "node": "n1", "status": "running"},
		{"type": "lxc", "vmid": 1e9, "node": "n1", "status": "running"},
		{"type": "lxc", "vmid": "101", "node": "n1", "status": "running", "cpu": 0.25},
		{"type": "lxc", "vmid": "-7", "node": "n1", "status": "running"},
	}
	list := NormaliseProxmox(rows, time.Now(), 5, NewRateTracker(), ProxmoxOpts{})
	encodes(t, "proxmox list", model.SystemList{Systems: list})
	by := map[string]model.System{}
	for _, s := range list {
		by[s.ID] = s
	}
	if len(list) != 4 || by["qemu-100"].ID == "" || by["lxc-101"].ID == "" {
		t.Fatalf("ids kept: %v", len(list))
	}
	if n := by["node-n1"]; n.CPUPercent != nil || n.Cores != nil || n.MemUsed != nil || n.MemTotal != nil || n.DiskUsed != nil || n.DiskTotal != nil || n.UptimeS != nil {
		t.Errorf("%+v", n)
	}
	if n := by["node-n2"]; *n.CPUPercent != 100 || *n.Cores != 8 || *n.MemUsed != 4 || *n.UptimeS != 60 {
		t.Errorf("%+v", n)
	}
	if *by["qemu-100"].CPUPercent != 0 || *by["lxc-101"].CPUPercent != 25 || *by["lxc-101"].VMID != 101 {
		t.Errorf("%+v", by["qemu-100"])
	}
	for _, tc := range []struct {
		v    any
		want int
	}{{100.0, 100}, {"100", 100}, {999999999.0, 999999999}, {0.0, 0}, {-1.0, 0}, {1e9, 0}, {big, 0}, {"abc", 0}, {nil, 0}, {true, 0}} {
		got := 0
		if p := VMID(jsonx.M{"vmid": tc.v}); p != nil {
			got = *p
		}
		if got != tc.want {
			t.Errorf("VMID(%v) = %d", tc.v, got)
		}
	}
}

// FuzzNormalise: whatever an agent or the Proxmox API answers, the result can be written as JSON and stays
// within the limits. Run as a normal test it checks the seeds below; `go test -fuzz=FuzzNormalise` searches on.
func FuzzNormalise(f *testing.F) {
	for _, seed := range []string{
		`{}`, `null`, `[]`, `{"cpu":{"percent":1e308,"logical":-1e308},"memory":{"total":1e999}}`,
		`{"power_control":{"pending":{"action":"sleep","at":1e12}}}`,
		`{"power_control":{"pending":{"action":"shutdown","at":-62135596801,"delay_s":1e308}}}`,
		`{"disks":[{"mount":"/","total":1e308,"used":1e308},{"mount":"/b","total":9007199254740992,"used":9007199254740992}]}`,
		`{"temps":[{"sensor":7,"current":1e308}],"fans":[{"rpm":-1e308}],"load_avg":[1e308,1e308,1e308,1]}`,
		`{"gpus":[{"util_percent":250,"vram_used":-1,"temp_c":1e400}],"battery":{"percent":1e308}}`,
		`{"type":"node","node":"n","status":"online","cpu":1.8e307,"maxcpu":1e308,"maxmem":-1}`,
		`{"type":"qemu","vmid":-5,"status":"running","netin":1e308}`,
		`{"helper":{"state":"running","since":"99999-01-01T00:00:00Z"},"net":{"bytes_recv":1e308,"bytes_sent":1e19}}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var m jsonx.M
		if json.Unmarshal(data, &m) != nil {
			return
		}
		rates := NewRateTracker()
		for _, ts := range []float64{1, 1 + 1e-9, 2} { // the second and third poll make rates
			s := NormaliseAgent(config.Agent{ID: "a"}, m, time.Now(), ts, rates)
			b, err := json.Marshal(s)
			if err != nil {
				t.Fatalf("agent entry cannot be written: %v", err)
			}
			if len(s.Disks) > MaxDisks || len(s.Temps) > MaxTemps || len(s.Fans) > MaxFans || len(s.Voltages) > MaxVoltages ||
				len(s.GPUs) > MaxGPUs || len(s.LoadAvg) > MaxLoadAvg || len(b) > 512<<10 {
				t.Fatalf("limits: %d bytes", len(b))
			}
			if p := s.CPUPercent; p != nil && !InRange(*p, 0, 100) {
				t.Fatalf("cpu %v", *p)
			}
			for _, v := range []*int64{s.MemUsed, s.MemTotal, s.DiskUsed, s.DiskTotal, s.UptimeS} {
				if v != nil && *v < 0 {
					t.Fatalf("negative size or uptime %d", *v)
				}
			}
			for _, v := range []*float64{s.NetRxBps, s.NetTxBps} {
				if v != nil && !InRange(*v, 0, MaxCounter) {
					t.Fatalf("rate %v", *v)
				}
			}
			for _, row := range NormaliseProxmox([]jsonx.M{m}, time.Now(), ts, rates, ProxmoxOpts{}) {
				if _, err := json.Marshal(row); err != nil {
					t.Fatalf("proxmox entry cannot be written: %v", err)
				}
				if p := row.CPUPercent; p != nil && !InRange(*p, 0, 100) {
					t.Fatalf("cpu %v", *p)
				}
				if v := row.VMID; v != nil && (*v < 1 || *v > MaxVMID) {
					t.Fatalf("vmid %d", *v)
				}
			}
		}
	})
}
