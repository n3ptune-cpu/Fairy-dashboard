package admin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/adminstate"
)

func (c *cli) healthData() map[string]any {
	b, err := os.ReadFile(filepath.Join(c.cfg.DataDir, "health.json"))
	if err != nil {
		return map[string]any{}
	}
	var h map[string]any
	if json.Unmarshal(b, &h) != nil {
		return map[string]any{}
	}
	return h
}

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func (c *cli) health() (int, error) {
	if err := c.load(); err != nil {
		return 1, err
	}
	h := c.healthData()
	if len(h) == 0 {
		fmt.Fprintf(c.out, "No %s: is the panel running (it writes this every 5 s)?\n", filepath.Join(c.cfg.DataDir, "health.json"))
		return 1, nil
	}
	written, _ := time.Parse(time.RFC3339Nano, str(h["written_at"]))
	age := time.Since(written).Seconds()
	state := "RUNNING"
	if age >= 30 {
		state = "STALE"
	}
	started := str(h["started_at"])
	if len(started) > 19 {
		started = started[:19]
	}
	fmt.Fprintf(c.out, "panel: %s (health written %.0fs ago, up since %s)\n", state, age, started)
	if cs := adminstate.New(c.cfg.DataDir).ControlDisabled(); cs != nil { // live state, not the 5 s snapshot
		r := cs.Reason
		if r == "" {
			r = "no reason"
		}
		fmt.Fprintf(c.out, "control: DISABLED by %s at %s (%s)\n", cs.By, cs.At.Format("2006-01-02T15:04:05"), r)
	} else {
		fmt.Fprintln(c.out, "control: enabled")
	}
	if sys, ok := h["systems"].(map[string]any); ok {
		var parts []string
		for _, k := range []string{"online", "stale", "offline", "stopped", "paused"} {
			parts = append(parts, fmt.Sprintf("%s %v", k, sys[k]))
		}
		fmt.Fprintln(c.out, "systems: "+strings.Join(parts, ", "))
	}
	var rows [][]string
	srcs, _ := h["sources"].([]any)
	for _, x := range srcs {
		s, _ := x.(map[string]any)
		st := "FAIL"
		if ok, _ := s["ok"].(bool); ok {
			st = "ok"
		}
		lat := "-"
		if l, ok := s["latency_ms"].(float64); ok {
			lat = fmt.Sprintf("%g ms", l)
		}
		rows = append(rows, []string{str(s["name"]), st, lat, agoISO(str(s["last_success"])), str(s["last_error"])})
	}
	for _, key := range []string{"power_sources", "ups"} {
		l, _ := h[key].([]any)
		prefix := "power:"
		if key == "ups" {
			prefix = "ups:"
		}
		for _, x := range l {
			s, _ := x.(map[string]any)
			rows = append(rows, []string{prefix + str(s["id"]), str(s["status"]), "-", "", str(s["error"])})
		}
	}
	fmt.Fprintln(c.out, table(rows, []string{"SOURCE", "STATE", "LATENCY", "LAST OK", "ERROR"}))
	if age >= 30 {
		return 1, nil
	}
	return 0, nil
}
