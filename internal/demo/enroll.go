package demo

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
)

// Add host in the demo: nobody runs the command, so the "machine" answers by itself a few seconds after the
// dialog opened, and then reports like a small Linux / Windows box.
var (
	pairMu    sync.Mutex
	pairFirst = map[string]time.Time{}
	paired    = map[string]string{} // host id -> platform
)

// PairDelay is how long the simulated machine takes to "run the command".
var PairDelay = 6 * time.Second

func token() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Pair simulates POST /pair on a new machine.
func Pair(_ context.Context, rawURL, proof string) (int, enroll.Response, string, error) {
	pairMu.Lock()
	defer pairMu.Unlock()
	first, ok := pairFirst[proof]
	if !ok {
		first = time.Now()
		pairFirst[proof] = first
	}
	if time.Since(first) < PairDelay {
		return 0, enroll.Response{}, "", errors.New("dial tcp: connect: connection refused")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, enroll.Response{}, "", err
	}
	id := managed.IDFor(u.Hostname())
	platform := "linux"
	if strings.Contains(strings.ToLower(u.Hostname()), "win") || strings.Contains(strings.ToLower(u.Hostname()), "furina") {
		platform = "windows"
	}
	paired[id] = platform
	return 200, enroll.Response{AgentVersion: "42-1a2b3c4", Hostname: u.Hostname(), Platform: platform,
		Features: map[string]bool{"processes": true, "power_control": true}, Token: token(), ControlToken: token()}, "", nil
}

// rememberPaired makes managed hosts from an earlier demo run report again.
func rememberPaired(entries []managed.Entry) {
	pairMu.Lock()
	defer pairMu.Unlock()
	for _, e := range entries {
		paired[e.Host.ID] = e.Host.OS
	}
}

func pairedPlatform(id string) (string, bool) {
	pairMu.Lock()
	defer pairMu.Unlock()
	p, ok := paired[id]
	return p, ok
}

// pairedMetrics is a plain machine added with Add host.
func pairedMetrics(id, platform string) map[string]any {
	t := since()
	pretty, sys := "Debian GNU/Linux 13 (trixie)", "Linux"
	disk := "/"
	if platform == "windows" {
		pretty, sys, disk = "Windows 11 Home 24H2", "Windows", `C:\`
	}
	return map[string]any{"agent_version": "42-1a2b3c4", "timestamp": epoch(), "load_avg": nil, "hostname": id,
		"features":      map[string]any{"processes": true, "cmdline": true, "fan_control": false, "power_control": true},
		"power_sources": []any{}, "uptime_s": int64(600 + t),
		"platform": map[string]any{"system": sys, "pretty": pretty},
		"cpu":      map[string]any{"percent": r1(wave(70, 3, 25, 1)), "logical": 12},
		"memory":   map[string]any{"total": int64(16 * GiB), "used": int64(wave(300, 5, 8, 0) * GiB)},
		"disks":    []any{map[string]any{"mount": disk, "fstype": "ext4", "total": int64(512 * GiB), "used": int64(180 * GiB)}},
		"net":      map[string]any{"bytes_recv": counter(id+"/rx", 1e9, wave(40, 1e4, 2e6, 0)), "bytes_sent": counter(id+"/tx", 5e8, wave(40, 1e4, 5e5, 0))},
		"temps":    []any{temp("k10temp", "Tctl", r1(wave(60, 40, 58, 0)))}, "gpus": []any{},
		"hardware": map[string]any{"system_vendor": "Micro-Star International Co., Ltd.", "system_model": "MS-7C56",
			"board_vendor": "Micro-Star International Co., Ltd.", "board_model": "B550-A PRO", "cpu_model": "AMD Ryzen 5 5600X 6-Core Processor"}}
}
