package web

import (
	"context"
	"net"
	"os"
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
