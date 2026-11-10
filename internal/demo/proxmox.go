package demo

import (
	"fmt"
	"os"
	"strconv"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
)

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
		key := fmt.Sprintf("%s/%s/%d", node, name, vmid)
		r["netin"] = float64(counter(key+"/rx", v*1e6, wave(45, 2e4, 4e6, v)))
		r["netout"] = float64(counter(key+"/tx", v*5e5, wave(50, 1e4, 1.5e6, v+1)))
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

var mainNodes = []nodeSpec{{"SRV-PVEC", 16, 64, 0.18, 0.55}, {"SRV-PVEA", 8, 32, 0.05, 0.2}, {"SRV-PVEB", 12, 64, 0.1, 0.35}}

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
		guest(100, "qemu", "win11-gaming", "SRV-PVEC", 8, 16, 256, 0.05, 0.6, "running", "windows;vm"),
		guest(101, "lxc", "minecraft", "SRV-PVEC", 4, 8, 32, 0.25, 0.85, "running", "games"),
		guest(102, "lxc", "jellyfin", "SRV-PVEC", 4, 4, 16, 0.02, 0.4, "running", "media"),
		guest(110, "lxc", "fairy", "SRV-PVEA", 1, 0.5, 4, 0.01, 0.04, "running", "infra"),
		guest(111, "lxc", "pocket-id", "SRV-PVEA", 1, 0.5, 4, 0.0, 0.02, "running", "infra;auth"),
		guest(120, "qemu", "truenas", "SRV-PVEB", 4, 16, 32, 0.03, 0.15, "running", "storage"),
		guest(121, "qemu", "home-assistant", "SRV-PVEB", 2, 4, 32, 0.02, 0.1, "running", "iot"),
		guest(122, "lxc", "backup-runner", "SRV-PVEB", 2, 2, 8, 0, 0, "stopped", "backup"),
		jsonx.M{"id": "storage/SRV-PVEA/local", "type": "storage", "node": "SRV-PVEA", "status": "available"})
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

func bobResources() []jsonx.M {
	t := since()
	return []jsonx.M{
		{"id": "node/MINI-i7", "type": "node", "node": "MINI-i7", "status": "online", "cpu": wave(55, 0.08, 0.3, 4),
			"maxcpu": 8.0, "mem": float64(int64(wave(250, 14, 19, 0) * GiB)), "maxmem": float64(32 * GiB),
			"disk": float64(61 * GiB), "maxdisk": float64(220 * GiB), "uptime": float64(int64(86400*2 + t))},
		guest(100, "lxc", "pihole", "MINI-i7", 1, 1, 8, 0.01, 0.06, "running", "dns"),
		guest(101, "lxc", "plex", "MINI-i7", 4, 4, 32, 0.05, 0.35, "running", "media"),
		guest(102, "qemu", "win10-steam", "MINI-i7", 4, 8, 128, 0, 0, "stopped", "games"),
	}
}

// newNodeResources is a Proxmox VE node added in the demo with Add host ("Yes: a new node").
func newNodeResources(node string) []jsonx.M {
	t := since()
	return []jsonx.M{
		{"id": "node/" + node, "type": "node", "node": node, "status": "online", "cpu": wave(50, 0.05, 0.25, 7),
			"maxcpu": 12.0, "mem": float64(int64(wave(260, 9, 14, 1) * GiB)), "maxmem": float64(32 * GiB),
			"disk": float64(40 * GiB), "maxdisk": float64(240 * GiB), "uptime": float64(int64(86400 + t))},
		guest(100, "lxc", "home-assistant", node, 2, 2, 16, 0.02, 0.2, "running", "home"),
		guest(101, "qemu", "nas", node, 4, 8, 64, 0.03, 0.3, "running", "storage"),
	}
}

var nodeCPU = map[string]string{"SRV-PVEC": "AMD Ryzen 9 5950X 16-Core Processor",
	"SRV-PVEA": "Intel(R) Core(TM) i7-8700T CPU @ 2.40GHz", "SRV-PVEB": "AMD Ryzen 7 5700G with Radeon Graphics",
	"MINI-i7": "Intel(R) Core(TM) i7-4790K CPU @ 4.00GHz"}
