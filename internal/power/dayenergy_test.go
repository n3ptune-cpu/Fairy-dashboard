package power

import (
	"math"
	"testing"
)

// The energy of a day whose counter is not a clean counter, judged stretch by stretch (dayEnergy): what the
// review of the first version found, in cases of this file's own.

// en is the energy of a day: n minutes of watts w(i) (NaN: no readings) and the counter k(i) (NaN: none).
func en(n int, w, k func(i int) float64) (kwh, est float64, label string) {
	s := daySummary(1000, bucketsOf(n, w, k))
	return s.EnergyKWh, s.EnergyEstKWh, s.Energy
}

func flat(v float64) func(int) float64 { return func(int) float64 { return v } }

// A stretch in which the counter stood still while the plug measured power is not counted by the counter,
// however well the counter counts before or after it. Such a stretch counts with its watts, and the answer
// says that a part is estimated. (The first version judged a whole run at once: a counter that had missed
// up to half of the run's energy was still taken for all of it, as "counter", without a mark.)
func TestDayEnergyAStretchTheCounterMissedCountsWithItsWatts(t *testing.T) {
	// a desk lamp: 30 W for 8 h; the counter reads 0 for the first 6 h (it came back from an outage and did
	// not count), then counts. Used: 0.240 kWh, of which the counter saw 0.060.
	kwh, est, label := en(480, flat(30), func(i int) float64 { return math.Max(0, float64(i-360)) * 0.0005 })
	if !near(kwh, 0.240, 0.002) || !near(est, 0.180, 0.002) || label != "mixed" {
		t.Errorf("30 W for 8 h, the counter at 0 for 6 h: %.3f kWh, %.3f estimated, %s; want 0.240, 0.180, mixed", kwh, est, label)
	}
	// a server: 600 W for 10 h; the counter stands still from hour 2 to hour 6 and counts before and after.
	// Used: 6.0 kWh; the counter's increase is 3.6.
	kwh, est, label = en(600, flat(600), func(i int) float64 {
		switch {
		case i < 120:
			return 80 + float64(i)*0.01
		case i < 360:
			return 80 + 119*0.01
		}
		return 80 + 119*0.01 + float64(i-359)*0.01
	})
	if !near(kwh, 6.0, 0.03) || !near(est, 2.4, 0.03) || label != "mixed" {
		t.Errorf("600 W for 10 h, the counter still for 4 h in the middle: %.3f kWh, %.3f estimated, %s; want 6.0, 2.4, mixed", kwh, est, label)
	}
	// a charger: 5 W all day (0.120 kWh), and the counter moved once, by 0.001. Next to nothing of the day is
	// the counter's: it is "estimated".
	kwh, est, label = en(1440, flat(5), func(i int) float64 {
		if i < 900 {
			return 7.512
		}
		return 7.513
	})
	if !near(kwh, 0.120, 0.002) || !near(est, 0.119, 0.002) || label != "estimated" {
		t.Errorf("5 W all day, the counter moved once by 0.001: %.3f kWh, %.3f estimated, %s; want 0.120, 0.119, estimated", kwh, est, label)
	}
}

// A counter that is merely late is not a counter that missed something: the plug's cloud hands Home Assistant
// the total every half hour, and each time it has gone up by the whole half hour. The energy is the counter's,
// and it is not counted twice (once as the watts of the still half hour, once as the step).
func TestDayEnergyACounterThatComesInStepsIsNotCountedTwice(t *testing.T) {
	kwh, est, label := en(1440, flat(100), func(i int) float64 { return 300 + math.Floor(float64(i)/30)*0.05 }) // 100 W: 0.05 kWh per half hour
	// 47 steps of 0.05 = 2.35 from the counter; the first minute and the last 29 minutes have no step yet
	if !near(kwh, 2.40, 0.005) || est > 0.06 || label == "estimated" {
		t.Errorf("a counter in half-hour steps: %.3f kWh, %.3f estimated, %s; want 2.40 with at most the last half hour estimated", kwh, est, label)
	}
	// and one whose steps and whose watts do not agree to the last digit (the watts read 20 % high): still the counter
	kwh, est, label = en(1440, flat(120), func(i int) float64 { return 300 + math.Floor(float64(i)/30)*0.05 })
	if kwh > 2.45 || kwh < 2.35 || label != "counter" && label != "mixed" || est > 0.07 {
		t.Errorf("a counter in half-hour steps under watts that read high: %.3f kWh, %.3f estimated, %s", kwh, est, label)
	}
}

