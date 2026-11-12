package web

import (
	"context"
	"fmt"
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

// r2-p2-04 with the real poller and powerctl: the wake request reaches the relay, the packet goes out, the
// relay's answer does not reach the panel. The run said "The wake could not be sent ... It was not woken",
// power "untouched", and the machine stayed on unseen.
func TestWakeUpdateALostWakeAnswerIsNotNothingSent(t *testing.T) {
	e, f := lostFarm(t, "nas /wol")
	if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"}); err != nil {
		t.Fatal(err)
	}
	run := f.finish()
	h := hostOf(run, "pc-a")
	if h.State != wakerun.StOff || h.On != wakerun.OnNo || f.on["pc-a"] ||
		h.Text != "Came online after a wake request that got no answer, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again." {
		t.Errorf("pc-a ended as %s (%s, really on: %v): %s", h.State, h.On, f.on["pc-a"], h.Text)
	}
	if got := strings.Join(f.requests(), ", "); got != "nas /wol, pc-a /update, pc-a /power" {
		t.Errorf("requests: %s", got)
	}
	// the audit log has the wake with what came of it
	found := false
	for _, en := range e.auditEntries("", "power.wake") {
		found = found || en["outcome"] == "error: agent unreachable: timeout"
	}
	if !found {
		t.Errorf("the audit log lacks the wake without an answer: %v", e.auditEntries("", "power.wake"))
	}
}

// r2-p2-05 a) with the real poller and powerctl: the shutdown request reaches the agent and its answer does
// not reach the panel. The agent counts down and the machine goes off. The run said "Left ON: the shutdown
// was not accepted (agent unreachable: timeout)", power "on".
func TestWakeUpdateALostShutdownAnswerIsWatched(t *testing.T) {
	e, f := lostFarm(t, "pc-a /power")
	if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"}); err != nil {
		t.Fatal(err)
	}
	h := hostOf(f.finish(), "pc-a")
	if h.State != wakerun.StOff || h.On != wakerun.OnNo || f.on["pc-a"] || !h.ShutdownUnsure ||
		h.Text != "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again." {
		t.Errorf("pc-a ended as %s (%s, really on: %v): %s", h.State, h.On, f.on["pc-a"], h.Text)
	}
	if got := strings.Join(f.requests(), ", "); got != "nas /wol, pc-a /update, pc-a /power" { // asked once, not again
		t.Errorf("requests: %s", got)
	}
}

// r2-p2-05 b) with the real poller: after the countdown the machine's agent is gone and the machine refuses
// the connection. The poller's own sign for it is "the machine refused the connection", which everywhere
// else means "on"; the run said "switched off again", power "off".
func TestWakeUpdateAMachineThatRefusesAfterItsCountdownIsNotOff(t *testing.T) {
	e, f := lostFarm(t, "")
	if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1200 && hostOf(activeRun(e), "pc-a").State != wakerun.StShutting; i++ {
		f.tick()
	}
	e.b.Mu.Lock()
	e.b.Metrics["pc-a"] = func() (jsonx.M, error) {
		if _, counting := f.pending["pc-a"]; counting {
			return f.metrics("pc-a")
		}
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	}
	e.b.Mu.Unlock()
	h := hostOf(f.finish(), "pc-a")
	info, _, _ := e.s.Poller.AgentSeen("pc-a")
	want := "It still shows as on 5 min after its shutdown was due (the machine refused the connection), and its agent does not answer: the shutdown did not finish. It is ON. It was updated to 17-bbbbbbb."
	if !info.Refused || h.State != wakerun.StLeftOn || h.On != wakerun.OnYes || h.Text != want {
		t.Errorf("refused=%v; pc-a ended as %s (%s): %s", info.Refused, h.State, h.On, h.Text)
	}
}

func activeRun(e *env) *wakerun.Run {
	a, _ := e.s.WakeRuns.Snapshot()
	if a == nil {
		return &wakerun.Run{}
	}
	return a
}

// r2-p2-05 c) through the route: "Cancel run" with "leave on" in the 20 s between the end of the countdown
// and the run seeing the machine go. The machine is off; the report said "its shutdown countdown was called
// off. It came online after the wake and it is ON." Now the report says what the last look saw, at every second.
func TestWakeUpdateACancelAfterTheCountdownSaysWhatIsSeen(t *testing.T) {
	for _, k := range []int{-30, -1, 0, 1, 5, 10, 19} { // seconds after the countdown's end
		t.Run(fmt.Sprintf("%+d s", k), func(t *testing.T) {
			e, f := lostFarm(t, "")
			r, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"})
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 1200 && hostOf(activeRun(e), "pc-a").State != wakerun.StShutting; i++ {
				f.tick()
			}
			due := *hostOf(activeRun(e), "pc-a").ShutdownAt
			for f.now.Before(due.Add(time.Duration(k) * time.Second)) {
				f.tick()
			}
			if activeRun(e).ID == "" {
				t.Fatal("the run is over before the cancel")
			}
			c := e.do("DELETE", "/api/admin/wake-update/"+r.ID, nil, append([]string{"Cookie", e.cookie(controller(), 0)}, wuWrite...)...)
			if c.code != 200 {
				t.Fatalf("cancel: %d %s", c.code, c.raw)
			}
			h := hostOf(f.finish(), "pc-a")
			on := f.on["pc-a"]
			t.Logf("really on: %v; %s/%s: %s", on, h.State, h.On, h.Text)
			switch {
			case on && (h.State != wakerun.StLeftOn || h.On != wakerun.OnYes || !strings.Contains(h.Text, "its shutdown countdown was called off. It came online after the wake and it is ON.")):
				t.Errorf("the machine is on; the report: %s/%s: %s", h.State, h.On, h.Text)
			case !on && (h.State != wakerun.StOff || h.On != wakerun.OnNo || !strings.HasSuffix(h.Text, " It is off (not answering).") || strings.Contains(h.Text, "it is ON")):
				t.Errorf("the machine is off; the report: %s/%s: %s", h.State, h.On, h.Text)
			}
		})
	}
}

// The page: "LEFT ON" is for a machine the run saw as on. A machine the last look could not see is "not
// confirmed", next to "on or off: not known".
func TestUIWakeUpdateLeftOnIsOnlySaidOfAMachineSeenOn(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`const label = h.state === "left_on" && h.on === "unknown" ? "not confirmed" : said;`,
		`unknown: ["warn", "on or off: not known"]`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
}
