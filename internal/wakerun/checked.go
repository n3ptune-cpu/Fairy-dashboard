package wakerun

import (
	"context"
	"fmt"
	"time"
)

// A machine that checks passkey approvals itself in a run (Fairy-agent v0.9.0; README, "Machines with the
// check in a run"). Such a machine takes an update or a shutdown only with an approval it verifies itself,
// and it is off when the run starts, so the passkey of the start cannot answer a challenge of it. What a run
// does with it:
//
//	queued -> waking -> online -> ARM -> awaiting_approval -> updating -> settling -> shutting_down -> off
//	                                          |
//	                                          +-> switching_itself_off -> off     nobody confirmed in time
//
//   - ARM. The passkey of the start signed a ticket for it. When it is up, has answered for onlineSettle, and
//     the run's own proof says the run woke it (mayArm: the evidence of mayShutDown), the run hands it the
//     ticket: one request, never a second one, whatever comes of the first (Ops.Arm). The machine checks the
//     ticket itself. Armed, it switches itself off again when the ticket's wait is over, with a notice,
//     unless the run's second confirmation or a cancel comes first. That holds without the panel.
//   - awaiting_approval. When no machine with the check is still on its way up, the run asks ONCE, for all
//     of them (Run.Approval "needed": the banner, the bell, the notification). A person confirms with a
//     passkey; that answers a fresh challenge of every waiting machine, and each machine accepts it itself.
//     The web package asks the challenges and sends the confirmation (it holds the token); the run is told
//     with BeginConfirm (before anything goes out) and Confirmed (what each machine answered).
//   - A confirmed machine goes on as every machine does. Its update and its shutdown name the run (Grant):
//     the machine lets one of each through, the shutdown with exactly the countdown the confirmation named.
//   - Nobody confirmed: a machine that is armed switches itself off, and the run watches until it is gone
//     and says "off" only with the proof it asks for after its own shutdown (seenOff). A machine that is
//     not armed is ON, and the sentence says so.
//
// What this file never does: it sends no update and no shutdown to a machine with the check that has not
// accepted this run's confirmation, it sends no ticket twice, and it says nothing about a machine's power
// that it has not seen. The machine's own word about its run (Observation.Run) is what the run goes by
// wherever an answer of the panel's was lost.
//
// A machine with the check that was chosen as "on" (update only) is not woken and never armed: the passkey
// of the start answered its own challenge for the update, and the panel sends that approval with its one
// update request (Grant.Start). A machine of the wake list that turns out to be on has no approval at all:
// nothing is sent to it.

// What came of handing a machine its ticket (Host.Arm).
const (
	ArmArmed      = "armed"      // it took the ticket: it switches itself off at Host.SelfOffAt unless confirmed or cancelled
	ArmRefused    = "refused"    // it said no (Host.ArmText): it is not armed
	ArmUnanswered = "unanswered" // the request left and got no answer: its agent's own word decides
	ArmNotSent    = "not_sent"   // nothing was sent (Host.ArmText): it is not armed
)

// What came of the run's confirmation for a machine (Host.Confirm).
const (
	ConfirmOK         = "confirmed"
	ConfirmRefused    = "refused"
	ConfirmUnanswered = "unanswered"
	ConfirmNotAsked   = "not_asked" // the confirmation that was given did not cover it (Host.ConfirmText)
)

// Ticket is the numbers of a run's tickets and of its confirmation, in seconds. They are what the passkeys
// sign; none of it is a secret.
type Ticket struct {
	ValidS  int `json:"valid_s"`  // how long after the start's passkey a ticket may be presented
	WaitS   int `json:"wait_s"`   // from the arming to the begin of the self-switch-off
	NoticeS int `json:"notice_s"` // the countdown of the self-switch-off
	WithinS int `json:"within_s"` // how long after the confirmation its one update and one shutdown are let through
	// ConfirmWaitS: how long the run asks for the confirmation once the machines are up.
	ConfirmWaitS int `json:"confirm_wait_s"`
}

// Timings of this file that are not settings.
const (
	// confirmMargin: a machine is no longer offered for confirmation this long before it begins to switch
	// itself off: the round trip to Pocket ID and to the machine needs its time.
	confirmMargin = 20 * time.Second
	// confirmingMax: how long the run waits to be told what came of a confirmation that was begun.
	confirmingMax = 90 * time.Second
	// selfOffLate: a machine begins its self-switch-off at most this long after its time (the agent's own
	// rule); selfOffUpdate: and that much later only while an update is being installed on it.
	selfOffLate   = 60 * time.Second
	selfOffUpdate = 15 * time.Minute
	// lateArm: a ticket whose request got no answer can still arrive until its valid_s is over, plus the 60 s
	// the machine allows for the clocks.
	lateArm = 60 * time.Second
	// The longest countdown a confirmation can name (the agent's limit for shutdown_delay_s).
	maxRunDelay = 10 * time.Minute
)

// checked is the working state of a machine with the check (in memory; a run is never continued).
type checked struct {
	armTried  bool          // the one arm request went out (or it was decided that none does)
	armBefore time.Duration // steady: the moment before that request left
	// offMin, offMax: the earliest and the latest begin of its self-switch-off, steady (armed: the two ends of
	// the request plus the wait). lateUntil: for a request without an answer, until when it can still arrive.
	offMin, offMax time.Duration
	lateUntil      time.Duration
	armSeen        bool      // its agent showed this run as armed, or as counting down
	noRun          bool      // answers of its agent from after the arm request showed no run of this run (two in a row)
	other          *RunBlock // another run is live on it by its latest word (armed, counting down or confirmed)
	none           int       // answers in a row in which its checker takes runs and names none, or another
	noneAt         time.Time // SeenAt of the latest of them
	confirming     bool      // BeginConfirm named it, and Confirmed has not said what came of it
	confirmMono    time.Duration
	tooLate        bool   // its confirmation was refused as "too late": it is switching itself off
	toResult       string // (To is "-") the result of its update step, and the words for it
	toText         string
	updateWaited   bool // the watch of its self-switch-off has allowed for an update that was being installed
	disarm         *disarming
	disarmed       bool // the machine's own word, or the end of the time a ticket can arrive: it does not switch itself off
	helperNoted    bool
	cancelNoted    bool
	guestsAsked    bool
	guestsMono     time.Duration
	guestsUnseen   bool
}

// disarming is a cancel of a machine's self-switch-off that the run is at (disarm).
type disarming struct {
	why      string        // begins the sentence: who or what wanted the machine kept on
	lastMono time.Duration // steady: when the latest cancel left
	sent     bool
	tries    int           // cancels that did not get through
	ok       int           // cancels its agent answered
	okMono   time.Duration // steady: when the latest such answer was here
	text     string        // why the latest try failed
	lost     bool          // one of them got no answer at all: it may have arrived
}

func (h *Host) confirmed() bool { return h.Confirm == ConfirmOK }

// armedMaybe reports whether the machine may switch itself off by the run's ticket: it took the ticket, or
// the request got no answer and can have arrived, or its agent showed this run as armed whatever the panel
// was answered (the machine's own word decides: a ticket can arrive twice, and the panel then holds the
// refusal of the second copy), and neither a confirmation nor the machine's own word has ended that.
func (h *Host) armedMaybe() bool {
	if !h.Checked || h.confirmed() || h.chk.disarmed {
		return false
	}
	return h.Arm == ArmArmed || h.Arm == ArmUnanswered || h.chk.armSeen
}

// armedSure: it took the ticket, or its agent showed this run as armed.
func (h *Host) armedSure() bool { return h.armedMaybe() && (h.Arm == ArmArmed || h.chk.armSeen) }

