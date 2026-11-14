package approval

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
)

// The run ticket (Fairy-agent v0.9.0, pkg/agent/api, "Runs"): what the passkey that starts an "Update
// unpowered hosts" run signs for a machine that checks approvals itself and is off.
//
// It is the one entry of an approval that answers no challenge of its machine: the machine was off. Its
// challenge member is 32 random bytes the PANEL makes up, and what ties it to its machine is the machine id
// alone: the id the panel has on record for that host, which the machine writes into its own entry before it
// compares the nonce. So two things that the panel may leave to the machine for every other approval are the
// panel's own here, and this file is where they are kept:
//
//   - exactly one ticket per machine id in a list, and one per host. A machine that finds two run_arm entries
//     with its id refuses the list (ambiguous): the plan is not made.
//   - the numbers a machine takes (api.RunValid..., RunWait..., RunNotice...): a text outside them has no
//     canonical form, and the plan is not made.
//
// A ticket allows one thing: the machine switches itself off again wait_s after it took the ticket, with a
// notice of notice_s, unless a confirmation or a cancel comes first; and only a machine that started after
// the ticket was signed takes it.

// TicketValidS is how long after it was signed a ticket may be presented: what the panel signs, always
// (the machine takes 120 to 600).
const TicketValidS = 420

// Ticket is the numbers of a run's tickets: one set for every machine of the run.
type Ticket struct {
	Run     string // the run's id (16 lower-case hex characters)
	ValidS  int    // TicketValidS
	WaitS   int    // from the arming to the start of the self-off
	NoticeS int    // the countdown of the self-off
}

// Request is the run_arm request of the ticket.
func (t Ticket) Request() api.ApprovalRequest {
	run, valid, wait, notice := t.Run, t.ValidS, t.WaitS, t.NoticeS
	return api.ApprovalRequest{Verb: api.ApproveRunArm, Run: &run, ValidS: &valid, WaitS: &wait, NoticeS: &notice}
}

// TicketBound is the bound entry of a run ticket for one host: the request of t, the machine id the panel has
// on record for the host, and 32 random bytes of the panel's (read from rnd; nil: crypto/rand) in the place
// of a challenge. It refuses a ticket whose numbers no machine takes and a host without an id on record.
func TicketBound(t Ticket, agentID, name, machineID string, rnd io.Reader) (Bound, error) {
	req := t.Request()
	canon, err := req.Canonical()
	if err != nil {
		return Bound{}, fmt.Errorf("%s: no machine takes this ticket (%w)", name, err)
	}
	if err := api.CheckMachineID(machineID); err != nil {
		return Bound{}, fmt.Errorf("%s: Fairy has no machine id on record for it (%w)", name, err)
	}
	if rnd == nil {
		rnd = rand.Reader
	}
	b := make([]byte, 32)
	if _, err := io.ReadFull(rnd, b); err != nil {
		return Bound{}, fmt.Errorf("%s: no random bytes for its ticket (%w)", name, err)
	}
	return Bound{Target: Target{AgentID: agentID, Name: name, Request: req}, Canonical: canon,
		Challenge: base64.RawURLEncoding.EncodeToString(b), MachineID: machineID}, nil
}

// checkTickets is NewPlan's look at the run tickets of a list: at most one per machine id and one per host,
// all for one run, and no machine id of a ticket on a second entry of another host (a ticket answers no
// challenge, so the id is all that says which machine it is for).
func checkTickets(bound []Bound) error {
	tickets := 0
	for _, b := range bound {
		if b.Request.Verb == api.ApproveRunArm {
			tickets++
		}
	}
	if tickets == 0 {
		return nil // a list without a ticket: every entry answers a challenge of its own machine, as before
	}
	byID, byAgent, idOf := map[string]string{}, map[string]bool{}, map[string]string{}
	run := ""
	for _, b := range bound {
		if other, ok := idOf[b.MachineID]; ok && other != b.AgentID {
			return fmt.Errorf("%s: its machine id is also the id of another host of this list", b.Name)
		}
		idOf[b.MachineID] = b.AgentID
	}
	for _, b := range bound {
		if b.Request.Verb != api.ApproveRunArm {
			continue
		}
		if first, twice := byID[b.MachineID]; twice {
			return fmt.Errorf("%s and %s: two run tickets for one machine id", first, b.Name)
		}
		if byAgent[b.AgentID] {
			return fmt.Errorf("%s: two run tickets for one host", b.Name)
		}
		if b.Request.Run == nil || (run != "" && *b.Request.Run != run) {
			return fmt.Errorf("%s: the tickets of one list are for one run", b.Name)
		}
		byID[b.MachineID], byAgent[b.AgentID], run = b.Name, true, *b.Request.Run
	}
	return nil
}

// Tickets reports how many run tickets the plan holds.
func (p *Plan) Tickets() int {
	n := 0
	for _, b := range p.bound {
		if b.Request.Verb == api.ApproveRunArm {
			n++
		}
	}
	return n
}
