package normalise

import (
	"math"
	"strconv"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
)

// Numbers from machines the panel does not control (agents, plugs, UPS servers, the Proxmox API) are checked
// where they enter and again after every sum, product and rounding. A value that is not a real number, or lies
// outside what the thing it measures can be, is dropped (shown as "no value"), never passed on: one NaN, one
// infinity or one date outside the years 0 to 9999 makes a whole answer impossible to write as JSON, and two huge
// but possible numbers add up to an impossible one.
const (
	// MaxReading bounds any sensor reading (°C, RPM, volts, amperes, watts) on both sides. It is far above
	// every real reading and far below where sums or scaling of a few thousand of them stop being numbers.
	MaxReading = 1e9
	// MaxBytes bounds a size (memory, a disk, video memory): 8 PiB, and every whole number up to it is exact.
	MaxBytes = 1 << 53
	// MaxCounter bounds a byte counter that only grows (network traffic since boot).
	MaxCounter = 1 << 62
	// MaxUnixTime bounds a time in Unix seconds (the year 2286) and a duration in seconds.
	MaxUnixTime = 1e10
	// MaxCores bounds a number of processor cores.
	MaxCores = 1 << 16
	// MaxEnergyKWh bounds an energy counter.
	MaxEnergyKWh = 1e12
	// MaxVMID is the highest guest id Proxmox itself accepts.
	MaxVMID = 999_999_999
)

// InRange reports whether v is a real number within [lo, hi]. It is false for NaN and for both infinities.
func InRange(v, lo, hi float64) bool { return v >= lo && v <= hi }

// Finite returns v when it is a real number within [lo, hi], and nil otherwise (nil stays nil).
func Finite(v *float64, lo, hi float64) *float64 {
	if v == nil || !InRange(*v, lo, hi) {
		return nil
	}
	return v
}

// Clamp returns v limited to [lo, hi]: for values that have a natural scale (a percentage), where a little over
// the end means the end. NaN and the infinities are not on any scale: nil (nil stays nil).
func Clamp(v *float64, lo, hi float64) *float64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return nil
	}
	c := math.Min(math.Max(*v, lo), hi)
	return &c
}

// Reading is Finite for a sensor reading (see MaxReading).
func Reading(v *float64) *float64 { return Finite(v, -MaxReading, MaxReading) }

// num is m[k] as a number within [lo, hi], or nil.
func num(m jsonx.M, k string, lo, hi float64) *float64 { return Finite(jsonx.Num(m, k), lo, hi) }

// reading is m[k] as a sensor reading, or nil.
func reading(m jsonx.M, k string) *float64 { return Reading(jsonx.Num(m, k)) }

// percent is m[k] as a percentage, limited to 0-100, or nil.
func percent(m jsonx.M, k string) *float64 { return Clamp(jsonx.Num(m, k), 0, 100) }

// whole is m[k] as a whole number within [lo, hi] (fractions cut off), or nil. The range is checked before the
// conversion: converting a float64 that does not fit is not defined (and differs between amd64 and arm64).
func whole(m jsonx.M, k string, lo, hi float64) *int64 {
	f := num(m, k, lo, hi)
	if f == nil {
		return nil
	}
	i := int64(*f)
	return &i
}

// size is m[k] as a number of bytes (0 to MaxBytes), or nil: no negative and no absurd sizes.
func size(m jsonx.M, k string) *int64 { return whole(m, k, 0, MaxBytes) }

// counter is m[k] as a byte counter (0 to MaxCounter), or nil.
func counter(m jsonx.M, k string) *int64 { return whole(m, k, 0, MaxCounter) }

// VMID is a Proxmox row's guest id (1 to MaxVMID; a number or digits in a string), or nil.
func VMID(r jsonx.M) *int {
	f := jsonx.Num(r, "vmid")
	if f == nil {
		if n, err := strconv.Atoi(jsonx.S(r, "vmid")); err == nil {
			x := float64(n)
			f = &x
		}
	}
	if f = Finite(f, 1, MaxVMID); f == nil {
		return nil
	}
	v := int(*f)
	return &v
}

// AddBytes adds two sizes without wrapping around: the sum of very many sizes stops at the largest int64.
func AddBytes(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	if b < 0 && a < math.MinInt64-b {
		return math.MinInt64
	}
	return a + b
}
