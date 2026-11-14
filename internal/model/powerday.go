package model

import (
	"math"
	"strconv"
)

// PowerDay is GET /api/power/day: one power source's last 24 hours, one element a minute.
//
// The lists are parallel: element i is the minute that starts at Start + i*StepS (Unix seconds). They run from
// the first minute of the last 24 hours that has readings to the last one that has; a minute without readings
// (the device, its Home Assistant or the agent that relays it did not answer; the panel was off) is null in
// every list. It is never a zero. Volts and Amps are left out when the device reported none all day.
type PowerDay struct {
	Source string `json:"source"`
	StepS  int    `json:"step_s"`
	Now    int64  `json:"now"`   // the panel's clock, Unix seconds: the window is the 24 hours that end here
	Start  int64  `json:"start"` // Unix seconds of element 0; 0 when the lists are empty
	// watts: the lowest, the average and the highest sample of each minute
	Min DayValues `json:"min"`
	Avg DayValues `json:"avg"`
	Max DayValues `json:"max"`
	// the last volts and amperes of each minute
	Volts DayValues `json:"volts,omitempty"`
	Amps  DayAmps   `json:"amps,omitempty"`
	// Summary is null when the device has no reading in the last 24 hours.
	Summary *PowerDaySummary `json:"summary"`
}

// PowerDaySummary is the day in numbers. Everything is over the minutes that have readings: a gap counts as
// nothing, not as zero watts.
type PowerDaySummary struct {
	From     int64 `json:"from"`      // Unix seconds: the first minute with readings
	To       int64 `json:"to"`        // the last one
	CoveredS int   `json:"covered_s"` // seconds with readings (minutes x 60)

	MinW  float64 `json:"min_w"`
	AvgW  float64 `json:"avg_w"`
	MaxW  float64 `json:"max_w"`
	MinAt int64   `json:"min_at"`
	MaxAt int64   `json:"max_at"`

	// EnergyKWh is the energy used in the minutes with readings. Energy says where it is from: "counter" (the
	// device's own kWh counter: only its increases are counted, a counter that went down has restarted),
	// "estimated" (average watts x time) or "mixed". EnergyEstKWh is the part of EnergyKWh that is estimated.
	EnergyKWh    float64 `json:"energy_kwh"`
	EnergyEstKWh float64 `json:"energy_est_kwh"`
	Energy       string  `json:"energy"`

	VoltsMin *float64 `json:"volts_min"`
	VoltsMax *float64 `json:"volts_max"`
	AmpsMin  *float64 `json:"amps_min"`
	AmpsMax  *float64 `json:"amps_max"`
}

// DayValues is a list of readings, one a minute, written with two decimals; NaN is a minute without one and
// is written as null. (A value that is not a number can therefore never break the answer.)
type DayValues []float32

// DayAmps is DayValues with three decimals: a plug's current is often well under one ampere.
type DayAmps []float32

// MarshalJSON writes the list with two decimals and null for the minutes without a reading.
func (v DayValues) MarshalJSON() ([]byte, error) { return dayJSON(v, 2), nil }

// MarshalJSON writes the list with three decimals and null for the minutes without a reading.
func (v DayAmps) MarshalJSON() ([]byte, error) { return dayJSON(v, 3), nil }

func dayJSON(v []float32, decimals int) []byte {
	b := make([]byte, 0, 2+len(v)*7)
	b = append(b, '[')
	p := math.Pow(10, float64(decimals))
	for i, x := range v {
		if i > 0 {
			b = append(b, ',')
		}
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			b = append(b, "null"...)
			continue
		}
		if r := math.Round(f*p) / p; !math.IsInf(r, 0) && !math.IsNaN(r) {
			f = r
		}
		if f == 0 { // never "-0"
			f = 0
		}
		b = strconv.AppendFloat(b, f, 'f', -1, 64)
	}
	return append(b, ']')
}
