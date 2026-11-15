package demo

import (
	"math"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/power"
)

// dayShape is how a demo power source's made-up last 24 hours look ("Last 24 hours" on the Power page).
type dayShape struct {
	id            string
	baseW, swingW float64 // the day's curve runs from baseW to baseW+swingW
	volts         float64 // 0: the device reports none
	amps          bool
	// counter: "" none, "good" a kWh counter all day, "restart" one that starts again after the gap, "stuck"
	// one that does not move
	counter string
	// gapFrom, gapTo: minutes before now without readings (the device or its agent did not answer)
	gapFrom, gapTo int
}

// The demo's sources, each with another case of what a real plug does: a counter that works; an outage of
// five hours after which the counter starts again; a counter that is stuck; no counter and no volts.
var dayShapes = []dayShape{
	{id: "plug-tower", baseW: 95, swingW: 140, volts: 230, amps: true, counter: "good", gapFrom: 545, gapTo: 510},
	{id: "plug-llm-box", baseW: 190, swingW: 620, volts: 230, amps: true, counter: "restart", gapFrom: 960, gapTo: 660},
	{id: "ups-rack", baseW: 210, swingW: 60, volts: 230, amps: true, counter: "stuck"},
	{id: "bob-pc/desk-plug", baseW: 95, swingW: 325, volts: 229.8, gapFrom: 300, gapTo: 240},
	{id: "mini-i7-agent/mini-plug", baseW: 38, swingW: 58},
	{id: "mini-i7-agent/corner-plug", baseW: 310, swingW: 250, gapFrom: 130, gapTo: 100},
	{id: "mini-i7-agent/desk-bar", baseW: 41, swingW: 56},
}

// SeedPowerDay gives every demo power source that has no day yet a made-up one, so that the graph can be
// looked at right after `fairy serve --demo`: a day's curve, a gap, a stretch of high load, one short spike.
// A source that has readings from an earlier demo run (power-day.json in the demo's folder) keeps those.
func SeedPowerDay(p *poller.Poller, now time.Time) {
	day := p.Power.Day
	for _, sh := range dayShapes {
		if day.Has(sh.id, now) {
			continue
		}
		// what the counter shows now (demoPower.Read goes on from 1234.5), counted back to each minute
		watts := make([]float64, power.DaySlots)
		for m := range watts {
			watts[m] = sh.at(m)
		}
		kwh := 1234.5
		back := make([]float64, power.DaySlots) // back[m]: the counter m minutes ago
		for m := 0; m < power.DaySlots; m++ {
			back[m] = kwh
			if sh.counter == "good" || sh.counter == "restart" {
				kwh -= watts[m] / 60000
			}
			if sh.counter == "restart" && m == sh.gapTo { // before the gap the counter was at another value
				kwh = 5120.75
			}
		}
		for m := power.DaySlots - 1; m >= 1; m-- {
			if m <= sh.gapFrom && m > sh.gapTo {
				continue
			}
			t := now.Add(-time.Duration(m) * time.Minute).Truncate(time.Minute)
			w := watts[m]
			for i, x := range []float64{w * 0.97, w * 1.04, w * 0.99} { // three samples: a little band around the average
				if m == 845 && i == 1 && sh.gapFrom < 845 { // one short spike, 14 hours ago: a single sample
					x = w * 3.2
				}
				s := power.DaySample{ID: sh.id, W: r1(x)}
				if sh.volts > 0 {
					v := r1(sh.volts + 1.5*math.Sin(float64(m)/37))
					s.V = &v
					if sh.amps {
						a := math.Round(x/v*1000) / 1000
						s.A = &a
					}
				}
				if sh.counter != "" {
					k := math.Round(back[m]*1000) / 1000
					s.KWh = &k
				}
				day.Record(t.Add(time.Duration(i)*20*time.Second), []power.DaySample{s}, nil)
			}
		}
	}
}

// at is the source's watts m minutes ago: a slow day curve (low at night), a faster ripple, and three hours
// ago 25 minutes of high load.
func (sh dayShape) at(m int) float64 {
	x := float64(m)
	day := (1 - math.Cos(2*math.Pi*(x+300)/float64(power.DaySlots))) / 2
	ripple := (math.Sin(x/9) + math.Sin(x/23+1.3)) / 4
	w := sh.baseW + sh.swingW*math.Max(0, math.Min(1, 0.15+0.55*day+0.3*ripple))
	if m <= 195 && m > 170 {
		w = sh.baseW + sh.swingW*1.25
	}
	return w
}
