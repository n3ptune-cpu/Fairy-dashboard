package demo

import (
	"context"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/power"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/ups"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

type demoPower struct {
	power.Defaults
	b                            power.Base
	baseW, swingW, period, volts float64
	profile                      string
}

func (d *demoPower) Info() power.Base { return d.b }

func (d *demoPower) DefaultInterval() float64 { return 5 }

// a plug that should not be asked more often than every 3 s
func (d *demoPower) FloorInterval() float64 { return 3 }

func (d *demoPower) Read(context.Context, time.Duration) ([]model.PowerReading, error) {
	var w float64
	if d.profile == "llm" {
		w = r1(d.baseW + d.swingW*llmLoad() + uniform(-8, 8))
	} else {
		w = r1(wave(d.period, d.baseW, d.baseW+d.swingW, float64(len(d.b.ID))))
	}
	v := r1(d.volts + uniform(-2, 2))
	a := math.Round(w/v*1000) / 1000
	e := math.Round((1234.5+since()*w/3.6e6)*1000) / 1000
	return []model.PowerReading{power.NewReading(d.b, devices.Reading{Watts: &w, Voltage: &v, Current: &a, EnergyKWh: &e})}, nil
}

func num(raw map[string]any, k string, def float64) float64 {
	if v, ok := raw[k]; ok {
		switch x := v.(type) {
		case int:
			return float64(x)
		case float64:
			return x
		}
	}
	return def
}

const outagePeriod, outageStart, outageLen = 150.0, 90.0, 40.0

// cyberpowerVars: CP1500EPFCLCD-style usbhid-ups variables (no ups.realpower, like many CyberPower units).
func cyberpowerVars() map[string]string {
	t := math.Mod(since(), outagePeriod)
	load := math.Round(wave(40, 22, 28, 0))
	onBatt := t >= outageStart && t < outageStart+outageLen
	charge := 100.0
	if onBatt {
		charge = 100 - 0.2*(t-outageStart)
	} else if t >= outageStart+outageLen {
		charge = math.Min(100, 92+0.4*(t-outageStart-outageLen))
	}
	status := "OL"
	if onBatt {
		status = "OB DISCHRG"
	} else if charge < 100 {
		status = "OL CHRG"
	}
	bv, in, out := 27.1, f1(wave(60, 229, 233, 0)), f1(wave(60, 229, 233, 0))
	if onBatt {
		bv, in, out = 23.6-(100-charge)*0.05, "0.0", "230.0"
	}
	v := map[string]string{"ups.status": status, "battery.charge": f0(charge),
		"battery.runtime": strconv.Itoa(int(2100 * charge / 100 * 25 / load)), "battery.voltage": f1(bv),
		"ups.load": f0(load), "ups.realpower.nominal": "900", "input.voltage": in, "output.voltage": out,
		"ups.model": "CP1500EPFCLCD", "ups.mfr": "CPS", "ups.serial": "CRBDEMO0000123",
		"battery.mfr.date": "2023/05/14", "ups.test.result": "Done and passed", "device.type": "ups",
		"driver.name": "usbhid-ups"}
	if onBatt {
		v["input.transfer.reason"] = "input voltage out of range"
	}
	return v
}

// rackUPS: backend-polled rack UPS that reports a measured ups.realpower (APC-style).
func rackUPS(context.Context, time.Duration) ([]map[string]any, error) {
	load := math.Round(wave(200, 28, 36, 0))
	return []map[string]any{ups.RowMap(devices.UPSFromVars("rack", map[string]string{
		"ups.status": "OL", "battery.charge": "100", "battery.runtime": strconv.Itoa(int(1900 * 30 / load)),
		"battery.voltage": "27.3", "ups.load": f0(load), "ups.realpower": f0(load * 10),
		"ups.realpower.nominal": "1000", "input.voltage": f1(wave(60, 229, 233, 0)), "output.voltage": "230.0",
		"input.frequency": "50.0", "ups.model": "Smart-UPS 1500", "ups.mfr": "American Power Conversion",
		"ups.serial": "AS1234DEMO", "battery.date": "2024-02-10", "ups.test.result": "No test initiated"}, ""))}, nil
}

var registerOnce sync.Once

func register() {
	registerOnce.Do(func() {
		power.Extra["demo"] = func(raw map[string]any) (power.Source, error) {
			b, err := power.NewBase(raw)
			if err != nil {
				return nil, err
			}
			p, _ := raw["profile"].(string)
			return &demoPower{b: b, baseW: num(raw, "base_w", 100), swingW: num(raw, "swing_w", 10),
				period: num(raw, "period_s", 90), volts: num(raw, "voltage", 230), profile: p}, nil
		}
		ups.Extra["demo"] = func(raw map[string]any) (*ups.Source, error) {
			id, _ := raw["id"].(string)
			name, _ := raw["name"].(string)
			p, err := ups.ParsePowers(id, raw["powers"])
			if err != nil {
				return nil, err
			}
			return &ups.Source{ID: id, Name: name, Powers: p, Read: rackUPS}, nil
		}
	})
}
