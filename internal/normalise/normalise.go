// Package normalise turns Proxmox and agent payloads into the unified System schema (pure functions, plus the
// tracker that turns byte counters into rates).
package normalise

import (
	"math"
	"regexp"
	"strings"
)

// Round to n decimals (Python round semantics are banker's rounding; the half-way difference is irrelevant here).
// Rounding never turns a number into an infinity: where v*10^n overflows, v has no decimals left to round and is
// returned as it is.
func Round(v float64, n int) float64 {
	p := math.Pow(10, float64(n))
	if r := math.Round(v*p) / p; !math.IsInf(r, 0) && !math.IsNaN(r) {
		return r
	}
	return v
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug lowercases v and turns every run of other characters into a dash (node names -> ids).
func Slug(v string) string {
	return strings.Trim(slugRe.ReplaceAllString(strings.ToLower(v), "-"), "-")
}
