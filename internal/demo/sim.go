package demo

import (
	"crypto/rand"
	"encoding/base64"
	"math"
	mrand "math/rand/v2"
	"strconv"
	"time"
)

const GiB = 1 << 30

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

// Straw's UPS: 40 s on battery every 150 s

func f0(v float64) string { return strconv.FormatFloat(v, 'f', 0, 64) }

func f1(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

func randTok() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
