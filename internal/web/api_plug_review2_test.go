package web

import (
	"testing"
)

// Tests for the second review of the fixes of stage 1a (its findings 1 to 6, and the mutations of its own that
// survived the suite). Each test names the finding it is for.

// Finding 3: a meter that reads below zero is no sign that the machine is off. The PC does not answer, and its
// own plug reads -30 W (a clamp that was put on backwards reads so for a PC that draws 30 W): "off" asked no
// question and went to the passkey. Only a reading from 0 W to under 5 W lets "off" through.
func TestPlugMeterBelowZeroIsNoSignOfOff(t *testing.T) {
	pe := newPlugEnv(t)
	pe.pcSilent()
	for _, c := range []struct {
		watts float64
		asks  bool
		said  string
	}{
		{-30, true, "DESK-PC does not answer, and its plug reads -30 W, which is no sign that it is off: it may still be running. Shut it down first, or look at it."},
		{-0.5, true, "DESK-PC does not answer, and its plug reads -0.5 W, which is no sign that it is off: it may still be running. Shut it down first, or look at it."},
		{0, false, ""},
		{4.99, false, ""},
		{5, true, "DESK-PC does not answer, and its plug still measures 5 W: it may still be running. Shut it down first, or look at it."},
	} {
		pe.measures("pc-plug", c.watts)
		r := pe.flip("reader/pc-plug", "main", "off")
		d, _ := r.body["detail"].(map[string]any)
		if _, _, passkey := approvalAsked(r); passkey == c.asks {
			t.Errorf("%v W: the passkey step came at once: %v (%d %s)", c.watts, passkey, r.code, r.raw)
			continue
		}
		if c.asks && (r.code != 409 || d["code"] != codeMachineRunning || d["state"] != "unknown" || d["machine"] != "DESK-PC" || d["message"] != c.said) {
			t.Errorf("%v W: %d %s\nwant machine_running, state unknown: %q", c.watts, r.code, r.raw, c.said)
		}
	}
	if n := len(plugSent(pe.reader)); n != 0 {
		t.Errorf("the machine got %d switch requests", n)
	}
}
