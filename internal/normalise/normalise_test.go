package normalise

import (
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

func i64(v int64) *int64 { return &v }

func TestRateTrackerHandlesReset(t *testing.T) {
	r := NewRateTracker()
	if a, b := r.Update("x", 0, i64(100), i64(100), 0); a != nil || b != nil {
		t.Fatal("first sample must have no rate")
	}
	a, b := r.Update("x", 5, i64(600), i64(350), 0)
	if *a != 100 || *b != 50 {
		t.Fatalf("got %v %v", *a, *b)
	}
	a, b = r.Update("x", 10, i64(10), i64(400), 0) // rx counter reset -> no bogus negative rate
	if a != nil || *b != 10 {
		t.Fatalf("got %v %v", a, *b)
	}
}

func TestRateTrackerHoldsUnchangedProxmoxCounters(t *testing.T) {
	r := NewRateTracker()
	r.Update("vm", 0, i64(0), i64(0), 15)
	a, _ := r.Update("vm", 10, i64(10000), i64(0), 15)
	if *a != 1000 {
		t.Fatal(*a)
	}
	a, _ = r.Update("vm", 12, i64(10000), i64(0), 15) // not refreshed yet: keep the last rate
	if a == nil || *a != 1000 {
		t.Fatalf("held rate %v", a)
	}
	a, _ = r.Update("vm", 20, i64(30000), i64(0), 15) // divided by the full 10 s since the last change
	if *a != 2000 {
		t.Fatal(*a)
	}
}

var pveResources = []jsonx.M{
	{"id": "node/SRV-PVEA", "type": "node", "node": "SRV-PVEA", "status": "online", "cpu": 0.125,
		"maxcpu": 8.0, "mem": float64(8 << 30), "maxmem": float64(32 << 30), "disk": float64(10 << 30), "maxdisk": float64(100 << 30), "uptime": 1000.0},
	{"id": "node/SRV-PVEB", "type": "node", "node": "SRV-PVEB", "status": "offline", "maxcpu": 12.0, "maxmem": float64(64 << 30)},
	{"id": "qemu/100", "type": "qemu", "vmid": 100.0, "name": "win11", "node": "SRV-PVEA", "status": "running",
		"cpu": 0.5, "maxcpu": 4.0, "mem": float64(4 << 30), "maxmem": float64(8 << 30), "disk": 0.0, "maxdisk": float64(64 << 30),
		"netin": 1000.0, "netout": 500.0, "uptime": 50.0, "tags": "windows;gaming", "template": 0.0},
	{"id": "lxc/101", "type": "lxc", "vmid": 101.0, "name": "minecraft", "node": "SRV-PVEA", "status": "stopped",
		"cpu": 0.0, "maxcpu": 2.0, "mem": 0.0, "maxmem": float64(1 << 31), "netin": 0.0, "netout": 0.0},
	{"id": "qemu/9000", "type": "qemu", "vmid": 9000.0, "name": "tmpl", "node": "SRV-PVEA", "status": "stopped", "template": 1.0},
	{"id": "qemu/120", "type": "qemu", "vmid": 120.0, "name": "truenas", "node": "SRV-PVEB", "status": "unknown"},
	{"id": "storage/SRV-PVEA/local", "type": "storage", "node": "SRV-PVEA", "status": "available"},
}

func TestNormaliseProxmox(t *testing.T) {
	list := NormaliseProxmox(pveResources, time.Now(), 0, NewRateTracker(), ProxmoxOpts{})
	s := map[string]model.System{}
	for _, x := range list {
		s[x.ID] = x
	}
	if len(s) != 5 || s["qemu-9000"].ID != "" {
		t.Fatalf("templates/storage must be skipped: %v", len(s))
	}
	n := s["node-srv-pvea"]
	if n.Kind != "node" || n.Status != "online" || *n.CPUPercent != 12.5 || *n.Cores != 8 || n.Parent != nil {
		t.Fatalf("node %+v", n)
	}
	vm := s["qemu-100"]
	if *vm.Parent != "SRV-PVEA" || *vm.VMID != 100 || *vm.CPUPercent != 50 || *vm.MemUsed != 4<<30 ||
		len(vm.Tags) != 2 || vm.Tags[0] != "windows" || vm.Tags[1] != "gaming" {
		t.Fatalf("vm %+v", vm)
	}
	if s["lxc-101"].Status != "stopped" || s["lxc-101"].CPUPercent != nil {
		t.Fatal("stopped guest")
	}
	if s["qemu-120"].Status != "offline" || s["node-srv-pveb"].Status != "offline" {
		t.Fatal("offline node / unknown guest")
	}
	pre := NormaliseProxmox(pveResources[:1], time.Now(), 0, NewRateTracker(), ProxmoxOpts{Prefix: "bob-"})
	if pre[0].ID != "bob-node-srv-pvea" {
		t.Fatal(pre[0].ID)
	}
}

func hw(kv ...string) jsonx.M {
	m := jsonx.M{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func TestHardwarePlaceholderFallback(t *testing.T) {
	cases := []struct {
		in   jsonx.M
		want string
	}{
		// custom build: SMBIOS placeholders -> mainboard
		{hw("system_vendor", "System manufacturer", "system_model", "System Product Name", "board_vendor", "Supermicro", "board_model", "X11SCA-F"), "Supermicro X11SCA-F"},
		{hw("system_vendor", "To be filled by O.E.M.", "system_model", "To be filled by O.E.M.", "board_vendor", "Gigabyte Technology Co., Ltd.", "board_model", "X670E AORUS MASTER"), "Gigabyte X670E AORUS MASTER"},
		// laptop: model already contains the brand
		{hw("system_vendor", "ASUSTeK COMPUTER INC.", "system_model", "Vivobook 16X K3605ZC", "board_model", "K3605ZC"), "ASUS Vivobook 16X K3605ZC"},
		{hw("system_vendor", "LENOVO", "system_model", "ThinkCentre M720q"), "Lenovo ThinkCentre M720q"},
		{hw("system_vendor", "Dell Inc.", "system_model", "Dell PowerEdge R730"), "Dell PowerEdge R730"},
		{hw("system_vendor", "ASUS", "system_model", "All Series", "board_vendor", "ASUSTeK COMPUTER INC.", "board_model", "Z97-A"), "ASUS Z97-A"},
		{hw("system_model", "Default string", "board_model", "  "), ""},
	}
	for _, c := range cases {
		h := HardwareDisplay(c.in)
		got := ""
		if h.Display != nil {
			got = *h.Display
		}
		if got != c.want {
			t.Errorf("%v: got %q want %q", c.in, got, c.want)
		}
	}
	if HardwareDisplay(nil) != nil {
		t.Error("nil hardware")
	}
}

func TestNormaliseAgentAndOverride(t *testing.T) {
	m := jsonx.M{"hostname": "laptop", "platform": jsonx.M{"system": "Linux", "pretty": "Ubuntu 24.04.1 LTS"}, "uptime_s": 3600.0,
		"cpu": jsonx.M{"percent": 12.5, "logical": 8.0}, "memory": jsonx.M{"total": float64(16 << 30), "used": float64(6 << 30)},
		"disks": []any{jsonx.M{"mount": "/", "fstype": "ext4", "total": float64(500 << 30), "used": float64(100 << 30)},
			jsonx.M{"mount": "/home", "total": float64(1000 << 30), "used": float64(300 << 30)}},
		"temps": []any{jsonx.M{"sensor": "coretemp", "label": "Package id 0", "current": 55.0}, jsonx.M{"sensor": "x"}},
		"gpus": []any{jsonx.M{"name": "", "vendor": "weird", "source": "elsewhere"},
			jsonx.M{"name": "AMD Radeon RX 5600 XT", "vendor": "amd", "source": "windows", "util_percent": 12.0}},
		"fans":          []any{jsonx.M{"name": "fan1", "rpm": 2400.0}, jsonx.M{"name": "bad", "rpm": true}},
		"power_control": jsonx.M{"pending": jsonx.M{"action": "shutdown", "at": 1.7e9, "delay_s": 120.0, "requested_by": "Alice"}},
		"features":      jsonx.M{"processes": false, "fan_control": true}, "hardware": hw("system_vendor", "LENOVO", "system_model", "T14", "cpu_model", "i7")}
	a := config.Agent{ID: "laptop", FanControl: &config.FanControl{Vendor: "supermicro"}}
	s := NormaliseAgent(a, m, time.Now(), 0, NewRateTracker())
	if s.Name != "laptop" || *s.DiskTotal != 1500<<30 || len(s.Temps) != 1 || len(s.Fans) != 1 || s.GPUs[0].Vendor != "other" ||
		s.GPUs[0].Name != "GPU" || s.ProcessesAvailable || !s.FanControl || *s.CPUModel != "i7" || s.PowerPending.DelayS != 120 {
		t.Fatalf("%+v", s)
	}
	if g := s.GPUs[1]; s.GPUs[0].Source != "" || g.Source != "windows" || g.Vendor != "amd" || g.TempC != nil || *g.UtilPercent != 12 {
		t.Fatalf("GPU via Windows %+v", g)
	}
	if s.LoadAvg != nil {
		t.Fatal("missing load_avg stays null")
	}
	o := &config.SystemOverride{ID: "laptop", Name: "Work laptop", Labels: config.Labels{Tags: []string{"x"}}, DWServiceURL: "https://d.example"}
	s = ApplyOverride(s, o)
	if s.Name != "Work laptop" || s.Tags[0] != "x" || *s.DWServiceURL != "https://d.example" {
		t.Fatalf("%+v", s)
	}
}

func TestSlug(t *testing.T) {
	if Slug("SRV-PVEA") != "srv-pvea" || Slug(" MINI i7 ") != "mini-i7" {
		t.Fatal("slug")
	}
}

func TestNormaliseAgentBattery(t *testing.T) {
	a := config.Agent{ID: "fedora"}
	// older agents (and machines without a battery) send no block
	if s := NormaliseAgent(a, jsonx.M{"hostname": "x"}, time.Now(), 0, NewRateTracker()); s.Battery != nil {
		t.Fatalf("%+v", s.Battery)
	}
	m := jsonx.M{"battery": jsonx.M{"percent": 81.5, "state": "discharging", "ac_online": false, "power_w": 12.3,
		"time_remaining_s": 9000.0, "health_pct": 91.2}}
	b := NormaliseAgent(a, m, time.Now(), 0, NewRateTracker()).Battery
	if b == nil || *b.Percent != 81.5 || b.State != "discharging" || *b.ACOnline || *b.PowerW != 12.3 ||
		*b.TimeRemainingS != 9000 || *b.HealthPct != 91.2 {
		t.Fatalf("%+v", b)
	}
	// garbage is dropped, an unknown state stays visible as "unknown"
	m = jsonx.M{"battery": jsonx.M{"percent": 140.0, "state": "exploding", "ac_online": "yes", "power_w": 1e6,
		"time_remaining_s": -5.0, "health_pct": 0.0}}
	b = NormaliseAgent(a, m, time.Now(), 0, NewRateTracker()).Battery
	if b == nil || b.Percent != nil || b.State != "unknown" || b.ACOnline != nil || b.PowerW != nil || b.TimeRemainingS != nil || b.HealthPct != nil {
		t.Fatalf("%+v", b)
	}
	if s := NormaliseAgent(a, jsonx.M{"battery": "nope"}, time.Now(), 0, NewRateTracker()); s.Battery != nil {
		t.Fatalf("%+v", s.Battery)
	}
}