// checkedStart is Start's look at a request with machines that check approvals themselves: the run's id, the
// ticket's numbers, and that every such machine of the wake list has a countdown a confirmation can name.
func checkedStart(req StartRequest, hosts []*Host) (id string, t *Ticket, err error) {
	id = req.ID
	if id == "" {
		id = newID()
	} else if !isRunID(id) {
		return "", nil, &StartError{Msg: "the run's id is not one"}
	}
	need := false
	for _, h := range hosts {
		if !h.Checked || h.Mode != ModeWake {
			continue
		}
		need = true
		if d := h.shutDelay; d < DelayFloor || d > maxRunDelay {
			return "", nil, &StartError{Msg: fmt.Sprintf("%s checks approvals itself, and such a machine takes a shutdown countdown of at most %s in a run; its countdown is %s",
				h.Name, span(maxRunDelay), span(d))}
		}
	}
	if !need {
		return id, nil, nil
	}
	if req.Ticket == nil {
		return "", nil, &StartError{Msg: "a machine that checks approvals itself is in the list, and the start names no ticket for it"}
	}
	tk := *req.Ticket
	for _, n := range []struct {
		name      string
		v, lo, hi int
	}{{"valid_s", tk.ValidS, 120, 600}, {"wait_s", tk.WaitS, 120, 3600}, {"notice_s", tk.NoticeS, 30, 600}, {"within_s", tk.WithinS, 300, 1800},
		{"confirm_wait_s", tk.ConfirmWaitS, 60, 3600}} {
		if n.v < n.lo || n.v > n.hi {
			return "", nil, &StartError{Msg: fmt.Sprintf("the ticket's %s must be %d-%d", n.name, n.lo, n.hi)}
		}
	}
	return id, &tk, nil
}

func isRunID(s string) bool {
	if len(s) != 16 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// runVersion reports whether s is a version a confirmation can name (the agent's rule for "to").
func runVersion(s string) bool {
	if len(s) == 0 || len(s) > 48 || s[0] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '+' || c == '-') {
			return false
		}
	}
	return true
}

func (t *Ticket) wait() time.Duration   { return time.Duration(t.WaitS) * time.Second }
func (t *Ticket) notice() time.Duration { return time.Duration(t.NoticeS) * time.Second }
func (t *Ticket) valid() time.Duration  { return time.Duration(t.ValidS) * time.Second }

// ticketSpan is what a run with tickets adds to maxRun: the longest a machine can wait and then switch
// itself off.
func ticketSpan(r *Run) time.Duration {
	if r.Ticket == nil {
		return 0
	}
	return r.Ticket.valid() + lateArm + r.Ticket.wait() + selfOffLate + selfOffUpdate + r.Ticket.notice() + offGrace
}

// The sentences of this file.
const (
	// notConfirmed is in the result of every machine with the check that the run's confirmation did not reach.
	notConfirmed = "Not confirmed, not updated."
	// noApprovalForIt: a machine with the check for which the run has no approval.
	noApprovalForIt = "it checks passkey approvals itself, and this run has no approval for it"
	// onNoApproval: a machine of the wake list that turned out to be on.
	onNoApproval = "it checks passkey approvals itself, and the run's confirmations are for machines the run wakes: update it on its own page"
	// helperStoppedNote: Windows, the helper counts the wait and switches the PC off.
	helperStoppedNote = "warning: its elevated helper is stopped, and the helper is what counts the wait and switches this PC off. " +
		"While it is stopped nothing does; a wait that ends while it is stopped ends with the PC ON"
)

// mayArm reports whether the run may hand the machine its ticket, and if not, why. It is the one way to
// Ops.Arm. The evidence is mayShutDown's (wokeProof): a ticket lets a machine switch itself off, and the
// machine arms itself for anybody's ticket if it started after the signature, so the panel sends one only for
// a machine that this run woke by its own proof. On top of that: the machine answers now, nothing else is
// pending on it, and a Proxmox node has no running guests that the panel can see.
func mayArm(h *Host, info HostInfo, o Observation, guests func() (running int, known bool, why string)) (bool, string) {
	if ok, why := wokeProof(h); !ok {
		return false, why
	}
	switch {
	case !info.OptedIn:
		return false, "it is no longer switched on for \"Update unpowered hosts\""
	case !o.Online:
		return false, "its agent is not answering right now"
	case o.Pending != nil:
		return false, fmt.Sprintf("another power action is already pending on it (%s at %s)", o.Pending.Action, clock(o.Pending.At))
	}
	if why := guestsKeepOn(guests()); why != "" {
		return false, why
	}
	return true, ""
}

// checkedOnline is the end of stepOnline for a machine with the check: it has answered for onlineSettle.
func (m *Manager) checkedOnline(ctx context.Context, r *Run, h *Host, info HostInfo, o Observation, now time.Time) {
	switch {
	case r.Cancel != nil:
		m.endAsIs(r, h, now, "The run was cancelled by "+r.Cancel.By)
		return
	case h.Mode == ModeUpdateOnly && h.StartApproved:
		m.enter(h, StUpdating, now, "online: asking its agent to update (approved with the start)")
		return
	case h.Mode == ModeUpdateOnly:
		m.updateDone(r, h, now, UpdApproval, noApprovalForIt)
		return
	case h.AlreadyOn: // also NotFresh: the run did not wake it, so no ticket is for it, and no confirmation
		m.updateDone(r, h, now, UpdApproval, onNoApproval)
		return
	}
	if !m.armStep(ctx, r, h, info, o, now) {
		return
	}
	h.To, h.chk.toResult, h.chk.toText = runTo(r, o)
	m.enter(h, StApproval, now, awaitingText(r, h, now, m.mono()))
}

// runTo is the version the confirmation names for the machine's update, and for "-" (no update) what the
// update step ends with.
func runTo(r *Run, o Observation) (to, result, text string) {
	switch {
	case !o.UpdateEnabled:
		return "-", UpdFailed, "self-update is not enabled on its agent"
	case o.Behind != nil && !*o.Behind:
		return "-", UpdCurrent, ""
	case !runVersion(r.Latest):
		return "-", UpdNotTried, "the panel did not know the newest signed release when the run started, so the confirmation names no version"
	}
	return r.Latest, "", ""
}

