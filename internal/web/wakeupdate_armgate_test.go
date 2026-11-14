package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// started is a rig with a run that has just started for pc-a: its ticket is kept and has not gone out.
func started(t *testing.T, checked ...string) (*runRig, *wakerun.Run) {
	t.Helper()
	r := newRunRig(t, checked...)
	if _, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}}); res["ok"] != true {
		t.Fatal(res)
	}
	run, _ := r.s.WakeRuns.Snapshot()
	if held, open := r.s.tickets.held(); held != run.ID || open != 1 {
		t.Fatalf("the kept ticket: %q %d", held, open)
	}
	return r, run
}

// The gate before a ticket leaves the panel (runOps.Arm, the one way to POST /run/arm): each refusal ends
// with "nothing was sent", the machine is not asked, and the ticket stays in the keeper untaken.
func TestNoTicketLeavesThePanelWhenSomethingStandsInTheWay(t *testing.T) {
	notSent := func(t *testing.T, r *runRig, run *wakerun.Run, want string) {
		t.Helper()
		_, err := runOps{r.s}.Arm(context.Background(), "pc-a", run.ID)
		var oe *wakerun.OpError
		if !asOp(err, &oe) || oe.Code != wakerun.CodeTicketGone || !strings.Contains(oe.Text, want) {
			t.Fatalf("Arm: %v (want a not-sent with %q)", err, want)
		}
		if n := askedPaths(r.m["pc-a"], api.PathRunArm); n != 0 || len(r.sent("pc-a", api.PathRunArm)) != 0 {
			t.Fatalf("the machine was asked %d times", n)
		}
		if held, open := r.s.tickets.held(); held != run.ID || open != 1 {
			t.Fatalf("the ticket was taken out of the keeper: %q %d", held, open)
		}
	}
	t.Run("control is switched off in the panel", func(t *testing.T) {
		r, run := started(t, "pc-a")
		if err := r.s.Poller.Admin.SetControlDisabled(true, "test", "gap"); err != nil {
			t.Fatal(err)
		}
		notSent(t, r, run, "ontrol")
	})
	t.Run("the machine is not the one on record any more", func(t *testing.T) {
		r, run := started(t, "pc-a", "pc-b")
		sharedID(t, r)
		if r.s.Poller.BindingOf("pc-a").OK() {
			t.Fatal("the setup: pc-a's binding is in order")
		}
		notSent(t, r, run, "machine id")
	})
	t.Run("another host has its machine id (rule 3, asked again when the ticket is handed over)", func(t *testing.T) {
		r, run := started(t, "pc-a", "pc-b")
		sharedID(t, r)
		// an admin says that PC-A keeps the id: in order for every other approval, not for a ticket
		if _, _, err := r.s.Poller.AcceptMachine("pc-a", "test admin", poller.AcceptWant{MachineID: machineIDOf("pc-a")}); err != nil {
			t.Fatal(err)
		}
		if b := r.s.Poller.BindingOf("pc-a"); !b.OK() || len(r.s.Poller.SharedMachineID("pc-a")) == 0 {
			t.Fatalf("the setup: binding ok %v, shared %v", b.OK(), r.s.Poller.SharedMachineID("pc-a"))
		}
		notSent(t, r, run, "PC-B has the same machine id now, and a run's ticket is tied to the machine id alone")
	})
}

// sharedID lets PC-B come on showing PC-A's machine id.
func sharedID(t *testing.T, r *runRig) {
	t.Helper()
	r.b.Mu.Lock()
	r.f.on["pc-b"], r.f.booted["pc-b"] = true, r.f.now.Add(-time.Hour)
	r.b.Mu.Unlock()
	r.boot["pc-b"] = r.f.booted["pc-b"]
	block := r.m["pc-b"].Block()
	block["machine_id"] = machineIDOf("pc-a")
	r.gb.Stale("pc-b", block)
	r.tick(2)
	if shared := r.s.Poller.SharedMachineID("pc-a"); len(shared) != 1 || shared[0] != "pc-b" {
		t.Fatalf("SharedMachineID: %v", shared)
	}
}

func asOp(err error, to **wakerun.OpError) bool {
	oe, ok := err.(*wakerun.OpError)
	*to = oe
	return ok
}
