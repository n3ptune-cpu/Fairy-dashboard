package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/redact"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/ups"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
)

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
		g := gpuRow("NVIDIA GeForce RTX 2080 SUPER", 3, 85, 8, 18, 210, 38, 72, 0)
		set(map[string]any{"hostname": "NSX-CYRENE", "uptime_s": int64(86400*6 + t),
			"platform": map[string]any{"system": "Windows", "pretty": "Windows 11 Enterprise 24H2"},
			"cpu":      map[string]any{"percent": r1(wave(100, 8, 55, 0)), "logical": 16},
			"memory":   map[string]any{"total": int64(64 * GiB), "used": int64(wave(500, 20, 34, 0) * GiB)},
			"disks": []any{map[string]any{"mount": `C:\`, "fstype": "NTFS", "total": int64(1000 * GiB), "used": int64(412 * GiB)},
				map[string]any{"mount": `D:\`, "fstype": "NTFS", "total": int64(4000 * GiB), "used": int64(2710 * GiB)}},
			"net": map[string]any{"bytes_recv": counter(id+"/rx", 5e9, wave(40, 1e5, 9e6, 0)), "bytes_sent": counter(id+"/tx", 2e9, wave(40, 5e4, 3e6, 0))},
			"temps": []any{temp("IPMI", "CPU Temp", r1(wave(100, 38, 66, 0))), temp("IPMI", "PCH Temp", r1(wave(300, 44, 50, 0))),
				temp("IPMI", "System Temp", r1(wave(300, 29, 33, 0))), temp("IPMI", "Peripheral Temp", r1(wave(300, 31, 36, 0))),
				temp("IPMI", "VcpuVRM Temp", r1(wave(100, 36, 52, 0)))},
			"fans": []any{map[string]any{"name": "IPMI FAN1", "rpm": 400 + 100*math.Round(wave(100, 0, 6, 0))},
				map[string]any{"name": "IPMI FAN2", "rpm": 400 + 100*math.Round(wave(100, 0, 6, 0))}, map[string]any{"name": "IPMI FANA", "rpm": 900.0}},
			"voltages": []any{map[string]any{"name": "IPMI 12V", "volts": 12.09}, map[string]any{"name": "IPMI 5VCC", "volts": 5.03},
				map[string]any{"name": "IPMI 3.3VCC", "volts": 3.31}, map[string]any{"name": "IPMI Vcpu", "volts": 0.91}},
			"gpus": []any{g},
			// fairy-agent on Windows has no CPU package power (that needs a kernel driver): GPU only
			"power": map[string]any{"cpu_package_w": nil, "gpu_w": g["power_w"], "battery_w": nil, "battery_state": nil,
				"bmc_w": nil, "sensors": []any{}, "source": []any{"gpu"}},
			"hardware": map[string]any{"system_vendor": "System manufacturer", "system_model": "System Product Name",
				"board_vendor": "Supermicro", "board_model": "X11SCA-F", "bios_version": "1.8",
				"cpu_model": "Intel(R) Xeon(R) E-2278G CPU @ 3.40GHz"}})
		feat["fan_control"] = true
	case "neptune-laptop":
		g := gpuRow("NVIDIA GeForce RTX 3050 Laptop GPU", 0, 35, 4, 4, 38, 40, 63, 1.5)
		set(map[string]any{"hostname": "NEPTUNE-VIVOBOOK", "uptime_s": int64(3600*5 + t),
			"platform": map[string]any{"system": "Windows", "pretty": "Windows 11 Home 24H2"},
			"cpu":      map[string]any{"percent": r1(wave(50, 4, 35, 0)), "logical": 12},
			"memory":   map[string]any{"total": int64(16 * GiB), "used": int64(wave(300, 8, 12, 0) * GiB)},
			"disks":    []any{map[string]any{"mount": `C:\`, "fstype": "NTFS", "total": int64(512 * GiB), "used": int64(301 * GiB)}},
			"net":      map[string]any{"bytes_recv": counter(id+"/rx", 1e9, wave(30, 2e4, 2e6, 0)), "bytes_sent": counter(id+"/tx", 3e8, wave(30, 1e4, 4e5, 0))},
			"temps":    []any{temp("acpi", "ACPI thermal zone TZ00", r1(wave(60, 48, 74, 0)))},
			"gpus":     []any{g},
			"power": map[string]any{"cpu_package_w": nil, "gpu_w": g["power_w"], "battery_w": nil,
				"battery_state": "discharging", "bmc_w": nil, "sensors": []any{}, "source": []any{"battery", "gpu"}},
			"battery": map[string]any{"percent": float64(max(5, 78-int(t)/300)), "state": "discharging", "ac_online": false,
				"power_w": nil, "time_remaining_s": int64(max(300, 3*3600-t)), "health_pct": nil},
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
			"net": map[string]any{"bytes_recv": counter(id+"/rx", 3e10, 2e5+4e5*load), "bytes_sent": counter(id+"/tx", 1e10, 1e5+9e5*load)},
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
		set(map[string]any{"hostname": "asus-i7", "uptime_s": int64(86400*2 + t), "platform": map[string]any{"system": "Linux", "pretty": "Proxmox VE 9.0"},
			"cpu":    map[string]any{"percent": r1(wave(55, 8, 30, 4)), "logical": 8},
			"memory": map[string]any{"total": int64(32 * GiB), "used": int64(wave(250, 14, 19, 0) * GiB)},
			"disks":  []any{}, "net": map[string]any{"bytes_recv": 0, "bytes_sent": 0},
			"temps": []any{temp("coretemp", "Package id 0", r1(wave(60, 41, 58, 0)))}, "gpus": []any{},
			"ups": []any{row},
			"hardware": map[string]any{"system_vendor": "ASUS", "system_model": "All Series", "board_vendor": "ASUSTeK COMPUTER INC.",
				"board_model": "Z97-A", "bios_version": "2801", "cpu_model": "Intel(R) Core(TM) i7-4790K CPU @ 4.00GHz"}})
		feat["processes"], feat["cmdline"] = false, false // Straw keeps processes: {enabled: false}
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
		set(map[string]any{"hostname": nil, "uptime_s": int64(3600*30 + t), // owner set hide_host_details: true
			"platform": map[string]any{"system": "Windows", "pretty": "Windows"},
			"cpu":      map[string]any{"percent": r1(wave(45, 6, 70, 2)), "logical": 16},
			"memory":   map[string]any{"total": int64(32 * GiB), "used": int64(wave(250, 11, 22, 0) * GiB)},
			"disks":    []any{map[string]any{"mount": `C:\`, "fstype": "NTFS", "total": int64(2000 * GiB), "used": int64(1220 * GiB)}},
			"net":      map[string]any{"bytes_recv": counter(id+"/rx", 8e9, wave(35, 5e4, 6e6, 0)), "bytes_sent": counter(id+"/tx", 1e9, wave(35, 2e4, 9e5, 0))},
			// Ryzen + Radeon on Windows: no ACPI thermal zone, and fairy-agent reads NVIDIA GPUs only
			"temps": []any{}, "gpus": []any{},
			"power_sources": []any{map[string]any{"id": "desk-plug", "name": "Straw's Tuya desk plug", "type": "tuya", "ok": true,
				"watts": r1(wave(45, 95, 420, 0)), "voltage": 229.8, "current": nil, "energy_kwh_total": nil,
				"system_hint": "self", "error": nil}}})
		feat["processes"], feat["cmdline"] = false, false
	default:
		if platform, ok := pairedPlatform(id); ok {
			return pairedMetrics(id, platform), nil
		}
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
			name, user  string
			cpu, rssMiB float64
			_           float64 // VRAM MiB (Windows reports none per process)
		}
		var names []p
		switch id {
		case "nsx-cyrene":
			names = []p{{"Plex Media Server.exe", `NSX-CYRENE\plex`, 12, 900, 0}, {"obs64.exe", `NSX-CYRENE\neptune`, 9, 620, 410},
				{"chrome.exe", `NSX-CYRENE\neptune`, 6, 1400, 120}, {"MsMpEng.exe", `NT AUTHORITY\SYSTEM`, 4, 310, 0},
				{"Ollama.exe", `NSX-CYRENE\neptune`, 3, 5200, 5600}, {"dwagsvc.exe", `NT AUTHORITY\SYSTEM`, 1.2, 45, 0},
				{"fairy-agent.exe", `NT AUTHORITY\SYSTEM`, 0.6, 18, 0}, {"svchost.exe", `NT AUTHORITY\SYSTEM`, 0.4, 60, 0},
				{"System", `NT AUTHORITY\SYSTEM`, 0.9, 2, 0}, {"tailscaled.exe", `NT AUTHORITY\SYSTEM`, 0.3, 40, 0}}
		case "neptune-laptop": // the agent runs as LocalService here: other accounts' programs are listed by name only
			names = []p{{"Code.exe", "", 7, 900, 0}, {"firefox.exe", "", 11, 1600, 90},
				{"Discord.exe", "", 2, 420, 60}, {"fairy-agent.exe", `NT AUTHORITY\LOCAL SERVICE`, 0.5, 16, 0}}
		}
		// fairy-agent on Windows sends the program's path (never reads another process's memory for its
		// arguments) and no per-process VRAM (nvidia-smi reports none under WDDM).
		path := map[string]string{"Ollama.exe": `C:\Users\neptune\AppData\Local\Programs\Ollama\ollama.exe`,
			"obs64.exe":       `C:\Program Files\obs-studio\bin\64bit\obs64.exe`,
			"chrome.exe":      `C:\Program Files\Google\Chrome\Application\chrome.exe`,
			"fairy-agent.exe": `C:\Program Files\Fairy\fairy-agent.exe`, "System": "",
			"Plex Media Server.exe": `C:\Program Files\Plex\Plex Media Server\Plex Media Server.exe`,
			"tailscaled.exe":        `C:\Program Files\Tailscale\tailscaled.exe`}
		for i, x := range names {
			row := map[string]any{"pid": 1000 + i*412, "name": x.name, "username": nil, "status": "running",
				"cpu_percent": nil, "rss": nil, "mem_percent": nil, "gpu_mem": nil, "create_time": nil, "cmdline": nil}
			if x.user != "" { // the agent could open it (PROCESS_QUERY_LIMITED_INFORMATION)
				c, ok := path[x.name]
				if !ok {
					c = `C:\Windows\System32\` + x.name
				}
				row["username"], row["cpu_percent"], row["rss"] = x.user, r1(x.cpu*uniform(0.6, 1.5)), int64(x.rssMiB*(1<<20))
				row["mem_percent"], row["create_time"] = r2(x.rssMiB/655.36), t0-3600*float64(i+1)
				if c != "" {
					row["cmdline"] = c
				}
			}
			rows = append(rows, row)
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
