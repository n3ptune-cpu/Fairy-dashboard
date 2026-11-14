package wakerun

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The world's machines that check passkey approvals themselves: a model of the agent's run rules
// (Fairy-agent v0.9.0, pkg/approval/run.go), as far as the panel can meet them. A machine keeps its run
// itself: armed by a ticket, it counts the wait on its own uptime and switches itself off through its
// ordinary delayed shutdown; a confirmation lets one update and one shutdown through; a cancel ends the run
// and closes the boot for tickets. The world also plays the panel's keeper of the tickets (one request per
// machine, none after valid_s), and it is the judge of the rules of the panel half:
//
//   - a ticket goes only to a machine the run woke (the judge of Wake and Shutdown, asked at Arm), once;
//   - no update and no shutdown request goes to a machine with the check unless that machine accepted this
//     run's confirmation (or, for a machine that was on, the start's approval covers its one update);
//   - a run is confirmed once.

// modelCheck is the checker of one machine.
type modelCheck struct {
	on            bool          // it checks approvals itself (a checker that takes runs)
	noRuns        bool          // its approval block says nothing about runs (the helper is not answering)
	helperStopped bool          // Windows: the helper is stopped; nothing counts the wait, nothing switches the PC off
	minDelay      time.Duration // its power_control.min_delay_s
	startApproved bool          // the start's passkey answered its challenge for one update (a machine that was on)
	startUsed     bool
	armLost       string // the answer to the ticket is lost: "arrives" | "never" | "late" (it reaches the machine armLate after it left)
	armLate       time.Duration
	armOnWay      time.Duration // steady: a ticket that is still on its way arrives then (0: none)
	armOnWayRun   string
	armSays       string // it refuses the ticket with this reason, whatever its rules say
	goLost        string // the answer to the confirmation is lost: "arrives" | "never"
	goSays        string // it refuses the confirmation with this reason
	lied          bool   // its agent showed this run as confirmed when it was not (the test says so: the judge allows what follows from that word)
	goTwice       bool   // the confirmation reaches it twice: it takes the first copy, and the panel holds its answer to the second
	cancelSays    string // its agent answers a cancel with this error (500 / 503): nothing is cancelled

	// its run record
	boot       time.Time // the boot the record's live part and its mark are of
	run        string
	state      string        // "" | armed | off_pending | confirmed
	waitEnds   time.Duration // steady: the wait of an armed run ends
	offAt      time.Time
	updates    int
	shutdowns  int
	delay      time.Duration
	withinEnds time.Duration
	closed     bool            // this boot takes no ticket any more
	used       map[string]bool // runs it had before (across boots)
	last       *RunBlock       // the run that ended last in this boot

	// for the judge
	arms    int
	gos     int
	goTaken map[string]bool // runs whose confirmation it accepted
	goOpen  bool            // a confirmation request for it got no answer
}

// modelTickets is the panel's keeper of a run's tickets and the judge's count of confirmations.
type modelTickets struct {
	t        Ticket
	signed   time.Time
	signedAt time.Duration // steady
	taken    map[string]bool
	confirms int
	runID    string // the id of the run the tickets are for (set when the run has started)
}

// checkedStart fills in, for a start request, what the web package would: which machines check approvals, the
// ticket's numbers, and which machines that are on were approved with the start.
func (w *world) checkedStart(req *StartRequest) {
	w.tk = modelTickets{t: Ticket{ValidS: 420, WaitS: w.m.Settings.OnlineTimeoutS + 600 + 60, NoticeS: 120, WithinS: 1500 + w.m.Settings.SettleS, ConfirmWaitS: 600},
		signed: w.now, signedAt: w.mono, taken: map[string]bool{}}
	if req.Ticket != nil {
		w.tk.t = *req.Ticket
	}
	need := false
	for _, id := range append(append([]string{}, req.Hosts...), req.UpdateOnly...) {
		mc := w.ms[id]
		if mc == nil || !mc.chk.on {
			continue
		}
		need = true
		if req.Checked == nil {
			req.Checked, req.StartApproved = map[string]bool{}, map[string]bool{}
		}
		req.Checked[id] = true
		if mc.chk.startApproved {
			req.StartApproved[id] = true
		}
		if mc.chk.used == nil {
			mc.chk.used = map[string]bool{}
		}
		mc.chk.goTaken, mc.chk.arms, mc.chk.gos, mc.chk.startUsed = map[string]bool{}, 0, 0, false
	}
	if need && req.Ticket == nil {
		t := w.tk.t
		req.Ticket = &t
	}
}

// bootOf brings the machine's record to its present boot: a live run and the mark that closes a boot are of
// the boot they were written in.
func (w *world) bootOf(mc *machine) *modelCheck {
	c := &mc.chk
	if !c.boot.Equal(mc.bootedAt) || !mc.on {
		c.boot, c.run, c.state, c.closed, c.last, c.armOnWay = mc.bootedAt, "", "", false, nil, 0
	}
	return c
}

