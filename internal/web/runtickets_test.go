package web

import (
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
)

// The keeper of a run's ticket by itself: what it hands out, how often, until when, and that nothing is left
// of the token afterwards. (That the token is in no file, log line or answer: wakeupdate_checked_test.go.)

const (
	ticketRun   = "00000000000000aa"
	ticketIDA   = "ticket-test-machine-id-a-01"
	ticketIDB   = "ticket-test-machine-id-b-01"
	ticketToken = "not-a-token" // obviously not one: the keeper never looks inside
)

// ticketPlan is a plan with a ticket for pc-a and one for pc-b.
func ticketPlan(t *testing.T) (*approval.Plan, map[string]api.ApprovalRequest) {
	t.Helper()
	tk := approval.Ticket{Run: ticketRun, ValidS: approval.TicketValidS, WaitS: 960, NoticeS: 120}
	a, err := approval.TicketBound(tk, "pc-a", "PC-A", ticketIDA, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := approval.TicketBound(tk, "pc-b", "PC-B", ticketIDB, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := approval.NewPlan([]approval.Bound{a, b})
	if err != nil {
		t.Fatal(err)
	}
	return plan, map[string]api.ApprovalRequest{"pc-a": tk.Request(), "pc-b": tk.Request()}
}

type ticketClock struct {
	at    time.Time
	fired []func()
	wait  []time.Duration
}

func newTicketKeeper() (*runTickets, *ticketClock) {
	c := &ticketClock{at: time.Unix(1_800_000_000, 0)}
	k := &runTickets{now: func() time.Time { return c.at }}
	k.after = func(d time.Duration, f func()) func() {
		c.wait, c.fired = append(c.wait, d), append(c.fired, f)
		return func() {}
	}
	return k, c
}

func TestTicketIsTakenOnce(t *testing.T) {
	k, _ := newTicketKeeper()
	plan, arm := ticketPlan(t)
	if !k.put(ticketRun, ticketToken, plan, arm, nil, 400*time.Second) {
		t.Fatal("not kept")
	}
	held := k.cur.token
	ap, why := k.take(ticketRun, "arm", "pc-a", ticketIDA)
	if ap == nil || ap.Token != ticketToken || len(ap.Entries) != 2 || why != "" {
		t.Fatalf("the first take: %+v %q", ap, why)
	}
	// a second take for the same machine gets nothing, whatever came of the first request
	if ap, why := k.take(ticketRun, "arm", "pc-a", ticketIDA); ap != nil || why != ticketNone {
		t.Fatalf("the second take: %+v %q", ap, why)
	}
	// another run, another kind, another machine: nothing
	for _, c := range [][3]string{{"00000000000000bb", "arm", "pc-b"}, {ticketRun, "update", "pc-b"}, {ticketRun, "arm", "pc-c"}} {
		if ap, _ := k.take(c[0], c[1], c[2], ticketIDB); ap != nil {
			t.Fatalf("take %v handed something out", c)
		}
	}
	if run, open := k.held(); run != ticketRun || open != 1 {
		t.Fatalf("pc-b's part is still there: %q %d", run, open)
	}
	// the last machine takes its part: nothing is kept after that, and the bytes of the token are gone
	if ap, _ := k.take(ticketRun, "arm", "pc-b", ticketIDB); ap == nil {
		t.Fatal("pc-b's take")
	}
	if run, open := k.held(); run != "" || open != 0 || k.cur != nil {
		t.Fatalf("kept after every machine had its part: %q %d", run, open)
	}
	for _, b := range held {
		if b != 0 {
			t.Fatal("the token's bytes were not wiped")
		}
	}
}

// An approval made for another machine id than the one on record now is not handed out, and the machine's
// part is used up all the same (nothing is tried again).
func TestTicketIsForTheMachineIDOnRecord(t *testing.T) {
	k, _ := newTicketKeeper()
	plan, arm := ticketPlan(t)
	k.put(ticketRun, ticketToken, plan, arm, nil, 400*time.Second)
	if ap, why := k.take(ticketRun, "arm", "pc-a", ticketIDB); ap != nil || why != ticketNotThis {
		t.Fatalf("with another id on record: %+v %q", ap, why)
	}
	if ap, why := k.take(ticketRun, "arm", "pc-a", ticketIDA); ap != nil || why != ticketNone {
		t.Fatalf("after that: %+v %q", ap, why)
	}
}

func TestTicketGoneAfterValidS(t *testing.T) {
	k, c := newTicketKeeper()
	plan, arm := ticketPlan(t)
	// nothing is kept without time left, or without anything to hand out
	if k.put(ticketRun, ticketToken, plan, arm, nil, 0) || k.put(ticketRun, ticketToken, plan, nil, nil, time.Minute) || k.put(ticketRun, "", plan, arm, nil, time.Minute) {
		t.Fatal("kept something that cannot be used")
	}
	plan, arm = ticketPlan(t)
	k.put(ticketRun, ticketToken, plan, arm, nil, 400*time.Second)
	if len(c.wait) != 1 || c.wait[0] != 400*time.Second {
		t.Fatalf("the timer: %v", c.wait)
	}
	held := k.cur.token
	// one second before its time it is handed out; at its time it is not, and it is dropped whole
	c.at = c.at.Add(399 * time.Second)
	if ap, _ := k.take(ticketRun, "arm", "pc-a", ticketIDA); ap == nil {
		t.Fatal("not handed out inside its time")
	}
	c.at = c.at.Add(time.Second)
	if ap, why := k.take(ticketRun, "arm", "pc-b", ticketIDB); ap != nil || why != ticketTooOld {
		t.Fatalf("at its time: %+v %q", ap, why)
	}
	if k.cur != nil || held[0] != 0 {
		t.Fatal("not dropped at its time")
	}
	// the timer drops it without a take; a timer of an older ticket does not drop a newer one
	plan, arm = ticketPlan(t)
	k.put(ticketRun, ticketToken, plan, arm, nil, 400*time.Second)
	c.fired[0]()
	if run, _ := k.held(); run != ticketRun {
		t.Fatal("the timer of an older ticket dropped this one")
	}
	c.fired[1]()
	if run, _ := k.held(); run != "" {
		t.Fatal("the timer did not drop it")
	}
	// the end of the run drops it; the end of another run does not
	plan, arm = ticketPlan(t)
	k.put(ticketRun, ticketToken, plan, arm, nil, 400*time.Second)
	k.drop("00000000000000bb")
	if run, _ := k.held(); run != ticketRun {
		t.Fatal("dropped for another run")
	}
	k.drop(ticketRun)
	if run, _ := k.held(); run != "" {
		t.Fatal("not dropped with its run")
	}
}

// A new run's ticket replaces what was kept: the bytes of the earlier token are wiped, and nothing of the
// earlier run can be taken any more.
func TestANewTicketReplacesAndWipesTheOneThatWasKept(t *testing.T) {
	k, _ := newTicketKeeper()
	plan, arm := ticketPlan(t)
	if !k.put(ticketRun, ticketToken, plan, arm, nil, 400*time.Second) {
		t.Fatal("not kept")
	}
	first := k.cur.token
	plan2, arm2 := ticketPlan(t)
	if !k.put("00000000000000bb", ticketToken, plan2, arm2, nil, 400*time.Second) {
		t.Fatal("the second one was not kept")
	}
	for _, b := range first {
		if b != 0 {
			t.Fatal("the bytes of the token that was replaced are still there")
		}
	}
	if ap, _ := k.take(ticketRun, "arm", "pc-a", ticketIDA); ap != nil {
		t.Fatal("the run that was replaced still hands out its ticket")
	}
	if run, open := k.held(); run != "00000000000000bb" || open != 2 {
		t.Fatalf("held: %q %d", run, open)
	}
	// a put that keeps nothing (no time left) still wipes what was there
	second := k.cur.token
	if k.put("00000000000000cc", ticketToken, plan, arm, nil, 0) {
		t.Fatal("kept with no time left")
	}
	for _, b := range second {
		if b != 0 {
			t.Fatal("a put that keeps nothing left the earlier token in memory")
		}
	}
	if run, _ := k.held(); run != "" {
		t.Fatalf("still held: %q", run)
	}
}
