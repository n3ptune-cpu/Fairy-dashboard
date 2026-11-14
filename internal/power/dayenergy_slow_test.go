package power

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
)

// The second review of the day's energy. A stretch is judged with a slack of three of the counter's steps,
// and the step was taken to be the day's smallest increase. For a counter whose total comes only every 30 or
// 60 minutes, the smallest increase is the energy of a whole such interval: six intervals in which the counter
// stood still passed as counted, and a day that was a fifth too low was shown as the counter's number.
//
// What is asked of the number now, whatever the counter's steps are: over the stretches that count with the
// counter, the counter must show what the watts of the same time show, to within energyShort of them. What
// it shows less is added, as estimated.

// every is a counter in steps of res kWh that hands out its total every n minutes: its value at minute i for
// a device that has used kwh(j) until minute j.
func every(n int, res float64, kwh func(j int) float64) func(i int) float64 {
	return func(i int) float64 { return math.Floor(kwh(i-i%n)/res+1e-9) * res }
}

// The review's four cases. A device with a steady load for 24 hours, a counter in 0.001 kWh that hands out
// its total every 30 or 60 minutes; from 6:00 on the counter stands still for some hours (it does not count
// what the device uses then) and goes on from its old value.
func TestDayEnergyASlowCounterThatStandsStill(t *testing.T) {
	for _, c := range []struct {
		watts        float64
		every, still int
		was          float64 // what the day showed before, as the counter's number
	}{
		{800, 60, 300, 15.213},
		{300, 60, 360, 5.405},
		{300, 30, 180, 6.305},
		{40, 30, 180, 0.841},
	} {
		name := fmt.Sprintf("%.0f W, a total every %d min, still for %d h", c.watts, c.every, c.still/60)
		const n, from = 1441, 360
		counted := func(j int) float64 { // what the counter has counted until minute j
			switch {
			case j >= from+c.still:
				j -= c.still
			case j >= from:
				j = from
			}
			return 50 + float64(j)*c.watts/60000
		}
		kwh, est, label := en(n, flat(c.watts), every(c.every, 0.001, counted))
		truth, missed := float64(n)*c.watts/60000, float64(c.still)*c.watts/60000
		t.Logf("%s: %.3f kWh, %.3f estimated, %s (true %.3f; was %.3f, counter)", name, kwh, est, label, truth, c.was)
		if !near(kwh, truth, 0.01*truth) {
			t.Errorf("%s: %.3f kWh, the device used %.3f", name, kwh, truth)
		}
		if label != "mixed" || !near(est, missed, 0.02*truth) {
			t.Errorf("%s: %.3f kWh estimated (%s); the counter missed %.3f, and the day must say that a part is estimated", name, est, label, missed)
		}
	}
}

// The same counters when they count: nothing is added, and the day is the counter's.
func TestDayEnergyASlowCounterThatCounts(t *testing.T) {
	for _, c := range []struct {
		watts float64
		every int
		res   float64
	}{{800, 60, 0.001}, {300, 30, 0.001}, {40, 30, 0.001}, {40, 15, 0.01}, {2000, 1, 0.1}, {5, 60, 0.001}} {
		const n = 1441
		kwh, est, label := en(n, flat(c.watts), every(c.every, c.res, func(j int) float64 { return 50 + float64(j)*c.watts/60000 }))
		if truth := float64(n) * c.watts / 60000; !near(kwh, truth, 0.01*truth) || label != "counter" || est > 0.02*truth {
			t.Errorf("%.0f W, a total every %d min in steps of %g: %.3f kWh, %.3f estimated (%s); want %.3f from the counter", c.watts, c.every, c.res, kwh, est, label, truth)
		}
	}
}