// armStep hands the machine the ticket of the start, once, and reports whether the step is over (false: the
// run comes back at its next step: a list of a node's guests is asked for, the request has to wait its turn).
func (m *Manager) armStep(ctx context.Context, r *Run, h *Host, info HostInfo, o Observation, now time.Time) bool {
	c := &h.chk
	if c.armTried {
		return true
	}
	none := func(kind, why string) bool {
		c.armTried = true
		h.Arm, h.ArmText = kind, why
		m.note(h, now, "not armed ("+why+"): it stays ON unless you confirm")
		return true
	}
	if ok, why := mayArm(h, info, o, func() (int, bool, string) { return m.Ops.Guests(h.ID, deref(h.OnlineAt)) }); !ok {
		return none(ArmNotSent, why)
	}
	// A node's guests from a list that is from now, as before the panel's own shutdown request (guestsCurrent).
	mono := m.mono()
	if !h.listWanted {
		h.listWanted, h.listWantMono = true, mono
	}
	if listed, ok, ago := m.Ops.AskGuests(h.ID); listed && !(ok && mono-ago > h.listWantMono && ago <= guestsOld) {
		if mono-h.listWantMono < guestsWait {
			m.say(h, "asking its Proxmox API which guests run on it")
			return false
		}
		h.listWanted = false
		return none(ArmNotSent, guestsKeepOn(0, false, noGuestList))
	}
	h.listWanted = false // the pause before the shutdown asks for a list of its own
	if !m.pace.fits(info.AgentID, 1, m.paceNow()) {
		m.say(h, "awake: waiting for its turn to be armed (its agent is sent only a few requests a minute)")
		return false
	}
	if !m.ask(r, h, "arm") { // written down before it goes out
		return false
	}
	if r.Cancel != nil || !m.pace.take(info.AgentID, 1, m.paceNow()) {
		m.done(h)
		return false
	}
	c.armTried = true // from here on no second request, whatever comes of this one
	t := r.Ticket
	c.armBefore = m.mono()
	c.lateUntil = r.startMono + t.valid() + lateArm // the ticket was signed before the run started
	var err error
	var offIn time.Duration
	id, run := h.ID, r.ID
	m.io(func() { offIn, err = m.Ops.Arm(ctx, id, run) })
	m.done(h)
	params := map[string]any{"wait_s": t.WaitS, "notice_s": t.NoticeS}
	switch {
	case err == nil:
		at := m.now()
		off := at.Add(t.wait())
		h.Arm, h.ArmedAt, h.SelfOffAt = ArmArmed, &at, &off
		c.offMin, c.offMax = c.armBefore+t.wait(), m.mono()+t.wait()
		m.emit(EvStep, r, h, "arm", params, "ok")
		if offIn != t.wait() { // the web package refuses such an answer; said here for an Ops that does not
			m.note(h, at, fmt.Sprintf("its agent names a wait of %s, the ticket one of %s: the ticket's counts", span(offIn), span(t.wait())))
		}
		m.note(h, at, fmt.Sprintf("armed: it switches itself off at about %s (a notice of %s counts first) unless you confirm, or cancel that", clock(off), span(t.notice())))
	case opCode(err) == CodeNoAnswer:
		h.Arm, h.ArmText = ArmUnanswered, opText(err)
		c.offMin, c.offMax = c.armBefore+t.wait(), c.lateUntil+t.wait()
		m.emit(EvStep, r, h, "arm", params, opText(err))
		m.note(h, m.now(), "the arming got no answer ("+opText(err)+"): it may have arrived. It is not sent again; what its agent shows decides")
	case opCode(err) == CodeTicketGone:
		h.Arm, h.ArmText = ArmNotSent, opText(err)
		m.emit(EvStep, r, h, "arm", params, opText(err))
		m.note(h, m.now(), "not armed ("+opText(err)+"): it stays ON unless you confirm")
	default:
		// A refusal is the answer to the request the panel sent. If a copy of the request arrived before it (a
		// proxy that repeats it), the machine is armed all the same: what its agent shows decides, and nothing
		// says "stays ON" before that was read (stepAwaiting). The times are the ones of a request that may
		// have arrived.
		h.Arm, h.ArmText = ArmRefused, opText(err)
		c.offMin, c.offMax = c.armBefore+t.wait(), c.lateUntil+t.wait()
		m.emit(EvStep, r, h, "arm", params, opText(err))
		m.note(h, m.now(), "its arming was refused ("+opText(err)+"): what its agent shows decides whether a run is on it all the same")
	}
	return true
}

// offWords is when the machine switches itself off, for a sentence: "about 21:42:10 UTC", the machine's own
// word with the words that say so, or the latest it can be when the arming got no answer.
func offWords(r *Run, h *Host, now time.Time, mono time.Duration) string {
	switch {
	case h.SelfOffAt != nil && h.SelfOffByMachine:
		return when(*h.SelfOffAt, now) + " by the machine's own clock"
	case h.SelfOffAt != nil:
		return "about " + when(*h.SelfOffAt, now)
	}
	return "about " + when(now.Add(h.chk.offMax-mono), now) + " at the latest"
}

// notArmedWords says, in a sentence, that and why the machine is not armed.
func notArmedWords(h *Host) string {
	switch {
	case h.Arm == ArmRefused:
		return "It could not be armed (" + h.ArmText + ")."
	case h.Arm == ArmUnanswered:
		return "Its arming got no answer (" + h.ArmText + "), and its agent never showed the run: it is not armed."
	case h.Arm == ArmArmed:
		return "It was armed, and its agent no longer shows the run (the machine was started again, or the run was ended on it): it is not armed any more."
	case h.ArmText != "":
		return "It was not armed (" + h.ArmText + ")."
	}
	return "It was not armed."
}

// armedTail ends the sentence of a machine that may switch itself off when the run lets go of it without
// having called that off.
func armedTail(r *Run, h *Host, now time.Time, mono time.Duration) string {
	notice := span(r.Ticket.notice())
	if h.Arm == ArmArmed || h.chk.armSeen {
		return fmt.Sprintf("%s and is armed: it begins to switch itself off at %s, with a notice of %s, unless that is cancelled (Keep on in the panel, or at the machine). "+
			"Fairy no longer watches it. %s", cameOnline(h), offWords(r, h, now, mono), notice, notConfirmed)
	}
	return fmt.Sprintf("%s. Its arming got no answer (%s): if it arrived, the machine switches itself off at %s, with a notice of %s, unless that is cancelled; if not, it stays ON. "+
		"Fairy no longer watches it. %s", cameOnline(h), h.ArmText, offWords(r, h, now, mono), notice, notConfirmed)
}

// awaitingText is what the row of a waiting machine says.
func awaitingText(r *Run, h *Host, now time.Time, mono time.Duration) string {
	c := &h.chk
	// No confirmation of the run can reach it any more (the question is over, the run was confirmed without it,
	// or it was cancelled), and it is still up and armed, or may be: its row does not go on saying "unless
	// you confirm". It says that it was not confirmed, when it switches itself off, and what keeps it on.
	if over := r.Approval == "given" || r.Approval == "over" || r.Cancel != nil; over && h.armedMaybe() {
		switch {
		case h.armedSure() && h.Confirm == ConfirmUnanswered:
			return fmt.Sprintf("Its confirmation got no answer (%s), and its agent does not show the run as confirmed so far: not updated. It is armed: unless the confirmation did arrive, it switches itself off at %s (Keep on cancels that)",
				h.ConfirmText, offWords(r, h, now, mono))
		case h.armedSure():
			return fmt.Sprintf("%s: not confirmed, not updated. It is armed: it switches itself off at %s (Keep on cancels that)", leadWhy(r, h), offWords(r, h, now, mono))
		case h.Confirm == ConfirmUnanswered:
			return fmt.Sprintf("Neither its arming nor its confirmation (%s) got an answer, and its agent does not show the run so far: not updated. If it is armed and not confirmed, it switches itself off at %s (Keep on cancels that)",
				h.ConfirmText, offWords(r, h, now, mono))
		default:
			return fmt.Sprintf("%s: not confirmed, not updated. Its arming got no answer, so Fairy does not know yet whether it is armed: if it is, it switches itself off at %s (Keep on cancels that)",
				leadWhy(r, h), offWords(r, h, now, mono))
		}
	}
	switch {
	case h.armedSure():
		return fmt.Sprintf("awake and armed: it switches itself off at %s unless you confirm", offWords(r, h, now, mono))
	case h.armedMaybe():
		return "awake; the arming got no answer, so Fairy does not know yet whether it is armed: waiting for you to confirm"
	case c.other != nil:
		return fmt.Sprintf("awake; not armed by this run (%s), but its agent shows another run (%s) as %s: by that run %s. Keep on cancels that", h.ArmText, c.other.ID, blockWords(c.other.State), otherOff(c.other, now))
	case c.disarmed && (h.Arm == ArmArmed || c.armSeen):
		return "awake; it is not armed any more (its agent no longer shows the run): it stays ON unless you confirm"
	case c.disarmed:
		return "awake; it is not armed (the arming, which got no answer, never arrived): it stays ON unless you confirm"
	case h.Arm == ArmRefused && !c.noRun:
		return "awake; its arming was refused (" + h.ArmText + "): waiting for its agent to show whether a run is on it all the same"
	case h.Arm == ArmRefused:
		return "awake; it could not be armed (" + h.ArmText + "): it stays ON unless you confirm"
	}
	return "awake; it was not armed (" + h.ArmText + "): it stays ON unless you confirm"
}

// blockWords says a run's state on a machine for a sentence.
func blockWords(state string) string {
	switch state {
	case BlockOffPending:
		return "counting down to its switch-off"
	case BlockConfirmed:
		return "confirmed"
	}
	return state
}