// Where the line is between "the counter counted this stretch" and "it missed it": the counter's increase
// over the stretch must be at least half of the stretch's watts, less three of the counter's own steps.
func TestDayEnergyWhereAStretchCountsWithItsWatts(t *testing.T) {
	// One stretch of n minutes at 60 W (0.001 kWh a minute) in which the counter stands still and then goes up
	// by `up`; before and after it the counter ticks every minute by 0.001 (which is its step: three are 0.003).
	stretch := func(n int, up float64) (kwh, est float64) {
		kwh, est, _ = en(200+n, flat(60), func(i int) float64 {
			switch {
			case i < 100:
				return 10 + float64(i)*0.001
			case i < 100+n:
				return 10 + 99*0.001
			}
			return 10 + 99*0.001 + up + float64(i-100-n)*0.001
		})
		return kwh, est
	}
	// 20 minutes still (0.021 kWh of watts with the minute of the step), then +0.008: half of 0.021 less 0.003 is
	// 0.0075, and 0.008 is not under it: the counter's
	if kwh, est := stretch(20, 0.008); !near(kwh, 0.207, 0.0006) || est > 0.0015 {
		t.Errorf("a step of 0.008 after 20 still minutes: %.4f kWh, %.4f estimated; want the counter's 0.207", kwh, est)
	}
	// the same with +0.007: under the line, so the stretch counts with its watts (0.021)
	if kwh, est := stretch(20, 0.007); !near(kwh, 0.220, 0.0006) || !near(est, 0.015, 0.0006) {
		t.Errorf("a step of 0.007 after 20 still minutes: %.4f kWh, %.4f estimated; want 0.220 with 0.015 estimated", kwh, est)
	}
	// three steps of slack: 6 still minutes (0.007 of watts) and +0.001 is the counter's (half of 0.007 less
	// 0.003 is 0.0005); without the slack it would not be (0.0035)
	if kwh, est := stretch(6, 0.001); !near(kwh, 0.200, 0.0006) || est > 0.0015 {
		t.Errorf("a step of 0.001 after 6 still minutes: %.4f kWh, %.4f estimated; want the counter's 0.200", kwh, est)
	}
	// minutes with watts and no counter value at all in between belong to the stretch: 100 minutes at 60 W
	// without a counter value, then the counter is 0.001 higher than before them
	kwh, est, label := en(300, flat(60), func(i int) float64 {
		switch {
		case i < 100:
			return 10 + float64(i)*0.001
		case i < 200:
			return math.NaN()
		}
		return 10 + 99*0.001 + float64(i-199)*0.001
	})
	if !near(kwh, 0.300, 0.0015) || !near(est, 0.101, 0.0015) || label != "mixed" {
		t.Errorf("100 minutes without a counter value that the counter did not count: %.4f kWh, %.4f estimated, %s; want 0.300, 0.101, mixed", kwh, est, label)
	}
}

