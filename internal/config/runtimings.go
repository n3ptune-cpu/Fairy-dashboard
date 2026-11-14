package config

// The timings of "Update unpowered hosts" that an admin can also change in the panel ("Run timings" on the
// Updates page): the five keys of the wake_update: block and approval.run_approval_wait_s. config.yaml stays
// the base; what is changed in the panel is kept in state.json as one decision per key laid over it
// (adminstate.RunTimings). This is the one list of them, with the range Validate checks for each key: the
// panel's API and the reader of state.json take their limits from here, so a value the file would refuse is
// refused there too.

// The keys, as config.yaml and state.json spell them.
const (
	KeyOffAfter        = "off_after_s"
	KeyOnlineTimeout   = "online_timeout_s"
	KeySettle          = "settle_s"
	KeyShutdownDelay   = "shutdown_delay_s"
	KeyRequestsPerMin  = "agent_requests_per_min"
	KeyRunApprovalWait = "run_approval_wait_s"
)

// RunTiming is one such timing: its key, the block of config.yaml it is a key of, its range and its default.
type RunTiming struct {
	Key               string
	Block             string // "wake_update" or "approval"
	Min, Max, Default int
}

// In reports whether v is a value the key can have.
func (t RunTiming) In(v int) bool { return v >= t.Min && v <= t.Max }

// runTimings is the list, in the order a run meets them (and the panel's dialog shows them).
var runTimings = []RunTiming{
	{KeyOffAfter, "wake_update", 300, 3600, 900},
	{KeyOnlineTimeout, "wake_update", 60, 1800, 300},
	{KeySettle, "wake_update", 10, 600, 60},
	{KeyShutdownDelay, "wake_update", 10, 3600, 120},
	{KeyRequestsPerMin, "wake_update", 1, 30, 8},
	{KeyRunApprovalWait, "approval", 60, 3600, 600},
}

// RunTimings returns the list (a copy).
func RunTimings() []RunTiming { return append([]RunTiming(nil), runTimings...) }

// RunTimingOf returns the timing with that key.
func RunTimingOf(key string) (RunTiming, bool) {
	for _, t := range runTimings {
		if t.Key == key {
			return t, true
		}
	}
	return RunTiming{}, false
}

// RunTiming is the value config.yaml gives the key (after Validate: a key that is left out has its default);
// 0 for a key that is none of them.
func (c *Panel) RunTiming(key string) int {
	switch key {
	case KeyOffAfter:
		return c.WakeUpdate.OffAfterS
	case KeyOnlineTimeout:
		return c.WakeUpdate.OnlineTimeoutS
	case KeySettle:
		return c.WakeUpdate.SettleS
	case KeyShutdownDelay:
		return c.WakeUpdate.ShutdownDelayS
	case KeyRequestsPerMin:
		return c.WakeUpdate.AgentRequestsPerMin
	case KeyRunApprovalWait:
		return c.Approval.RunApprovalWaitS
	}
	return 0
}

// MachineRunTimings are the two timings one machine can have of its own (the wake_update: block of its
// systems: entry; in the panel: "Edit host"), with the same ranges as the panel's.
func MachineRunTimings() []RunTiming {
	var out []RunTiming
	for _, t := range runTimings {
		if t.Key == KeyOffAfter || t.Key == KeyShutdownDelay {
			out = append(out, t)
		}
	}
	return out
}