// otherOff says what another run that is live on the machine does to it, with the machine's own time.
func otherOff(b *RunBlock, now time.Time) string {
	switch {
	case b.State == BlockConfirmed:
		return "an update and a shutdown are let through on it"
	case b.OffAt.IsZero():
		return "it switches itself off (its agent names no time)"
	}
	return "it switches itself off at " + when(b.OffAt, now) + " by the machine's own clock"
}

// otherRun is another run than r that is live on the machine by its latest answer, when that answer counts
// (as for runWord): armed, counting down or confirmed. prev is kept while the latest answer says nothing.
func (h *Host) otherRun(r *Run, o Observation, word string, prev *RunBlock) *RunBlock {
	if word == "" {
		return prev
	}
	if o.Run != nil && o.Run.ID != r.ID && (o.Run.State == BlockArmed || o.Run.State == BlockOffPending || o.Run.State == BlockConfirmed) {
		b := *o.Run
		return &b
	}
	return nil
}

// runWord is what the machine's latest answer says about THIS run, when that answer came after since (steady)
// and under the entry the run knows: one of the Block... states; "none" when its checker takes runs and names
// no run, or names another one (this run is not live on it); "" when the answer says nothing that counts.
func (h *Host) runWord(r *Run, o Observation, mono, since time.Duration) string {
	if !o.Online || o.Entry != h.entry || mono-o.SeenAgo <= since {
		return ""
	}
	switch {
	case o.Run != nil && o.Run.ID == r.ID:
		return o.Run.State
	case o.Run != nil || o.Runs:
		return "none"
	}
	return ""
}

// seeNoRun counts the answers in a row that name no run of this run's (runWord "none"), each answer once.
func (h *Host) seeNoRun(o Observation, word string) int {
	c := &h.chk
	if word != "none" {
		if word != "" {
			c.none = 0
		}
		return c.none
	}
	if !o.SeenAt.Equal(c.noneAt) {
		c.noneAt, c.none = o.SeenAt, c.none+1
	}
	return c.none
}

// askConfirmation is the run's one question for the second confirmation, and its end: asked when a machine
// with the check waits and none is still on its way up; over when it was given, when its time is over, when
// the run was cancelled or stopped, or when nobody waits any more.
func (m *Manager) askConfirmation(r *Run, now time.Time) {
	if r.Ticket == nil {
		return
	}
	waiting, early := 0, 0
	for _, h := range r.Hosts {
		if !h.Checked || h.Mode != ModeWake {
			continue
		}
		switch h.State {
		case StApproval:
			if h.Confirm == "" && h.chk.disarm == nil {
				waiting++
			}
		case StQueued, StWaking, StOnline:
			early++
		}
	}
	mono := m.mono()
	switch {
	case r.Approval == "" && r.Cancel == nil && r.stopped == "" && waiting > 0 && early == 0:
		by := now.Add(time.Duration(r.Ticket.ConfirmWaitS) * time.Second)
		r.Approval, r.ConfirmBy, r.confirmMono = "needed", &by, mono+time.Duration(r.Ticket.ConfirmWaitS)*time.Second
		m.dirty = true
		m.emit(EvApproval, r, nil, "", nil, "")
	case r.Approval == "needed" && (waiting == 0 || r.Cancel != nil || r.stopped != "" || mono > r.confirmMono):
		m.confirmationOver(r)
	}
}

// confirmationOver ends the question without an answer.
func (m *Manager) confirmationOver(r *Run) {
	if r.Approval != "needed" {
		return
	}
	r.Approval = "over"
	m.dirty = true
	m.emit(EvApprovalEnd, r, nil, "", nil, "")
}

// offerable reports whether the machine can still be offered for the run's confirmation at mono.
func (h *Host) offerable(r *Run, mono time.Duration) bool {
	if !h.Checked || h.State != StApproval || h.Confirm != "" || h.chk.confirming || h.chk.disarm != nil || h.verdict != nil {
		return false
	}
	if h.armedMaybe() && mono > h.chk.offMin-confirmMargin {
		return false
	}
	return true
}

// WaitingHost is a machine that waits for the run's confirmation, with what the confirmation names for it.
type WaitingHost struct {
	ID, Name       string
	To             string // the version its update must install; "-": no update, only the shutdown
	ShutdownDelayS int    // the countdown of its shutdown
	WithinS        int
	Armed          bool
	// SelfOffAt: when it begins to switch itself off if it is not confirmed (nil: it is not armed, or the run
	// does not know when); ByMachine: that time is the machine's own word, by its own clock.
	SelfOffAt *time.Time
	ByMachine bool
}

// Waiting returns the machines of the active run id that can be confirmed now. An error says why none can:
// the run is not the active one, it does not ask (any more), or it was confirmed already.
func (m *Manager) Waiting(id string) ([]WaitingHost, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.confirmable(id)
	if err != nil {
		return nil, err
	}
	var out []WaitingHost
	mono := m.mono()
	for _, h := range r.Hosts {
		if !h.offerable(r, mono) {
			continue
		}
		w := WaitingHost{ID: h.ID, Name: h.Name, To: h.To, ShutdownDelayS: int(h.delay(r) / time.Second), WithinS: r.Ticket.WithinS,
			Armed: h.armedMaybe() && (h.Arm == ArmArmed || h.chk.armSeen), ByMachine: h.SelfOffByMachine}
		if h.SelfOffAt != nil && h.armedMaybe() {
			at := *h.SelfOffAt
			w.SelfOffAt = &at
		}
		out = append(out, w)
	}
	if len(out) == 0 {
		return nil, &StartError{Msg: "no machine of this run can be confirmed any more: each is too close to the moment it switches itself off, or has ended"}
	}
	return out, nil
}

// confirmable returns the active run id if its confirmation is asked for right now. The caller holds m.mu.
func (m *Manager) confirmable(id string) (*Run, error) {
	r := m.active
	switch {
	case r == nil || r.ID != id:
		return nil, &StartError{Msg: "that run is not active (any more)"}
	case r.Approval == "given":
		return nil, &StartError{Msg: "this run was confirmed already (by " + r.ConfirmedBy + "): a run is confirmed once"}
	case r.Cancel != nil:
		return nil, &StartError{Msg: "this run was cancelled: it is not confirmed any more"}
	case r.stopped != "" || r.journalErr != "":
		return nil, &StartError{Msg: "this run has stopped: it is not confirmed any more"}
	case r.Approval != "needed" || r.Ticket == nil || m.mono() > r.confirmMono:
		return nil, &StartError{Msg: "this run does not ask for a confirmation (any more)"}
	}
	return r, nil
}

// BeginConfirm is called when a person has given the run's confirmation with a passkey, before anything of it
// is sent to a machine: the run is confirmed from here on (once: a second call is refused), and the machines
// of hosts that can still be confirmed are marked as "the confirmation is going out", which the journal
// knows before the first request leaves. It returns those machines; nothing may be sent to any other.
func (m *Manager) BeginConfirm(id, by string, hosts []string) ([]string, error) {
	m.mu.Lock()
	r, err := m.confirmable(id)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	mono := m.mono()
	var begun []string
	var marked []*Host
	for _, h := range r.Hosts {
		named := false
		for _, x := range hosts {
			named = named || x == h.ID
		}
		if !named || !h.offerable(r, mono) {
			continue
		}
		h.chk.confirming, h.chk.confirmMono, h.Asking = true, mono, "confirm"
		begun, marked = append(begun, h.ID), append(marked, h)
	}
	if len(begun) == 0 {
		m.mu.Unlock()
		return nil, &StartError{Msg: "no machine of this run can be confirmed any more: each is too close to the moment it switches itself off, or has ended"}
	}
	r.Approval, r.ConfirmedBy = "given", by
	m.emit(EvApprovalEnd, r, nil, "", nil, "")
	m.mu.Unlock()
	if err := m.save(); err != nil { // nothing goes out that the journal does not know of
		m.mu.Lock()
		if m.active == r {
			r.Approval, r.ConfirmedBy = "over", ""
			for _, h := range marked {
				h.chk.confirming, h.Asking = false, ""
			}
		}
		m.mu.Unlock()
		return nil, &StartError{Msg: "the run journal cannot be written, so the confirmation was not sent to any machine: " + journalErrText(err)}
	}
	return begun, nil
}