func (w *world) uptime(mc *machine) time.Duration { return w.now.Sub(mc.bootedAt) }

// armNow is the machine's own check of a ticket for run ("": it is armed).
func (w *world) armNow(mc *machine, run string) string {
	c := w.bootOf(mc)
	t := w.tk.t
	switch {
	case c.armSays != "":
		return c.armSays
	case w.mono-w.tk.signedAt > time.Duration(t.ValidS+60)*time.Second:
		return "clock" // older than valid_s: the token is refused
	case time.Duration(t.NoticeS)*time.Second < c.minDelay:
		return "limits"
	case w.uptime(mc) >= 600*time.Second:
		return "not_fresh"
	case mc.bootedAt.Before(w.tk.signed.Add(-60 * time.Second)):
		return "started_before"
	case c.closed:
		return "already_armed"
	case c.used[run]:
		return "run_used"
	case c.state != "":
		return "other_run"
	case mc.pending != nil:
		return "power_pending"
	}
	c.run, c.state, c.closed, c.last = run, BlockArmed, true, nil
	c.used[run] = true
	c.waitEnds = w.mono + time.Duration(t.WaitS)*time.Second
	c.offAt = w.now.Add(time.Duration(t.WaitS) * time.Second)
	return ""
}

func (w *world) Arm(_ context.Context, id, run string) (time.Duration, error) {
	mc := w.ms[id]
	w.calls = append(w.calls, "arm "+id)
	w.arrived("arm " + id)
	wait := time.Duration(w.tk.t.WaitS) * time.Second
	// the judge: a ticket lets a machine switch itself off, so it goes where a shutdown request may go
	switch {
	case !mc.chk.on:
		w.t.Errorf("RULE BROKEN: a ticket sent to %s, which does not check approvals", id)
	case mc.onAtStart:
		w.t.Errorf("RULE BROKEN: a ticket sent to %s, which was running when the run started", id)
	case !mc.wakeAccepted:
		w.t.Errorf("RULE BROKEN: a ticket sent to %s, for which the run never had a wake accepted", id)
	case *mc.onAtFirstWake && !mc.knownLimit:
		w.t.Errorf("RULE BROKEN: a ticket sent to %s, which was already running when the run's first wake request went out", id)
	case mc.guests > 0 || mc.guestsWhy != "":
		w.t.Errorf("RULE BROKEN: a ticket sent to %s with guests running or unknown", id)
	}
	if run != w.runID() {
		w.t.Errorf("RULE BROKEN: a ticket for run %q sent to %s; the run is %q", run, id, w.runID())
	}
	// the panel's keeper: one request per machine, whatever comes of it, and none after valid_s
	if w.tk.taken[id] {
		w.t.Errorf("RULE BROKEN: a second ticket request for %s", id)
		return 0, &OpError{Code: CodeTicketGone, Text: "the ticket of the start was handed to it already"}
	}
	w.tk.taken[id] = true
	if w.mono-w.tk.signedAt > time.Duration(w.tk.t.ValidS)*time.Second {
		return 0, &OpError{Code: CodeTicketGone, Text: "the ticket of the start was older than 7 min when it was up"}
	}
	mc.chk.arms++
	if !w.answers(mc) {
		return 0, &OpError{Text: "agent unreachable: connection failed"}
	}
	if !w.hit(mc.info.AgentID) {
		return 0, errBusy
	}
	if l := mc.chk.armLost; l != "" && !mc.busyDone["arm lost"] { // a timeout: the panel does not learn whether the machine took it
		mc.busyDone["arm lost"] = true
		switch l {
		case "arrives":
			w.armNow(mc, run)
		case "late":
			mc.chk.armOnWay, mc.chk.armOnWayRun = w.mono+mc.chk.armLate, run
		}
		return 0, errLost
	}
	if why := w.armNow(mc, run); why != "" {
		return 0, &OpError{Code: CodeRunRefused, Reason: why, Text: "run refused (" + why + ")"}
	}
	return wait, nil
}

func (w *world) runID() string {
	if a, _ := w.m.Snapshot(); a != nil {
		return a.ID
	}
	return ""
}

