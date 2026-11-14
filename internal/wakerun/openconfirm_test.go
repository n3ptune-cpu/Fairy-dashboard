package wakerun

import (
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
)

// "Nothing was confirmed" was said too early (second look at 4f94383, N1). The run's one confirmation got no
// answer from its only machine. No machine "waited" any more, so the run ended its question at once as "no
// machine waits for it any more", which the panel turns into the notification "nothing was confirmed"; seconds
// later the machine's agent was read, it showed the run as confirmed, and the run was "confirmed by Ann". What
// was said first was not known, and was not so.
//
// While a confirmation that left for a machine is open (it got no answer, or the machine answered that the run
// is confirmed already, and its agent has not been read since), the question is not ended as "nobody waits".
// It ends when the machine's word is read, or when its time is over, and then the end names what is open.

// openAtEnd are the machines the question's end named as ones with an open confirmation.
func openAtEnd(w *world) (names []string, ended bool) {
	for _, e := range w.events {
		if e.Kind == EvApprovalEnd && e.Run.Approval == "over" {
			names, _ = e.Params["open"].([]string)
			ended = true
		}
	}
	return names, ended
}

func TestTheQuestionIsNotEndedWhileAConfirmationThatLeftIsOpen(t *testing.T) {
	const noAnswer = "Its confirmation got no answer (agent unreachable: timeout), and its agent does not show the run as confirmed so far: not updated. It is armed: unless the confirmation did arrive, it switches itself off at "
	// the link that lost the answer stays down for down; what the row says meanwhile
	outage := func(t *testing.T, w *world, pc *machine, act *Run, down int, row string) {
		t.Helper()
		pc.unreachable = true
		for i := 0; i < down; i++ {
			w.tick()
			cur, _ := w.m.Snapshot()
			if cur == nil || cur.Approval != "needed" || cur.ApprovalEnd != "" {
				t.Fatalf("%d s after the confirmation, its agent not read: the question is %q (%q)", i+1, cur.Approval, cur.ApprovalEnd)
			}
			if h := host(cur, "pc-a"); !strings.HasPrefix(h.Text, row) || strings.Contains(h.Text, "unless you confirm") {
				t.Fatalf("the row meanwhile: %q", h.Text)
			}
		}
		if ws, err := w.m.Waiting(act.ID); err == nil { // nothing is sent twice: the strip does not ask for it
			t.Fatalf("offered for another confirmation: %+v", ws)
		}
		if _, ended := openAtEnd(w); ended {
			t.Fatal("the question was ended")
		}
		pc.unreachable = false
	}
	t.Run("it did arrive", func(t *testing.T) {
		w, pc, act := askedRun(t, func(_ *world, pc *machine) { pc.chk.goLost = "arrives" })
		if _, err := w.confirm("Ann"); err != nil {
			t.Fatal(err)
		}
		outage(t, w, pc, act, 8, noAnswer)
		w.advance(10 * time.Second)
		done := w.finish()
		has(t, host(done, "pc-a"), StOff, "updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.")
		if got := strings.Join(question(w), ", "); got != "asked, given by Ann" {
			t.Fatalf("the question: %s", got)
		}
	})
	t.Run("it did not arrive", func(t *testing.T) {
		w, pc, act := askedRun(t, func(_ *world, pc *machine) { pc.chk.goLost = "never" })
		if _, err := w.confirm("Ann"); err != nil {
			t.Fatal(err)
		}
		outage(t, w, pc, act, 8, noAnswer)
		w.advance(10 * time.Second) // its agent is read: it shows the run as armed, not as confirmed
		cur, _ := w.m.Snapshot()
		if cur.Approval != "over" || cur.ApprovalEnd != EndNobodyWaits {
			t.Fatalf("after its agent was read: the question is %q (%q)", cur.Approval, cur.ApprovalEnd)
		}
		if h := host(cur, "pc-a"); !logHas(h, "its agent was read after the confirmation and does not show the run as confirmed") {
			t.Fatalf("the machine's history:%s", logOf(h))
		}
		if open, _ := openAtEnd(w); len(open) != 0 {
			t.Fatalf("the end names an open confirmation after the machine's word was read: %v", open)
		}
		if got := strings.Join(question(w), ", "); got != "asked, over "+EndNobodyWaits {
			t.Fatalf("the question: %s", got)
		}
	})
	t.Run("the machine answered that the run is confirmed already", func(t *testing.T) {
		w, pc, act := askedRun(t, func(_ *world, pc *machine) { pc.chk.goSays = api.RunReasonConfirmed })
		if _, err := w.confirm("Ann"); err != nil {
			t.Fatal(err)
		}
		outage(t, w, pc, act, 8, "Its confirmation was not accepted (run refused (already_confirmed)), and its agent does not show the run as confirmed so far: not updated. It is armed: unless a confirmation did arrive, it switches itself off at ")
		w.advance(10 * time.Second)
		if got := strings.Join(question(w), ", "); got != "asked, over "+EndNobodyWaits {
			t.Fatalf("the question: %s", got)
		}
		done := w.finish()
		has(t, host(done, "pc-a"), StOff, "Its confirmation was not accepted (run refused (already_confirmed)), and its agent did not show the run as confirmed: not confirmed, not updated. It switched itself off again.")
	})
	t.Run("its agent answers, and says nothing about runs", func(t *testing.T) {
		w, pc, _ := askedRun(t, func(_ *world, pc *machine) { pc.chk.goLost = "never" })
		if _, err := w.confirm("Ann"); err != nil {
			t.Fatal(err)
		}
		pc.chk.noRuns = true // its helper does not answer its agent: the agent's answers name no run, and do not say that there is none
		for i := 0; i < 8; i++ {
			w.tick()
			if cur, _ := w.m.Snapshot(); cur == nil || cur.Approval != "needed" {
				t.Fatalf("%d s after the confirmation: the question is %q (%q), and its agent has said nothing about the run", i+1, cur.Approval, cur.ApprovalEnd)
			}
		}
		pc.chk.noRuns = false
		w.advance(10 * time.Second)
		if got := strings.Join(question(w), ", "); got != "asked, over "+EndNobodyWaits {
			t.Fatalf("the question: %s", got)
		}
	})
	t.Run("its agent is not read before the question's time is over", func(t *testing.T) {
		w, pc, act := askedRun(t, func(_ *world, pc *machine) { pc.chk.goLost = "never" })
		if _, err := w.confirm("Ann"); err != nil {
			t.Fatal(err)
		}
		pc.unreachable = true
		by := *act.ConfirmBy
		cur := w.waitFor(15*time.Minute, "the question over", func(r *Run) bool { return r.Approval == "over" })
		if cur.ApprovalEnd != EndTime || w.now.Before(by) || w.now.After(by.Add(5*time.Second)) {
			t.Fatalf("the question ended as %q at %s; it asked until %s", cur.ApprovalEnd, w.now.Format("15:04:05"), by.Format("15:04:05"))
		}
		if open, _ := openAtEnd(w); len(open) != 1 || open[0] != host(cur, "pc-a").Name {
			t.Fatalf("the end of the question names as open: %v", open)
		}
	})
}