// A counter value that is a little lower than the one before is a late value from the plug's cloud, not a
// restart: it is left out, and the next value is compared with the one before it. The first version took
// every decrease for a restart and counted the way back up as an increase: a counter with two decimals that
// stepped back by 0.01 every ten minutes showed twice the day's energy.
func TestDayEnergyALateCounterValueIsNotARestart(t *testing.T) {
	k := func(i int) float64 { // 90 W: 0.0015 kWh a minute, a counter with two decimals
		v := 40 + math.Floor(float64(i)*0.0015*100)/100
		if i%10 == 7 {
			v -= 0.01
		}
		return v
	}
	kwh, est, label := en(1440, flat(90), k)
	if !near(kwh, 2.16, 0.02) || label != "counter" || est > 0.02 {
		t.Errorf("90 W all day, the counter one step back every 10 minutes: %.3f kWh, %.3f estimated, %s; want 2.16 from the counter", kwh, est, label)
	}
	// Where a late value ends and a restart begins: a drop of up to 0.05 kWh is a late value, a larger one is a
	// restart. Here the counter stays lower for good and goes on counting, 600 minutes at 120 W (1.2 kWh).
	drop := func(by float64) (kwh, est float64) {
		kwh, est, _ = en(600, flat(120), func(i int) float64 {
			if i >= 300 {
				return 20 + 299*0.002 - by + float64(i-300)*0.002
			}
			return 20 + float64(i)*0.002
		})
		return kwh, est
	}
	// A restart: the counter goes on from the lower value, and only the minute of the restart (and the
	// counter's first) counts with its watts.
	if kwh, est := drop(0.051); !near(kwh, 1.2, 0.003) || !near(est, 0.004, 0.0006) {
		t.Errorf("a counter that drops by 0.051: %.3f kWh, %.4f estimated; want 1.200 with 0.004 estimated (a restart)", kwh, est)
	}
	// Late values: every value is left out until the counter is past the value from before the drop, 26 minutes
	// later. The counter did not count those minutes (its step up is a small one), so they count with their
	// watts: the same 1.2 kWh, by another way, and 0.05 of it estimated.
	if kwh, est := drop(0.049); !near(kwh, 1.2, 0.003) || !near(est, 0.053, 0.0025) {
		t.Errorf("a counter that is 0.049 lower for good: %.3f kWh, %.4f estimated; want 1.200 with 0.053 estimated (late values)", kwh, est)
	}
}

// An increase no device behind this plug can have made is not counted, also over a gap. Without readings in
// between, the limit is ten times the day's highest watts (at least 200 W) for the time of the gap; the first
// version allowed 4 kW there, and a small device whose counter came back 19 kWh higher after five hours
// without readings (the counter was set to another base) was shown with those 19 kWh.
func TestDayEnergyAJumpOverAGapIsNotCounted(t *testing.T) {
	over := func(watts, up float64) (kwh, est float64, label string) { // 2.5 h of readings, 5 h without, 2.5 h of readings
		return en(600, func(i int) float64 {
			if i >= 150 && i < 450 {
				return math.NaN()
			}
			return watts
		}, func(i int) float64 {
			if i < 150 {
				return 7 + float64(i)*watts/60000
			}
			return 7 + 149*watts/60000 + up + float64(i-450)*watts/60000
		})
	}
	// a 50 W device: its five hours of readings are 0.25 kWh
	if kwh, _, label := over(50, 19); kwh > 0.3 || label == "counter" && kwh > 0.3 {
		t.Errorf("50 W, and the counter 19 kWh higher after the gap: %.3f kWh (%s); want about 0.25", kwh, label)
	}
	// what it can have used in the gap (5 h at up to ten times its highest watts: 2.5 kWh) is the counter's
	if kwh, _, label := over(50, 0.25); !near(kwh, 0.5, 0.005) || label != "counter" {
		t.Errorf("50 W, and the counter 0.25 kWh higher after the gap: %.3f kWh (%s); want 0.50 from the counter", kwh, label)
	}
	if kwh, _, label := over(50, 2.4); !near(kwh, 2.65, 0.005) || label != "counter" {
		t.Errorf("50 W, and the counter 2.4 kWh higher after the gap: %.3f kWh (%s); want 2.65 from the counter", kwh, label)
	}
	// (the limit over this gap is 5 h at 500 W and the two minutes around it at 4 kW: 2.63 kWh)
	if kwh, _, _ := over(50, 2.7); kwh > 0.3 {
		t.Errorf("50 W, and the counter 2.7 kWh higher after the gap (more than ten times its watts for five hours): %.3f kWh", kwh)
	}
	// a device that idles at 4 W: the floor of 200 W allows 1 kWh over five hours (1.13 with the two minutes
	// around the gap), not 1.2
	if kwh, _, label := over(4, 1.1); !near(kwh, 1.12, 0.005) || label != "counter" {
		t.Errorf("4 W, and the counter 1.1 kWh higher after the gap: %.3f kWh (%s); want 1.12 from the counter", kwh, label)
	}
	if kwh, _, _ := over(4, 1.2); kwh > 0.03 {
		t.Errorf("4 W, and the counter 1.2 kWh higher after the gap: %.3f kWh; want the 0.02 of its readings", kwh)
	}
	// between two minutes that both have readings the limit is the old one: 4 kW (or ten times the day's highest
	// watts) for the two minutes, 0.133 kWh
	step := func(up float64) float64 {
		kwh, _, _ := en(200, flat(60), func(i int) float64 {
			if i < 100 {
				return 3 + float64(i)*0.001
			}
			return 3 + 99*0.001 + up + float64(i-100)*0.001
		})
		return kwh
	}
	if got := step(0.13); !near(got, 0.199+0.13, 0.002) {
		t.Errorf("a step of 0.13 kWh from one minute to the next: %.3f kWh, want it counted (%.3f)", got, 0.199+0.13)
	}
	if got := step(0.14); !near(got, 0.200, 0.002) {
		t.Errorf("a step of 0.14 kWh from one minute to the next: %.3f kWh, want it left out (0.200)", got)
	}
}