// runTick is the machine's own second: a ticket that arrives late, the end of the wait, the end of a
// confirmation's time.
func (w *world) runTick(mc *machine) {
	if !mc.chk.on {
		return
	}
	c := w.bootOf(mc)
	if !mc.on {
		return
	}
	if c.armOnWay != 0 && w.mono >= c.armOnWay && w.answers(mc) {
		c.armOnWay = 0
		w.armNow(mc, c.armOnWayRun)
	}
	end := func(state string) {
		c.last, c.run, c.state = &RunBlock{ID: c.run, State: state}, "", ""
	}
	switch {
	case c.state == BlockArmed && w.mono >= c.waitEnds:
		switch {
		case c.helperStopped: // nothing looks; a minute after its time the run is over for good, and the PC stays on
			if w.mono >= c.waitEnds+time.Minute {
				c.helperStopped = false
				end(BlockCancelled)
			}
		case mc.updating && w.mono < c.waitEnds+15*time.Minute:
		case mc.guests > 0 || mc.pending != nil:
			end(BlockCancelled)
		default:
			notice := time.Duration(w.tk.t.NoticeS) * time.Second
			c.state, c.offAt = BlockOffPending, w.now.Add(notice)
			mc.pending = &Pending{Action: "shutdown", At: w.now.Add(notice), DelayS: w.tk.t.NoticeS}
		}
	case c.state == BlockOffPending && mc.pending == nil: // its countdown was stopped on the machine itself
		end(BlockCancelled)
	case c.state == BlockConfirmed && w.mono >= c.withinEnds:
		end(BlockDone)
	}
}

// runCancelled: a cancel reached the machine. It ends the run it is in, whatever its state, and nothing arms
// it again in this boot. (A cancel that finds no run changes nothing.)
func (w *world) runCancelled(mc *machine) {
	if !mc.chk.on {
		return
	}
	if c := w.bootOf(mc); c.state != "" {
		c.last, c.run, c.state, c.closed = &RunBlock{ID: c.run, State: BlockCancelled}, "", "", true
	}
}

// runShown puts what the machine says about runs into an answer.
func (w *world) runShown(mc *machine, o *Observation) {
	if !mc.chk.on {
		return
	}
	c := w.bootOf(mc)
	o.HelperStopped = c.helperStopped
	if c.noRuns {
		return
	}
	o.Runs = true
	switch {
	case c.state != "":
		o.Run = &RunBlock{ID: c.run, State: c.state}
		if c.state != BlockConfirmed {
			o.Run.OffAt = c.offAt
		}
	case c.last != nil:
		b := *c.last
		o.Run = &b
	}
}

// judgeGrant fails the test when an update or a shutdown request goes to a machine with the check that has not
// accepted this run's confirmation (or whose one update the start did not approve), whatever the machine would
// answer.
func (w *world) judgeGrant(mc *machine, what string, grant any, delay time.Duration) {
	g, _ := grant.(*Grant)
	if !mc.chk.on {
		if grant != nil {
			w.t.Errorf("RULE BROKEN: %s sent to %s, which does not check approvals, with a grant", what, mc.info.ID)
		}
		return
	}
	switch {
	case g == nil:
		w.t.Errorf("RULE BROKEN: %s sent to %s, which checks approvals itself, without naming a run", what, mc.info.ID)
	case g.Start && (what != "update" || !mc.chk.startApproved):
		w.t.Errorf("RULE BROKEN: %s sent to %s on the start's approval, which does not cover it", what, mc.info.ID)
	case g.Start:
	case g.Run == w.tk.run() && mc.chk.lied:
		// its agent showed the run as confirmed after a confirmation left for it: the panel goes by that word,
		// and the machine itself refuses what the panel then sends
	case g.Run != w.tk.run() || !mc.chk.goTaken[g.Run]:
		w.t.Errorf("RULE BROKEN: %s sent to %s without a confirmation that machine accepted for this run", what, mc.info.ID)
	case what == "shutdown" && delay != mc.chk.delay && mc.chk.goTaken[g.Run] && mc.chk.delay != 0:
		w.t.Errorf("RULE BROKEN: shutdown sent to %s with a countdown of %s; its confirmation names %s", mc.info.ID, delay, mc.chk.delay)
	}
}

func (t *modelTickets) run() string { return t.runID }

// admit is the machine's own gate for a request that names a run ("": let through, once).
func (w *world) admit(mc *machine, what string, grant any, delay time.Duration) string {
	if !mc.chk.on {
		return ""
	}
	c := w.bootOf(mc)
	g, _ := grant.(*Grant)
	no := "approval required: confirm this action with Pocket ID"
	switch {
	case g == nil:
		return no
	case g.Start:
		if what != "update" || !c.startApproved || c.startUsed {
			return no
		}
		c.startUsed = true
		return ""
	case c.state != BlockConfirmed || c.run != g.Run:
		return no + "; the run it names does not cover it: no run is confirmed on this machine in this boot"
	case what == "update" && c.updates < 1:
		return no + "; the run it names does not cover it: its one update was used"
	case what == "shutdown" && (c.shutdowns < 1 || delay != c.delay):
		return no + "; the run it names does not cover it"
	}
	if what == "update" {
		c.updates = 0
		return ""
	}
	c.last, c.run, c.state = &RunBlock{ID: c.run, State: BlockDone}, "", ""
	return ""
}