// Where the line is. Over the stretches that count with the counter, the counter may show up to energyShort
// less than the watts (the watts are samples, the counter is the device's own sum): then the day is the
// counter's. More than that is added.
func TestDayEnergyWhatTheCounterShowsLessThanTheWatts(t *testing.T) {
	// 600 W for ten hours; the counter counts every minute, but in the first hours only a part of what is used
	day := func(part float64, minutes int) (kwh, est float64, label string) {
		return en(601, flat(600), func(i int) float64 {
			slow := math.Min(float64(i), float64(minutes))
			return 9 + (slow*part+float64(i)-slow)*0.01
		})
	}
	// 0.6 of the watts for 70 minutes: 0.28 of 6.00 kWh less than the watts, under a twentieth: the counter's
	if kwh, est, label := day(0.6, 70); !near(kwh, 6.01-0.28, 0.002) || label != "counter" || est > 0.011 {
		t.Errorf("the counter shows 4.7 %% less than the watts: %.3f kWh, %.3f estimated (%s); want the counter's 5.730", kwh, est, label)
	}
	// for 80 minutes: 0.32 kWh less, over a twentieth: added
	if kwh, est, label := day(0.6, 80); !near(kwh, 6.01, 0.002) || label != "mixed" || !near(est, 0.33, 0.002) {
		t.Errorf("the counter shows 5.3 %% less than the watts: %.3f kWh, %.3f estimated (%s); want 6.010 with 0.330 estimated (mixed)", kwh, est, label)
	}
	// a counter that shows MORE than the watts is the counter's, by whatever part (the watts are read too low)
	if kwh, _, label := day(1.5, 600); !near(kwh, 0.01+9.0, 0.002) || label != "counter" {
		t.Errorf("the counter shows half more than the watts: %.3f kWh (%s); want the counter's 9.010", kwh, label)
	}
}

// Days made up at random, with a fixed seed: a device whose load changes, a counter with steps and a cadence,
// and what counters do: stand still for a while and go on from their old value, start again at 0 (and read 0
// for a while), hand out a late value, and stretches without readings in which the device goes on and the
// counter with it. The watts are exact here.
//
// What must hold for every day that is shown as the counter's number, without a mark: it is not more than a
// few percent under what the device used in the minutes with readings, and not over what it used in all.
func TestDayEnergyOfRandomDaysIsNotShownTooLowAsTheCounters(t *testing.T) {
	rng := rand.New(rand.NewSource(1114))
	const days = 12000
	labels := map[string]int{}
	low, high, worst := 0, 0, 1.0
	for d := 0; d < days; d++ {
		res := []float64{0.001, 0.001, 0.01, 0.1}[rng.Intn(4)]
		cadence := []int{1, 1, 5, 15, 30, 60}[rng.Intn(6)]
		load := []float64{4, 25, 60, 150, 400, 900, 2200}[rng.Intn(7)]
		n := 240 + rng.Intn(1201)
		var (
			bs           = make([]dayBucket, n)
			w            = load
			used, missed float64 // by the device since the counter's last start; of that, not counted
			base         = float64(rng.Intn(300))
			shown        = base
			still, away  int
			covered, all float64
			what         string
			maxW         float64
			late         float64 // not 0: the next value is an older total, lower by this much
		)
		for i := 0; i < n; i++ {
			if rng.Intn(90) == 0 {
				w = load * (0.1 + 2.5*rng.Float64())
			}
			switch r := rng.Intn(2400); {
			case r < 3 && still == 0:
				still, what = 15+rng.Intn(420), what+"still "
			case r < 5:
				base, used, missed, what = 0, 0, 0, what+"restart "
				if rng.Intn(2) == 0 {
					still, what = 15+rng.Intn(360), what+"at 0 "
				}
			case r < 8 && away == 0:
				away, what = 5+rng.Intn(300), what+"away "
			case r < 14 && res <= 0.01 && covered > 0: // (lower by more than energyLateKWh it would be a restart; as the first value of all it is not known to be late)
				late = res * float64(1+rng.Intn(3))
			}
			e := w / 60000
			used, all = used+e, all+e
			if still > 0 {
				still--
				missed += e
			}
			if i%cadence == 0 {
				shown = base + math.Floor((used-missed)/res+1e-9)*res
			}
			k := math.Max(0, shown-late)
			late = 0
			if away > 0 {
				away--
				continue
			}
			covered, maxW = covered+e, math.Max(maxW, w)
			bs[i] = dayBucket{minute: uint32(5000 + i), n: 1, min: float32(w), avg: float32(w), max: float32(w), volts: nan32, amps: nan32, kwh: k}
		}
		s := daySummary(5000, bs)
		if s == nil {
			continue
		}
		labels[s.Energy]++
		if s.Energy != "counter" || covered < 0.05 {
			continue
		}
		kwh, _, _ := dayEnergy(bs, maxW)
		worst = math.Min(worst, kwh/covered)
		if kwh < (1-energyShort-0.005)*covered {
			if low++; low <= 5 {
				t.Errorf("shown as the counter's: %.3f kWh, the device used %.3f in the minutes with readings (%.0f %%) [steps of %g, a total every %d min, about %.0f W: %s]",
					kwh, covered, 100*kwh/covered, res, cadence, load, what)
			}
		}
		if kwh > 1.1*all {
			if high++; high <= 5 {
				t.Errorf("shown as the counter's: %.3f kWh, the device used %.3f in all (%.0f %%) [steps of %g, a total every %d min, about %.0f W: %s]",
					kwh, all, 100*kwh/all, res, cadence, load, what)
			}
		}
	}
	t.Logf("%d days: %v; shown as the counter's and too low: %d, too high: %d; the lowest is %.1f %% of the minutes with readings", days, labels, low, high, 100*worst)
	if labels["counter"] < days/20 || labels["mixed"] < days/20 {
		t.Errorf("the days are not the mix this test is for: %v", labels)
	}
}

