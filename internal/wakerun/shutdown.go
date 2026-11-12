package wakerun

import (
	"context"
	"fmt"
	"time"
)

// The rule of this package: a run only ever switches off a machine that was off before the run and that this
// run woke. It is kept here, in one function, and nowhere else. shutDown below is the only code that calls Ops.Shutdown; it calls
// mayShutDown first and sends nothing unless that says yes; and shutDown has one caller, the end of the pause
// after the update (stepSettling). A machine that was on never gets that far (updateDone ends it), but
// mayShutDown does not rely on that: it reads the evidence itself, every time.
//
// The evidence is collected by the run while it works and is in the Host record (and so in the journal, for
// the report after a restart; a run is never continued from it):
//
//	Mode         the admin chose it as a machine that is off
//	StartStatus  "offline" when the run started: its agent had not answered for at least offline_after
//	QuietForS    how long the panel had asked it without an answer when the run started: at least MinOff
//	             (15 minutes). A machine that restarts is quiet for a few minutes and then comes back by
//	             itself with a fresh uptime; without this it would look like a machine the run woke.
//	ProbedOff    asked directly right before the wake, and nothing answered (the panel's view can be stale)
//	WakeSentAt   this run sent a wake for it, and the relay accepted it (Wakes). Or the wake request left
//	             and got no answer (WakeUnsure): the packet may have gone out, and WakeSentAt is the moment
//	             before the request left, the earliest the packet can have been sent.
//	OnlineAt     its first answered poll after that wake
//	BootUptimeS  the uptime it reported in that poll: shorter than the time since the wake, so the machine's
//	             system started after the wake. A machine that was on all along and only unreachable has been
//	             up for longer; so has one that slept (NotFresh).
//
//	             What this does NOT show (audit 5, r1-p2-01, a limit of the design): that the wake is what
//	             started it. Uptime counts from the start of the operating system, not from the power button.
//	             A machine that a person switches on shortly before the run's wake, and that is still in its
//	             firmware or boot loader when the wake goes out (seconds on a desktop, minutes on a server,
//	             without limit at a boot menu or a disk password), does not answer the direct question, starts
//	             its system after the wake and reports a fresh uptime. So does one that starts by a firmware
//	             schedule or when the power returns. Every line of mayShutDown is then true, and the run
//	             switches it off after its update. The panel cannot close this: it would need the agent to
//	             say why the machine started. Only one corner is cut off: a machine that answers sooner after
//	             the wake than any machine that was off can (minBoot, TooSoon) was already starting.
//	wakeMono, onlineMono (in memory only): the same two moments on the panel's steady clock. The uptime
//	             must also be shorter than their difference, so that a clock that was set forward between
//	             the wake and the answer cannot make the time since the wake longer than it was.
//	AlreadyOn    set as soon as anything says it was on before the run acted: then never

// mayShutDown reports whether the run may ask this machine to shut down, and if not, why (a sentence for the
// report). o is the poller's view now; guests is what the panel knows about the guests of a Proxmox node.
func mayShutDown(h *Host, info HostInfo, o Observation, guests func() (running int, known bool, why string)) (bool, string) {
	switch {
	case h.Mode != ModeWake:
		return false, "it was chosen as a machine that is on: Fairy only updates those"
	case h.NotFresh:
		return false, "it answered after the wake without looking freshly started"
	case h.AlreadyOn:
		return false, "it was already on before this run did anything"
	case h.StartStatus != "offline":
		return false, "the panel did not show it as off when the run started"
	case !offAtStart(h):
		return false, "Fairy is not sure that it was off before the run: it had not gone " + span(MinOff) + " without an answer"
	case !h.ProbedOff:
		return false, "the run did not confirm that it was off right before the wake"
	case h.WakeSentAt == nil || (h.Wakes < 1 && !h.WakeUnsure):
		return false, "this run sent no wake for it"
	case h.OnlineAt == nil || !h.OnlineAt.After(*h.WakeSentAt):
		return false, "it was not seen coming online after this run's wake"
	case h.BootUptimeS == nil:
		return false, "its agent reports no uptime, so Fairy cannot tell that it started after the wake"
	case *h.BootUptimeS < 0 || time.Duration(*h.BootUptimeS)*time.Second >= h.OnlineAt.Sub(*h.WakeSentAt):
		return false, "it had been running for longer than since the wake: it was already on"
	case time.Duration(*h.BootUptimeS)*time.Second >= h.onlineMono-h.wakeMono: // the same by the steady clock
		return false, "it had been running for longer than since the wake: it was already on"
	case !info.OptedIn:
		return false, "it is no longer switched on for \"Update unpowered hosts\""
	case !o.Online:
		return false, "its agent is not answering right now"
	case o.Pending != nil:
		return false, fmt.Sprintf("another power action is already pending on it (%s at %s)", o.Pending.Action, clock(o.Pending.At))
	case o.UpdateState == "downloading" || o.UpdateState == "applying":
		// Whatever the run believes about its own update request (its answer may have been lost, or someone
		// else started an update): a machine whose agent says that it is installing is not switched off.
		return false, "its agent says that an update is being installed on it right now (" + o.UpdateState + ")"
	}
	// A Proxmox node is left on while guests run on it, and also when the panel cannot see its guests.
	switch running, known, why := guests(); {
	case !known:
		return false, "Fairy cannot see whether guests run on it (" + why + ")"
	case running == 1:
		return false, "1 guest is running on it" + guestNames(why)
	case running > 1:
		return false, fmt.Sprintf("%d guests are running on it", running) + guestNames(why)
	}
	return true, ""
}