// A machine that was never handed a ticket (its arming was refused) and whose confirmation got no answer is
// not ended as "nothing of this run switches it off" before its agent was read: the confirmation did arrive,
// and it is updated and switched off as confirmed.
func TestAnUnarmedMachineWhoseConfirmationGotNoAnswerIsReadFirst(t *testing.T) {
	w := newWorld(t)
	pc := w.checkedOff("pc-a")
	pc.chk.armSays, pc.chk.goLost = api.RunReasonLimits, "arrives"
	w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
	w.waitFor(10*time.Minute, "the run asks", func(r *Run) bool { return r.Approval == "needed" })
	w.advance(30 * time.Second) // its agent showed that no run is on it
	if _, err := w.confirm("Ann"); err != nil {
		t.Fatal(err)
	}
	pc.unreachable = true
	for i := 0; i < 5; i++ {
		w.tick()
		cur, _ := w.m.Snapshot()
		if h := host(cur, "pc-a"); cur.Approval != "needed" || h.State != StApproval ||
			h.Text != "awake; it could not be armed (run refused (limits)): it stays ON unless its confirmation, which got no answer (agent unreachable: timeout), did arrive: what its agent shows decides" {
			t.Fatalf("%d s after the confirmation: the question is %q; pc-a %s %q", i+1, cur.Approval, h.State, h.Text)
		}
	}
	pc.unreachable = false
	done := w.finish()
	has(t, host(done, "pc-a"), StOff, "updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.")
	if got := strings.Join(question(w), ", "); got != "asked, given by Ann" {
		t.Fatalf("the question: %s", got)
	}
}

