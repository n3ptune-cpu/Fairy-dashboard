package demo

import (
	"context"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// The demo's made-up day (SeedPowerDay): every one of its power sources has one, with the gap its shape names
// and nothing after now, and the four cases of a kWh counter give what the page should say for each. The ids
// are the ones the demo's panel lists, so the day can be asked for.
func TestDemoHasAMadeUpDayForEveryPowerSource(t *testing.T) {
	_, _, p, err := Build(t.TempDir(), "http://127.0.0.1:8090")
	if err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	listed := map[string]bool{}
	for _, s := range p.Power.Overview().Sources {
		listed[s.ID] = true
	}
	now := time.Now()
	energy := map[string]string{"plug-tower": "counter", "plug-llm-box": "counter", "ups-rack": "estimated", "bob-pc/desk-plug": "estimated"}
	for _, sh := range dayShapes {
		if !listed[sh.id] {
			t.Errorf("%s has a made-up day and is not a source of the demo: %v", sh.id, listed)
			continue
		}
		delete(listed, sh.id)
		d := p.Power.Day.Series(sh.id, now)
		if d.Summary == nil || len(d.Avg) < 1430 || d.Start > now.Add(-23*time.Hour).Unix() {
			t.Errorf("%s: %d minutes, summary %+v", sh.id, len(d.Avg), d.Summary)
			continue
		}
		gaps := 0
		for _, v := range d.Avg {
			if v != v { // NaN: a minute without readings
				gaps++
			}
		}
		if want := sh.gapFrom - sh.gapTo; gaps < want-2 || gaps > want+2 {
			t.Errorf("%s: %d minutes without readings, want %d", sh.id, gaps, want)
		}
		if d.Summary.To > now.Unix() || d.Summary.MaxW < sh.baseW+sh.swingW || d.Summary.MinW > sh.baseW+0.5*sh.swingW {
			t.Errorf("%s: the day's numbers: %+v", sh.id, d.Summary)
		}
		if want := energy[sh.id]; want != "" && d.Summary.Energy != want {
			t.Errorf("%s: energy is %q (%v kWh), want %q", sh.id, d.Summary.Energy, d.Summary.EnergyKWh, want)
		}
		if (sh.volts > 0) != (d.Volts != nil) || sh.amps != (d.Amps != nil) {
			t.Errorf("%s: volts %v, amps %v", sh.id, d.Volts != nil, d.Amps != nil)
		}
		// the energy is of the size of the watts, whatever the counter did (a restart from 5120 is not -3886 kWh)
		if est := d.Summary.AvgW * float64(d.Summary.CoveredS) / 3.6e6; d.Summary.EnergyKWh < 0.8*est || d.Summary.EnergyKWh > 1.2*est {
			t.Errorf("%s: %v kWh over the day, the watts say about %.2f", sh.id, d.Summary.EnergyKWh, est)
		}
	}
	for id := range listed {
		t.Errorf("the demo's source %s has no made-up day", id)
	}
	// a second start of the demo with the day of the first in its folder: nothing is made up again
	var before model.PowerDay = p.Power.Day.Series("plug-tower", now)
	SeedPowerDay(p, now)
	if after := p.Power.Day.Series("plug-tower", now); len(after.Avg) != len(before.Avg) || after.Summary.AvgW != before.Summary.AvgW {
		t.Error("a source that has a day was given a made-up one again")
	}
}