// goNow is the machine's own answer to the run's confirmation.
func (w *world) goNow(mc *machine, run, to string, delay, within time.Duration) ConfirmResult {
	c := w.bootOf(mc)
	w.calls = append(w.calls, "go "+mc.info.ID)
	c.gos++
	if c.goTaken[run] || c.goOpen { // one it accepted, or one whose answer the panel never got: no other is sent
		w.t.Errorf("RULE BROKEN: a confirmation request for %s after one it accepted, or one that got no answer", mc.info.ID)
	}
	if !w.answers(mc) {
		return ConfirmResult{Text: "agent unreachable: connection failed"} // nothing left the panel
	}
	accept := func() string {
		switch {
		case c.goSays != "":
			return c.goSays
		case delay < c.minDelay:
			return "limits"
		case c.state == BlockConfirmed && c.run == run:
			return "already_confirmed"
		case c.state != "" && c.run != run:
			return "other_run"
		case c.state == BlockOffPending || (c.state == BlockArmed && w.mono >= c.waitEnds):
			return "too_late"
		case c.state == "" && c.used[run]:
			return "run_over"
		}
		c.run, c.state, c.last, c.delay, c.withinEnds = run, BlockConfirmed, nil, delay, w.mono+within
		c.updates, c.shutdowns = 0, 1
		if to != "-" {
			c.updates = 1
		}
		c.used[run], c.goTaken[run] = true, true
		return ""
	}
	if l := c.goLost; l != "" {
		c.goOpen = true
		if l == "arrives" {
			accept()
		}
		return ConfirmResult{Unanswered: true, Text: "agent unreachable: timeout"}
	}
	if c.goTwice {
		accept()
	}
	if why := accept(); why != "" {
		return ConfirmResult{Sent: true, Reason: why, Text: "run refused (" + why + ")"}
	}
	return ConfirmResult{OK: true}
}

// confirm is the admin's second passkey, as the web package carries it out: the machines that can be confirmed
// are asked (a machine that would refuse says so before the passkey), the run is told that the confirmation
// goes out, each machine gets it once, and the run is told what came of it. It returns the ids it was sent to.
func (w *world) confirm(by string) ([]string, error) {
	id := w.runID()
	ws, err := w.m.Waiting(id)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, h := range ws {
		ids = append(ids, h.ID)
	}
	begun, err := w.m.BeginConfirm(id, by, ids)
	if err != nil {
		return nil, err
	}
	accepted := false
	for _, h := range ws {
		sent := false
		for _, b := range begun {
			sent = sent || b == h.ID
		}
		if !sent {
			continue
		}
		res := w.goNow(w.ms[h.ID], id, h.To, time.Duration(h.ShutdownDelayS)*time.Second, time.Duration(h.WithinS)*time.Second)
		accepted = accepted || res.OK
		w.m.Confirmed(id, h.ID, res)
	}
	for _, h := range ws { // as the web package does: the machines that waited and were not part of it, afterwards
		if !slices.Contains(begun, h.ID) {
			w.m.Confirmed(id, h.ID, ConfirmResult{NotAsked: true, Text: "it was too close to its switch-off"})
		}
	}
	// the judge: a run is confirmed once: after a confirmation a machine accepted, no other is begun
	if w.tk.confirms > 0 {
		w.t.Errorf("RULE BROKEN: a confirmation was begun after one that a machine had accepted")
	}
	if accepted {
		w.tk.confirms++
	}
	return begun, nil
}

// checkedOff adds a machine that is off and checks approvals itself.
func (w *world) checkedOff(id string) *machine {
	mc := w.off(id)
	mc.chk.on, mc.chk.used = true, map[string]bool{}
	return mc
}

// saysItMayGo reports whether a sentence says that the machine may still switch itself off (or that the run
// does not know whether it is off).
func saysItMayGo(text string) bool {
	for _, s := range []string{"may switch itself off", "may have switched itself off", "it switches itself off at", "begins to switch itself off", "the machine switches itself off at",
		"not confirmed that it is off", "It switched itself off again"} {
		if strings.Contains(text, s) {
			return true
		}
	}
	return false
}

// waitFor lets the world run until cond holds, at most d.
func (w *world) waitFor(d time.Duration, what string, cond func(r *Run) bool) *Run {
	w.t.Helper()
	for end := w.now.Add(d); w.now.Before(end); {
		a, hist := w.m.Snapshot()
		if a == nil && len(hist) > 0 {
			a = hist[0]
		}
		if a != nil && cond(a) {
			return a
		}
		w.tick()
	}
	a, hist := w.m.Snapshot()
	if a == nil && len(hist) > 0 {
		a = hist[0]
	}
	w.t.Fatalf("%s: not within %s: %s", what, d, describe(a))
	return nil
}

func stateOf(r *Run, id string) string { return host(r, id).State }

var _ = fmt.Sprintf
