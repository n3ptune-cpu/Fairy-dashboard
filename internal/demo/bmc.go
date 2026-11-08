package demo

import (
	"fmt"
	"math"
	"strconv"
	"sync"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
)

type simBMC struct {
	mu     sync.Mutex
	mode   string
	duty   map[int]int
	events []map[string]any
}

var modeRPM = map[string]float64{"standard": 900, "full": 1900, "optimal": 420, "heavyio": 1200}

func (b *simBMC) status() map[string]any {
	rpm := func(zone int, base float64) float64 {
		target := modeRPM[b.mode]
		if d, ok := b.duty[zone]; ok && b.mode == "full" {
			target = float64(d) / 100 * 1900
		}
		v := math.Round(target*base*uniform(0.97, 1.03)/100) * 100
		if v == 0 {
			v = 100
		}
		return v
	}
	duty := map[string]any{}
	for k, v := range b.duty {
		duty[strconv.Itoa(k)] = v
	}
	ev := b.events
	if len(ev) > 10 {
		ev = ev[len(ev)-10:]
	}
	return map[string]any{"enabled": true, "vendor": "supermicro", "mode": b.mode, "error": nil,
		"overridden": b.mode != "optimal", "duty": duty, "min_duty": 30, "max_temp_c": 80.0, "events": ev,
		"fans": []any{map[string]any{"name": "CPU_FAN1", "rpm": rpm(0, 1.0)}, map[string]any{"name": "CPU_FAN2", "rpm": rpm(0, 0.95)},
			map[string]any{"name": "SYS_FAN3", "rpm": rpm(0, 1.1)}}}
}

func (b *simBMC) handle(path string, body map[string]any) (int, jsonx.M) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch path {
	case "/fan/status":
		return 200, b.status()
	case "/fan/mode":
		mode, _ := body["mode"].(string)
		if _, ok := modeRPM[mode]; !ok {
			return 400, jsonx.M{"error": "bad mode"}
		}
		b.mode, b.duty = mode, map[int]int{}
		b.events = append(b.events, map[string]any{"t": epoch(), "action": "mode=" + mode, "detail": "api"})
		return 200, jsonx.M{"mode": mode}
	case "/fan/duty":
		zone, _ := body["zone"].(int)
		pct, ok := body["percent"].(int)
		if b.mode != "full" {
			return 409, jsonx.M{"error": fmt.Sprintf("fan mode is %s; set mode 'full' first (duty is ignored otherwise)", b.mode)}
		}
		if !ok || pct < 30 || pct > 100 {
			return 400, jsonx.M{"error": "percent must be an integer between 30 and 100"}
		}
		b.duty[zone] = pct
		b.events = append(b.events, map[string]any{"t": epoch(), "action": fmt.Sprintf("duty zone %d=%d%%", zone, pct), "detail": "api"})
		return 200, jsonx.M{"zone": zone, "percent": pct}
	}
	return 404, jsonx.M{"error": "not found"}
}