// offAtStart: the panel had asked the machine without an answer for at least MinOff when the run started.
func offAtStart(h *Host) bool {
	return h.QuietForS != nil && time.Duration(*h.QuietForS)*time.Second >= MinOff
}

func guestNames(names string) string {
	if names == "" {
		return ""
	}
	return " (" + names + ")"
}

// shutDown asks a machine the run woke to shut down, with the delay that can be cancelled in the panel, or
// leaves it on and says why. Caller: stepSettling only.
func (m *Manager) shutDown(ctx context.Context, r *Run, h *Host, info HostInfo, o Observation, now time.Time) {
	ok, why := mayShutDown(h, info, o, func() (int, bool, string) { return m.Ops.Guests(h.ID, deref(h.OnlineAt)) })
	if !ok {
		state := StLeftOn
		if h.AlreadyOn || h.Mode == ModeUpdateOnly {
			state = StStayedOn
		}
		m.end(r, h, state, OnYes, now, "Left ON: "+why+"."+updateTail(h))
		return
	}
	if !m.pace.fits(info.AgentID, 1, now) || !m.ask(r, h, "shutdown") { // written down before it goes out
		return
	}
	if !m.pace.take(info.AgentID, 1, now) {
		m.done(h)
		return
	}
	delay := r.Settings.ShutdownDelay
	var at time.Time
	var err error
	id, by, grant := h.ID, r.By, h.grant // read under the lock; the request goes out without it
	m.io(func() { at, err = m.Ops.Shutdown(ctx, id, delay, by, grant) })
	m.done(h)
	params := map[string]any{"delay_s": int(delay.Seconds())}
	if err != nil {
		m.emit(EvStep, r, h, "shutdown", params, opText(err))
		switch {
		case opCode(err) == CodeApproval:
			m.end(r, h, StLeftOn, OnYes, now, "Left ON: its agent asks for an approval (Pocket ID) before a shutdown, and this run has none for it."+updateTail(h))
		case opCode(err) == CodeNoAgentRoute: // the route the run confirmed at its start is gone: nothing was sent
			m.end(r, h, StLeftOn, OnYes, now, NoAgentRoute+updateTail(h))
		case opCode(err) == CodeRateLimited && h.retries < maxRetries:
			h.retries++
			m.say(h, "its agent is busy (its request limit): trying the shutdown again")
		default:
			m.end(r, h, StLeftOn, OnYes, now, "Left ON: the shutdown was not accepted ("+opText(err)+")."+updateTail(h))
		}
		return
	}
	m.emit(EvStep, r, h, "shutdown", params, "ok")
	sent := m.now()
	if at.IsZero() {
		at = sent.Add(delay)
	}
	h.ShutdownAt, h.shutdownSent = &at, sent
	if m.farCountdown(r, h, at, sent) { // the agent took the request with a countdown of its own choosing
		return
	}
	m.enter(h, StShutting, sent, fmt.Sprintf("shutting down at %s (it can be cancelled on its page until then)", clock(at)))
}

func deref(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