// ConfirmResult is what came of the run's confirmation for one machine.
type ConfirmResult struct {
	OK         bool   // the machine accepted it
	Unanswered bool   // the request left and got no usable answer
	NotAsked   bool   // nothing was sent to it (Text says why)
	Reason     string // refused: the machine's word for the rule ("too_late", "run_over", ...)
	Text       string // for people
}

// Confirmed tells the run what came of the confirmation for one machine BeginConfirm returned, or, with
// NotAsked, that a waiting machine was not part of it.
func (m *Manager) Confirmed(id, hostID string, res ConfirmResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.active
	if r == nil || r.ID != id {
		return
	}
	for _, h := range r.Hosts {
		if h.ID != hostID {
			continue
		}
		c := &h.chk
		now := m.now()
		if res.NotAsked {
			if h.State == StApproval && h.Confirm == "" && !c.confirming {
				h.Confirm, h.ConfirmText = ConfirmNotAsked, res.Text
				m.note(h, now, "it was not part of the confirmation ("+res.Text+")")
			}
			return
		}
		if !c.confirming {
			return // the run stopped waiting for this answer (confirmingMax): its agent's own word decides
		}
		c.confirming = false
		m.done(h)
		switch {
		case res.OK:
			h.Confirm, h.ConfirmText = ConfirmOK, ""
			h.grant = &Grant{Run: r.ID}
			m.emit(EvStep, r, h, "confirm", map[string]any{"to": h.To}, "ok")
			m.note(h, now, fmt.Sprintf("confirmed by %s: its agent lets one update (%s) and one shutdown of this run through, for %s",
				r.ConfirmedBy, toWords(h.To), span(time.Duration(r.Ticket.WithinS)*time.Second)))
		case res.Unanswered:
			h.Confirm, h.ConfirmText = ConfirmUnanswered, res.Text
			m.emit(EvStep, r, h, "confirm", map[string]any{"to": h.To}, res.Text)
			m.note(h, now, "its confirmation got no answer ("+res.Text+"): it may have arrived. It is not sent again; what its agent shows decides")
		default:
			h.Confirm, h.ConfirmText = ConfirmRefused, res.Text
			c.tooLate = res.Reason == "too_late"
			m.emit(EvStep, r, h, "confirm", map[string]any{"to": h.To}, res.Text)
			m.note(h, now, "its confirmation was not accepted ("+res.Text+")")
		}
		m.dirty = true
		return
	}
}

func toWords(to string) string {
	if to == "-" || to == "" {
		return "none: no update"
	}
	return "to " + to
}

// leadWhy begins the result of a machine the confirmation did not reach: why not.
func leadWhy(r *Run, h *Host) string {
	switch {
	case h.Confirm == ConfirmRefused:
		return "Its confirmation was not accepted (" + h.ConfirmText + ")"
	case h.Confirm == ConfirmUnanswered:
		return "Its confirmation got no answer (" + h.ConfirmText + "), and its agent did not show the run as confirmed"
	case h.Confirm == ConfirmNotAsked:
		return "The run was confirmed without it (" + h.ConfirmText + ")"
	case r.Cancel != nil:
		return "The run was cancelled by " + r.Cancel.By
	case r.Approval == "given":
		return "The run was confirmed without it"
	case r.Approval == "over" && r.Ticket != nil:
		return "Nobody confirmed within " + span(time.Duration(r.Ticket.ConfirmWaitS)*time.Second)
	}
	return "Nobody confirmed in time"
}

// stepAwaiting is the step of a machine with the check that is awake and waits for the run's confirmation.
func (m *Manager) stepAwaiting(ctx context.Context, r *Run, h *Host, info HostInfo, now time.Time) {
	c := &h.chk
	mono := m.mono() // before the poller is asked
	o := m.Ops.Observe(h.ID)
	word := h.runWord(r, o, mono, c.armBefore)
	if c.confirming {
		if mono-c.confirmMono <= confirmingMax {
			return
		}
		c.confirming = false
		m.done(h)
		h.Confirm, h.ConfirmText = ConfirmUnanswered, "the panel did not learn what came of it"
		m.note(h, now, "the panel did not learn what came of its confirmation: what its agent shows decides")
	}
	if h.Confirm == ConfirmUnanswered && word == BlockConfirmed {
		h.Confirm, h.grant = ConfirmOK, &Grant{Run: r.ID}
		m.note(h, now, "its agent shows the run as confirmed: the confirmation did arrive")
	}
	if h.confirmed() {
		m.afterConfirm(r, h, o, now)
		return
	}
	none := h.seeNoRun(o, word)
	if word != "" {
		c.noRun = none >= 2
	}
	if was := c.other; true {
		c.other = h.otherRun(r, o, word, was)
		if c.other != nil && (was == nil || was.ID != c.other.ID || was.State != c.other.State) {
			m.note(h, now, fmt.Sprintf("its agent shows another run (%s) as %s: by that run %s", c.other.ID, blockWords(c.other.State), otherOff(c.other, now)))
		}
	}
	switch {
	case (word == BlockCancelled || word == BlockDone) && h.armedMaybe():
		m.endCalledOff(r, h, now)
		return
	case word == BlockOffPending:
		c.armSeen, c.disarmed = true, false
		m.enterSelfOff(r, h, o, mono, now)
		return
	case word == BlockArmed && h.Arm != ArmArmed && !c.armSeen:
		// Its agent shows THIS run as armed: a ticket did arrive, whatever the panel was answered. When it
		// switches itself off is the machine's own word from here on.
		c.armSeen, c.disarmed = true, false
		if !o.Run.OffAt.IsZero() {
			at := o.Run.OffAt
			h.SelfOffAt, h.SelfOffByMachine = &at, true
		}
		if c.offMax == 0 { // no request of the run that could have armed it: its own word is all there is
			c.offMin, c.offMax = mono, mono+r.Ticket.wait()
		}
		how := "the arming did arrive"
		switch h.Arm {
		case ArmRefused:
			how = "its agent shows that it is armed by this run, although its arming was answered with a refusal (" + h.ArmText + "): a ticket did arrive"
		case ArmUnanswered:
			how = "its agent shows that it is armed: the arming did arrive"
		default:
			how = "its agent shows that it is armed by this run, although the panel handed it no ticket (" + h.ArmText + ")"
		}
		m.note(h, now, fmt.Sprintf("%s. It switches itself off at %s unless you confirm, or cancel that", how, offWords(r, h, now, mono)))
	case word == BlockArmed:
		c.armSeen, c.disarmed = true, false
	case none >= 2 && (h.Arm == ArmArmed || c.armSeen) && !c.disarmed:
		// It took the ticket, and its checker, which takes runs, names no run of this id in two answers: the
		// machine was started again, or the run was ended on it. It does not switch itself off.
		c.disarmed = true
		m.note(h, now, "its agent no longer shows the run (the machine was started again, or the run was ended on it): it is not armed any more, and stays ON unless you confirm")
	case none >= 2 && h.Arm == ArmUnanswered && !c.armSeen && !c.disarmed && mono-o.SeenAgo > c.lateUntil:
		// The ticket can no longer be presented, and its agent shows no run: the request never arrived.
		c.disarmed = true
		m.note(h, now, "the arming, which got no answer, never arrived (its agent shows no run, and the ticket is no longer valid): it is not armed")
	}
	if h.armedMaybe() && o.Online && o.HelperStopped && !c.helperNoted {
		c.helperNoted = true
		m.note(h, now, helperStoppedNote)
	}
	if h.armedMaybe() {
		if why := m.guestsSince(h, mono); why != "" {
			m.disarm(ctx, r, h, now, "Kept ON: "+why+" since it was armed")
			return
		}
		if now.Sub(h.lastKick) >= kickEvery {
			h.lastKick = now
			m.Ops.Kick(h.ID)
		}
	}
	over := r.Approval == "given" || r.Approval == "over" || r.Cancel != nil // no confirmation can reach it any more
	switch {
	case c.tooLate, h.armedSure() && mono >= c.offMin:
		m.enterSelfOff(r, h, o, mono, now)
	case h.armedSure():
		m.say(h, awaitingText(r, h, now, mono)+helperWords(o))
	case h.armedMaybe(): // the arming got no answer, and its agent has not shown the run
		if over && mono > c.lateUntil+lookFor {
			// It answers no question about the run (it is out of reach, or its checker does not say): the run
			// does not know, and says exactly that.
			m.endAsIs(r, h, now, leadWhy(r, h))
			return
		}
		m.say(h, awaitingText(r, h, now, mono))
	case over && c.other != nil:
		// Not armed by this run, and another run is live on it by its own word: it is not said to stay on.
		text := cameOnline(h) + ". " + leadWhy(r, h) + ": not confirmed, not updated. " + notArmedWords(h)
		if b := c.other; b.State != BlockConfirmed && !b.OffAt.IsZero() {
			at := b.OffAt
			h.SelfOffAt, h.SelfOffByMachine = &at, true
		}
		m.endSeen(r, h, StLeftOn, OnYes, now, fmt.Sprintf("%s It is ON, and its agent shows another run (%s) as %s: by that run %s unless that is cancelled (Keep on in the panel, or at the machine).",
			text, c.other.ID, blockWords(c.other.State), otherOff(c.other, now)), text)
	case over && h.Arm == ArmRefused && !c.noRun && mono-c.armBefore <= lookFor:
		m.say(h, awaitingText(r, h, now, mono)) // its agent is read first
	case over && h.Arm == ArmRefused && !c.noRun:
		// The refusal is all the run has: no answer of its agent from after the request said that no run is on it.
		text := cameOnline(h) + ". " + leadWhy(r, h) + ": not confirmed, not updated. Its arming was refused (" + h.ArmText + "), and its agent did not say afterwards whether a run is on it."
		m.endSeen(r, h, StLeftOn, OnYes, now, text+" It was ON when Fairy last saw it; Fairy could not read that nothing of this run is on it.", text)
	case over:
		text := cameOnline(h) + ". " + leadWhy(r, h) + ": not confirmed, not updated. " + notArmedWords(h)
		m.endSeen(r, h, StLeftOn, OnYes, now, text+" It is ON: nothing of this run switches it off.", text)
	default:
		m.say(h, awaitingText(r, h, now, mono))
	}
}

