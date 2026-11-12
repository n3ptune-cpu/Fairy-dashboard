package web

import (
	"context"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// Audit 5, round 2 (r2-p2-03, -04, -05): requests of a run whose answer was lost.

// A request of a run that left the panel and got no answer reaches the run as "no answer" (the machine may
// have taken it); one that never left, and an agent's own refusal, do not.
func TestWakeUpdateARequestWithoutAnAnswerIsToldApart(t *testing.T) {
	e := setup(t)
	ops := runOps{e.s}
	ctx := context.Background()
	code := func(err error) string {
		oe, _ := err.(*wakerun.OpError)
		if oe == nil {
			return "?"
		}
		return oe.Code + "|" + oe.Text
	}
	for _, c := range []struct {
		reply func() (int, jsonx.M, error)
		want  string
	}{
		{func() (int, jsonx.M, error) { return 0, nil, context.DeadlineExceeded }, "no_answer|agent unreachable: timeout"},
		{func() (int, jsonx.M, error) {
			return 0, nil, &poller.NotSent{Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}}
		}, "|agent unreachable: connection failed"},
		{func() (int, jsonx.M, error) {
			return 409, jsonx.M{"error": "a shutdown is already pending; cancel it first"}, nil
		}, "|a shutdown is already pending; cancel it first"},
	} {
		e.b.Reply = func(string, string) (int, jsonx.M, error) { return c.reply() }
		if got := code(ops.Update(ctx, "laptop", "Ann", nil)); got != c.want {
			t.Errorf("update: %q, want %q", got, c.want)
		}
		if _, err := ops.Shutdown(ctx, "laptop", 2*time.Minute, "Ann", nil); code(err) != c.want {
			t.Errorf("shutdown: %q, want %q", code(err), c.want)
		}
	}
}

// lostFarm is the farm with pc-a and pc-b off for more than a quarter of an hour and switched on for the
// runs. lost names the request whose answer never reaches the panel ("nas /wol", "pc-a /update", "pc-a
// /power"): the request goes out, the machine acts on it, the panel gets a timeout.
func lostFarm(t *testing.T, lost string) (*env, *farm) {
	t.Helper()
	e := setup(t)
	f := newFarm(t, e)
	f.offline()
	f.optIn("pc-a", "pc-b")
	e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
		if agent+" "+path == lost {
			return 0, nil, context.DeadlineExceeded
		}
		if path == "/power" {
			return 200, jsonx.M{"action": "shutdown", "pending": jsonx.M{"action": "shutdown", "at": float64(f.now.Add(120 * time.Second).Unix()), "delay_s": 120.0, "requested_by": "x"}}, nil
		}
		return 0, nil, nil
	}
	return e, f
}

// r2-p2-03 with the real poller and powerctl: the update request's answer is lost and the agent installs for
// five minutes. The run asked for the shutdown one minute into the install and said "NOT updated".
func TestWakeUpdateALostUpdateAnswerIsNotAFailedUpdate(t *testing.T) {
	e, f := lostFarm(t, "pc-a /update")
	if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"}); err != nil {
		t.Fatal(err)
	}
	slow, midInstall := false, false
	for i := 0; i < 3600; i++ {
		a, _ := e.s.WakeRuns.Snapshot()
		if a == nil {
			break
		}
		f.e.b.Mu.Lock()
		if at, asked := f.upd["pc-a"]; asked && !slow {
			slow, f.upd["pc-a"] = true, at.Add(5*time.Minute) // a slow download
		}
		installing := slow && f.now.Before(f.upd["pc-a"])
		f.e.b.Mu.Unlock()
		for _, r := range f.requests() {
			midInstall = midInstall || (r == "pc-a /power" && installing)
		}
		f.tick()
	}
	_, hist := e.s.WakeRuns.Snapshot()
	h := hostOf(hist[0], "pc-a")
	if midInstall {
		t.Errorf("the shutdown was asked for while the agent reported an update in progress: %s", h.Text)
	}
	if h.State != wakerun.StOff || h.Update != wakerun.UpdOK || !strings.Contains(h.Text, "updated from 16-aaaaaaa to 17-bbbbbbb") || f.on["pc-a"] {
		t.Errorf("pc-a ended as %s (update %s, on %v): %s", h.State, h.Update, f.on["pc-a"], h.Text)
	}
	if got := strings.Join(f.requests(), ", "); got != "nas /wol, pc-a /update, pc-a /power" {
		t.Errorf("requests: %s", got)
	}
}
