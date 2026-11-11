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
	info, ok := m.Ops.Host(h.ID)
	if !ok {
		m.endAsIs(r, h, now, "It is no longer in the panel, so the run let go of it")
		return
	}
	if !info.OptedIn {
		m.endAsIs(r, h, now, "It was switched off for \"Update machines that are off\" during the run, so the run let go of it")
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
		m.stepShutting(r, h, now)
	}
}

// cancelled handles a machine of a cancelled run and reports whether the machine still has steps to do. With
// the default (leave on) it ends where it is, and a countdown the run started is called off. With "switch off
// again" a machine the run woke goes on to its shutdown without anything new being started for it.
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
	var err error
	id, by := h.ID, r.By
	m.io(func() { err = m.Ops.CancelShutdown(ctx, id, by) })
	if err != nil {
		m.emit(EvStep, r, h, "cancel", nil, opText(err))
		m.end(r, h, StLeftOn, OnYes, now, fmt.Sprintf("%s, but its shutdown countdown could not be called off (%s): it may switch off at %s. "+
			"Cancel it on the machine's page.", why, opText(err), clock(*h.ShutdownAt)))
		return false
	}
	m.emit(EvStep, r, h, "cancel", nil, "ok")
	m.end(r, h, StLeftOn, OnYes, now, why+": its shutdown countdown was called off. The run woke it and it is ON."+updateTail(h))
	return false
}