// helperWords is the warning in the row of an armed PC whose helper is stopped.
func helperWords(o Observation) string {
	if o.Online && o.HelperStopped {
		return " · warning: its helper is stopped, and only the helper switches it off"
	}
	return ""
}

// afterConfirm moves a machine whose confirmation was accepted on to its update.
func (m *Manager) afterConfirm(r *Run, h *Host, o Observation, now time.Time) {
	c := &h.chk
	h.guestsAsked, h.guestsUnseen = false, false
	if h.To == "-" || h.To == "" {
		if o.Online {
			h.VersionBefore = o.Version
		}
		m.note(h, now, "confirmed: the confirmation names no update for it")
		m.updateDone(r, h, now, orText(c.toResult, UpdNotTried), c.toText)
		return
	}
	m.enter(h, StUpdating, now, "confirmed: asking its agent to update to "+h.To)
}

// guestsSince asks, every guestsEvery, whether guests run on an armed Proxmox node, and returns why they keep
// it on ("": they do not, or it was not asked now). The rule of the countdown's watch (stepShutting): a guest
// that runs counts at once, "cannot see them" when it is said twice in a row, and "no guest runs" only from a
// list that is from now.
func (m *Manager) guestsSince(h *Host, mono time.Duration) string {
	c := &h.chk
	if c.guestsAsked && mono-c.guestsMono < guestsEvery {
		return ""
	}
	c.guestsAsked, c.guestsMono = true, mono
	running, known, names := m.Ops.Guests(h.ID, deref(h.OnlineAt))
	if listed, ok, ago := m.Ops.AskGuests(h.ID); listed && known && running == 0 && (!ok || ago > guestsOld) {
		running, known, names = 0, false, noGuestList
	}
	switch why := guestsKeepOn(running, known, names); {
	case why == "":
		c.guestsUnseen = false
	case !known && !c.guestsUnseen:
		c.guestsUnseen = true
	default:
		return why
	}
	return ""
}

// enterSelfOff moves a machine that nobody confirmed in time to the watch of its self-switch-off. The run
// sends it nothing. endMin and dueMono are the earliest and the latest end of its countdown on the steady
// clock, as for the countdown of a shutdown the run asked for (seeSilence, seenOff read them).
func (m *Manager) enterSelfOff(r *Run, h *Host, o Observation, mono time.Duration, now time.Time) {
	c := &h.chk
	t := r.Ticket
	h.endMin, h.dueMono = c.offMin+t.notice(), c.offMax+selfOffLate+t.notice()
	if c.offMax == 0 {
		// The run has no moment of its own for the arming (it never sent a ticket that was taken, and the
		// machine says that it is switching itself off): counted from here, and from its latest answer.
		h.endMin, h.dueMono = mono-o.SeenAgo, mono+selfOffLate+t.notice()
	}
	h.silRow, h.silN, h.silAfter, h.cutEarly, h.offQuiet, h.kickMono = 0, 0, 0, false, false, 0
	lead := leadWhy(r, h)
	m.enter(h, StSelfOff, now, fmt.Sprintf("%s: it is switching itself off (a notice of %s counts on it from %s; Keep on cancels that)", lead, span(t.notice()), offWords(r, h, now, mono)))
	m.emit(EvStep, r, h, "self_off", map[string]any{"notice_s": t.NoticeS}, "ok")
}