// Time without readings. What the device used then is in the number when the counter went on through it by
// an amount a device could have used (dayEnergy: energyGapFloorW). An increase over such a time that is not
// believable is left out, and then the number is less than the device used: the day does not call it the
// counter's number. (The review: a plug that reads 2 W all day, its site away for five hours in which the PC
// behind it ran at 250 W; 0.038 kWh was shown as the counter's, the plug used 1.288.)
func TestDayEnergyAnIncreaseOverAGapThatIsLeftOutIsMarked(t *testing.T) {
	seen := func(i int) float64 { // 2 W, no readings from 10:00 to 15:00
		if i >= 600 && i < 900 {
			return math.NaN()
		}
		return 2
	}
	over := func(gap float64) func(int) float64 {
		return func(i int) float64 {
			if i < 600 {
				return 8 + float64(i)*2/60000
			}
			return 8 + 600*2.0/60000 + gap + float64(i-900)*2/60000
		}
	}
	// 1.25 kWh in five hours is 250 W: more than ten times what the plug was ever seen to use, and more than
	// 200 W. It is left out, and the day is marked.
	kwh, est, label := en(1440, seen, over(1.25))
	if !near(kwh, 0.038, 0.001) || est > 0.001 || label != "mixed" {
		t.Errorf("an increase of 1.25 kWh over five hours without readings, on a plug seen at 2 W: %.3f kWh, %.3f estimated (%s); want 0.038, marked as mixed", kwh, est, label)
	}
	// 0.9 kWh in five hours is 180 W, under the limit: it is the counter's, and nothing is marked
	kwh, est, label = en(1440, seen, over(0.9))
	if !near(kwh, 0.938, 0.001) || est > 0.001 || label != "counter" {
		t.Errorf("an increase of 0.9 kWh over five hours without readings: %.3f kWh, %.3f estimated (%s); want the counter's 0.938", kwh, est, label)
	}
	// a jump with readings in every minute is another matter: its minutes count with their watts, which are
	// known, and a minute or two of them do not mark a day (day_test.go: TestDayEnergyIgnoresAJumpNoDeviceCanMake)
	kwh, _, label = en(1440, flat(2), func(i int) float64 {
		if i < 600 {
			return 8 + float64(i)*2/60000
		}
		return 9.25 + float64(i)*2/60000
	})
	if !near(kwh, 0.048, 0.001) || label != "counter" {
		t.Errorf("a jump of 1.25 kWh from one minute to the next, with readings: %.3f kWh (%s); want 0.048, the counter's", kwh, label)
	}
}
