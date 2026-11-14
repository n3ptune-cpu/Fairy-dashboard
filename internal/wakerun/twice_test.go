package wakerun

import (
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
)

// A confirmation can reach a machine twice, as a ticket can (a proxy that repeats the POST). The machine takes
// the first copy: it is confirmed, and no longer switches itself off. The panel holds its honest answer to the
// second copy, 409 already_confirmed (second look at 4f94383, N2). The machine's own word decides here as it
// does for the ticket: its agent shows THIS run as confirmed, a confirmation of this run did leave the panel
// for this machine, so it is confirmed, whatever the panel was answered.

// The run's question, as the events told the web package: "asked", then each end ("given by Ann", "over time").
func question(w *world) (said []string) {
	for _, e := range w.events {
		switch e.Kind {
		case EvApproval:
			said = append(said, "asked")
		case EvApprovalEnd:
			if e.Run.Approval == "given" {
				said = append(said, "given by "+e.Run.ConfirmedBy)
			} else {
				said = append(said, e.Run.Approval+" "+e.Run.ApprovalEnd)
			}
		}
	}
	return said
}

func askedRun(t *testing.T, set func(w *world, pc *machine)) (*world, *machine, *Run) {
	t.Helper()
	w, pc, _ := armedRun(t, set)
	act := w.waitFor(10*time.Minute, "the run asks", func(r *Run) bool { return r.Approval == "needed" })
	return w, pc, act
}

func TestAConfirmationThatArrivesTwiceIsSettledByTheMachinesOwnWord(t *testing.T) {
	w, pc, act := askedRun(t, func(_ *world, pc *machine) { pc.chk.goTwice = true })
	if _, err := w.confirm("Ann"); err != nil {
		t.Fatal(err)
	}
	if c := w.bootOf(pc); c.state != BlockConfirmed || c.run != act.ID {
		t.Fatalf("the model: the machine is %q for %q", c.state, c.run)
	}
	cur, _ := w.m.Snapshot()
	if h := host(cur, "pc-a"); h.Confirm != ConfirmRefused || h.ConfirmText != "run refused (already_confirmed)" || cur.Approval != "needed" {
		t.Fatalf("what the panel was answered: confirm %q (%q), approval %q", h.Confirm, h.ConfirmText, cur.Approval)
	}
	if ws, err := w.m.Waiting(act.ID); err == nil {
		t.Fatalf("a machine that answered \"confirmed already\" is offered for another confirmation: %+v", ws)
	}
	w.advance(10 * time.Second) // its agent is read
	cur, _ = w.m.Snapshot()
	h := host(cur, "pc-a")
	if cur.Approval != "given" || cur.ConfirmedBy != "Ann" || cur.Tried != "" {
		t.Fatalf("the run after its agent showed the run as confirmed: approval %q by %q, tried %q", cur.Approval, cur.ConfirmedBy, cur.Tried)
	}
	if h.Confirm != ConfirmOK || h.State == StApproval || h.State == StSelfOff || strings.Contains(h.Text, "switches itself off") || strings.Contains(h.Text, "unless you confirm") {
		t.Fatalf("pc-a: confirm %q, %s %q", h.Confirm, h.State, h.Text)
	}
	if !logHas(h, "its agent shows the run as confirmed, although the panel was answered that its confirmation was not accepted (run refused (already_confirmed)): a confirmation did arrive") {
		t.Fatalf("the machine's history does not say how it came to be confirmed:%s", logOf(h))
	}
	if a := named(w.m.MayGoOff(w.now), "pc-a"); a != nil {
		t.Fatalf("a confirmed machine is named as one that may switch itself off: %+v", a)
	}
	done := w.finish()
	has(t, host(done, "pc-a"), StOff, "updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.")
	if got := strings.Join(question(w), ", "); got != "asked, given by Ann" {
		t.Fatalf("the question: %s", got)
	}
	if pc.chk.gos != 1 || pc.on {
		t.Fatalf("the model: %d confirmations were sent, the machine is on: %v", pc.chk.gos, pc.on)
	}
}

// The reasons after which a machine is not offered for another confirmation are the machines' own words
// (Fairy-agent api.RunReason...): the list had "confirmed" for api.RunReasonConfirmed, "already_confirmed",
// so a machine that answered "this run is confirmed already" was offered again, with "unless you confirm" in
// its row. A reason another person's passkey can change (the machine does not take this one's) is offered.
func TestAMachineIsOfferedAgainOnlyAfterARefusalAnotherPasskeyCanChange(t *testing.T) {
	for _, c := range []struct {
		reason string
		again  bool
	}{
		{api.RunReasonConfirmed, false}, {api.RunReasonTooLate, false}, {api.RunReasonOver, false}, {api.RunReasonOtherRun, false},
		{"user", true}, {api.RunReasonLimits, true},
	} {
		t.Run(c.reason, func(t *testing.T) {
			w := newWorld(t)
			a, b := w.checkedOff("pc-a"), w.checkedOff("pc-b")
			a.chk.goSays, b.chk.goSays = c.reason, "user"
			w.start(StartRequest{Hosts: []string{"pc-a", "pc-b"}, Latest: "17-bbbbbbb"})
			act := w.waitFor(10*time.Minute, "the run asks", func(r *Run) bool { return r.Approval == "needed" })
			if begun, err := w.confirm("Bob"); err != nil || len(begun) != 2 {
				t.Fatalf("Bob's confirmation: %v %v", begun, err)
			}
			ws, err := w.m.Waiting(act.ID)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, x := range ws {
				ids = append(ids, x.ID)
			}
			want := "pc-b"
			if c.again {
				want = "pc-a pc-b"
			}
			if got := strings.Join(ids, " "); got != want {
				t.Fatalf("offered for another confirmation: %q, want %q", got, want)
			}
			cur, _ := w.m.Snapshot()
			if h := host(cur, "pc-a"); c.again != (h.Confirm == "") || c.again != logHas(h, "it can be confirmed again while the run asks") {
				t.Fatalf("pc-a: confirm %q%s", h.Confirm, logOf(h))
			}
		})
	}
}

