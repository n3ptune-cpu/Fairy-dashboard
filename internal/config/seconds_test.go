package config

import (
	"os"
	"strings"
	"testing"
)

// S-h: the two session timings take whole seconds, as before, or a duration text.
func TestSessionTimingsAcceptSecondsAndDurations(t *testing.T) {
	for _, c := range []struct {
		yaml      string
		idle, max Seconds
	}{
		{"", 4 * 3600, 86400}, // defaults: idle 4 h, max age 1 d
		{"session_idle_timeout: 43200\nsession_max_age: 604800\n", 43200, 604800}, // what the example config had until the defaults changed: kept
		{"session_idle_timeout: 14400\n", 14400, 86400},
		{"session_idle_timeout: 4h\n", 4 * 3600, 86400},
		{"session_idle_timeout: 30m\n", 1800, 86400},
		{"session_idle_timeout: 1d\n", 86400, 86400}, // as long as the default session: allowed
		{"session_idle_timeout: 12h\nsession_max_age: 7d\n", 12 * 3600, 7 * 86400},
		{"session_idle_timeout: \"12h\"\nsession_max_age: '2d'\n", 12 * 3600, 2 * 86400},
		{"session_idle_timeout: 1h30m\nsession_max_age: 1.5d\n", 5400, 129600},
		{"session_idle_timeout: 90s\nsession_max_age: 1d12h\n", 90, 129600},
		{"session_idle_timeout: \"3600\"\n", 3600, 86400},
		{"session_idle_timeout:\nsession_max_age: ~\n", 4 * 3600, 86400}, // empty: the default
		{"session_idle_timeout: 0\n", 4 * 3600, 86400},                   // 0 has always meant "default"
		{"session_max_age: 0\n", 4 * 3600, 86400},
		{"session_max_age: 1h\n", 3600, 3600},                         // idle defaults to the whole session when that is shorter than 4 h
		{"session_max_age: 7d\n", 4 * 3600, 7 * 86400},                // a longer session keeps the 4 h idle default
		{"session_idle_timeout: 60\nsession_max_age: 300\n", 60, 300}, // the smallest values
		{"session_idle_timeout: 7d\nsession_max_age: 7d\n", 7 * 86400, 7 * 86400},
		// Upgrade in place: every way YAML writes a whole number loaded while these keys were integer fields
		// (the release before S-h), so each must still load, with the same value.
		{"session_idle_timeout: 43_200\nsession_max_age: 604_800\n", 43200, 604800},
		{"session_idle_timeout: +43200\nsession_max_age: +604800\n", 43200, 604800},
		{"session_idle_timeout: 43200.0\nsession_max_age: 604800.0\n", 43200, 604800},
		{"session_idle_timeout: 4.32e4\nsession_max_age: 6.048e5\n", 43200, 604800},
		{"session_idle_timeout: 0xA8C0\nsession_max_age: 0x93A80\n", 43200, 604800},
		{"session_idle_timeout: 0o124300\n", 43200, 86400},
		{"session_idle_timeout: 1e3\n", 1000, 86400},
		{"session_idle_timeout: 0x3c\n", 60, 86400},
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
		"session_idle_timeout: 43200.5\n":                     "not a length of time",
		"session_idle_timeout: 1e-3\n":                        "not a length of time",
		"session_idle_timeout: -0x3c\n":                       "must not be negative",
		"session_idle_timeout: -43200.0\n":                    "must not be negative",
		"session_idle_timeout: .inf\n":                        "not a length of time",
		"session_idle_timeout: .nan\n":                        "not a length of time",
		"session_max_age: 1e12\n":                             "ten years",
		"session_max_age: 99999999999999999999\n":             "ten years",
		"session_max_age: 100000d\n":                          "ten years",
		"session_idle_timeout: 59\n":                          "at least 60 s",
		"session_idle_timeout: 59s\n":                         "at least 60 s",
		"session_idle_timeout: 1\n":                           "at least 60 s",
		"session_max_age: 299\n":                              "at least 300 s",
		"session_max_age: 4m\n":                               "at least 300 s",
		"session_idle_timeout: 8d\n":                          "must not be longer than session_max_age",
		"session_idle_timeout: 2d\n":                          "must not be longer than session_max_age", // was allowed while the default session was 7 d
		"session_idle_timeout: 86401\n":                       "must not be longer than session_max_age",
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

// The defaults are whole seconds, as config.yaml has always written these two keys: 14400 and 86400.
func TestSessionDefaults(t *testing.T) {
	if DefaultSessionIdleTimeout != 14400 || DefaultSessionMaxAge != 86400 {
		t.Fatalf("defaults: idle %d, max age %d", DefaultSessionIdleTimeout, DefaultSessionMaxAge)
	}
	p, err := Parse([]byte(base))
	if err != nil || p.SessionIdleTimeout != 14400 || p.SessionMaxAge != 86400 {
		t.Fatalf("a config.yaml without the keys: idle %d, max age %d, %v", p.SessionIdleTimeout, p.SessionMaxAge, err)
	}
	// the example config names the defaults, as numbers
	ex, err := os.ReadFile("../../deploy/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"session_max_age: 86400 ", "session_idle_timeout: 14400 "} {
		if !strings.Contains(string(ex), want) {
			t.Errorf("deploy/config.example.yaml lacks %q", want)
		}
	}
	for _, old := range []string{"604800", "43200"} {
		if strings.Contains(string(ex), old) {
			t.Errorf("deploy/config.example.yaml still names %s", old)
		}
	}
}

func TestSecondsString(t *testing.T) {
	for s, want := range map[Seconds]string{604800: "7d", 43200: "12h", 86400: "1d", 14400: "4h", 1800: "30m", 90: "90s", 0: "0s", 129600: "36h"} {
		if s.String() != want {
			t.Errorf("%d: %s, want %s", int(s), s, want)
		}
	}
}
