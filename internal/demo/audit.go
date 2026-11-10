package demo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// SeedAudit writes sample history for the demo's Audit page (only when the demo log is empty).
func SeedAudit(path string) error {
	if st, err := os.Stat(path); err == nil && st.Size() > 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	now := time.Now()
	type row struct {
		dt                  time.Duration
		actor               string
		sub                 any
		action, target      string
		params              map[string]any
		outcome, source, ip string
	}
	h := time.Hour
	rows := []row{
		{-48 * h, "root", nil, "token.create", "home-assistant", map[string]any{"id": "3fa1c2d9", "scope": "read", "owner": "system"}, "ok", "cli", "local"},
		{-48*h + time.Minute, "root", nil, "control.disable", "", map[string]any{"reason": "replacing PSU in SRV-TOWER"}, "ok", "cli", "local"},
		{-48*h + 90*time.Minute, "root", nil, "control.enable", "", map[string]any{}, "ok", "cli", "local"},
		{-26 * h, "alice@example.invalid", "demo-alice", "auth.login", "", map[string]any{}, "ok", "ui", "203.0.113.24"},
		{-26*h + 30*time.Second, "alice@example.invalid", "demo-alice", "auth.step_up", "", map[string]any{}, "ok", "ui", "203.0.113.24"},
		{-26*h + 40*time.Second, "alice@example.invalid", "demo-alice", "fan.mode", "srv-tower", map[string]any{"mode": "full"}, "ok", "ui", "203.0.113.24"},
		{-26*h + 70*time.Second, "alice@example.invalid", "demo-alice", "fan.duty", "srv-tower", map[string]any{"zone": 0, "percent": 45}, "ok", "ui", "203.0.113.24"},
		{-20 * h, "bob@example.invalid", "demo-bob", "auth.login", "", map[string]any{}, "ok", "ui", "198.51.100.7"},
		{-20*h + 15*time.Second, "bob@example.invalid", "demo-bob", "auth.denied", "/api/systems/srv-tower/power",
			map[string]any{"method": "POST"}, "403: not allowed to control", "ui", "198.51.100.7"},
		{-9 * h, "home-assistant (token, owner system)", "token:3fa1c2d9", "auth.denied", "/api/systems/llm-box/poll",
			map[string]any{"method": "PUT"}, "403: not allowed to control", "token:home-assistant", "100.64.0.31"},
		{-5 * h, "unknown@example.invalid", "x9", "auth.login", "", map[string]any{}, "denied: not in an allowed group", "ui", "192.0.2.50"},
		{-3 * h, "alice@example.invalid", "demo-alice", "settings.update", "", map[string]any{"background": "upload", "dim": 55}, "ok", "ui", "203.0.113.24"},
		{-2 * h, "alice@example.invalid", "demo-alice", "poll.set", "llm-box", map[string]any{"interval_s": 2}, "ok", "ui", "203.0.113.24"},
		{-90 * time.Minute, "alice@example.invalid", "demo-alice", "power.start", "lxc-122", map[string]any{"action": "start", "via": "proxmox-api"}, "ok", "ui", "203.0.113.24"},
		{-40 * time.Minute, "root", nil, "host.power_control.disable", "bob-pc", map[string]any{}, "ok", "cli", "local"},
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, r := range rows {
		b, _ := json.Marshal(map[string]any{"ts": now.Add(r.dt).UTC().Format("2006-01-02T15:04:05.000000-07:00"),
			"actor": r.actor, "sub": r.sub, "action": r.action, "target": r.target, "params": r.params,
			"outcome": r.outcome, "source": r.source, "ip": r.ip})
		if _, err := f.Write(append(b, '\n')); err != nil {
			return err
		}
	}
	return nil
}