// The same for a machine without a ticket that answered "this run is confirmed already": its row does not go
// on saying "unless you confirm", and when its agent shows no run the question ends and it stays on.
func TestAnUnarmedMachineThatAnsweredConfirmedAlreadyIsReadFirst(t *testing.T) {
	w := newWorld(t)
	pc := w.checkedOff("pc-a")
	pc.chk.armSays, pc.chk.goSays = api.RunReasonLimits, api.RunReasonConfirmed
	w.start(StartRequest{Hosts: []string{"pc-a"}, Latest: "17-bbbbbbb"})
	w.waitFor(10*time.Minute, "the run asks", func(r *Run) bool { return r.Approval == "needed" })
	w.advance(30 * time.Second)
	if _, err := w.confirm("Ann"); err != nil {
		t.Fatal(err)
	}
	pc.unreachable = true
	w.tick()
	cur, _ := w.m.Snapshot()
	if h := host(cur, "pc-a"); cur.Approval != "needed" ||
		h.Text != "awake; it could not be armed (run refused (limits)): it stays ON unless a confirmation did arrive (its confirmation was not accepted: run refused (already_confirmed)): what its agent shows decides" {
		t.Fatalf("the question is %q; pc-a %s %q", cur.Approval, h.State, h.Text)
	}
	pc.unreachable = false
	done := w.finish()
	has(t, host(done, "pc-a"), StLeftOn, "Its confirmation was not accepted (run refused (already_confirmed)), and its agent did not show the run as confirmed: not confirmed, not updated.", "It is ON: nothing of this run switches it off.")
	if got := strings.Join(question(w), ", "); got != "asked, over "+EndNobodyWaits {
		t.Fatalf("the question: %s", got)
	}
}

// What "left for the machine" keeps: the moment before the FIRST confirmation left (an answer of its agent
// counts as "confirmed" only from after that), who gave the LATEST, and when the panel learned what came of the
// latest (only an answer from after that settles whether it is still open).
func TestWhatIsKeptOfTheConfirmationsThatLeftForAMachine(t *testing.T) {
	var c checked
	c.left("Bob", 100*time.Second, 103*time.Second)
	c.goRead = true
	c.left("Ann", 200*time.Second, 207*time.Second)
	if !c.goSent || c.goSince != 100*time.Second || c.goBy != "Ann" || c.goEnd != 207*time.Second || c.goRead {
		t.Fatalf("%+v", c)
	}
	h := &Host{ID: "pc-a", Checked: true}
	h.chk = c
	o := Observation{Online: true, SeenAgo: 4 * time.Second}
	if h.saidAfterConfirm(o, BlockArmed, 211*time.Second) {
		t.Fatal("an answer made when the panel learned of the confirmation counts as one from after it")
	}
	if !h.saidAfterConfirm(o, BlockArmed, 212*time.Second) || h.saidAfterConfirm(o, "", 212*time.Second) {
		t.Fatal("an answer from after it that names the run counts, one that says nothing about runs does not")
	}
}
