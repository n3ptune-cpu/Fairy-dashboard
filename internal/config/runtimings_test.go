package config

import (
	"fmt"
	"strings"
	"testing"
)

// The list the panel's "Run timings" takes its limits from is the one config.yaml is checked by: every key of
// it is accepted by Parse at both ends of its range and refused one step outside, has its default when it is
// left out, and the list has every key of the wake_update: block (wakeUpdateRanges) with the same numbers.
func TestRunTimingsAreTheRangesConfigYAMLIsCheckedBy(t *testing.T) {
	const base = "oidc: {issuer: \"https://id.example.com\", client_id: fairy}\npublic_url: https://fairy.example.com\n"
	doc := func(tm RunTiming, v int) string {
		return fmt.Sprintf("%s%s: {%s: %d}\n", base, tm.Block, tm.Key, v)
	}
	plain, err := Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tm := range RunTimings() {
		seen[tm.Key] = true
		if got := plain.RunTiming(tm.Key); got != tm.Default {
			t.Errorf("%s left out: %d, the list says %d", tm.Key, got, tm.Default)
		}
		if !tm.In(tm.Default) || tm.In(tm.Min-1) || tm.In(tm.Max+1) || !tm.In(tm.Min) || !tm.In(tm.Max) {
			t.Errorf("%s: In does not follow %d-%d", tm.Key, tm.Min, tm.Max)
		}
		for _, v := range []int{tm.Min, tm.Max} {
			c, err := Parse([]byte(doc(tm, v)))
			if err != nil {
				t.Errorf("%s.%s: %d must be accepted: %v", tm.Block, tm.Key, v, err)
				continue
			}
			if got := c.RunTiming(tm.Key); got != v {
				t.Errorf("%s.%s: %d is read back as %d", tm.Block, tm.Key, v, got)
			}
		}
		for _, v := range []int{tm.Min - 1, tm.Max + 1} {
			if _, err := Parse([]byte(doc(tm, v))); err == nil || !strings.Contains(err.Error(), tm.Key) {
				t.Errorf("%s.%s: %d must be refused with the key's name, got %v", tm.Block, tm.Key, v, err)
			}
		}
		if r, ok := wakeUpdateRanges[tm.Key]; tm.Block == "wake_update" && (!ok || r != [3]int{tm.Min, tm.Max, tm.Default}) {
			t.Errorf("%s: the list says %d-%d (%d), the block's own table %v", tm.Key, tm.Min, tm.Max, tm.Default, r)
		}
		if got, ok := RunTimingOf(tm.Key); !ok || got != tm {
			t.Errorf("RunTimingOf(%s) = %v, %v", tm.Key, got, ok)
		}
	}
	for key := range wakeUpdateRanges {
		if !seen[key] {
			t.Errorf("wake_update.%s is not in the list", key)
		}
	}
	if len(seen) != len(wakeUpdateRanges)+1 || !seen[KeyRunApprovalWait] {
		t.Errorf("the list: %v", seen)
	}
	if _, ok := RunTimingOf("url"); ok || plain.RunTiming("url") != 0 {
		t.Error("a key that is no timing has one")
	}
	// one machine's own: the two keys a systems: entry takes, with the panel's ranges
	var keys []string
	for _, tm := range MachineRunTimings() {
		keys = append(keys, fmt.Sprintf("%s %d-%d", tm.Key, tm.Min, tm.Max))
	}
	if got := strings.Join(keys, ", "); got != "off_after_s 300-3600, shutdown_delay_s 10-3600" {
		t.Errorf("a machine's own timings: %s", got)
	}
	for _, bad := range []string{"wake_update: {off_after_s: 299}", "wake_update: {off_after_s: 3601}", "wake_update: {shutdown_delay_s: 9}", "wake_update: {shutdown_delay_s: 3601}"} {
		if _, err := Parse([]byte(base + "systems:\n  - {id: pc-a, " + bad + "}\n")); err == nil {
			t.Errorf("a systems: entry with %s must be refused", bad)
		}
	}
}
