package wakerun

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The steps of one machine, in order:
//
//	queued -> waking -> online -> [awaiting_approval] -> updating -> settling -> shutting_down -> off
//
// A machine that was already on goes queued -> online -> updating -> stayed_on and can never reach settling,
// the only step that leads to the shutdown request (shutdown.go). Every other way out of a step is one of the
// end states, with the sentence that says why.

// opText is an error of a request, as a short text.
func opText(err error) string {
	var oe *OpError
	if errors.As(err, &oe) {
		return oe.Text
	}
	return "the request failed"
}

func opCode(err error) string {
	var oe *OpError
	if errors.As(err, &oe) {
		return oe.Code
	}
	return ""
}

// stepHost does at most the next thing for one machine. The caller holds m.mu.
func (m *Manager) stepHost(ctx context.Context, r *Run, h *Host, now time.Time) {
	if h.verdict != nil { // its steps are over: only the last look is left
		m.look(r, h, now)
		return
	}
	info, ok := m.Ops.Host(h.ID)
	if !ok {
		m.endAsIs(r, h, now, "It is no longer in the panel, so the run let go of it")
		return
	}
	if !info.OptedIn {
		m.endAsIs(r, h, now, "It was switched off for \"Update unpowered hosts\" during the run, so the run let go of it")
		return
	}
	if info.Entry != h.entry { // removed and added again between two steps (Re-pair): what the run knows is about the old entry
		m.endAsIs(r, h, now, "It was paired again during the run (its entry in the panel was replaced), so the run let go of it")
		return
	}
	if r.Cancel != nil && !m.cancelled(ctx, r, h, now) {
		return
	}
	switch h.State {
	case StQueued:
		m.stepQueued(ctx, r, h, info, now)
	case StWaking:
		m.stepWaking(ctx, r, h, info, now)
	case StOnline:
		m.stepOnline(r, h, info, now)
	case StApproval:
		switch {
		case r.Cancel != nil: // cancelled with "switch off again": the shutdown is tried, the agent has the last word
			m.updateDone(r, h, now, UpdNotTried, "the run was cancelled before its update")
		case h.grant != nil:
			m.enter(h, StUpdating, now, "approved: asking its agent to update")
		case now.Sub(h.Since) > approvalTimeout:
			m.endAsIs(r, h, now, fmt.Sprintf("Nobody approved the update and the shutdown within %s", span(approvalTimeout)))
		}
	case StUpdating:
		m.stepUpdating(ctx, r, h, info, now)
	case StSettling:
		m.stepSettling(ctx, r, h, info, now)
	case StShutting:
		m.stepShutting(ctx, r, h, now)
	}
}

// cancelled handles a machine of a cancelled run and reports whether the machine still has steps to do. With
// the default (leave on) it ends where it is, and a countdown the run started is called off while it can
// still be running (callOff). With "switch off again" a machine the run woke goes on to its shutdown without
// anything new being started for it.
func (m *Manager) cancelled(ctx context.Context, r *Run, h *Host, now time.Time) bool {
	c := r.Cancel
	why := "The run was cancelled by " + c.By
	woke := h.Mode == ModeWake && !h.AlreadyOn && h.WakeSentAt != nil
	if c.SwitchOff && woke {
		if !h.cancelHandled {
			h.cancelHandled = true
			m.note(h, now, why+": it is still switched off again, nothing else is started for it")
		}
		return true
	}
	if h.State != StShutting {
		m.endAsIs(r, h, now, why)
		return false
	}
	// the run's own countdown: call it off, so "cancelled" leaves the machine on
	m.callOff(ctx, r, h, now, why)
	return false
}