// alreadyOn handles a machine of the wake list that turns out to be on: it is updated and left on if the
// admin ticked that, otherwise nothing is sent to it. Either way it can no longer be switched off by this run.
func (m *Manager) alreadyOn(r *Run, h *Host, now time.Time, why string) {
	h.AlreadyOn = true
	switch {
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
	if !m.pace.fits(info.RelayID, info.MACs, now) {
		m.say(h, "waiting for its turn: the relay is sent only a few requests a minute")
		return
	}
	// Then the machine itself, now: the panel's view can be a minute old, or the last of a row of failed polls.
	var answer string
	id, by := h.ID, r.By
	m.io(func() { answer = m.Ops.Probe(ctx, id) })
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
	if r.Cancel != nil { // cancelled while the machine was being asked
		return
	}
	if !m.pace.take(info.RelayID, info.MACs, now) {
		return
	}
	var err error
	m.io(func() { err = m.Ops.Wake(ctx, id, by) })
	if err != nil {
		m.emit(EvStep, r, h, "wake", nil, opText(err))
		if opCode(err) == CodeRateLimited && h.retries < maxRetries {
			h.retries++
			m.say(h, "the relay is busy (its request limit): trying again")
			return
		}
		m.end(r, h, StGaveUp, OnUntouched, now, "The wake could not be sent: "+opText(err)+". It was not woken.")
		return
	}
	m.emit(EvStep, r, h, "wake", nil, "ok")
	t := m.now() // after the relay's answer: the machine cannot have booted because of this run before now
	h.WakeSentAt, h.lastWake, h.Wakes = &t, t, 1
	m.enter(h, StWaking, t, "wake sent: waiting for it to come online, up to "+span(r.Settings.OnlineTimeout)+" (the run goes on as soon as its agent answers)")
}

func (m *Manager) stepWaking(ctx context.Context, r *Run, h *Host, info HostInfo, now time.Time) {
	o := m.Ops.Observe(h.ID)
	if o.Online && o.SeenAt.After(*h.WakeSentAt) {
		at := o.SeenAt
		h.OnlineAt, h.BootUptimeS = &at, o.UptimeS
		since := at.Sub(*h.WakeSentAt)
		switch {
		case o.UptimeS == nil:
			m.enter(h, StOnline, now, "its agent answers, but reports no uptime: Fairy cannot tell that it started after the wake, so it will not switch it off")
		case time.Duration(*o.UptimeS)*time.Second >= since:
			// It has been running for longer than since the wake: it was on all along (the panel could not
			// reach it), or someone switched it on before the run did.
			m.alreadyOn(r, h, now, fmt.Sprintf("it has been up for %s, longer than the %s since the wake", span(time.Duration(*o.UptimeS)*time.Second), span(since)))
		default:
			m.enter(h, StOnline, now, fmt.Sprintf("its agent answers %s after the wake: waiting %s to see that it stays online", span(since), span(onlineSettle)))
		}
		return
	}
	if now.Sub(*h.WakeSentAt) >= r.Settings.OnlineTimeout {
		m.end(r, h, StGaveUp, OnUnknown, now, fmt.Sprintf("It did not come online within %s after the wake. It may still be starting, or be on without its network "+
			"connection (Tailscale): if it did switch on, it is ON. Fairy has not switched it off and will not.", span(r.Settings.OnlineTimeout)))
		return
	}
	if now.Sub(h.lastKick) >= kickEvery {
		h.lastKick = now
		m.Ops.Kick(h.ID)
	}
	if r.Cancel == nil && now.Sub(h.lastWake) >= wakeResend && m.pace.take(info.RelayID, info.MACs, now) {
		h.lastWake = now
		var err error
		id, by := h.ID, r.By
		m.io(func() { err = m.Ops.Wake(ctx, id, by) })
		if err != nil {
			m.emit(EvStep, r, h, "wake", map[string]any{"again": true}, opText(err))
			return
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
	if h.AlreadyOn || h.Mode == ModeUpdateOnly {
		lead := "It was already on"
		if h.Mode == ModeUpdateOnly {
			lead = "It was on"
		}
		m.end(r, h, StStayedOn, OnYes, now, lead+" and stays on."+updateTail(h))
		return
	}
	if result == UpdStuck { // an install may be in flight: switching it off now could leave it half done
		m.end(r, h, StLeftOn, OnYes, now, "Its update did not finish ("+text+"). The run woke it and left it ON, so an install that may still be running is not cut off.")
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
		m.updateDone(r, h, now, UpdStuck, fmt.Sprintf("no result %s after it was asked for", span(updateTimeout)))
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
	if !m.pace.take(info.AgentID, 1, now) {
		return
	}
	h.lastAtBefore = o.LastAt
	var err error
	id, by, grant := h.ID, r.By, h.grant // read under the lock; the request goes out without it
	m.io(func() { err = m.Ops.Update(ctx, id, by, grant) })
	params := map[string]any{"from": o.Version, "latest": r.Latest}
	if err != nil {
		m.emit(EvStep, r, h, "update", params, opText(err))
		switch {
		case opCode(err) == CodeApproval:
			m.updateDone(r, h, now, UpdApproval, "its agent asks for an approval (Pocket ID) before an update, and this run has none for it")
		case opCode(err) == CodeRateLimited && h.retries < maxRetries:
			h.retries++
			m.say(h, "its agent is busy (its request limit): trying again")
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
			m.end(r, h, StLeftOn, OnYes, now, "Its agent did not answer again after the update, so Fairy could not ask it to shut down. The run woke it and it is "+
				"probably still ON."+updateTail(h))
		}
		return
	}
	m.shutDown(ctx, r, h, info, o, now)
}

// stepShutting watches the countdown the run started until the machine is gone.
func (m *Manager) stepShutting(r *Run, h *Host, now time.Time) {
	o := m.Ops.Observe(h.ID)
	due := *h.ShutdownAt
	if o.Online && o.SeenAt.After(h.shutdownSent) && o.Pending != nil && o.Pending.Action == "shutdown" {
		if !h.pendingSeen || !o.Pending.At.Equal(due) { // the agent's own time for it (Linux rounds up to minutes)
			h.pendingSeen, h.ShutdownAt, due = true, &o.Pending.At, o.Pending.At
			m.dirty = true
		}
	}
	if !o.Online {
		if h.offSince.IsZero() {
			h.offSince = now
		}
		if now.After(due) && now.Sub(h.offSince) >= offConfirm {
			m.end(r, h, StOff, OnNo, now, "Woken, "+updateWords(h)+", and switched off again.")
		}
		return
	}
	h.offSince = time.Time{}
	// An answer from after the request, well before the countdown ends, that has no countdown in it: it was
	// cancelled. One such answer can come from a poll that was already on its way when the shutdown was asked
	// for, so it counts when the run has seen the countdown before, or with the next answer (a cancel within
	// a second of the request is never seen as pending at all).
	gone := o.Pending == nil && o.SeenAt.After(h.shutdownSent) && o.SeenAt.Before(due.Add(-3*time.Second))
	if gone && !h.pendingSeen && h.noPendingAt.IsZero() {
		h.noPendingAt = o.SeenAt
	}
	switch {
	case gone && (h.pendingSeen || o.SeenAt.After(h.noPendingAt)):
		m.end(r, h, StLeftOn, OnYes, now, "Its shutdown was cancelled, in the panel or on the machine, so it is ON."+updateTail(h))
	case now.Sub(due) > offGrace:
		m.end(r, h, StLeftOn, OnYes, now, fmt.Sprintf("It is still on %s after its shutdown was due: the shutdown did not work. It is ON.", span(offGrace))+updateTail(h))
	}
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