// stepSelfOff watches a machine that is switching itself off until it is gone, or until its own word or its
// answers say that it is not going.
func (m *Manager) stepSelfOff(ctx context.Context, r *Run, h *Host, now time.Time) {
	c := &h.chk
	t := r.Ticket
	mono := m.mono()
	o := m.Ops.Observe(h.ID)
	word := h.runWord(r, o, mono, c.armBefore)
	if h.Confirm == ConfirmUnanswered && word == BlockConfirmed { // the confirmation arrived after all, in time
		h.Confirm, h.grant = ConfirmOK, &Grant{Run: r.ID}
		m.note(h, now, "its agent shows the run as confirmed: the confirmation did arrive, and it is not switching itself off")
		m.afterConfirm(r, h, o, now)
		return
	}
	none := h.seeNoRun(o, word)
	lead := cameOnline(h) + ". " + leadWhy(r, h) + ": not confirmed, not updated."
	switch {
	case word == BlockCancelled || word == BlockDone:
		m.endCalledOff(r, h, now)
		return
	case none >= 2 && (h.Arm == ArmArmed || c.armSeen):
		c.disarmed = true
		text := lead + " It was switching itself off, and its agent no longer shows the run (the machine was started again, or the run was ended on it)."
		m.endSeen(r, h, StLeftOn, OnYes, now, text+" It is ON.", text)
		return
	case word == BlockOffPending:
		// Its countdown runs: it began before this answer, so it ends no later than the answer plus the notice,
		// and it had not ended when the answer was made.
		c.armSeen = true
		at := mono - o.SeenAgo
		h.dueMono = min(h.dueMono, at+t.notice()+offConfirm)
		h.endMin = max(h.endMin, at)
	case word == BlockArmed:
		c.armSeen = true
	}
	if o.Online && mono-o.SeenAgo > c.armBefore && (o.UpdateState == "downloading" || o.UpdateState == "applying") && !c.updateWaited {
		// The machine waits with its switch-off while an update is being installed (not the run's: nobody
		// confirmed): its end can be that much later.
		c.updateWaited = true
		h.dueMono += selfOffUpdate
		m.note(h, now, "an update is being installed on it ("+o.UpdateState+"): it waits for that with its switch-off, up to "+span(selfOffUpdate))
	}
	if mono < h.dueMono-guestsMargin {
		if why := m.guestsSince(h, mono); why != "" {
			m.disarm(ctx, r, h, now, "Kept ON: "+why+" while it was switching itself off")
			return
		}
	}
	if h.kickMono == 0 || mono-h.kickMono >= kickEvery {
		h.kickMono = mono
		m.Ops.Kick(h.ID)
	}
	h.seeSilence(o, mono)
	switch {
	case o.Paused:
		if mono-h.dueMono > offConfirm {
			m.end(r, h, StLeftOn, OnUnknown, now, lead+" It was switching itself off. Polling is paused for it, so Fairy could not see whether it went off: not confirmed that it is off.")
		}
	case !o.Online && o.OnBy == "": // nothing at all from it
		switch {
		case h.seenOff(mono):
			m.end(r, h, StOff, OnNo, now, lead+" It switched itself off again.")
		case mono-h.dueMono <= offGrace:
		case h.cutEarly:
			m.end(r, h, StLeftOn, OnUnknown, now, lead+" It was switching itself off, was already out of reach before its countdown ended, and does not answer now: Fairy cannot tell "+
				"a machine that went off from a connection that dropped. Not confirmed that it is off.")
		default:
			m.end(r, h, StLeftOn, OnUnknown, now, lead+" It was switching itself off and does not answer now, but the panel could not ask it often enough after its countdown: not confirmed that it is off.")
		}
	case mono-h.dueMono <= offGrace: // it answers, or shows as on another way, and its time is not over
		m.say(h, fmt.Sprintf("%s: it is switching itself off (a notice of %s counts on it from %s; Keep on cancels that)%s", leadWhy(r, h), span(t.notice()), offWords(r, h, now, mono), helperWords(o)))
	case !o.Online:
		text := fmt.Sprintf("%s It was to switch itself off and still shows as on %s after that (%s), and its agent does not answer.", lead, span(offGrace), o.OnBy)
		m.endSeen(r, h, StLeftOn, OnYes, now, text+" It is ON.", text)
	default:
		shows := "its agent shows no run"
		switch word {
		case BlockArmed:
			shows = "its agent still shows it as armed"
		case BlockOffPending:
			shows = "its agent still shows the switch-off as pending"
		case "":
			shows = "its agent says nothing about the run"
		}
		stopped := ""
		if o.HelperStopped {
			stopped = " Its elevated helper is stopped, and only the helper switches it off."
		}
		text := fmt.Sprintf("%s It was to switch itself off and is still on %s after that; %s.%s", lead, span(offGrace), shows, stopped)
		m.endSeen(r, h, StLeftOn, OnYes, now, text+" It is ON.", text)
	}
}

// endCalledOff ends a machine whose own word is that the run was called off on it: a cancel (Keep on in the
// panel, the Cancel of its page, at the machine), or the machine itself did not go through with the switch-off
// (a guest runs on it, the wait ended while its checker was not running, its trust was changed).
func (m *Manager) endCalledOff(r *Run, h *Host, now time.Time) {
	h.chk.disarmed = true
	text := cameOnline(h) + ". Its agent reports that its switch-off was called off (a cancel in the panel or at the machine, or the machine itself did not go through with it). " + notConfirmed
	m.endSeen(r, h, StLeftOn, OnYes, now, text+" It is ON.", text)
}

// cancelledUnconfirmed is cancelled for a machine with the check that the run's confirmation has not reached:
// the panel can neither update it nor switch it off, so "switch off again" is the machine's own switch-off or
// none, and "leave on" calls that off.
func (m *Manager) cancelledUnconfirmed(ctx context.Context, r *Run, h *Host, now time.Time, why string, woke bool) bool {
	c := &h.chk
	switch {
	case c.confirming: // a confirmation is on its way to it: what comes of it is taken in first
		return true
	case h.armedMaybe() && r.Cancel.SwitchOff:
		if !c.cancelNoted {
			c.cancelNoted = true
			m.note(h, now, why+": it can no longer be confirmed, and it still switches itself off, as its ticket says. Nothing else is started for it")
		}
		return true
	case h.armedMaybe():
		m.disarm(ctx, r, h, now, why)
		return false
	case r.Cancel.SwitchOff && woke && h.OnlineAt != nil:
		text := why + " with \"switch off again\". " + cameOnline(h) + ". " + notArmedWords(h) + " Without your second confirmation Fairy cannot switch off a machine that checks approvals itself. " + notConfirmed
		m.endSeen(r, h, StLeftOn, OnYes, now, text+" It is ON.", text)
		return false
	case r.Cancel.SwitchOff && woke: // not up yet: it is waited for, as every woken machine is
		if !h.cancelHandled {
			h.cancelHandled = true
			m.note(h, now, why+": nothing else is started for it")
		}
		return true
	}
	m.endAsIs(r, h, now, why)
	return false
}

// disarm asks the machine's agent to call off its self-switch-off and ends the machine with what came of
// that. why begins the sentence. It is the agent's one cancel (Ops.CancelShutdown), which needs no approval
// and also ends the run on the machine; what the machine says AFTER it is what counts:
//
//   - its agent shows the run as cancelled, or names no run: it stays on;
//   - it still shows the run as armed or as counting down: the cancel is sent again, every cancelRetry;
//   - a cancel that got an error or no answer is NOT a cancel: it is sent again.
//
// until the latest moment the machine can switch itself off by the ticket. If the machine's word never came,
// the sentence says "it may switch itself off at ...". After a ticket whose request got no answer, a cancel
// that finds nothing ends nothing: the cancel is repeated for as long as the ticket can still arrive.
//
// The cancel goes to the agent the panel has under this id now. After a Re-pair that can be another machine:
// nothing is sent until that agent shows this run (its id is the run's own), and never if it does not.
func (m *Manager) disarm(ctx context.Context, r *Run, h *Host, now time.Time, why string) {
	c := &h.chk
	d := c.disarm
	if d == nil {
		d = &disarming{why: why}
		c.disarm = d
	}
	why = d.why
	t := r.Ticket
	mono := m.mono()
	until := c.offMax + selfOffLate + t.notice()
	if h.State == StSelfOff {
		until = h.dueMono
	}
	info, known := m.Ops.Host(h.ID)
	o := m.Ops.Observe(h.ID)
	fresh := known && o.Online && o.Entry == info.Entry
	shows := "" // what the agent that has the id now says about this run
	switch {
	case !fresh:
	case o.Run != nil && o.Run.ID == r.ID:
		shows = o.Run.State
	case o.Run != nil || o.Runs:
		shows = "none"
	default:
		shows = "silent" // an answer that says nothing about runs
	}
	late := h.Arm == ArmUnanswered && !c.armSeen // the ticket's request can still arrive
	if d.ok > 0 && fresh && mono-o.SeenAgo > d.okMono {
		// an answer from after a cancel its agent answered
		switch {
		case shows == BlockCancelled || shows == BlockDone:
			m.endKeptOn(r, h, now, why, "its agent shows the run as cancelled")
			return
		case (shows == "none" || shows == "silent") && !(late && mono <= c.lateUntil):
			m.endKeptOn(r, h, now, why, "its agent shows no run any more")
			return
		}
	}
	if shows == BlockArmed || shows == BlockOffPending {
		c.armSeen = true
	}
	if !known {
		m.endNotCalledOff(r, h, now, "it is no longer in the panel, so nothing can be sent to it")
		return
	}
	if info.Entry != h.entry && !(shows == BlockArmed || shows == BlockOffPending) {
		// another entry than the run started with, whose agent has not shown this run
		if mono > until {
			m.endNotCalledOff(r, h, now, "its entry in the panel was replaced, and the agent that has this id now never showed the run, so no cancel was sent to it")
			return
		}
		if h.kickMono == 0 || mono-h.kickMono >= kickEvery {
			h.kickMono = mono
			m.Ops.Kick(h.ID)
		}
		m.say(h, "paired again while it was armed: waiting for its agent to show the run before a cancel is sent")
		return
	}
	if h.kickMono == 0 || mono-h.kickMono >= kickEvery {
		h.kickMono = mono
		m.Ops.Kick(h.ID)
	}
	switch {
	case mono > until && d.ok > 0 && !d.lost && d.tries == 0 && late:
		// every cancel was answered, the ticket can no longer arrive, and no answer showed the run
		m.endKeptOn(r, h, now, why, "its agent answered every cancel, and the ticket is no longer valid")
		return
	case mono > until:
		m.endNotCalledOff(r, h, now, "")
		return
	case d.sent && mono-d.lastMono < cancelRetry:
		return
	}
	began := m.mono()
	var err error
	id, by := h.ID, r.By
	m.io(func() { err = m.Ops.CancelShutdown(ctx, id, by) })
	d.sent, d.lastMono = true, began
	params := map[string]any{"self_off": true}
	if d.tries+d.ok > 0 {
		params["again"] = true
	}
	if err != nil {
		if d.tries++; d.text == "" || (opCode(err) == CodeNoAnswer && !d.lost) {
			d.text = opText(err) // what stood in the way first; and the first try that got no answer at all
		}
		d.lost = d.lost || opCode(err) == CodeNoAnswer
		m.emit(EvStep, r, h, "cancel", params, opText(err))
		if d.tries == 1 {
			m.note(h, m.now(), fmt.Sprintf("%s: its switch-off could not be called off yet (%s): trying again every %s", why, opText(err), span(cancelRetry)))
		}
		m.say(h, "its switch-off could not be called off yet ("+opText(err)+"): trying again; it may switch itself off at "+offWords(r, h, now, mono))
		return
	}
	d.ok, d.okMono = d.ok+1, m.mono()
	m.emit(EvStep, r, h, "cancel", params, "ok")
	if d.ok == 1 {
		m.note(h, m.now(), why+": its agent was asked to call off its switch-off, and answered. Waiting for it to show that the run is over on it")
	}
	m.say(h, "asked to stay on: waiting for its agent to show that its switch-off is called off")
}

