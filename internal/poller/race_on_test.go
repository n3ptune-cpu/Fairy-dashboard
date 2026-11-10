//go:build race

package poller

// raceEnabled: the tests run under the race detector (memory measurements are logged, not asserted).
const raceEnabled = true
