// Package normalise turns Proxmox and agent payloads into the unified System schema (pure functions, plus the
// tracker that turns byte counters into rates).
package normalise

import (
	"math"
	"regexp"
	"strings"
)

// Round to n decimals (Python round semantics are banker's rounding; the half-way difference is irrelevant here).
func Round(v float64, n int) float64 {
	p := math.Pow(10, float64(n))
	return math.Round(v*p) / p
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug lowercases v and turns every run of other characters into a dash (node names -> ids).
func Slug(v string) string {
	return strings.Trim(slugRe.ReplaceAllString(strings.ToLower(v), "-"), "-")
}
