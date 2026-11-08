package demo

import (
	"crypto/rand"
	"encoding/base64"
	"math"
	mrand "math/rand/v2"
	"strconv"
	"sync"
	"time"
)

// GiB is 2^30 bytes.
const GiB = 1 << 30

// T0 is when the simulation started; the simulated values drift with time since T0.
var T0 = time.Now()

func since() float64 { return time.Since(T0).Seconds() }

func epoch() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func uniform(a, b float64) float64 { return a + (b-a)*mrand.Float64() }

func r1(v float64) float64 { return math.Round(v*10) / 10 }

func r2(v float64) float64 { return math.Round(v*100) / 100 }

// wave: a smooth drifting value in [lo, hi] plus a little noise.
func wave(period, lo, hi, phase float64) float64 {
	x := (math.Sin(2*math.Pi*since()/period+phase) + 1) / 2
	v := lo + (hi-lo)*x
	return math.Max(lo*0.9, v*(1+uniform(-0.03, 0.03)))
}

// llmLoad: 0..1 'generating' factor for the llama.cpp box: long busy stretches with short idle dips.
func llmLoad() float64 {
	t := math.Mod(since(), 47)
	busy := 1.0
	if t >= 30 && t < 37 { // a 7 s idle gap every 47 s (between requests)
		busy = 0.04
	}
	return busy * uniform(0.93, 1.0)
}

// counters holds the simulated cumulative byte counters (network rx/tx), keyed per host and direction.
var counters = struct {
	sync.Mutex
	m map[string]*counterState
}{m: map[string]*counterState{}}

type counterState struct {
	value, at float64 // bytes so far, seconds since T0 of the last sample
}

// counter returns a cumulative byte counter that only ever grows: the first call starts it at base (+ t*rate so
// hosts don't all start equal), later calls add rate*dt for the time since the previous call. A real agent's
// counters behave like this; deriving them as base + t*rate(t) with a jittery rate made them go backwards, which
// the rate tracker treats as a counter reset (no rate, so the UI hid the Network row every other second).
func counter(key string, base, rate float64) int64 {
	return counterAt(key, base, rate, since())
}

func counterAt(key string, base, rate, t float64) int64 {
	rate = math.Max(rate, 0)
	counters.Lock()
	defer counters.Unlock()
	c := counters.m[key]
	if c == nil {
		c = &counterState{value: base + t*rate, at: t}
		counters.m[key] = c
	} else if dt := t - c.at; dt > 0 {
		c.value += rate * dt
		c.at = t
	}
	return int64(c.value)
}

func f0(v float64) string { return strconv.FormatFloat(v, 'f', 0, 64) }

func f1(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

func randTok() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
