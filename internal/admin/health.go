package admin

import (
	"fmt"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy/internal/health"
	"github.com/n3ptune-cpu/Fairy/internal/model"
)

// health prints the panel's last health report (`fairy admin health`). Exit code 1 when the report is
// missing or older than 30 s (the panel rewrites it every 5 s).
func (c *cli) health() (int, error) {
	if err := c.load(); err != nil {
		return 1, err
	}
	h, err := health.Read(c.cfg.DataDir)
	if err != nil || h.WrittenAt == "" {
		fmt.Fprintf(c.out, "No %s: is the panel running (it writes this every 5 s)?\n", health.Path(c.cfg.DataDir))
		return 1, nil
	}
	written, _ := time.Parse(time.RFC3339Nano, h.WrittenAt)
	age := time.Since(written).Seconds()
	state := "RUNNING"
	if age >= 30 {
		state = "STALE"
	}
	started := h.StartedAt
	if len(started) > 19 {
		started = started[:19]
	}
	fmt.Fprintf(c.out, "panel: %s (health written %.0fs ago, up since %s)\n", state, age, started)
	c.printControlState()
	if h.Systems != nil {
		var parts []string
		for _, k := range []string{"online", "stale", "offline", "stopped", "paused"} {
			parts = append(parts, fmt.Sprintf("%s %v", k, h.Systems[k]))
		}
		fmt.Fprintln(c.out, "systems: "+strings.Join(parts, ", "))
	}
	fmt.Fprintln(c.out, table(sourceRows(h), []string{"SOURCE", "STATE", "LATENCY", "LAST OK", "ERROR"}))
	if age >= 30 {
		return 1, nil
	}
	return 0, nil
}

// printControlState prints the live kill-switch state from state.json (not the 5 s old health snapshot).
func (c *cli) printControlState() {
	cs := adminstate.New(c.cfg.DataDir).ControlDisabled()
	if cs == nil {
		fmt.Fprintln(c.out, "control: enabled")
		return
	}
	r := cs.Reason
	if r == "" {
		r = "no reason"
	}
	fmt.Fprintf(c.out, "control: DISABLED by %s at %s (%s)\n", cs.By, cs.At.Format("2006-01-02T15:04:05"), r)
}

// sourceRows is the health table: polled sources (Proxmox, agents), then power sources, then UPSes.
func sourceRows(h health.Report) [][]string {
	var rows [][]string
	for _, s := range h.Sources {
		st := "FAIL"
		if s.OK {
			st = "ok"
		}
		lat := "-"
		if s.LatencyMS != nil {
			lat = fmt.Sprintf("%g ms", *s.LatencyMS)
		}
		rows = append(rows, []string{s.Name, st, lat, ago(s.LastSuccess), model.Deref(s.LastError)})
	}
	for _, s := range h.PowerSources {
		rows = append(rows, []string{"power:" + s.ID, s.Status, "-", "", model.Deref(s.Error)})
	}
	for _, s := range h.UPS {
		rows = append(rows, []string{"ups:" + s.ID, s.Status, "-", "", model.Deref(s.Error)})
	}
	return rows
}
