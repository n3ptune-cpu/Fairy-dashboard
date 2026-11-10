package normalise

import "strings"

// Limits for what one agent may put into its system entry. An agent is a machine the panel does not control: its
// /metrics answer is kept in memory, copied into every snapshot and sent to every open page, so each list and
// each text has an upper bound. The bounds are far above what a real machine reports; what is over them is cut
// off (lists keep their first entries, texts their first bytes).
const (
	MaxDisks        = 64
	MaxTemps        = 256
	MaxFans         = 64
	MaxVoltages     = 128
	MaxGPUs         = 16
	MaxPowerSensors = 64
	MaxLoadAvg      = 3
	MaxRelayTargets = 32

	maxSourceNames = 16  // power.source: where the agent's component power comes from (rapl, battery, ...)
	maxName        = 128 // sensor, fan, voltage, GPU and power sensor names, labels
	maxShort       = 64  // file system type, battery state, platform, relay target ids
	maxLong        = 256 // host name, OS, CPU and board model, notes
	maxMount       = 512
)

// ClipText cuts s to at most n bytes (on a character boundary, with "…" added); shorter texts are returned as
// they are.
func ClipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}

// clipP is ClipText for an optional text.
func clipP(p *string, n int) *string {
	if p == nil || len(*p) <= n {
		return p
	}
	s := ClipText(*p, n)
	return &s
}