// callOff asks the machine's agent to call off the countdown the run started, and ends the machine with what
// came of that. why begins the sentence: who or what wanted the machine left on.
//
// It asks only while the run's own countdown can still be running: up to the run's due time on its steady
// clock (dueMono). The agent has one cancel, and it calls off whatever action of the panel is pending on the
// machine. After the run's due time that can no longer be the run's shutdown (it has started, or it did not
// work), but it can be an action a person asked for on the machine's page since: the run's cancel took that
// away, and the report said "its shutdown countdown was called off" (second review of fix7a, R7A-03). So
// after the due time nothing is sent, and the machine ends as it is (endPastDue).
func (m *Manager) callOff(ctx context.Context, r *Run, h *Host, now time.Time, why string) {
	if m.mono() > h.dueMono {
		m.endPastDue(r, h, now, why)
		return
	}
	var err error
	id, by := h.ID, r.By
	m.io(func() { err = m.Ops.CancelShutdown(ctx, id, by) })
	// A cancel that comes when the countdown has run out (the run takes offConfirm to see a machine go) finds a
	// machine that is off, or going: what is written is what the last look sees, not "it is ON" (r2-p2-05).
	due := clock(*h.ShutdownAt)
	if err != nil {
		m.emit(EvStep, r, h, "cancel", nil, opText(err))
		if opCode(err) == CodeNoAnswer {
			// The cancel left and its answer did not come: it may have reached the agent, and then the countdown
			// is called off and the machine stays on. The last look must not take its silence for "off".
			h.cancelUnsure = true
			m.endSeen(r, h, StLeftOn, OnYes, now, fmt.Sprintf("%s, and the request to call its shutdown countdown off got no answer (%s): it may switch off at %s. "+
				"Cancel it on the machine's page.", why, opText(err), due),
				fmt.Sprintf("%s, and the request to call its shutdown countdown (due at %s) off got no answer (%s): it may have arrived.", why, due, opText(err)))
			return
		}
		m.endSeen(r, h, StLeftOn, OnYes, now, fmt.Sprintf("%s, but its shutdown countdown could not be called off (%s): it may switch off at %s. "+
			"Cancel it on the machine's page.", why, opText(err), due),
			fmt.Sprintf("%s, but its shutdown countdown (due at %s) could not be called off (%s).", why, due, opText(err)))
		return
	}
	if m.mono() < h.endMin {
		// Its agent took the cancel before the countdown can have been over: the machine runs, and its countdown
		// is gone. That answer is what the last look goes by; a machine that is out of reach a moment later is
		// not "off". (A cancel answered after the countdown ran out says nothing: the machine is on its way
		// down. What counts is when the answer came, not when the request left: the agent answers a cancel with
		// "ok" also when nothing is pending any more, and a request can be on its way for seconds. "Over" is
		// the run's own record of its request on the steady clock, never the agent's time: see shutDown.)
		h.askedAt = now
	}
	m.emit(EvStep, r, h, "cancel", nil, "ok")
	m.endSeen(r, h, StLeftOn, OnYes, now, why+": its shutdown countdown was called off. "+cameOnline(h)+" and it is ON."+updateTail(h),
		why+": its shutdown countdown (due at "+due+") was asked to be called off."+updateTail(h))
}

// endPastDue ends a machine whose countdown is over by the run's own clock, for a run that was cancelled or
// that the kill switch stopped: no cancel is sent (callOff), and the sentence says neither that a countdown
// runs nor that one was called off. The last look adds what is seen: a machine that is going down, or has
// gone, is reported as that.
func (m *Manager) endPastDue(r *Run, h *Host, now time.Time, why string) {
	lead := fmt.Sprintf("%s after its shutdown countdown had run out (it was due at %s), so the run sent no cancel.", why, when(*h.ShutdownAt, now))
	if h.lostShutdown() { // no countdown the run knows of
		lead = fmt.Sprintf("%s after the time its shutdown would have been due (%s; the request got no answer), so the run sent no cancel.", why, when(*h.ShutdownAt, now))
	}
	m.endSeen(r, h, StLeftOn, OnYes, now, lead+" "+cameOnline(h)+" and it is ON now. Fairy no longer watches it: if the shutdown is still under way, it switches off by itself."+
		updateTail(h), lead+updateTail(h))
}

// alreadyOn handles a machine of the wake list that turns out to be on: it is updated and left on if the
// admin ticked that, otherwise nothing is sent to it. Either way it can no longer be switched off by this run.
func (m *Manager) alreadyOn(r *Run, h *Host, now time.Time, why string) {
	h.AlreadyOn = true
	switch {
	case h.NotFresh && r.UpdateIfOn && r.Cancel == nil:
		m.enter(h, StOnline, now, "answered after the wake but does not look freshly started (it may have been asleep): it is only updated and left on")
	case h.NotFresh:
		m.end(r, h, StLeftOn, OnYes, now, notFreshText(h)+" It was not updated.")
	case r.UpdateIfOn && r.Cancel == nil:
		m.enter(h, StOnline, now, "was already on ("+why+"): it is only updated and stays on")
	case h.WakeSentAt != nil: // a wake packet changes nothing for a machine that is on
		m.end(r, h, StStayedOn, OnYes, now, "It was already on ("+why+"). It was not updated and stays on.")
	default:
		m.end(r, h, StSkipped, OnYes, now, "It was already on ("+why+"). Nothing was sent to it and it stays on.")
	}
}