// The binding stays strict. "Confirmed" is taken from a machine's agent only for THIS run, on this machine,
// and only after a confirmation of this run really left the panel for it.
func TestTheWordConfirmedCountsOnlyAfterAConfirmationThatLeftForTheMachine(t *testing.T) {
	notConfirmed := func(t *testing.T, w *world) {
		t.Helper()
		w.advance(30 * time.Second)
		cur, _ := w.m.Snapshot()
		if h := host(cur, "pc-a"); h.Confirm == ConfirmOK || cur.Approval == "given" || logHas(h, "its agent shows the run as confirmed") {
			t.Fatalf("taken for confirmed: confirm %q, approval %q%s", h.Confirm, cur.Approval, logOf(h))
		}
		done := w.finish()
		none(t, w.callsFor("pc-a"), "update", "shutdown")
		if h := host(done, "pc-a"); h.Confirm == ConfirmOK || h.VersionAfter != "" || done.Approval == "given" {
			t.Fatalf("the report: confirm %q, approval %q: %q", h.Confirm, done.Approval, h.Text)
		}
	}
	t.Run("the panel could not send it", func(t *testing.T) {
		w, pc, act := askedRun(t, nil)
		pc.unreachable = true // the connection fails: nothing leaves the panel
		if _, err := w.confirm("Ann"); err != nil {
			t.Fatal(err)
		}
		pc.unreachable = false
		c := w.bootOf(pc)
		c.run, c.state, c.withinEnds = act.ID, BlockConfirmed, w.mono+20*time.Minute // and yet its agent shows this run as confirmed
		notConfirmed(t, w)
	})
	t.Run("another run is confirmed on it", func(t *testing.T) {
		w, pc, _ := askedRun(t, func(_ *world, pc *machine) { pc.chk.goSays = "user" })
		if _, err := w.confirm("Bob"); err != nil {
			t.Fatal(err)
		}
		c := w.bootOf(pc)
		c.run, c.state, c.withinEnds = "wu-another", BlockConfirmed, w.mono+20*time.Minute
		notConfirmed(t, w)
	})
	t.Run("an answer of its agent from before the confirmation left", func(t *testing.T) {
		r := &Run{ID: "wu-1"}
		h := &Host{ID: "pc-a", Checked: true, entry: 3}
		h.chk.goSent, h.chk.goSince = true, 100*time.Second
		o := Observation{Entry: 3, Online: true, SeenAgo: 4 * time.Second, Run: &RunBlock{ID: "wu-1", State: BlockConfirmed}}
		word := func(mono time.Duration) string { return h.runWord(r, o, mono, 0) }
		if mono := 104 * time.Second; h.showsConfirmed(o, word(mono), mono) {
			t.Fatal("an answer made at the moment the confirmation left was taken")
		}
		if mono := 105 * time.Second; !h.showsConfirmed(o, word(mono), mono) {
			t.Fatal("an answer from after the confirmation left was not taken")
		}
		h.chk.goSent = false
		if mono := 105 * time.Second; h.showsConfirmed(o, word(mono), mono) {
			t.Fatal("taken without a confirmation that left for it")
		}
	})
}

// A confirmation by a person the machine does not take (refused, and so offered again) that arrived twice all
// the same: the machine's word settles it, and the run is confirmed by the one whose confirmation it was.
func TestTheMachinesWordNamesTheOneWhoseConfirmationLeftForIt(t *testing.T) {
	w := newWorld(t)
	a, b := w.checkedOff("pc-a"), w.checkedOff("pc-b")
	a.chk.goLost, b.chk.goSays = "arrives", "user"
	w.start(StartRequest{Hosts: []string{"pc-a", "pc-b"}, Latest: "17-bbbbbbb"})
	act := w.waitFor(10*time.Minute, "the run asks", func(r *Run) bool { return r.Approval == "needed" })
	if _, err := w.confirm("Bob"); err != nil {
		t.Fatal(err)
	}
	a.unreachable = true // the link that lost the answer stays down for now
	// Ann's confirmation is on its way to pc-b when pc-a's agent is read
	if begun, err := w.m.BeginConfirm(act.ID, "Ann", []string{"pc-a", "pc-b"}); err != nil || strings.Join(begun, " ") != "pc-b" {
		t.Fatalf("Ann's confirmation: %v %v", begun, err)
	}
	a.unreachable = false
	w.advance(10 * time.Second)
	cur, _ := w.m.Snapshot()
	if cur.Approval != "given" || cur.ConfirmedBy != "Bob" {
		t.Fatalf("approval %q by %q, want given by Bob: it was Bob's confirmation that reached pc-a", cur.Approval, cur.ConfirmedBy)
	}
}
