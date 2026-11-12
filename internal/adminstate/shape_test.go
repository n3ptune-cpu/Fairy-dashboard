package adminstate

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// logged runs fn and returns what was logged meanwhile.
func logged(t *testing.T, fn func()) string {
	t.Helper()
	old := slog.Default()
	defer slog.SetDefault(old)
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	fn()
	return buf.String()
}

// Audit 5, round 3 (r3-p2-07): in state.json a "control_disabled" that is not the object Fairy writes (true,
// "disabled by Ann", 1, a list, null: a hand edit) was read as "no kill switch": control stayed ON. Every
// value under the key now means that control is off; only a file without the key allows control. One ERROR
// names the key, once for each change of the file.
func TestAKillSwitchOfAnyShapeIsOn(t *testing.T) {
	for _, c := range []struct {
		value string // what is under "control_disabled" ("": the key is not there)
		off   bool
		odd   bool // not the shape Fairy writes: an ERROR
	}{
		{``, false, false},
		{`{"by": "root (cli)", "at": "2026-11-12T10:00:00Z", "reason": "maintenance"}`, true, false},
		{`{}`, true, false},
		{`{"by": 5}`, true, false},
		{`true`, true, true},
		{`false`, true, true},
		{`"disabled by Ann"`, true, true},
		{`""`, true, true},
		{`1`, true, true},
		{`0`, true, true},
		{`[]`, true, true},
		{`["root"]`, true, true},
		{`null`, true, true},
	} {
		dir := t.TempDir()
		a := New(dir)
		content := `{"sessions_revoked_before": 0}`
		if c.value != "" {
			content = `{"control_disabled": ` + c.value + `, "sessions_revoked_before": 0}`
		}
		if err := os.WriteFile(a.StatePath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		var off bool
		log := logged(t, func() {
			for i := 0; i < 5; i++ { // asked with every request: said once
				off = a.ControlDisabled() != nil
			}
		})
		if off != c.off {
			t.Errorf("control_disabled %s: control off = %v, want %v", c.value, off, c.off)
		}
		if n := strings.Count(log, "level=ERROR"); (n == 1) != c.odd || n > 1 {
			t.Errorf("control_disabled %s: %d ERROR lines, want one: %v\n%s", c.value, n, c.odd, log)
		}
		if c.odd && (!strings.Contains(log, `\"control_disabled\" is not the object {by, at, reason} that Fairy writes: control is OFF`) || !strings.Contains(log, "key=control_disabled") || !strings.Contains(log, a.StatePath)) {
			t.Errorf("control_disabled %s: the ERROR does not name the key and the file: %s", c.value, log)
		}
		if c.odd {
			st := a.ControlDisabled()
			if st.By != "fairy" || !strings.Contains(st.Reason, "control_disabled") || !strings.Contains(st.Reason, "fairy admin control enable") {
				t.Errorf("control_disabled %s: what the page shows: %+v", c.value, st)
			}
			// the remedy the message names works, whatever is under the key
			if err := a.SetControlDisabled(false, "root (cli)", ""); err != nil || a.ControlDisabled() != nil {
				t.Errorf("control_disabled %s: after \"control enable\": %v %+v", c.value, err, a.ControlDisabled())
			}
		}
	}
}

// The same for the per-host power switch: a "power_control_disabled" that is not a list is "off for every
// host", never "off for none".
func TestAPowerSwitchListOfAnyShapeIsOn(t *testing.T) {
	for _, c := range []struct {
		value   string
		off, pc bool // for "laptop", for "pc-a"
		odd     bool
	}{
		{``, false, false, false},
		{`[]`, false, false, false},
		{`["laptop"]`, true, false, false},
		// (second review of fix7a, R7A-05: a list with an entry that is not a text is no list of system ids. It
		// read as "off for laptop, on for everybody else", and the two below as "on for every host", without a word.)
		{`["laptop", 5, null]`, true, true, true},
		{`[{"id": "pc-a"}]`, true, true, true},
		{`[["pc-a"]]`, true, true, true},
		{`["pc-a", {"id": "laptop"}]`, true, true, true},
		{`"laptop"`, true, true, true},
		{`true`, true, true, true},
		{`{"laptop": true}`, true, true, true},
		{`null`, true, true, true},
		{`0`, true, true, true},
	} {
		dir := t.TempDir()
		a := New(dir)
		content := `{}`
		if c.value != "" {
			content = `{"power_control_disabled": ` + c.value + `}`
		}
		if err := os.WriteFile(a.StatePath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		var off, pc bool
		log := logged(t, func() {
			for i := 0; i < 5; i++ {
				off, pc = a.PowerDisabled("laptop"), a.PowerDisabled("pc-a")
			}
		})
		if off != c.off || pc != c.pc {
			t.Errorf("power_control_disabled %s: off for laptop %v, for pc-a %v; want %v, %v", c.value, off, pc, c.off, c.pc)
		}
		if n := strings.Count(log, "level=ERROR"); (n == 1) != c.odd || n > 1 || (c.odd && !strings.Contains(log, "key=power_control_disabled")) {
			t.Errorf("power_control_disabled %s: %d ERROR lines, want one that names the key: %v\n%s", c.value, n, c.odd, log)
		}
		if c.odd && !strings.Contains(log, `\"power_control_disabled\" is not the list of system ids that Fairy writes: power control is OFF for every host`) {
			t.Errorf("power_control_disabled %s: the ERROR: %s", c.value, log)
		}
		// Second review of fix7a (R7A-05): switching ONE host on or off wrote the list anew from what the command
		// knew, an empty list plus that host: every other host, off a moment ago, was ON again, without a word.
		// While the key has such a shape nothing is written, and the error says how to put it right.
		if c.odd {
			before, _ := os.ReadFile(a.StatePath)
			for _, disable := range []bool{false, true} {
				err := a.SetPowerDisabled("laptop", disable)
				after, _ := os.ReadFile(a.StatePath)
				if err == nil || !bytes.Equal(before, after) || !a.PowerDisabled("laptop") || !a.PowerDisabled("pc-a") {
					t.Errorf("power_control_disabled %s: set laptop disabled=%v: error %v, file changed %v, off for laptop %v, for pc-a %v", c.value, disable, err,
						!bytes.Equal(before, after), a.PowerDisabled("laptop"), a.PowerDisabled("pc-a"))
					continue
				}
				for _, part := range []string{`"power_control_disabled"`, a.StatePath, "nothing was changed", `"power_control_disabled": []`, "run this command again"} {
					if !strings.Contains(err.Error(), part) {
						t.Errorf("power_control_disabled %s: the error lacks %q: %v", c.value, part, err)
					}
				}
			}
		} else { // a list of the shape Fairy writes, or no key: the command works as ever
			if err := a.SetPowerDisabled("pc-a", true); err != nil || !a.PowerDisabled("pc-a") || a.PowerDisabled("laptop") != c.off {
				t.Errorf("power_control_disabled %s: after switching pc-a off: %v; off for pc-a %v, for laptop %v", c.value, err, a.PowerDisabled("pc-a"), a.PowerDisabled("laptop"))
			}
			if err := a.SetPowerDisabled("pc-a", false); err != nil || a.PowerDisabled("pc-a") || a.PowerDisabled("laptop") != c.off {
				t.Errorf("power_control_disabled %s: after switching pc-a on again: %v; off for pc-a %v, for laptop %v", c.value, err, a.PowerDisabled("pc-a"), a.PowerDisabled("laptop"))
			}
		}
	}
}

// The reviewer's proof of R7A-05 (b): with "power_control_disabled": true every host is off. An admin switches
// pc-a off with the command: pc-b, off a moment ago, was ON again.
func TestSettingOneHostDoesNotSwitchTheOthersOn(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"power_control_disabled": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a := New(dir)
	if !a.PowerDisabled("pc-a") || !a.PowerDisabled("pc-b") {
		t.Fatal("an odd shape must be off for every host")
	}
	err := a.SetPowerDisabled("pc-a", true)
	if !a.PowerDisabled("pc-b") {
		t.Errorf("after \"host pc-a power-control disable\" power control for pc-b, which was off a moment ago, is ON (the command said: %v)", err)
	}
	if err == nil {
		t.Errorf("the command wrote over a \"power_control_disabled\" that is not a list, and said nothing")
	}
}