func (m *Manager) stepQueued(ctx context.Context, r *Run, h *Host, info HostInfo, now time.Time) {
	o := m.Ops.Observe(h.ID)
	if h.Mode == ModeUpdateOnly {
		if !o.Online {
			m.end(r, h, StSkipped, OnUntouched, now, "It was chosen as a machine that is on, and its agent does not answer now. Nothing was sent to it; "+
				"it was not woken.")
			return
		}
		h.AlreadyOn = true
		m.enter(h, StOnline, now, "is on: it is only updated and stays on")
		return
	}
	// The panel's own view first. Off is: shown as offline, and no answer for MinOff, when the run started and
	// now. On is: its agent answers, or something else shows it as on. Everything between is "not sure", and a
	// machine the run is not sure about is left alone: it may be restarting.
	switch {
	case o.Paused:
		m.end(r, h, StSkipped, OnUntouched, now, "Polling is paused for it, so Fairy could not see it come online. Nothing was sent to it.")
		return
	case o.Online:
		m.alreadyOn(r, h, now, "its agent answers")
		return
	case o.OnBy != "":
		h.AlreadyOn = true
		m.end(r, h, StSkipped, OnYes, now, fmt.Sprintf(onByText, o.OnBy))
		return
	case h.StartStatus != "offline" || !offAtStart(h) || !OffNow(o, now):
		m.end(r, h, StSkipped, OnUntouched, now, NotSure)
		return
	}
	if !m.pace.fits(info.RelayID, info.MACs, m.paceNow()) {
		m.say(h, "waiting for its turn: the relay is sent only a few requests a minute")
		return
	}
	// The wake is written down as "going out" first (ask), so that the direct question is the last thing before
	// the packet: nothing but the question's own answer lies between "is it still silent" and the wake.
	if !m.ask(r, h, "wake") {
		return
	}
	// Then the machine itself, now: the panel's view can be a minute old, or the last of a row of failed polls.
	var answer string
	id, by := h.ID, r.By
	m.io(func() { answer = m.Ops.Probe(ctx, id) })
	if answer != "timeout" && answer != "connection failed" {
		m.done(h)       // no wake goes out
		h.askedAt = now // something at its address answered the run itself, just now
	}
	switch answer {
	case "":
		m.alreadyOn(r, h, now, "its agent answered when the run asked it directly, before any wake")
		return
	case "timeout", "connection failed":
		h.ProbedOff = true
	case ProbeRefused: // the machine itself turned the connection down: it runs
		h.AlreadyOn = true
		m.end(r, h, StSkipped, OnYes, now, fmt.Sprintf(onByText, OnByRefused))
		return
	default:
		m.end(r, h, StSkipped, OnUntouched, now, "Fairy could not tell that it is off: something at its address answered ("+answer+"). Nothing was sent to it.")
		return
	}
	if r.Cancel != nil || !m.pace.take(info.RelayID, info.MACs, m.paceNow()) { // cancelled while the machine was being asked
		m.done(h)
		return
	}
	var err error
	before, beforeMono := m.now(), m.mono() // the packet cannot have left before now
	m.io(func() { err = m.Ops.Wake(ctx, id, by) })
	m.done(h)
	if err != nil {
		m.emit(EvStep, r, h, "wake", nil, opText(err))
		if opCode(err) == CodeRateLimited && h.retries < maxRetries {
			h.retries++
			m.say(h, "the relay is busy (its request limit): trying again")
			return
		}
		if opCode(err) == CodeNoAnswer {
			// The request left and the relay's answer did not come: the packet may be out and the machine
			// starting (r2-p2-04). The run does not say "it was not woken" and walk away: it waits for the
			// machine exactly as after a wake that was accepted, counting from before the request left, and
			// goes on asking the relay. A machine that comes online without any wake request for it having
			// been accepted is updated and left on: mayShutDown says no to it.
			h.WakeSentAt, h.lastWake, h.wakeMono, h.WakeUnsure = &before, m.now(), beforeMono, true
			m.enter(h, StWaking, m.now(), "the wake request got no answer ("+opText(err)+"): the wake may have gone out. Waiting for it to come online, up to "+
				span(r.Settings.OnlineTimeout))
			return
		}
		m.end(r, h, StGaveUp, OnUntouched, now, "The wake could not be sent: "+opText(err)+". It was not woken.")
		return
	}
	m.emit(EvStep, r, h, "wake", nil, "ok")
	t := m.now() // after the relay's answer: the machine cannot have booted because of this run before now
	h.WakeSentAt, h.lastWake, h.Wakes, h.wakeMono = &t, t, 1, m.mono()
	m.enter(h, StWaking, t, "wake sent: waiting for it to come online, up to "+span(r.Settings.OnlineTimeout)+" (the run goes on as soon as its agent answers)")
}

