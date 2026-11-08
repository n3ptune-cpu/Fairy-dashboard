package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
)

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

// NewBackend returns a simulator for the demo configuration.
func NewBackend(cfg *config.Panel) *Backend {
	return &Backend{cfg: cfg, bmc: &simBMC{mode: "optimal", duty: map[int]int{}}, pending: map[string]map[string]any{},
		downUntil: map[string]float64{}, guestStatus: map[guestKey]string{}, guestPending: map[guestKey]guestChange{}}
}

// FetchProxmox returns simulated cluster/resources rows, with guests' simulated power state applied.
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

// FetchNodeStatus returns a simulated node status (CPU model).
func (b *Backend) FetchNodeStatus(_ context.Context, _ config.Proxmox, node string) (jsonx.M, error) {
	m, ok := nodeCPU[node]
	if !ok {
		m = "Unknown CPU"
	}
	return jsonx.M{"cpuinfo": map[string]any{"model": m, "cpus": 8.0, "sockets": 1.0}}, nil
}

// FetchAgentMetrics returns a simulated /metrics payload, or an error while the host is "down".
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

// PVEPower changes the simulated guest's state after a short delay (or shuts a node's agent down).
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

// FetchProcesses returns a simulated process list for the agent's host.
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

// AgentRequest answers the agent's fan, power and Wake-on-LAN endpoints from the simulation.
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
