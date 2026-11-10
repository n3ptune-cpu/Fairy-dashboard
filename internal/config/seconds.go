package config

import (
	"fmt"
	"regexp"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Seconds is a length of time in whole seconds (the session timings). In config.yaml it is a whole number, as
// it always was (session_idle_timeout: 43200, in any form YAML has for one), or a text with units: 90s, 30m,
// 4h, 1d, 1h30m, 1.5h. The rest of the program reads it as a number of seconds.
type Seconds int

// maxSeconds bounds what can be written (ten years), so nothing downstream can overflow.
const maxSeconds = 10 * 366 * 86400

var (
	wholeNumberRe = regexp.MustCompile(`^[0-9]+$`)
	// daysRe splits a leading day count off a duration text: Go's own durations stop at hours.
	daysRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)d(.*)$`)
	// unitsRe is what may follow: hours, minutes and seconds, each at most once and in that order.
	unitsRe = regexp.MustCompile(`^(?:[0-9]+(?:\.[0-9]+)?h)?(?:[0-9]+(?:\.[0-9]+)?m)?(?:[0-9]+(?:\.[0-9]+)?s)?$`)
)

// ParseSeconds reads "43200", "12h", "30m", "1d", "1h30m" or "1.5h". It refuses anything negative, anything
// that is not a whole number of seconds, units other than d / h / m / s, and more than ten years.
func ParseSeconds(text string) (Seconds, error) {
	bad := func(why string) (Seconds, error) {
		return 0, fmt.Errorf("%q is not a length of time (%s): write whole seconds, like 43200, or a text like 30m, 4h, 1d", text, why)
	}
	if text == "" {
		return bad("it is empty")
	}
	if text[0] == '-' || text[0] == '+' {
		return bad("it must not be negative or signed")
	}
	if wholeNumberRe.MatchString(text) {
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil || n > maxSeconds {
			return bad("it is longer than ten years")
		}
		return Seconds(n), nil
	}
	var total time.Duration
	rest := text
	if m := daysRe.FindStringSubmatch(text); m != nil {
		days, err := strconv.ParseFloat(m[1], 64)
		if err != nil || days > maxSeconds/86400 {
			return bad("it is longer than ten years")
		}
		total, rest = time.Duration(days*24*float64(time.Hour)), m[2]
	}
	if rest != "" {
		if !unitsRe.MatchString(rest) {
			return bad("the units are d, h, m and s, largest first")
		}
		d, err := time.ParseDuration(rest)
		if err != nil {
			return bad("it is longer than ten years")
		}
		total += d
	}
	if total < 0 || total > maxSeconds*time.Second {
		return bad("it is longer than ten years")
	}
	if total%time.Second != 0 {
		return bad("it is not a whole number of seconds")
	}
	return Seconds(total / time.Second), nil
}

// UnmarshalYAML accepts an integer (seconds) or a duration text; see ParseSeconds. An empty value leaves the
// default in place.
func (s *Seconds) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: a length of time must be one value: whole seconds, like 43200, or a text like 30m, 4h, 1d", n.Line)
	}
	if n.Tag == "!!null" {
		*s = 0
		return nil
	}
	// Until now these keys were plain integer fields, and a config.yaml written for that must keep loading
	// after an upgrade: besides 43200 that is every other way YAML writes a whole number (604_800, +604800,
	// 0x93A80, 604800.0, 6.048e5). Whatever the old field took is taken first, the same way, with two
	// exceptions that loaded then and are refused now: a negative number, and a fraction (it was cut off).
	var whole int64
	var exact float64
	if n.Decode(&whole) == nil && (n.Decode(&exact) != nil || exact == float64(whole)) {
		switch {
		case whole < 0:
			_, err := ParseSeconds("-")
			return fmt.Errorf("line %d: %q: %w", n.Line, n.Value, err)
		case whole > maxSeconds:
			return fmt.Errorf("line %d: %q is not a length of time (it is longer than ten years)", n.Line, n.Value)
		}
		*s = Seconds(whole)
		return nil
	}
	v, err := ParseSeconds(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*s = v
	return nil
}

// String writes s the way it may be written in the config: whole days, hours or minutes when it is that, else
// seconds.
func (s Seconds) String() string {
	switch {
	case s > 0 && s%86400 == 0:
		return strconv.Itoa(int(s/86400)) + "d"
	case s > 0 && s%3600 == 0:
		return strconv.Itoa(int(s/3600)) + "h"
	case s > 0 && s%60 == 0:
		return strconv.Itoa(int(s/60)) + "m"
	}
	return strconv.Itoa(int(s)) + "s"
}