func (m *Manager) stepWaking(ctx context.Context, r *Run, h *Host, info HostInfo, now time.Time) {
	o := m.Ops.Observe(h.ID)
	if o.Online && o.SeenAt.After(*h.WakeSentAt) {
		at := o.SeenAt
		h.OnlineAt, h.BootUptimeS = &at, o.UptimeS
		if o.UptimeS != nil && !uptimeUsable(*o.UptimeS) {
			// An uptime that cannot be right (negative, or longer than any machine was ever up) is no uptime: it
			// is kept for the record and for mayShutDown's "no", and not used below (r2-p2-08).
			m.note(h, now, fmt.Sprintf("its agent reports an uptime that cannot be right (%d s): it is not used", *o.UptimeS))
			o.UptimeS = nil
		}
		// How long after the wake: by the two times, and by the steady clock, which a clock that was set
		// forward meanwhile does not stretch. The shorter one counts (the steady one is read now, a moment
		// after the answer, so it is the longer one unless the clock was set).
		h.onlineMono = m.mono()
		since := at.Sub(*h.WakeSentAt)
		if steady := h.onlineMono - h.wakeMono; steady < since {
			since = steady
		}
		secs := since.Seconds()
		h.OnlineAfterS = &secs
		switch {
		case o.UptimeS != nil && time.Duration(*o.UptimeS)*time.Second >= since:
			// Its uptime is longer than the time since the wake, so it did not start after it. It gave
			// nothing at all for 15 minutes and at the direct question, and it answers now, after the wake:
			// it resumed from sleep or hibernate, or is a Windows PC with Fast Startup (all of them go on
			// counting the uptime), or it was on all along and out of reach. The run cannot tell which. It
			// never switches such a machine off, and it does not say "it was already on" either: the wake may
			// well be what brought it back. It is left on and reported as that, a warning (r1-p2-02).
			//
			// This is asked before the minBoot case below: a resume from sleep is quick, and a machine that
			// was asleep for days and answers 4 s after the run's own wake was not "switched on by someone".
			h.NotFresh = true
			m.alreadyOn(r, h, now, "")
		case h.onlineMono-h.wakeMono < minBoot:
			// Its agent answers sooner after the wake than a machine that was off can start (firmware, system,
			// agent, network), and its uptime is fresh (or it reports none): it was already on its way up
			// when the wake went out. Someone switched it on.
			h.NotFresh, h.TooSoon = true, true
			m.alreadyOn(r, h, now, "")
		case o.UptimeS == nil:
			m.enter(h, StOnline, now, "its agent answers, but reports no uptime: Fairy cannot tell that it started after the wake, so it will not switch it off")
		default:
			m.enter(h, StOnline, now, fmt.Sprintf("its agent answers %s after the wake: waiting %s to see that it stays online", span(since), span(onlineSettle)))
			switch {
			case h.lostWake():
				m.note(h, now, lostWakeNote)
			case h.startedBeforeAcceptedWake(): // only the words of the history: mayShutDown decides, later, by itself
				m.note(h, now, lateWakeNote)
			default:
				m.note(h, now, wokenNote) // the report's detail says what "came online after the wake" rests on, and its limit
			}
		}
		return
	}
	// (on either clock: a time of day that was set back during the wait must not make the wait longer, r3-p2-05)
	if now.Sub(*h.WakeSentAt) >= r.Settings.OnlineTimeout || m.mono()-h.wakeMono >= r.Settings.OnlineTimeout {
		lead := fmt.Sprintf("It did not come online within %s after the wake.", span(r.Settings.OnlineTimeout))
		if h.lostWake() {
			lead = fmt.Sprintf("The wake request got no answer, so Fairy does not know whether the wake went out. It did not come online within %s.", span(r.Settings.OnlineTimeout))
		}
		m.end(r, h, StGaveUp, OnUnknown, now, lead+" It may still be starting, or be on without its network "+
			"connection (Tailscale): if it did switch on, it is ON. Fairy has not switched it off and will not.")
		return
	}
	if now.Sub(h.lastKick) >= kickEvery {
		h.lastKick = now
		m.Ops.Kick(h.ID)
	}
	if r.Cancel == nil && now.Sub(h.lastWake) >= wakeResend && m.pace.fits(info.RelayID, info.MACs, m.paceNow()) && m.ask(r, h, "wake") {
		if r.Cancel != nil || !m.pace.take(info.RelayID, info.MACs, m.paceNow()) {
			m.done(h)
			return
		}
		h.lastWake = now
		var err error
		id, by := h.ID, r.By
		m.io(func() { err = m.Ops.Wake(ctx, id, by) })
		m.done(h)
		if err != nil {
			m.emit(EvStep, r, h, "wake", map[string]any{"again": true}, opText(err))
			return
		}
		if h.Wakes == 0 {
			// The first request got no answer, and this is the first one the relay accepted: the first wake the
			// run knows of. Read after the relay's answer, like the time of a first wake that was accepted.
			t := m.now()
			h.WakeOKAt, h.wakeOKMono = &t, m.mono()
		}
		h.Wakes++
		m.dirty = true
		m.emit(EvStep, r, h, "wake", map[string]any{"again": true}, "ok")
	}
}

func (m *Manager) stepOnline(r *Run, h *Host, info HostInfo, now time.Time) {
	o := m.Ops.Observe(h.ID)
	if !o.Online {
		h.goodSince = time.Time{}
		if now.Sub(h.lastKick) >= kickEvery {
			h.lastKick = now
			m.Ops.Kick(h.ID)
		}
		if now.Sub(h.Since) > r.Settings.OnlineTimeout {
			m.endAsIs(r, h, now, "Its agent stopped answering before the update")
		}
		return
	}
	if h.goodSince.IsZero() {
		h.goodSince = now
	}
	if now.Sub(h.goodSince) < onlineSettle {
		return
	}
	if r.Cancel != nil { // cancelled with "switch off again": no update is started any more
		m.updateDone(r, h, now, UpdNotTried, "the run was cancelled before its update")
		return
	}
	if m.Approval != nil && h.grant == nil && m.Approval.Needed(info, o) {
		m.enter(h, StApproval, now, "awake: waiting for your approval of its update and shutdown")
		return
	}
	m.enter(h, StUpdating, now, "online: asking its agent to update")
}

// updateDone records the result of the update step and moves the machine on: a machine that was already on
// ends here, a machine the run woke goes to the pause before its shutdown. A failed update does not keep a
// woken machine on: it was off before, and the old version runs.
func (m *Manager) updateDone(r *Run, h *Host, now time.Time, result, text string) {
	h.Update, h.UpdateText = result, text
	if h.NotFresh {
		m.end(r, h, StLeftOn, OnYes, now, notFreshText(h)+updateTail(h))
		return
	}
	if h.AlreadyOn || h.Mode == ModeUpdateOnly {
		lead := "It was already on"
		if h.Mode == ModeUpdateOnly {
			lead = "It was on"
		}
		m.end(r, h, StStayedOn, OnYes, now, lead+" and stays on."+updateTail(h))
		return
	}
	if result == UpdStuck { // an install may be in flight: switching it off now could leave it half done
		m.end(r, h, StLeftOn, OnYes, now, "Its update did not finish ("+text+"). "+cameOnline(h)+" and was left ON, so an install that may still be running is not cut off.")
		return
	}
	lead := map[string]string{UpdOK: "updated" + toVersion(h), UpdCurrent: "already runs the newest release", UpdNotTried: "not updated"}[result]
	if lead == "" {
		lead = "update failed (" + text + ")"
	}
	m.enter(h, StSettling, now, fmt.Sprintf("%s: waiting %s before the shutdown", lead, span(r.Settings.Settle)))
}

