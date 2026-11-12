//go:build !windows

package adminstate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Audit 6, r4-p2-04, as the running panel sees it: state.json becomes a link whose target is away. The kill
// switch, every host's power switch and the sessions must read as they do for every other shape the panel
// cannot read: control off, power off, no session valid. It read as a fresh start (all three open).
func TestStateJSONAsALinkToNothingUnderARunningPanel(t *testing.T) {
	dir := t.TempDir()
	panel := New(dir)
	if err := panel.AddSession("sid-of-alice-0001", NewSession{Sub: "alice"}); err != nil {
		t.Fatal(err)
	}
	iat := float64(time.Now().Unix())
	if panel.ControlDisabled() != nil || panel.PowerDisabled("pc-a") || !panel.SessionValid("sid-of-alice-0001", "alice", iat) {
		t.Fatal("the set-up: control must be on and the session valid")
	}
	p := filepath.Join(dir, "state.json")
	_ = os.Remove(p)
	if err := os.Symlink(filepath.Join(dir, "not-mounted", "state.json"), p); err != nil {
		t.Fatal(err)
	}
	if panel.SessionValid("sid-of-alice-0001", "alice", iat) { // (this one looks at the file at every call)
		t.Error("a session is valid while state.json is a link to nothing")
	}
	if panel.ControlDisabled() == nil {
		t.Error("control is allowed while state.json is a link to nothing")
	}
	if !panel.PowerDisabled("pc-a") {
		t.Error("power control for a host is on while state.json is a link to nothing")
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if !panel.SessionValid("sid-of-alice-0001", "alice", iat) || panel.ControlDisabled() != nil || panel.PowerDisabled("pc-a") {
		t.Error("with the link gone (no state.json: a fresh start) control is still off")
	}
}