// What the energy is called: "estimated" when less than 2 % of it is the counter's, "counter" when less than
// 2 % of it is estimated, "mixed" between. Also for a source that feeds in (watts under 0), whose energy is
// under 0: it was called "counter" without having one.
func TestDayEnergyLabels(t *testing.T) {
	if kwh, est, label := en(600, flat(-300), nil); !near(kwh, -3, 1e-9) || est != kwh || label != "estimated" {
		t.Errorf("-300 W for 10 h without a counter: %.3f kWh, %.3f estimated, %s", kwh, est, label)
	}
	if kwh, est, label := en(600, flat(0), nil); kwh != 0 || est != 0 || label != "estimated" {
		t.Errorf("0 W for 10 h without a counter: %.3f kWh, %.3f estimated, %s", kwh, est, label)
	}
	// 100 minutes at 60 W with a counter, then n minutes at 60 W without one
	withTail := func(n int) string {
		_, _, label := en(100+n, flat(60), func(i int) float64 {
			if i >= 100 {
				return math.NaN()
			}
			return float64(i) * 0.001
		})
		return label
	}
	// one minute (the counter's first) of 100 is estimated: 1 %; with one more, 2 of 101: still under 2 %
	if withTail(0) != "counter" || withTail(1) != "counter" || withTail(2) != "mixed" {
		t.Errorf("a counter with 0, 1 and 2 more minutes of watts: %s, %s, %s; want counter, counter, mixed", withTail(0), withTail(1), withTail(2))
	}
	// the counter's part is 99 minutes; of 4950 minutes that is 2 %, of 5000 less
	if withTail(4800) != "mixed" || withTail(4900) != "estimated" {
		t.Errorf("99 minutes of counter in 4900 and in 5000 minutes: %s, %s; want mixed, estimated", withTail(4800), withTail(4900))
	}
}

// The minutes between the counter's last step and its restart are the device's too. A counter that stood
// still for an hour and then started again at 0: the hour counts with its watts (it used to be the counter's
// last stretch that was open at the restart, and a change that dropped it went unnoticed).
func TestDayEnergyTheMinutesBeforeARestartCount(t *testing.T) {
	// 60 W for four hours: 0.240 kWh. The counter moves in steps of 0.01 kWh, one every ten minutes; it stands
	// still from 0:50 on, starts again at 0 at 2:00 and counts from there.
	k := func(i int) float64 {
		if i < 120 {
			return 7 + math.Floor(math.Min(float64(i), 50)/10)*0.01
		}
		return math.Floor(float64(i-120)/10) * 0.01
	}
	kwh, est, label := en(240, flat(60), k)
	// the counter's: 5 steps before and 11 after the restart. From the watts: the first minute, the 69 minutes
	// it stood still and the minute of the restart, and the 9 minutes after its last step.
	if !near(kwh, 0.240, 1e-6) || !near(est, 0.080, 1e-6) || label != "mixed" {
		t.Errorf("%.4f kWh, %.4f of them estimated (%s); want 0.2400, 0.0800 (mixed)", kwh, est, label)
	}
}