func (m *Manager) stepUpdating(ctx context.Context, r *Run, h *Host, info HostInfo, now time.Time) {
	o := m.Ops.Observe(h.ID)
	if h.UpdateSentAt == nil {
		m.startUpdate(ctx, r, h, info, o, now)
		return
	}
	since := now.Sub(*h.UpdateSentAt)
	if o.Online && o.SeenAt.After(*h.UpdateSentAt) {
		changed := o.Version != "" && h.VersionBefore != "" && o.Version != h.VersionBefore
		if changed && h.versionNewAt.IsZero() {
			h.versionNewAt = now
		}
		switch {
		case o.LastAt > h.lastAtBefore && o.LastOK: // the installer's own verdict, written after its health check
			h.VersionAfter = o.Version
			m.updateDone(r, h, now, UpdOK, "")
			return
		case o.LastAt > h.lastAtBefore:
			m.updateDone(r, h, now, UpdFailed, orText(o.LastMessage, "the installer reported a failure")+"; the version from before runs")
			return
		case changed && now.Sub(h.versionNewAt) >= verdictWait: // the new version answers and the installer never took it back
			h.VersionAfter = o.Version
			m.updateDone(r, h, now, UpdOK, "")
			return
		case changed:
			m.say(h, "the new version answers: waiting for the installer's check")
		case h.UpdateUnsure && (o.UpdateState == "downloading" || o.UpdateState == "applying"):
			m.say(h, "updating: "+orText(o.UpdateMessage, o.UpdateState)) // the request did arrive: wait for its result
		case h.UpdateUnsure && h.updBefore == o.UpdateState+"|"+o.UpdateMessage && o.SeenAt.Sub(*h.UpdateSentAt) < unsureIdleAfter:
			// The request's answer was lost and the agent says what it said before the request: wait a little.
			// An agent that took the request shows it in its next answers.
		case h.UpdateUnsure && h.updBefore == o.UpdateState+"|"+o.UpdateMessage:
			// The agent answers, says what it said before the request, runs no update and has no new result:
			// the request did not arrive, or changed nothing. The run cannot tell which and says so; no
			// install is running. (An agent whose state or message changed did take the request: the cases
			// below read its result as after a request that was answered.)
			m.updateDone(r, h, now, UpdFailed, "the update request got no answer ("+h.UpdateText+"), and its agent reports no update running and no new result")
			return
		case o.UpdateState == "failed":
			m.updateDone(r, h, now, UpdFailed, orText(o.UpdateMessage, "the agent reported a failure"))
			return
		case o.UpdateState == "idle" && o.SeenAt.Sub(*h.UpdateSentAt) >= updateIdleAfter:
			// back to idle without a new result: there was nothing newer to install
			m.updateDone(r, h, now, UpdCurrent, "")
			return
		case o.UpdateMessage != "":
			m.say(h, "updating: "+o.UpdateMessage)
		}
	}
	if since > updateTimeout {
		text := fmt.Sprintf("no result %s after it was asked for", span(updateTimeout))
		if h.UpdateUnsure { // the run could not find out, and does not guess
			text = fmt.Sprintf("not confirmed: the update request got no answer (%s), and its agent did not say how it ended within %s", h.UpdateText, span(updateTimeout))
		}
		m.updateDone(r, h, now, UpdStuck, text)
	}
}

