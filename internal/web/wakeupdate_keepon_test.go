package web

import (
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
)

// Keep on for a machine whose ticket can still arrive, pressed while a confirmation of the run is on its way
// to that machine. The run takes no cancel over in that moment (what comes of the confirmation is taken in
// first), and the answer was "The run it was in is not going any more, so nothing repeats the cancel": the run
// was going. It says what is so: a confirmation is on its way, and Keep on can be pressed again in a moment.
func TestKeepOnWhileAConfirmationIsOnItsWaySaysSo(t *testing.T) {
	r := newRunRig(t, "pc-a")
	keep := make(chan resp, 1)
	var hdr []string
	r.gb.Relay("pc-a", func(path string, body any) (int, jsonx.M) {
		switch path {
		case api.PathRunArm:
			return 504, jsonx.M{} // no answer: the ticket may still reach the machine
		case api.PathRunGo: // the confirmation is on its way: somebody presses Keep on
			keep <- r.do("POST", "/api/systems/pc-a/wake-update/keep-on", map[string]any{}, hdr...)
		}
		return 0, nil
	})
	if _, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}}); res["ok"] != true {
		t.Fatal(res)
	}
	r.until(t, "asked", 300, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a != nil && a.Approval == "needed" })
	run, _ := r.s.WakeRuns.Snapshot()
	hdr = []string{"Cookie", r.cookie(controller(), 0), "X-Requested-With", "fairy", "Content-Type", "application/json"}
	r.approved(t, "POST", "/api/admin/wake-update/"+run.ID+"/confirm", map[string]any{})
	var yes resp
	select {
	case yes = <-keep:
	default:
		t.Fatal("no confirmation left the panel")
	}
	msg, _ := yes.body["message"].(string)
	if yes.code != 200 || yes.body["stays_on"] != false || !strings.HasPrefix(msg, "Not known until ") ||
		!strings.HasSuffix(msg, " may still reach it. A confirmation of the run is on its way to PC-A at this moment, so the run did not take the cancel over: press Keep on again in a moment.") {
		t.Fatalf("Keep on while a confirmation is on its way: %d %s", yes.code, yes.raw)
	}
	if strings.Contains(msg, "not going any more") {
		t.Fatalf("the run is going: %q", msg)
	}
}