// endKeptOn ends a machine whose self-switch-off was called off, by its own word (how).
func (m *Manager) endKeptOn(r *Run, h *Host, now time.Time, why, how string) {
	h.chk.disarm, h.chk.disarmed = nil, true
	text := fmt.Sprintf("%s: its switch-off was called off (%s). %s. %s", why, how, cameOnline(h), notConfirmed)
	m.endSeen(r, h, StLeftOn, OnYes, now, text+" It is ON.", text)
}

// endNotCalledOff ends a machine whose self-switch-off the run could not call off, or could not see called
// off, before the machine's own time: it may go. how: why nothing could be sent ("": cancels were sent).
func (m *Manager) endNotCalledOff(r *Run, h *Host, now time.Time, how string) {
	c := &h.chk
	d := c.disarm
	c.disarm = nil
	mono := m.mono()
	at := offWords(r, h, now, mono)
	may := "it may switch itself off at " + at
	if h.SelfOffAt != nil && !now.Before(h.SelfOffAt.Add(r.Ticket.notice())) && !h.SelfOffByMachine {
		may = "it may have switched itself off (its time was " + at + ")"
	}
	var text string
	switch {
	case how != "":
		text = fmt.Sprintf("%s, but %s: %s.", d.why, how, may)
	case d.ok > 0 && d.tries == 0:
		text = fmt.Sprintf("%s. Its agent answered the request to call off its switch-off (%s), and did not show afterwards that the run is over on it: %s.", d.why, requests(d.ok), may)
	case d.lost:
		text = fmt.Sprintf("%s, and the request to call off its switch-off got no answer (%s): it may have arrived, and then the machine stays ON; if it did not, %s.", d.why, d.text, may)
	default:
		text = fmt.Sprintf("%s, but its switch-off could not be called off (%s): %s.", d.why, d.text, may)
	}
	text += " Keep on for it in the panel, or cancel at the machine. " + notConfirmed
	m.endSeen(r, h, StLeftOn, OnYes, now, text+" Fairy no longer watches it.", text)
}

// ArmedHost is a machine that may still switch itself off by a run's ticket: of the active run, or of a run
// that is over (interrupted by a restart of the panel, stopped, or ended while the machine did not answer).
type ArmedHost struct {
	RunID, ID, Name string
	// At: when it begins to switch itself off, as far as the panel knows (nil: not known). Latest: At is the
	// latest moment, for a machine whose arming got no answer. ByMachine: At is the machine's own word.
	At        *time.Time
	Latest    bool
	ByMachine bool
	// Sure: it took the ticket, or its agent showed the run as armed (false: its arming got no answer).
	Sure bool
	// Active: it is a machine of the run that is going.
	Active bool
}

// MayGoOff returns the machines that may still switch themselves off by a ticket at now: the banner names
// them, Keep on is offered for them, and their helper is not stopped from the panel. For a run that is over
// the panel no longer watches: a machine counts until the latest moment its self-switch-off can begin, plus
// its notice.
func (m *Manager) MayGoOff(now time.Time) []ArmedHost {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ArmedHost
	mono := m.mono()
	if r := m.active; r != nil && r.Ticket != nil {
		for _, h := range r.Hosts {
			if h.MayGoOff && h.On != OnNo && (h.verdict != nil || h.Final()) { // the run let go of it already
				out = append(out, letGo(r, h, now)...)
				continue
			}
			if b := h.chk.other; b != nil && !h.armedMaybe() && h.verdict == nil && !h.Final() && b.State != BlockConfirmed {
				// another run's ticket armed it, by its own word: named with that time, Keep on is the same cancel
				a := ArmedHost{RunID: r.ID, ID: h.ID, Name: h.Name, ByMachine: true, Sure: true, Active: true}
				if !b.OffAt.IsZero() {
					at := b.OffAt
					a.At = &at
				}
				out = append(out, a)
				continue
			}
			if !h.armedMaybe() || h.verdict != nil {
				continue
			}
			a := ArmedHost{RunID: r.ID, ID: h.ID, Name: h.Name, ByMachine: h.SelfOffByMachine, Sure: h.armedSure(), Active: true}
			if h.SelfOffAt != nil {
				at := *h.SelfOffAt
				a.At = &at
			} else {
				at := now.Add(h.chk.offMax - mono)
				a.At, a.Latest = &at, true
			}
			out = append(out, a)
		}
	}
	for _, r := range m.hist {
		if r.Ticket == nil {
			continue
		}
		for _, h := range r.Hosts {
			if h.MayGoOff && h.On != OnNo {
				out = append(out, letGo(r, h, now)...)
			}
		}
	}
	return out
}

// letGo is MayGoOff's entry for a machine the run no longer watches (nil: its time is over).
func letGo(r *Run, h *Host, now time.Time) []ArmedHost {
	a := ArmedHost{RunID: r.ID, ID: h.ID, Name: h.Name, ByMachine: h.SelfOffByMachine, Sure: h.Arm == ArmArmed || h.chk.armSeen || h.chk.other != nil}
	at := r.StartedAt.Add(r.Ticket.valid() + lateArm + r.Ticket.wait())
	if h.SelfOffAt != nil {
		at = *h.SelfOffAt
	} else {
		a.Latest = true
	}
	// over; or the panel's clock was set far back since: not shown for ever
	if now.After(at.Add(r.Ticket.notice()+selfOffLate+selfOffUpdate)) || now.Before(r.StartedAt.Add(-time.Hour)) {
		return nil
	}
	a.At = &at
	return []ArmedHost{a}
}
