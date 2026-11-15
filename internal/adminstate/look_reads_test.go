package adminstate

import (
	"os"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
)

// The kill switch, a host's power switch and the check of a session are asked at every request (and the power
// switch once per host of a page). Each looks at its file now, which is one stat, and does not read it: a
// file is read when a look finds another file, and at every look only for the short time after the panel
// first saw it (statefile.YoungFor). A panel that read and parsed state.json and sessions.json for every
// request would still be right, and nobody would see it but in the load.
func TestTheSwitchesAndTheSessionCheckDoNotReadTheirFilesAtEveryRequest(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)
	if err := a.SetPowerDisabled("pc-a", true); err != nil {
		t.Fatal(err)
	}
	if err := a.AddSession("sid-of-ann-000001", NewSession{Sub: "ann"}); err != nil {
		t.Fatal(err)
	}
	iat := float64(time.Now().Unix()) - 5
	ask := func(when string) {
		t.Helper()
		if a.ControlDisabled() != nil || !a.PowerDisabled("pc-a") || a.PowerDisabled("pc-b") || !a.SessionValid("sid-of-ann-000001", "ann", iat) {
			t.Fatalf("%s: control off %v, pc-a off %v, pc-b off %v, the session valid %v", when,
				a.ControlDisabled() != nil, a.PowerDisabled("pc-a"), a.PowerDisabled("pc-b"), a.SessionValid("sid-of-ann-000001", "ann", iat))
		}
	}
	ask("after the writes")
	time.Sleep(statefile.YoungFor + 100*time.Millisecond)
	ask("the files are no longer young") // the last read of each
	state, sessions := a.state.Reads, a.sessions.Reads
	for i := 0; i < 200; i++ {
		ask("a request")
	}
	if a.state.Reads != state || a.sessions.Reads != sessions {
		t.Errorf("200 requests read state.json %d times and sessions.json %d times; nothing was written", a.state.Reads-state, a.sessions.Reads-sessions)
	}
	// and what another process writes is there at the next request, as before
	if err := New(dir).SetControlDisabled(true, "root", "a test"); err != nil {
		t.Fatal(err)
	}
	if a.ControlDisabled() == nil || a.state.Reads != state+1 {
		t.Errorf("the kill switch of another process: control off %v after %d reads (want 1)", a.ControlDisabled() != nil, a.state.Reads-state)
	}
}

// Revoke-all writes sessions.json and then state.json. When the second write is refused (state.json is
// damaged), the first one is made, and the panel's view of sessions.json has it at once, not at its next look.
func TestRevokeAllTellsTheViewOfSessionsWhenStateJSONCannotBeWritten(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)
	if err := a.AddSession("sid-of-ann-000001", NewSession{Sub: "ann"}); err != nil {
		t.Fatal(err)
	}
	if l, err := a.Sessions(); err != nil || len(l) != 1 { // the view, which SessionValid decides by
		t.Fatalf("the set-up: %v, %v", l, err)
	}
	if err := os.WriteFile(a.StatePath, []byte(`{"control_disabled": `), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RevokeAllSessions(); err == nil {
		t.Fatal("the rig: revoke-all wrote a damaged state.json")
	}
	if b, _ := os.ReadFile(a.SessionsPath); len(b) > 40 {
		t.Fatalf("the rig: sessions.json was not emptied: %s", b)
	}
	if l, err := a.Sessions(); err != nil || len(l) != 0 {
		t.Errorf("sessions.json holds no session, the panel's view of it holds %d (%v)", len(l), err)
	}
}
