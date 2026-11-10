package config

import (
	"strings"
	"testing"
)

// S-h: the two session timings take whole seconds, as before, or a duration text.
func TestSessionTimingsAcceptSecondsAndDurations(t *testing.T) {
	for _, c := range []struct {
		yaml      string
		idle, max Seconds
	}{
		{"", 12 * 3600, 7 * 86400}, // defaults: idle 12 h, max age 7 d
		{"session_idle_timeout: 43200\nsession_max_age: 604800\n", 43200, 604800}, // the example config, unchanged
		{"session_idle_timeout: 14400\n", 14400, 7 * 86400},
		{"session_idle_timeout: 4h\n", 4 * 3600, 7 * 86400},
		{"session_idle_timeout: 30m\n", 1800, 7 * 86400},
		{"session_idle_timeout: 1d\n", 86400, 7 * 86400},
		{"session_idle_timeout: 12h\nsession_max_age: 7d\n", 12 * 3600, 7 * 86400},
		{"session_idle_timeout: \"12h\"\nsession_max_age: '2d'\n", 12 * 3600, 2 * 86400},
		{"session_idle_timeout: 1h30m\nsession_max_age: 1.5d\n", 5400, 129600},
		{"session_idle_timeout: 90s\nsession_max_age: 1d12h\n", 90, 129600},
		{"session_idle_timeout: \"3600\"\n", 3600, 7 * 86400},
		{"session_idle_timeout:\nsession_max_age: ~\n", 12 * 3600, 7 * 86400}, // empty: the default
		{"session_idle_timeout: 0\n", 12 * 3600, 7 * 86400},                   // 0 has always meant "default"
		{"session_max_age: 1h\n", 3600, 3600},                                 // idle defaults to the whole session when that is shorter than 12 h
		{"session_idle_timeout: 60\nsession_max_age: 300\n", 60, 300},         // the smallest values
		{"session_idle_timeout: 7d\nsession_max_age: 7d\n", 7 * 86400, 7 * 86400},
	} {
		p, err := Parse([]byte(base + c.yaml))
		if err != nil {
			t.Errorf("%q: %v", c.yaml, err)
			continue
		}
		if p.SessionIdleTimeout != c.idle || p.SessionMaxAge != c.max {
			t.Errorf("%q: idle %d max %d, want %d %d", c.yaml, p.SessionIdleTimeout, p.SessionMaxAge, c.idle, c.max)
		}
	}
}

// S-h, S-e: negatives and nonsense are refused with a message that says what is wrong; the idle timeout must be
// at least a minute and not longer than the maximum age.
func TestSessionTimingsRefuseNonsense(t *testing.T) {
	for y, want := range map[string]string{
		"session_idle_timeout: -1\n":                          "must not be negative",
		"session_idle_timeout: -5\n":                          "must not be negative",
		"session_idle_timeout: -4h\n":                         "must not be negative",
		"session_max_age: -604800\n":                          "must not be negative",
		"session_idle_timeout: abc\n":                         "not a length of time",
		"session_idle_timeout: 1.5\n":                         "not a length of time",
		"session_idle_timeout: 4 h\n":                         "not a length of time",
		"session_idle_timeout: 4hours\n":                      "not a length of time",
		"session_idle_timeout: 30m4h\n":                       "largest first",
		"session_idle_timeout: 4w\n":                          "units are d, h, m and s",
		"session_idle_timeout: 500ms\n":                       "units are d, h, m and s",
		"session_idle_timeout: 1.0000001s\n":                  "whole number of seconds",
		"session_idle_timeout: true\n":                        "not a length of time",
		"session_idle_timeout: [1, 2]\n":                      "must be one value",
		"session_idle_timeout: 1e3\n":                         "not a length of time",
		"session_idle_timeout: 0x3c\n":                        "not a length of time",
		"session_max_age: 99999999999999999999\n":             "ten years",
		"session_max_age: 100000d\n":                          "ten years",
		"session_idle_timeout: 59\n":                          "at least 60 s",
		"session_idle_timeout: 59s\n":                         "at least 60 s",
		"session_idle_timeout: 1\n":                           "at least 60 s",
		"session_max_age: 299\n":                              "at least 300 s",
		"session_max_age: 4m\n":                               "at least 300 s",
		"session_idle_timeout: 8d\n":                          "must not be longer than session_max_age",
		"session_idle_timeout: 604801\n":                      "must not be longer than session_max_age",
		"session_idle_timeout: 2h\nsession_max_age: 1h\n":     "must not be longer than session_max_age",
		"session_idle_timeout: 43201\nsession_max_age: 12h\n": "must not be longer than session_max_age",
	} {
		_, err := Parse([]byte(base + y))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want an error with %q", y, err, want)
		}
	}
	// the error says where and what
	_, err := Parse([]byte(base + "session_idle_timeout: abc\n"))
	if err == nil || !strings.Contains(err.Error(), "line 4") || !strings.Contains(err.Error(), `"abc"`) || !strings.Contains(err.Error(), "4h") {
		t.Fatalf("message must name the line, the value and an example: %v", err)
	}
}

func TestSecondsString(t *testing.T) {
	for s, want := range map[Seconds]string{604800: "7d", 43200: "12h", 1800: "30m", 90: "90s", 0: "0s", 129600: "36h"} {
		if s.String() != want {
			t.Errorf("%d: %s, want %s", int(s), s, want)
		}
	}
}
