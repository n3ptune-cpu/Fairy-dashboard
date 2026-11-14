package wakerun

import (
	"strings"
	"testing"
	"time"
)

// wordOnce is the panel's view of a machine whose agent shows this run as "confirmed" while lie is set (a
// stale or forged answer, a machine that lies), and its real word otherwise.
type wordOnce struct {
	*world
	id  string
	lie *bool
}

func (x wordOnce) Observe(id string) Observation {
	o := x.world.Observe(id)
	if id == x.id && *x.lie && o.Online && o.Run != nil {
		b := *o.Run
		b.State = BlockConfirmed
		o.Run = &b
	}
	return o
}

// A machine the run took for confirmed by its agent's word, and the word did not hold: the machine is armed,
// refuses the run's update and its shutdown as not approved, and switches itself off by its ticket. It was
// ended as "Left ON: its agent did not let the run's shutdown through", and the strip no longer named it
// (third look at cdd080f). The run reads what its agent shows when the shutdown is refused: armed, or counting
// down, is said with the time, and the machine stays in the strip with Keep on.
func TestAWordConfirmedMachineThatIsArmedAfterAllIsEndedAsArmed(t *testing.T) {
	w, pc, act := askedRun(t, func(_ *world, pc *machine) { pc.chk.goSays = "user" })
	lie := false
	w.m.Ops = wordOnce{w, "pc-a", &lie}
	if _, err := w.confirm("Ann"); err != nil { // it leaves, and the machine refuses it
		t.Fatal(err)
	}
	lie, pc.chk.lied = true, true
	w.tick()
	w.tick()
	lie = false
	cur, _ := w.m.Snapshot()
	if h := host(cur, "pc-a"); h.Confirm != ConfirmOK || cur.Approval != "given" {
		t.Fatalf("the word was not taken: confirm %q, approval %q", h.Confirm, cur.Approval)
	}
	if c := w.bootOf(pc); c.state != BlockArmed || c.run != act.ID {
		t.Fatalf("the model: the machine is %q", c.state)
	}
	done := w.waitFor(20*time.Minute, "pc-a ended", func(r *Run) bool { return host(r, "pc-a").Final() })
	h := host(done, "pc-a")
	if h.State != StLeftOn || h.On != OnYes || h.Confirm == ConfirmOK || !h.MayGoOff {
		t.Fatalf("[%s] power %q, confirm %q, may go off %v: %s", h.State, h.On, h.Confirm, h.MayGoOff, h.Text)
	}
	for _, part := range []string{"Its agent did not let the run's shutdown through (", "and shows the run as armed now, not as confirmed.",
		"is armed: it begins to switch itself off at ", "unless that is cancelled (Keep on in the panel, or at the machine)"} {
		if !strings.Contains(h.Text, part) {
			t.Fatalf("the report lacks %q: %s", part, h.Text)
		}
	}
	if strings.HasPrefix(h.Text, "Left ON") {
		t.Fatalf("the report: %s", h.Text)
	}
	if a := named(w.m.MayGoOff(w.now), "pc-a"); a == nil || !a.Sure || a.At == nil || !a.ByMachine { // with the machine's own time
		t.Fatalf("the strip does not name it with Keep on: %+v", a)
	}
	none(t, w.callsFor("pc-a"), "cancel")
}
