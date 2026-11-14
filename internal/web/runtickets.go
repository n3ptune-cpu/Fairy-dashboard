package web

import (
	"log/slog"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
)

// The tickets of a run (README, "Machines with the check in a run").
//
// The passkey that starts an "Update unpowered hosts" run with machines that check approvals themselves gives
// one ID token. For a machine that is off it is a ticket: the machine takes it once it has started (POST
// /run/arm). For a machine that is on it answers that machine's own challenge for an update. Either way the
// token must still be here when the run gets to the machine, which is minutes after the round trip to Pocket
// ID, so it cannot die with the replayed request as every other approval does. This file is the one place
// where it is kept meanwhile, and these are its rules:
//
//   - in memory only: in this struct and nowhere else. Not in the run journal, not in state.json, not in a
//     log line, an audit entry, a notification or an answer to a browser. A restart of the panel forgets it;
//     the run is then reported as interrupted and the machines say for themselves what they do.
//   - once per machine: take hands the approval for a machine out one time, whatever comes of the request it
//     is sent with. A second take gets nothing, so nothing is ever sent again.
//   - not after its time: the ticket is dropped when it is as old as its valid_s by the token's own "issued
//     at" (the machines refuse it from then on), counted on the panel's steady clock from the moment it came
//     here; when every machine has taken its part; and when the run ends.
//
// What a ticket lets its holder do is little, and it is the machine that says so, not this file: arm a
// self-switch-off on a machine with the right machine id that started after the ticket was signed.
type runTickets struct {
	mu  sync.Mutex
	cur *heldTicket
	// now is the steady clock (nil: time.Now, whose values carry Go's monotonic reading: setting the system's
	// clock does not move a difference of two of them). after is time.AfterFunc. Both are the tests' to set.
	now   func() time.Time
	after func(d time.Duration, f func()) (stop func())
}

// heldTicket is the token of one run's start with what it may still be used for.
type heldTicket struct {
	run   string
	token []byte // the raw ID token; wiped (zeroed) when the ticket is dropped
	plan  *approval.Plan
	// arm: the agents whose ticket has not been taken (the request each is for). update: the agents whose
	// approved update has not been taken.
	arm    map[string]api.ApprovalRequest
	update map[string]api.ApprovalRequest
	until  time.Time // steady: from here on nothing is handed out
	stop   func()
}

func (k *runTickets) clock() time.Time {
	if k.now != nil {
		return k.now()
	}
	return time.Now()
}

// put keeps the token of a run's start for validFor (what is left of the ticket's valid_s), replacing whatever
// was kept (one run at a time). It keeps nothing when no time is left or nothing is to be handed out.
func (k *runTickets) put(run, token string, plan *approval.Plan, arm, update map[string]api.ApprovalRequest, validFor time.Duration) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.wipeLocked()
	if validFor <= 0 || token == "" || plan == nil || len(arm)+len(update) == 0 {
		return false
	}
	h := &heldTicket{run: run, token: []byte(token), plan: plan, arm: arm, update: update, until: k.clock().Add(validFor)}
	after := k.after
	if after == nil {
		after = func(d time.Duration, f func()) func() { t := time.AfterFunc(d, f); return func() { t.Stop() } }
	}
	h.stop = after(validFor, func() { k.expire(h) })
	k.cur = h
	return true
}

// expire drops h when its time is over (the timer of put).
func (k *runTickets) expire(h *heldTicket) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cur == h {
		slog.Info("wake-update: the ticket of the run's start is as old as it may get and was dropped", "run", h.run, "not_taken", len(h.arm)+len(h.update))
		k.wipeLocked()
	}
}

// drop forgets the ticket of run (the run ended, or was never started). "" drops whatever is kept.
func (k *runTickets) drop(run string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cur != nil && (run == "" || k.cur.run == run) {
		k.wipeLocked()
	}
}

func (k *runTickets) wipeLocked() {
	h := k.cur
	if h == nil {
		return
	}
	for i := range h.token {
		h.token[i] = 0
	}
	if h.stop != nil {
		h.stop()
	}
	h.token, h.plan, h.arm, h.update = nil, nil, nil, nil
	k.cur = nil
}

// Why take hands nothing out.
const (
	ticketNone    = "this panel holds no ticket of the run's start for it (a ticket is handed over once, and kept in memory only: a restart of the panel forgets it)"
	ticketTooOld  = "the ticket of the run's start was older than it may be when the machine was up"
	ticketNotThis = "the machine id Fairy has on record for it is not the one the ticket was signed for"
)

// take hands out the approval for one machine of the run, once: kind "arm" (its ticket) or "update" (the
// update the start approved). machineID is the id on record for the agent NOW: an approval made for another
// id is not handed out. Whatever the answer, the machine's part is used up. why says why nothing came.
func (k *runTickets) take(run, kind, agentID, machineID string) (ap *api.Approval, why string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	h := k.cur
	if h == nil || h.run != run {
		return nil, ticketNone
	}
	set := h.arm
	if kind == "update" {
		set = h.update
	}
	req, ok := set[agentID]
	if !ok {
		return nil, ticketNone
	}
	delete(set, agentID) // used up, before anything else is decided
	defer func() {
		if k.cur == h && len(h.arm)+len(h.update) == 0 {
			k.wipeLocked() // every machine has had its part: nothing is kept a second longer
		}
	}()
	if !k.clock().Before(h.until) {
		k.wipeLocked()
		return nil, ticketTooOld
	}
	out, err := approval.For(h.plan, string(h.token), agentID, machineID, req)
	if err != nil {
		return nil, ticketNotThis
	}
	return out, ""
}

// held reports what is kept: the run and how many machines have not taken their part ("" and 0: nothing).
func (k *runTickets) held() (run string, open int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cur == nil {
		return "", 0
	}
	return k.cur.run, len(k.cur.arm) + len(k.cur.update)
}
