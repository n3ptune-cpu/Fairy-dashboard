//go:build unix

package wakerun

import (
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A cancel that comes in while the journal is being told "asking: arm" (the run holds no lock during that
// write) is seen before the ticket goes out: no ticket is handed to a machine of a run that was cancelled a
// moment before. (Independent review of 270e7ab: the mutant that drops this look survived; the window is a
// single write, so the test holds the journal's lock file to stand in it.)

// cancelAtArm is the panel's Ops with a hook at the last question the run asks before it writes "asking: arm".
type cancelAtArm struct {
	*world
	hook func(id string)
}

func (c cancelAtArm) AskGuests(id string) (bool, bool, time.Duration) {
	c.hook(id)
	return c.world.AskGuests(id)
}

func TestACancelDuringTheJournalsAskingArmSendsNoTicket(t *testing.T) {
	w := newWorld(t)
	pc := w.checkedOff("pc-a")
	type seen struct {
		arming bool
		err    error
	}
	done := make(chan seen, 1)
	fired := false
	w.m.Ops = cancelAtArm{w, func(id string) {
		// on the run's goroutine, under its lock: the step that is about to hand pc-a its ticket
		h := host(w.m.active, "pc-a")
		if fired || id != "pc-a" || h.State != StOnline || h.Arm != "" || h.chk.armTried {
			return
		}
		fired = true
		// the journal's next write waits for this lock: the run stands in "asking: arm", its own lock released
		f, err := os.OpenFile(w.m.path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			t.Error(err)
			return
		}
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
			t.Error(err)
			return
		}
		run := w.m.active.ID
		go func() {
			arming := w.m.Arming("pc-a") // answers once the run has released its lock for the write
			_, err := w.m.CancelRun(run, "Ann", false)
			_ = unix.Flock(int(f.Fd()), unix.LOCK_UN) // now the write goes through, and the run looks again
			f.Close()
			done <- seen{arming, err}
		}()
	}}
	w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
	for i := 0; i < 600 && !fired; i++ {
		w.tick()
	}
	if !fired {
		t.Fatal("the run never came to arm pc-a")
	}
	got := <-done
	if got.err != nil || !got.arming {
		t.Fatalf("the cancel did not come in while the journal was told \"asking: arm\": arming=%v, %v", got.arming, got.err)
	}
	w.advance(5 * time.Second)
	if calls := w.callsFor("pc-a"); strings.Contains(" "+calls+" ", " arm ") || pc.chk.arms != 0 {
		t.Fatalf("a ticket was handed over after the run was cancelled: %q (arms %d)", calls, pc.chk.arms)
	}
	finished := w.finish()
	h := host(finished, "pc-a")
	if h.Arm != "" || !strings.Contains(h.Text, "The run was cancelled by Ann") || !pc.on {
		t.Fatalf("the report: arm %q, %q; the machine is on: %v", h.Arm, h.Text, pc.on)
	}
	if w.bootOf(pc).state != "" || w.bootOf(pc).closed {
		t.Fatalf("the machine has a run on record: %+v", w.bootOf(pc))
	}
}
