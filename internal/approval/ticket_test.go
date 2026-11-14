package approval

import (
	"bytes"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
)

const (
	tidA = "machine-id-of-pc-a-0000001"
	tidB = "machine-id-of-pc-b-0000002"
	run  = "0123456789abcdef"
)

func ticket() Ticket { return Ticket{Run: run, ValidS: TicketValidS, WaitS: 960, NoticeS: 120} }

func mustTicket(t *testing.T, agent, name, mid string) Bound {
	t.Helper()
	b, err := TicketBound(ticket(), agent, name, mid, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The text a ticket signs is the agent module's canonical text, with the panel's numbers: valid_s is 420.
func TestTicketText(t *testing.T) {
	b := mustTicket(t, "pc-a", "PC-A", tidA)
	if b.Canonical != "run_arm run="+run+" valid_s=420 wait_s=960 notice_s=120" {
		t.Fatalf("%q", b.Canonical)
	}
	if got, err := api.ParseRunRequest(b.Canonical); err != nil || got.Verb != api.ApproveRunArm || *got.ValidS != TicketValidS {
		t.Fatalf("the agent's own reader: %+v %v", got, err)
	}
	if raw, err := api.DecodeChallenge(b.Challenge); err != nil || len(raw) != 32 {
		t.Fatalf("the challenge member is 32 bytes of the panel's: %q %v", b.Challenge, err)
	}
	if other := mustTicket(t, "pc-a", "PC-A", tidA); other.Challenge == b.Challenge {
		t.Fatal("two tickets with the same random bytes")
	}
	p, err := NewPlan([]Bound{b})
	if err != nil || p.Tickets() != 1 || !p.Covers("pc-a", tidA, ticket().Request()) {
		t.Fatalf("a plan of one ticket: %v", err)
	}
	if p.Covers("pc-a", tidB, ticket().Request()) || p.Covers("pc-b", tidA, ticket().Request()) {
		t.Fatal("a ticket is for the host and the machine id it was made for, and no other")
	}
	other := ticket()
	other.WaitS = 961
	if p.Covers("pc-a", tidA, other.Request()) {
		t.Fatal("another number is another ticket")
	}
}

// Numbers no machine takes, and a host without an id on record, make no ticket.
func TestTicketBoundRefuses(t *testing.T) {
	for name, c := range map[string]struct {
		t   Ticket
		mid string
	}{
		"valid_s below the machine's floor":   {Ticket{run, 119, 960, 120}, tidA},
		"valid_s above the machine's ceiling": {Ticket{run, 601, 960, 120}, tidA},
		"wait_s above the ceiling":            {Ticket{run, 420, 3601, 120}, tidA},
		"wait_s below the floor":              {Ticket{run, 420, 119, 120}, tidA},
		"notice_s above the ceiling":          {Ticket{run, 420, 960, 601}, tidA},
		"notice_s below the floor":            {Ticket{run, 420, 960, 29}, tidA},
		"a run id that is none":               {Ticket{"0123456789ABCDEF", 420, 960, 120}, tidA},
		"no machine id on record":             {ticket(), ""},
		"a machine id that is none":           {ticket(), "short"},
	} {
		if _, err := TicketBound(c.t, "pc-a", "PC-A", c.mid, nil); err == nil {
			t.Errorf("%s: a ticket was made", name)
		}
	}
	if _, err := TicketBound(ticket(), "pc-a", "PC-A", tidA, bytes.NewReader(make([]byte, 5))); err == nil {
		t.Error("a ticket was made without 32 random bytes")
	}
}

// Exactly one ticket per machine id and per host: a machine that finds two run_arm entries with its id cannot
// tell which was meant, and a ticket answers no challenge, so the id is its only tie to a machine.
func TestTicketPlanRefusesTwoEntriesForOneMachineID(t *testing.T) {
	a, b := mustTicket(t, "pc-a", "PC-A", tidA), mustTicket(t, "pc-b", "PC-B", tidB)
	if p, err := NewPlan([]Bound{a, b}); err != nil || p.Tickets() != 2 {
		t.Fatalf("two machines, two ids: %v", err)
	}
	for name, list := range map[string][]Bound{
		"one id on two hosts":      {a, mustTicket(t, "pc-b", "PC-B", tidA)},
		"two tickets for one host": {a, mustTicket(t, "pc-a", "PC-A", tidA)},
		"one host under two ids":   {a, mustTicket(t, "pc-a", "PC-A", tidB)},
		"tickets of two runs": {a, func() Bound {
			x, _ := TicketBound(Ticket{"fedcba9876543210", 420, 960, 120}, "pc-b", "PC-B", tidB, nil)
			return x
		}()},
		"a ticket's id on a challenge": {a, {Target: Target{AgentID: "pc-b", Name: "PC-B", Request: api.ApprovalRequest{Verb: api.ApproveApplyUpdate}}, Canonical: "apply_update", Challenge: b.Challenge, MachineID: tidA}},
	} {
		if _, err := NewPlan(list); err == nil {
			t.Errorf("%s: a plan was made", name)
		} else if strings.Contains(err.Error(), tidA) || strings.Contains(err.Error(), tidB) {
			t.Errorf("%s: the refusal names a machine id: %v", name, err)
		}
	}
	// a ticket next to the challenge of a machine that is on (another host, another id): one list, one passkey
	upd := Bound{Target: Target{AgentID: "pc-b", Name: "PC-B", Request: api.ApprovalRequest{Verb: api.ApproveApplyUpdate}}, Canonical: "apply_update", Challenge: b.Challenge, MachineID: tidB}
	p, err := NewPlan([]Bound{a, upd})
	if err != nil || p.Tickets() != 1 || len(p.Entries()) != 2 {
		t.Fatalf("a ticket and an update challenge: %v", err)
	}
}