func orText(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// startUpdate sends the update request, unless the machine does not need one or cannot take one.
func (m *Manager) startUpdate(ctx context.Context, r *Run, h *Host, info HostInfo, o Observation, now time.Time) {
	if r.Cancel != nil { // cancelled with "switch off again": no update is started any more
		m.updateDone(r, h, now, UpdNotTried, "the run was cancelled before its update")
		return
	}
	waited := now.Sub(h.Since) > updateStart
	if !o.Online {
		if waited {
			m.updateDone(r, h, now, UpdFailed, "its agent stopped answering before the update could be asked for")
		}
		return
	}
	h.VersionBefore = o.Version
	switch {
	case !o.UpdateEnabled:
		m.updateDone(r, h, now, UpdFailed, "self-update is not enabled on its agent")
		return
	case o.UpdateState == "unavailable": // on Windows the helper may still be starting after the boot
		if waited {
			m.updateDone(r, h, now, UpdFailed, orText(o.UpdateMessage, "its agent cannot update itself right now"))
		} else {
			m.say(h, "its agent cannot update yet ("+orText(o.UpdateMessage, "starting")+"): waiting")
		}
		return
	case o.UpdateState == "downloading" || o.UpdateState == "applying": // someone else started one: watch that one
		h.lastAtBefore = o.LastAt
		t := now
		h.UpdateSentAt = &t
		m.note(h, now, "an update was already running on it: waiting for its result")
		m.say(h, "updating: "+orText(o.UpdateMessage, o.UpdateState))
		return
	case o.Behind != nil && !*o.Behind:
		m.updateDone(r, h, now, UpdCurrent, "")
		return
	}
	if !m.pace.fits(info.AgentID, 1, m.paceNow()) || !m.ask(r, h, "update") { // written down before it goes out
		return
	}
	if r.Cancel != nil || !m.pace.take(info.AgentID, 1, m.paceNow()) { // cancelled while the journal was written
		m.done(h)
		return
	}
	h.lastAtBefore = o.LastAt
	var err error
	id, by, grant := h.ID, r.By, h.grant // read under the lock; the request goes out without it
	m.io(func() { err = m.Ops.Update(ctx, id, by, grant) })
	m.done(h)
	params := map[string]any{"from": o.Version, "latest": r.Latest}
	if err != nil {
		m.emit(EvStep, r, h, "update", params, opText(err))
		switch {
		case opCode(err) == CodeApproval:
			m.updateDone(r, h, now, UpdApproval, "its agent asks for an approval (Pocket ID) before an update, and this run has none for it")
		case opCode(err) == CodeRateLimited && h.retries < maxRetries:
			h.retries++
			m.say(h, "its agent is busy (its request limit): trying again")
		case opCode(err) == CodeNoAnswer:
			// The request left and its answer did not come: the agent may be installing right now (r2-p2-03).
			// That is not "the update failed". From here the agent's own word counts: the run watches its
			// update state like after a request that was accepted, counted from before the request left.
			h.UpdateSentAt, h.UpdateUnsure, h.UpdateText = &now, true, opText(err)
			h.updBefore = o.UpdateState + "|" + o.UpdateMessage
			m.note(h, m.now(), "the update request got no answer ("+opText(err)+"): it may have arrived. Watching what its agent reports, up to "+span(updateTimeout))
			m.say(h, "update asked for, no answer: watching what its agent reports")
		default:
			m.updateDone(r, h, now, UpdFailed, opText(err))
		}
		return
	}
	m.emit(EvStep, r, h, "update", params, "ok")
	t := m.now()
	h.UpdateSentAt = &t
	m.note(h, t, "update asked for (it runs "+orText(o.Version, "an unknown version")+"): waiting for its result, up to "+span(updateTimeout))
	m.say(h, "updating: the agent downloads, checks and installs the newest signed release")
}

// stepSettling is the pause after the update ("about a minute"), then the shutdown.
func (m *Manager) stepSettling(ctx context.Context, r *Run, h *Host, info HostInfo, now time.Time) {
	if r.Cancel == nil && now.Sub(h.Since) < r.Settings.Settle {
		return
	}
	o := m.Ops.Observe(h.ID)
	if !o.Online { // the agent restarts after an update
		if now.Sub(h.Since) > r.Settings.Settle+agentBack {
			lead := "Its agent did not answer again after the update, so Fairy could not ask it to shut down. " + cameOnline(h)
			m.endSeen(r, h, StLeftOn, OnYes, now, lead+" and it is ON."+updateTail(h), lead+"."+updateTail(h))
		}
		return
	}
	m.shutDown(ctx, r, h, info, o, now)
}

// stepShutting watches the countdown the run started until the machine is gone.
//
// The countdown is timed by the run's own record, on the panel's steady clock (Manager.mono): the request was
// answered at a moment the run read itself, and it asked for a delay it knows. dueMono is that moment plus the
// delay: the latest the countdown can end. The agent also says when its countdown ends ("at", in its answer and
// in every poll), but by the machine's clock, and a machine that was off for weeks has a clock that is behind,
// or ahead, until it is set; the panel's own time of day can be set too. So the agent's "at" is never compared
// with a clock of the panel (audit 5, r3-p2-01: with the machine's clock ten minutes behind, the run wrote "the
// shutdown did not work. It is ON." one second into the countdown, and the machine went off two minutes later
// with nobody told). What the agent reports is used for one thing: whether it still shows the action.
func (m *Manager) stepShutting(ctx context.Context, r *Run, h *Host, now time.Time) {
	o := m.Ops.Observe(h.ID)
	mono := m.mono()
	// counting: an answer from after the request, in which its agent shows the countdown
	counting := o.Online && o.SeenAt.After(h.shutdownSent) && o.Pending != nil && o.Pending.Action == "shutdown"
	if counting {
		h.pendingSeen = true
	}
	// The guests of a Proxmox node were asked about before the shutdown request (mayShutDown). One that starts
	// on the node, or arrives on it, while the countdown runs went unnoticed, and the node went off under it
	// (second review, N2). So the run asks again every few seconds while its own countdown runs, and calls the
	// countdown off when a guest runs: the node is then left on and reported. It never asks for anything else
	// here. "Cannot see the guests" counts when it is said twice in a row: one failed poll of the Proxmox API in
	// two minutes must not keep a node on. The last guestsMargin of the countdown are left out: a cancel needs
	// its time. Both are counted on the run's own clock, and the question does not wait for the agent's poll to
	// show the countdown (r3-p2-02: a node whose clock was two minutes behind was never asked about, and went
	// off under a guest that had started ninety seconds before).
	if mono < h.dueMono-guestsMargin && (!h.guestsAsked || mono-h.guestsMono >= guestsEvery) {
		h.guestsAsked, h.guestsMono = true, mono
		running, known, names := m.Ops.Guests(h.ID, deref(h.OnlineAt))
		switch why := guestsKeepOn(running, known, names); {
		case why == "":
			h.guestsUnseen = false
		case !known && !h.guestsUnseen:
			h.guestsUnseen = true
		default:
			m.callOff(ctx, r, h, now, "Left ON: "+why+" since its shutdown was asked for")
			return
		}
	}
	// The poller is asked to poll the machine every few seconds while the countdown is watched: a machine that
	// is polled once a minute, or one whose failed polls were put off, would otherwise not be looked at when
	// the countdown ends.
	if h.kickMono == 0 || mono-h.kickMono >= kickEvery {
		h.kickMono = mono
		m.Ops.Kick(h.ID)
	}
	h.seeSilence(o, mono)
	// lost: all the run knows of its shutdown is a request that got no answer
	lost := h.lostShutdown()
	asked := woken(h) + ", " + updateWords(h) + ", and asked to switch off. "
	if o.Paused {
		// Polling is paused for it: what the run reads is the last poll before the pause, for ever. That poll
		// says nothing about the machine after its countdown, whatever it was (r3-p2-04: one failed poll, then
		// the pause, was "switched off again" for a machine that was on). The run waits for the countdown, in
		// case polling is taken up again, and then says that it could not look.
		if mono-h.dueMono > offConfirm {
			m.end(r, h, StLeftOn, OnUnknown, now, asked+"Polling is paused for it, so Fairy could not see whether it went off: not confirmed that it is off.")
		}
		return
	}
	if !o.Online && o.OnBy == "" { // nothing at all from it
		if !h.offQuiet {
			h.offQuiet, h.offSince = true, mono
		}
		switch {
		case lost && mono > h.dueMono && mono-h.offSince >= offConfirm:
			// Silence is also what a dead link looks like, and a dead link is the usual reason an answer is lost:
			// the request may never have arrived, and the machine may be on and out of reach. The run saw no
			// countdown, so it does not say "off" (second review, P5). Left on as far as the run knows, power
			// not known, and named in the notification.
			m.end(r, h, StLeftOn, OnUnknown, now, cameOnline(h)+". Its shutdown request got no answer, Fairy never saw its countdown, and it does not answer now: "+
				"not confirmed that it is off."+updateTail(h))
		case lost:
		case h.seenOff(mono):
			// Off: after the run's own due time the machine gave nothing at all for offConfirm, as seen by at
			// least offPolls polls in a row that were made after that time, and it had not gone out of reach
			// while its countdown was still running (seeSilence).
			m.end(r, h, StOff, OnNo, now, woken(h)+", "+updateWords(h)+", and switched off again.")
		case mono-h.dueMono <= offGrace:
		case h.cutEarly:
			m.end(r, h, StLeftOn, OnUnknown, now, asked+"It was already out of reach before its countdown ended, and it does not answer now: Fairy cannot tell "+
				"a machine that went off from a connection that dropped. Not confirmed that it is off.")
		default: // the panel's polls of it did not run, or not in a row
			m.end(r, h, StLeftOn, OnUnknown, now, asked+"It does not answer now, but the panel could not ask it often enough after its countdown: not confirmed that it is off.")
		}
		return
	}
	h.offQuiet = false
	if !o.Online {
		// Its agent does not answer, and something else shows that the machine runs (it refuses the connection:
		// its agent stopped, the system hangs in its shutdown). That is a machine that is on, here as everywhere
		// else (r2-p2-05): it is never reported as "switched off".
		if mono-h.dueMono > offGrace {
			m.end(r, h, StLeftOn, OnYes, now, fmt.Sprintf("It still shows as on %s after its shutdown was due (%s), and its agent does not answer: the shutdown did not finish. It is ON.",
				span(offGrace), o.OnBy)+updateTail(h))
		}
		return
	}
	// An answer from after the request, well before the countdown can end, that has no countdown in it: it was
	// cancelled. One such answer can come from a poll that was already on its way when the shutdown was asked
	// for, so it counts when the run has seen the countdown before, or with the next answer (a cancel within
	// a second of the request is never seen as pending at all).
	gone := o.Pending == nil && o.SeenAt.After(h.shutdownSent) && mono < h.endMin-3*time.Second
	if gone && !h.pendingSeen && h.noPendingAt.IsZero() {
		h.noPendingAt = o.SeenAt
	}
	switch {
	case gone && lost && o.SeenAt.After(h.noPendingAt):
		m.end(r, h, StLeftOn, OnYes, now, "The shutdown request got no answer, and its agent shows no countdown: the request did not arrive. It is ON."+updateTail(h))
	case gone && (h.pendingSeen || o.SeenAt.After(h.noPendingAt)):
		m.end(r, h, StLeftOn, OnYes, now, "Its shutdown was cancelled, in the panel or on the machine, so it is ON."+updateTail(h))
	case mono-h.dueMono > offGrace && counting:
		// The run's own countdown is long over, and its agent still shows a shutdown as pending: the agent
		// counts another, longer one (an agent that took the request with a delay of its own; one that rounds
		// up is covered by offGrace). The run asked for a countdown of Settings.ShutdownDelay and watched that
		// one: no agent decides how long the one run slot is taken (r1-p2-07). The machine is on now; whether
		// and when it goes off is the agent's word, and the report says exactly that. The time is the agent's,
		// by the machine's clock, and is only repeated here.
		m.end(r, h, StLeftOn, OnYes, now, fmt.Sprintf("It is still on %s after the shutdown the run asked for was due, and its agent still shows a shutdown as pending "+
			"(it gives %s as its end, by the machine's own clock). Fairy no longer watches it: "+
			"it is ON until then, and switches off then unless someone cancels that on its page.", span(offGrace), when(o.Pending.At, now))+updateTail(h))
	case mono-h.dueMono > offGrace:
		m.end(r, h, StLeftOn, OnYes, now, fmt.Sprintf("It is still on %s after its shutdown was due: the shutdown did not work. It is ON.", span(offGrace))+updateTail(h))
	}
}

// seeSilence reads the poller's row of silent polls for a machine whose countdown the run watches, and keeps
// what "off" is proven with (seenOff). It is called at every look at such a machine.
//
// "Off" used to be: no answer at the run's look, since 20 s, once the countdown was over. The 20 s could lie
// before the end of the countdown (a link that dropped 20 s before it: "off" one second after it), they could
// be two failed polls that were both made before the end, and they could be one failed poll of a machine whose
// polling was then paused, read again and again (audit 5, r3-p2-04). In each case a machine whose shutdown did
// not happen was reported as "switched off again".
//
// Now the silence must be seen by polls that were made after the run's own due time (dueMono): each poll of
// the poller's row is placed on the run's steady clock, and only one that began after dueMono counts. The row
// must be unbroken: an answer, a sign of life, paused polling or a time the panel did not watch ends it and
// the count starts again.
//
// And a machine that was out of reach while its countdown was still running proves nothing by its silence
// afterwards, however long that gets: a machine that counts down answers; this one did not, and what comes
// after is the same silence (cutEarly). The poller says since when it has had nothing at all from the machine
// (Silence.SinceAgo): when that began before endMin, the earliest the countdown can end, cutEarly is set,
// whichever polls the run's own looks happened to see (second review of fix7a, R7A-02: a look that came late
// saw one row of two polls and never the first of them). cutEarly then holds until the machine answers or
// shows a sign of life, and nothing else takes it back: not a new row of the poller, not a look without a row,
// not a pause. Those only start the count again. (R7A-01: the poller starts a new row whenever the panel was
// not watching, also when the time of day was set back by two seconds; the run took the new row for a new
// silence and wrote "switched off again" for a machine that was on and had only been out of reach.)
func (h *Host) seeSilence(o Observation, mono time.Duration) {
	s := o.Silent
	if o.Online || o.OnBy != "" { // it answers, or shows that it runs: only this ends "out of reach"
		h.silRow, h.silN, h.silAfter, h.cutEarly = 0, 0, 0, false
		return
	}
	if o.Paused || s.N == 0 { // nothing was asked, or the poller has no row: the count starts again
		h.silRow, h.silN, h.silAfter = 0, 0, 0
		return
	}
	if mono-s.SinceAgo < h.endMin { // nothing at all from it since before its countdown can have ended
		h.cutEarly = true
	}
	if s.Row != h.silRow { // another row than at the last look: the count starts again
		h.silRow, h.silN, h.silAfter = s.Row, 0, 0
	}
	if s.N == h.silN { // no new poll since the last look
		return
	}
	h.silN = s.N
	switch began, ended := mono-s.BeganAgo, mono-s.EndedAgo; {
	case ended < h.endMin:
		h.cutEarly = true
	case began > h.dueMono:
		if h.silAfter == 0 {
			h.silSince = began
		}
		h.silAfter++ // one per look, also when several polls ran since the last one
	}
}

// seenOff reports whether the silence the run has seen proves that the machine went off after its countdown
// (seeSilence): at least offPolls polls in a row, all made after the run's own due time, the first of them
// offConfirm ago, and the machine was not out of reach before the countdown can have ended (or has answered
// again since).
func (h *Host) seenOff(mono time.Duration) bool {
	return !h.cutEarly && h.silAfter >= offPolls && mono-h.silSince >= offConfirm
}

// updateWords is the update's result inside the sentence of a machine that was switched off again.
func updateWords(h *Host) string {
	switch h.Update {
	case UpdOK:
		if h.VersionBefore != "" && h.VersionAfter != "" {
			return "updated from " + h.VersionBefore + " to " + h.VersionAfter
		}
		return "updated" + toVersion(h)
	case UpdCurrent:
		return "already on the newest release" + map[bool]string{true: " (" + h.VersionBefore + ")"}[h.VersionBefore != ""]
	case UpdNotTried:
		return "not updated (" + h.UpdateText + ")"
	}
	return "NOT updated (" + h.UpdateText + ")"
}
