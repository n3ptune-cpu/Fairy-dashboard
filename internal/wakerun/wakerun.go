// Package wakerun runs "Update unpowered hosts": for machines that are normally switched off it sends
// Wake-on-LAN, waits for the agent, asks the agent to update itself, waits for the result and switches the
// machine off again. It does by itself what an admin would do by hand with the existing Wake, Update and Shut
// down buttons, in that order, and it sends nothing else: no new agent request exists for it.
//
// The rules it keeps (README, "Wake, update, shut down"):
//
//   - It only ever switches off a machine that was off before the run and that this run woke: no sign of life
//     from it for MinOff (15 minutes) when the run started, online only after the run's own wake. That is decided in
//     one function, mayShutDown (shutdown.go), which is the only way to the shutdown request. A machine that was
//     on is never switched off; it is updated only if the admin asked. A machine the run is not sure about (it
//     stopped answering a few minutes ago: it may be restarting) is left alone: nothing is sent to it.
//   - A Proxmox node with running guests, or whose guests the panel cannot see, stays on.
//   - One run at a time. A run that the panel's restart interrupted is never continued: the journal
//     (journal.go) is only read to report which machines the run had woken. Every request of a run is entered
//     in the journal before it goes out; a run whose journal cannot be written sends nothing further and
//     stops, so the report after a restart never knows less than what was sent.
//   - A run sends each agent at most a few control requests a minute (pacer.go).
//   - Every machine of a run ends in a state with a sentence that says what happened; nothing ends unreported.
//
// The package has no HTTP in it and does not know the poller: it talks to the panel through Ops, so the tests
// drive it with a clock and machines of their own. The web package starts runs (after "Confirm it's you") and
// writes the audit log and the notifications from the events.
package wakerun

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// How a machine is in a run.
const (
	ModeWake       = "wake"        // off: wake it, update it, switch it off again
	ModeUpdateOnly = "update_only" // on: update it; it is never switched off
)

// The states of one machine in a run. The first eight are steps; the rest are where a machine ends.
const (
	StQueued   = "queued"            // waiting for its turn
	StWaking   = "waking"            // the wake was sent; waiting for it to come online (its agent answers)
	StOnline   = "online"            // its agent answers; checking that it stays
	StApproval = "awaiting_approval" // waiting for the approval after the wake (item 1.1; not used yet)
	StUpdating = "updating"          // the agent's self-update
	StSettling = "settling"          // the update is over; the pause before the shutdown request
	StShutting = "shutting_down"     // the shutdown countdown runs on the machine

	StOff         = "off"         // woken, updated (or not), switched off again
	StStayedOn    = "stayed_on"   // was already on: updated (or not) and left on, as it must be
	StLeftOn      = "left_on"     // it came online after the run's wake and the run did not switch it off: it is ON
	StGaveUp      = "gave_up"     // it never came online after the wake, or the wake could not be sent
	StSkipped     = "skipped"     // nothing was sent to it
	StInterrupted = "interrupted" // the panel restarted while the machine was in the run
)

// The states of a run.
const (
	RunRunning     = "running"
	RunDone        = "done"
	RunCancelled   = "cancelled"
	RunStopped     = "stopped"     // the run stopped itself (control disabled, or it took too long)
	RunInterrupted = "interrupted" // the panel restarted
)

// What the run knows about a machine's power when it lets go of it.
const (
	OnYes       = "on"        // it answers
	OnNo        = "off"       // the run switched it off and saw it go
	OnUnknown   = "unknown"   // a wake or a shutdown was sent and the run did not see how it ended
	OnUntouched = "untouched" // nothing was sent to it: it is whatever it was
)

// The results of the update step.
const (
	UpdOK       = "ok"
	UpdCurrent  = "up_to_date"
	UpdFailed   = "failed"
	UpdApproval = "approval_required" // the agent asks for an approval the run does not have (item 1.1)
	UpdStuck    = "stuck"             // no result in time
	UpdNotTried = "not_tried"
)

// MaxHosts is how many machines one run takes.
const MaxHosts = 16

// MinOff is how long the panel must have asked a machine's agent, poll after poll, and got nothing at all
// before a run takes the machine for off. It counts from the last sign of life: an answer, a refused
// connection (the machine runs, only its agent is stopped), any other answer from its address (an HTTP error,
// a body that is not the agent's, a connection that was taken and then gave nothing) or a Proxmox node shown
// as online each start it again, so a machine that refused for 14 minutes and is silent for one is not off.
// One minute without an answer (offline_after) is also what a machine looks like
// while it restarts, for Windows Update or because its owner restarted it, and such a machine comes back by
// itself with a fresh uptime: a run would take it for a machine it woke, and switch it off. Fifteen minutes is
// longer than a restart takes. It is not a setting: a shorter time would bring that mistake back.
const MinOff = 15 * time.Minute

// NotSure is the one sentence for a machine of which the run cannot say that it is off. Nothing is sent to it.
var NotSure = "Not sure that it is off, so nothing was sent to it: Fairy takes a machine for off only after " + span(MinOff) +
	" without an answer (it may be restarting, or be on and out of reach)."

// notFreshText is the sentence for a machine that answered after the run's wake without looking freshly
// started (Host.NotFresh).
func notFreshText(h *Host) string {
	if h.TooSoon {
		after := ""
		if h.OnlineAfterS != nil {
			after = " " + span(time.Duration(*h.OnlineAfterS*float64(time.Second)))
		}
		return "It answered" + after + " after the wake, sooner than a machine that was off can start: it was already starting (someone switched it on). It was left on."
	}
	up := ""
	if h.BootUptimeS != nil {
		up = " (up for " + span(time.Duration(*h.BootUptimeS)*time.Second) + ")"
	}
	return "It answered after the wake but did not look freshly started" + up + ": it may have been asleep, or use Fast Startup. It was left on."
}

// wokenNote goes into the history of a machine that came online after the wake and looks freshly started: the
// run treats it as woken by the run from here on, and this is what that rests on (r2-p2-01).
const wokenNote = "it was off before the run and came up freshly started after the wake: the run treats it as woken by the run. " +
	"A machine switched on by a person while the run waits for it looks the same, and is treated the same."

// lostWakeNote goes into the history of a machine that came online freshly started after a wake request that
// got no answer, with no wake request for it accepted: the run does not treat it as woken by the run.
const lostWakeNote = "it came up freshly started, but the run's wake request for it got no answer: Fairy cannot tell that the run woke it. " +
	"It is updated and left on."

// lateWakeNote goes into the history of a machine whose first wake request got no answer and that was already
// starting when a later one was accepted (mayShutDown, Host.startedBeforeAcceptedWake).
const lateWakeNote = "it came up freshly started, but the run's first wake request for it got no answer, and it was already starting when a later one " +
	"was accepted: Fairy cannot tell that the run woke it. It is updated and left on."

// The sentence for a machine that is on although its agent does not answer, with what shows that it is on.
const onByText = "It is on (%s), and its agent does not answer. Nothing was sent to it and it stays on."

// OnByRefused is what shows that a machine is on when it refused the connection: its own network stack sent
// the refusal, so it runs, and only its agent is not listening.
const OnByRefused = "the machine refused the connection"

// Timings that are not settings.
const (
	wakeResend      = 60 * time.Second // the wake is sent again this often while the machine does not answer
	kickEvery       = 5 * time.Second  // the poller is asked to poll a machine the run waits for
	onlineSettle    = 15 * time.Second // the agent must answer this long without a gap before the update
	updateStart     = 2 * time.Minute  // how long the run waits for an agent to be able to update (helper starting)
	updateTimeout   = 15 * time.Minute // no result of the update by then: the machine is left on
	updateIdleAfter = 10 * time.Second // an "idle" state only counts when it was polled this long after the request
	// unsureIdleAfter: the same after an update request whose answer was lost. An agent that took the request
	// reports "downloading" at its next answer; a minute of "idle" with no new result is an agent that runs no
	// update. (mayShutDown looks at the agent's update state once more before a shutdown, whatever was seen.)
	unsureIdleAfter = time.Minute
	verdictWait     = 90 * time.Second // after the new version answers: how long the installer's own verdict is waited for
	agentBack       = 3 * time.Minute  // after the update: how long the agent may take to answer again
	// offConfirm, offPolls: "off" after a countdown is nothing at all from the machine for this long, seen by
	// at least this many polls in a row that were made after the run's own due time (Host.seeSilence).
	offConfirm   = 20 * time.Second
	offPolls     = 2
	guestsEvery  = 5 * time.Second  // while a countdown runs: how often the guests of the machine are asked about again
	guestsMargin = 15 * time.Second // ... and not in the last seconds of the countdown (stepShutting)
	// offGrace: still answering this long after the run's own due time: the shutdown failed, or its agent counts
	// a longer countdown than the run asked for. Either way the run lets go of the machine then, so that no
	// agent decides how long the one run slot is taken (r1-p2-07). It also covers an agent that does not count
	// to the second (one that rounds up to the next whole minute).
	offGrace = 5 * time.Minute
	// The last look at a machine before its result is written (look): an answer this fresh is "it answers
	// now"; a machine that gives nothing is waited for this long, and then the result says "not confirmed".
	lookFresh = 5 * time.Second
	lookFor   = offConfirm + 10*time.Second
	// minBoot: no machine that was off answers this soon after a wake. Its firmware has to start, then the
	// system, the agent and the network (with Tailscale), and the poller has to ask it: on the fastest PC that
	// is well over eight seconds. An answer sooner than this comes from a machine that was already starting.
	// The value is on the careful side on purpose: a machine wrongly taken for "already starting" would be
	// left on at every run.
	minBoot         = 8 * time.Second
	approvalTimeout = 15 * time.Minute
	journalRetry    = 5 * time.Second // a journal that could not be written is tried again this often
	maxRetries      = 5
	maxLog          = 40
	historyKept     = 10
)

// Settings are the timings of a run (config.yaml, wake_update).
type Settings struct {
	OnlineTimeout time.Duration `json:"-"`
	Settle        time.Duration `json:"-"`
	ShutdownDelay time.Duration `json:"-"`
	PerAgentMin   int           `json:"agent_requests_per_min"`
	// the same three in seconds, for the journal and the page
	OnlineTimeoutS int `json:"online_timeout_s"`
	SettleS        int `json:"settle_s"`
	ShutdownDelayS int `json:"shutdown_delay_s"`
}

// NewSettings makes Settings from whole seconds.
func NewSettings(onlineTimeoutS, settleS, shutdownDelayS, perAgentMin int) Settings {
	return Settings{OnlineTimeout: time.Duration(onlineTimeoutS) * time.Second, Settle: time.Duration(settleS) * time.Second,
		ShutdownDelay: time.Duration(shutdownDelayS) * time.Second, PerAgentMin: perAgentMin,
		OnlineTimeoutS: onlineTimeoutS, SettleS: settleS, ShutdownDelayS: shutdownDelayS}
}

// HostInfo is a machine as the panel's configuration knows it now.
type HostInfo struct {
	ID      string
	Name    string
	AgentID string // its own agent: the update and the shutdown go to it
	RelayID string // the agent that sends its wake packets
	MACs    int    // one wake request to the relay per network card
	OptedIn bool   // an admin switched it on for these runs
	// Entry names the panel's entry for the machine's agent: another value means the agent was taken out and
	// added again (Re-pair) since. A run lets go of a machine whose entry changed under it.
	Entry int
}

// Pending is a power action an agent has scheduled.
type Pending struct {
	Action string
	At     time.Time
}

// Observation is what the panel's poller last heard from a machine's agent.
type Observation struct {
	Online bool      // the latest poll was answered
	SeenAt time.Time // when the last answered poll was
	// Status is the agent's state as the panel keeps it: "online", "stale" (no answer for less than
	// offline_after), "offline", or "" when the panel never polled it.
	Status string
	Paused bool // polling is paused for it: the panel would never see it come online
	// QuietSince: since when the panel has asked its agent, poll after poll, and got nothing at all (no answer
	// and no refused connection). Zero while it answers or shows another sign of life (OnBy), when it was never
	// polled, and when the panel stopped asking in between (longer than the machine's poll interval allows).
	// The poller measures the silence with its steady clock and gives its beginning as a time of day.
	QuietSince time.Time
	// OnBy: what else than its agent's answer shows the machine as on right now ("the Proxmox API shows it as
	// online", OnByRefused, "its address answered: http 401", ...); "": nothing does.
	OnBy string
	// Silent: the unbroken row of polls that got nothing at all from the machine, up to the latest poll, as
	// the poller counted it (N is 0 while the machine answers or shows a sign of life, and when the panel
	// stopped asking). QuietSince says since when; this says which polls saw it.
	Silent  Silence
	UptimeS *int64 // from the last answered poll (nil: the agent did not say)
	Version string
	// the agent's self-update
	UpdateEnabled bool
	UpdateState   string // idle | downloading | applying | failed | unavailable | ""
	UpdateMessage string
	LastAt        float64 // when the installer wrote its last result (Unix seconds; 0: never)
	LastOK        bool
	LastMessage   string
	Behind        *bool // older than the newest release the panel knows (nil: unknown)
	Pending       *Pending
}

// Silence is a row of polls of a machine's agent that got nothing at all: no answer and no other sign that
// the machine runs. The two times are "how long ago", measured by the poller on its steady clock: the run
// places them on its own (Manager.mono).
type Silence struct {
	Row      uint64        // names the row: another value is another row (an answer or a sign of life lies between)
	N        int           // polls in the row (0: the latest poll was not such a poll)
	BeganAgo time.Duration // how long ago the latest of them began (the request left) ...
	EndedAgo time.Duration // ... and ended without an answer
	// SinceAgo: how long ago the first poll began that got nothing at all since the machine last answered or
	// showed a sign of life. That can be before the row: the poller starts a new row when it was not watching
	// for a while, and the machine has not answered in between.
	SinceAgo time.Duration
}

// ProbeRefused is Ops.Probe's answer for a connection the machine itself refused, reset or closed.
const ProbeRefused = "connection refused"

// QuietFor is how long the machine has gone without an answer, as far as the panel watched it (0: it answers,
// polling is paused, or the panel does not know).
func QuietFor(o Observation, now time.Time) time.Duration {
	if o.Online || o.Paused || o.QuietSince.IsZero() || now.Before(o.QuietSince) {
		return 0
	}
	return now.Sub(o.QuietSince)
}

// OffNow reports whether the panel's own view is "off": shown as offline, and no answer for MinOff. It is the
// first of the things a run asks for (the direct question right before the wake is the next), and what the
// page offers as "off".
func OffNow(o Observation, now time.Time) bool {
	return o.Status == "offline" && o.OnBy == "" && QuietFor(o, now) >= MinOff
}

// OpError is why a request to a machine was not carried out.
type OpError struct {
	Code string // CodeApproval, CodeRateLimited or "" (anything else)
	Text string // for people; no secrets
}

func (e *OpError) Error() string { return e.Text }

// The codes of an OpError the run handles in a way of its own.
const (
	CodeApproval    = "approval_required" // the agent wants an approval (Pocket ID) for this request
	CodeRateLimited = "rate_limited"      // the agent's control rate limit: try again later
	// CodeNoAgentRoute: the request would no longer go to the machine's own agent (its power control was
	// switched off during the run). Nothing was sent. A run's shutdown is the agent's delayed, cancellable one
	// or none: the machine is left on (NoAgentRoute).
	CodeNoAgentRoute = "no_agent_route"
	// CodeNoAnswer: the request left the panel, or may have, and no usable answer came back (a timeout, a
	// connection that broke). The machine may have got it: the wake packet may be out, the update or the
	// shutdown countdown may be running. The run never takes this for "nothing happened".
	CodeNoAnswer = "no_answer"
)

// NoAgentRoute is the one sentence for a woken machine whose agent can no longer shut it down.
const NoAgentRoute = "Left ON: its agent can no longer shut it down (power control was switched off for it during the run), " +
	"and a run switches a machine off through its own agent or not at all. Nothing was sent."

// Ops is everything a run asks of the panel. The four requests are the existing ones: wake (through the
// relay), the agent's self-update, the delayed shutdown and its cancel.
type Ops interface {
	// Host returns the machine as configured now; false when the panel no longer has it.
	Host(id string) (HostInfo, bool)
	// Observe returns the poller's latest view of the machine's agent.
	Observe(id string) Observation
	// Probe asks the machine's agent itself, now: "" when it answered, "timeout" or "connection failed" when
	// nothing did (no route, no answer to the attempt to connect), ProbeRefused when the machine refused the
	// connection (it is on, its agent is not listening), anything else when something answered that is not
	// usable, or took the connection and then gave nothing.
	Probe(ctx context.Context, id string) string
	// Kick asks the poller to poll the machine's agent at once.
	Kick(id string)
	Wake(ctx context.Context, id, by string) error
	Update(ctx context.Context, id, by string, grant any) error
	// Shutdown asks for the delayed, cancellable shutdown and returns when its agent says the countdown ends:
	// a time by the machine's own clock, which the run does not count with (stepShutting).
	Shutdown(ctx context.Context, id string, delay time.Duration, by string, grant any) (time.Time, error)
	CancelShutdown(ctx context.Context, id, by string) error
	// Guests: how many guests run on the machine if it is a Proxmox node (0, true for any other machine). known
	// is false when the panel cannot tell; why then says so. since is when the machine came online.
	Guests(id string, since time.Time) (running int, known bool, why string)
	// Blocked is why no control request may be sent at all right now ("": allowed): the kill switch. The run
	// stops then. The one request it still sends is CancelShutdown, for a countdown it started itself: that
	// only ever stops a power action, and the panel lets it through the kill switch (stepRun).
	Blocked() string
}

// Approver is the hook for item 1.1: a machine whose agent verifies a Pocket ID approval itself before an
// update or a shutdown. A machine that is off cannot hand out the challenge such an approval is made for, so a
// run asks for it once, after the machines are awake, for all of them together: a machine for which Needed
// says yes waits in StApproval until Manager.Approve hands the run a grant, which then goes to Ops.Update and
// Ops.Shutdown. Manager.Approval is nil today, so no machine ever waits there.
type Approver interface {
	Needed(h HostInfo, o Observation) bool
}

// LogLine is one line of a machine's history in a run.
type LogLine struct {
	T    time.Time `json:"t"`
	Text string    `json:"text"`
}

// Host is one machine in a run. Everything in it is written to the journal; none of it is a secret.
type Host struct {
	ID    string    `json:"id"`
	Name  string    `json:"name"`
	Mode  string    `json:"mode"`
	State string    `json:"state"`
	Since time.Time `json:"since"` // when it entered State
	Text  string    `json:"text"`  // what is happening, or how it ended, in a sentence

	// What the run knows about "this run woke it" (mayShutDown reads these).
	StartStatus string     `json:"start_status"`           // the agent's state when the run started
	QuietForS   *int64     `json:"quiet_for_s,omitempty"`  // how long it had gone without an answer then (nil: not known)
	ProbedOff   bool       `json:"probed_off,omitempty"`   // asked directly right before the wake, nothing answered
	WakeSentAt  *time.Time `json:"wake_sent_at,omitempty"` // the run's first wake for it
	Wakes       int        `json:"wakes,omitempty"`        // wake requests the relay accepted
	// WakeUnsure: the run's first wake request for it left the panel and got no answer (CodeNoAnswer): the
	// packet may have gone out. WakeSentAt is then the moment before that request left, and the run waits for
	// the machine exactly as after a wake the relay accepted. It is switched off again only if a wake request
	// for it was accepted as well (Wakes, a later one of the wait): mayShutDown.
	WakeUnsure bool `json:"wake_unconfirmed,omitempty"`
	// WakeOKAt (only after a first request without an answer, WakeUnsure): when a wake request of the run for
	// it was first accepted, read after the relay's answer. That is the first wake the run knows of, and what
	// "started after the wake" is measured from then: a machine whose system was already starting at that
	// moment is left on (mayShutDown).
	WakeOKAt    *time.Time `json:"wake_accepted_at,omitempty"`
	OnlineAt    *time.Time `json:"online_at,omitempty"`     // its first answered poll after that wake
	BootUptimeS *int64     `json:"boot_uptime_s,omitempty"` // the uptime it reported in that poll
	// OnlineAfterS: how long after the wake that answer came, in seconds. It is the shorter of two
	// measurements: the difference of the two times above, and the panel's steady clock (Manager.Mono), which
	// does not move when someone, or a time server, sets the clock. The uptime was compared with this. It is
	// here for the record; mayShutDown measures again from the run's own readings.
	OnlineAfterS *float64 `json:"online_after_s,omitempty"`
	AlreadyOn    bool     `json:"already_on,omitempty"` // it was on before the run did anything
	// NotFresh: the run asked it directly, got nothing, sent its wake, and the machine then answered with an
	// uptime longer than the time since the wake. It did not start after the wake: it resumed from sleep or
	// hibernate, or is a Windows PC with Fast Startup (all three keep counting the uptime), or it was on all
	// along and out of reach. The run cannot tell which, so it never switches it off (AlreadyOn is set too),
	// and it does not call that "it was already on": the machine ends as left on, with NotFreshText, a warning.
	NotFresh bool `json:"not_fresh,omitempty"`
	// TooSoon (with NotFresh): its agent answered sooner after the wake than a machine that was off can start
	// (minBoot), with a fresh uptime: it was already starting when the wake went out. Never set for a machine
	// whose uptime is longer than the time since the wake (that one was asleep, or on: NotFresh alone).
	TooSoon bool `json:"too_soon,omitempty"`
	// Asking: the request that is about to go out to the machine, or is on its way ("wake", "update",
	// "shutdown"); "" between requests. It is written to the journal before the request is sent (Manager.ask),
	// so a journal read after a restart knows of every request that may have left, also when what came of it
	// could not be written any more.
	Asking string `json:"asking,omitempty"`

	VersionBefore string     `json:"version_before,omitempty"`
	VersionAfter  string     `json:"version_after,omitempty"`
	Update        string     `json:"update,omitempty"` // the result of the update step (Upd...)
	UpdateText    string     `json:"update_text,omitempty"`
	UpdateSentAt  *time.Time `json:"update_sent_at,omitempty"`
	// UpdateUnsure: the run's update request left the panel and got no answer (CodeNoAnswer). The agent may
	// have taken it. The run then goes by what the agent itself reports about its update (stepUpdating), and
	// never by the missing answer.
	UpdateUnsure bool `json:"update_unconfirmed,omitempty"`
	// ShutdownAt: when the countdown ends, by the panel's clock: the moment the run's shutdown request was
	// answered plus the delay it asked for. It is for the words of the report and for the journal. The run
	// itself counts on the steady clock (dueMono), and never with the time the agent gives.
	ShutdownAt *time.Time `json:"shutdown_at,omitempty"`
	// ShutdownUnsure: the run's shutdown request left the panel and got no answer (CodeNoAnswer). The countdown
	// may be running: ShutdownAt is then when it would end, and the run watches the machine as after a request
	// that was accepted, until it sees how it ended. "Off" is only said of it when its agent showed the
	// countdown: silence alone is also what a request that never arrived looks like.
	ShutdownUnsure bool      `json:"shutdown_unconfirmed,omitempty"`
	On             string    `json:"on,omitempty"` // set when the machine ends (On...)
	Log            []LogLine `json:"log"`

	// Working values of the run in memory. A run is never continued after a restart, so they are not kept.
	wakeMono     time.Duration // the steady clock when the run's first wake was accepted
	wakeOKMono   time.Duration // the steady clock at WakeOKAt
	onlineMono   time.Duration // the steady clock when the run saw its first answer after that wake
	lastWake     time.Time
	lastKick     time.Time
	goodSince    time.Time // the agent has answered without a gap since
	lastAtBefore float64   // the installer's last result before the run's update request
	updBefore    string    // the agent's update state and message before that request (UpdateUnsure)
	versionNewAt time.Time // when another version than VersionBefore was first seen
	shutdownSent time.Time
	// endMin, dueMono: the earliest and the latest end of the countdown the run asked for, on the steady clock:
	// the moment before the shutdown request left, and the moment its answer was here (or the wait for it
	// ended), each plus the delay. dueMono is the run's own due time.
	endMin, dueMono time.Duration
	pendingSeen     bool          // the agent reported the run's shutdown as pending
	noPendingAt     time.Time     // the first answer after the request that had no countdown in it (see stepShutting)
	offQuiet        bool          // it gives nothing at all right now ...
	offSince        time.Duration // ... since this moment on the steady clock
	// What the run has seen of the poller's row of silent polls while it watches the countdown (seeSilence):
	silRow        uint64        // the row it is counting in
	silN          int           // how many polls of that row it has looked at
	silAfter      int           // polls of the row that were made after the run's own due time (dueMono)
	silSince      time.Duration // steady: when the first of those began
	cutEarly      bool          // it was out of reach before the countdown can have ended, and has not answered since
	kickMono      time.Duration // steady: when the poller was last asked to poll it during the countdown
	retries       int
	lateSeen      bool // gave up, and it came online later
	grant         any  // the approval for this machine (item 1.1)
	cancelHandled bool
	cancelUnsure  bool          // the run's cancel of its countdown left and got no answer: the countdown may be called off
	guestsAsked   bool          // the guests were asked about during the countdown ...
	guestsMono    time.Duration // ... last at this moment on the steady clock
	guestsUnseen  bool          // ... and the answer then was "cannot see them"
	entry         int           // HostInfo.Entry when the run started
	verdict       *verdict      // how the run would end it: the last look (look) is going on
	askedAt       time.Time     // when the run's own direct question to it got an answer of any kind (it runs)
}

// Final reports whether the machine has ended in this run.
func (h *Host) Final() bool {
	switch h.State {
	case StOff, StStayedOn, StLeftOn, StGaveUp, StSkipped, StInterrupted:
		return true
	}
	return false
}

// Cancel is who cancelled a run and how.
type Cancel struct {
	By string    `json:"by"`
	At time.Time `json:"at"`
	// SwitchOff: the machines the run had already woken are still switched off again. Without it they stay on.
	SwitchOff bool `json:"switch_off"`
}

// Run is one run. It is the journal entry and what the page shows.
type Run struct {
	ID         string     `json:"id"`
	State      string     `json:"state"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	By         string     `json:"by"`
	BySub      string     `json:"by_sub,omitempty"`
	ListHash   string     `json:"list_hash"` // names the confirmed list and options (dialog, audit log, run)
	UpdateIfOn bool       `json:"update_if_on"`
	Settings   Settings   `json:"settings"`
	Latest     string     `json:"latest,omitempty"` // the newest agent release the panel knew at the start
	Hosts      []*Host    `json:"hosts"`
	Cancel     *Cancel    `json:"cancel,omitempty"`
	Note       string     `json:"note,omitempty"`     // why the run stopped itself
	Approval   string     `json:"approval,omitempty"` // "", "needed", "given" (item 1.1)
	Summary    string     `json:"summary,omitempty"`  // one line per machine, when the run is over
	// Reported: the interrupted run was reported after the restart (once).
	Reported bool `json:"reported,omitempty"`

	// journalErr: a write of the journal failed while this run was going (in memory only). The run sends
	// nothing further and stops at its next step.
	journalErr string
	// stopped: why the run stopped itself (in memory only; Note has it for the page). From then on nothing is
	// sent; the run ends when the last look at each machine is over.
	stopped string
	// startMono: the steady clock when the run started (in memory only), for the run's own limit (maxRun).
	startMono time.Duration
}

func (r *Run) clone() *Run {
	c := *r
	c.Hosts = make([]*Host, len(r.Hosts))
	for i, h := range r.Hosts {
		hc := *h
		hc.Log = slices.Clone(h.Log)
		c.Hosts[i] = &hc
	}
	if r.Cancel != nil {
		cc := *r.Cancel
		c.Cancel = &cc
	}
	return &c
}

// Event is something the audit log and the notifications are told. Run and Host are copies.
type Event struct {
	Kind    string // EvStep, EvHostEnd, EvRunEnd, EvInterrupted, EvApproval, EvLate
	Run     *Run
	Host    *Host          // nil for the run's own events
	Action  string         // EvStep: wake | update | shutdown | cancel
	Params  map[string]any // EvStep: details for the audit entry
	Outcome string         // EvStep: "ok" or the error text
}

// The kinds of Event.
const (
	EvStep        = "step"            // a request was sent (or refused) for one machine
	EvHostEnd     = "host_end"        // a machine ended
	EvRunEnd      = "run_end"         // the run is over
	EvInterrupted = "interrupted"     // the panel restarted during this run (reported at the next start)
	EvApproval    = "approval_needed" // the woken machines wait for an approval (item 1.1)
	EvLate        = "late_online"     // a machine the run gave up on came online later
)

// StartRequest is what an admin confirmed: the machines and the options, nothing else.
type StartRequest struct {
	Hosts      []string // machines that are off: wake, update, switch off again
	UpdateOnly []string // machines that are on: update, never switch off
	UpdateIfOn bool     // a machine of Hosts that turns out to be on is updated (and left on) instead of skipped
	By, BySub  string
	Latest     string
}

// ListHash names a list of machines and options with eight hex digits: the dialog, the audit log and the run
// show the same value for the same request.
func ListHash(hosts, updateOnly []string, updateIfOn bool) string {
	a, b := slices.Clone(hosts), slices.Clone(updateOnly)
	sort.Strings(a)
	sort.Strings(b)
	h := sha256.Sum256([]byte(fmt.Sprintf("fairy-wake-update-v1\x00%s\x00%s\x00%v", strings.Join(a, "\x01"), strings.Join(b, "\x01"), updateIfOn)))
	return hex.EncodeToString(h[:4])
}

// StartError is why a run was not started. Busy: another run is active.
type StartError struct {
	Busy bool
	Msg  string
}

func (e *StartError) Error() string { return e.Msg }

// Manager holds the one active run and the last finished ones.
type Manager struct {
	Ops      Ops
	Settings Settings
	Now      func() time.Time
	// Mono is a clock that only ever goes forward at the speed of time: it is not the time of day, and setting
	// the system's clock does not move it. nil: Go's own (the time since the manager was made). "How long
	// after the wake did the machine answer" is measured with it as well as with Now: if the time of day jumps
	// forward between the wake and the answer, the difference of the two times is too long, and a machine
	// that had been on for less than the jump would look like one that started after the wake.
	Mono func() time.Duration
	// Event is called for every Event, outside the manager's lock (nil: nobody listens).
	Event func(Event)
	// Approval is the hook for item 1.1 (nil: no machine needs an approval after the wake).
	Approval Approver

	path string    // the journal ("" keeps nothing)
	born time.Time // for Mono: it holds Go's steady clock reading

	tickMu sync.Mutex // one Tick at a time
	mu     sync.Mutex
	active *Run
	hist   []*Run // finished runs, newest first
	jerr   error  // the journal cannot be read: no run starts
	// werr: the latest write of the journal failed (nil again after one that worked). No run starts while it
	// is set; a run that was going when it was set stops (Run.journalErr). werrAt: when it was last tried.
	werr   error
	werrAt time.Time
	pace   *pacer
	events []Event
	dirty  bool
}

// New returns a manager that keeps its journal at path. Call Load before the first Start.
func New(path string, settings Settings, ops Ops) *Manager {
	return &Manager{Ops: ops, Settings: settings, Now: time.Now, path: path, born: time.Now(), pace: newPacer(settings.PerAgentMin)}
}

func (m *Manager) now() time.Time { return m.Now().UTC() }

// mono is the steady clock (see Manager.Mono).
func (m *Manager) mono() time.Duration {
	if m.Mono != nil {
		return m.Mono()
	}
	return time.Since(m.born) // both hold a steady clock reading, so this is not the difference of two times of day
}

// paceNow is the steady clock as a time, for the pacer: its minute must not get longer or shorter when the
// time of day is set during a run (r3-p2-05).
func (m *Manager) paceNow() time.Time { return time.Unix(0, 0).Add(m.mono()) }

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Snapshot returns copies of the active run (nil: none) and of the finished ones, newest first.
func (m *Manager) Snapshot() (active *Run, history []*Run) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active != nil {
		active = m.active.clone()
	}
	for _, r := range m.hist {
		history = append(history, r.clone())
	}
	return active, history
}

// JournalError is why no run can start (nil: the journal is fine).
func (m *Manager) JournalError() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jerr
}

// JournalWriteError is why the journal could not be written at the latest try (nil: it could). No run starts
// while it is set.
func (m *Manager) JournalWriteError() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.werr
}

// Start begins a run with exactly the machines of req. It refuses while another run is active, without
// keeping or queueing anything.
func (m *Manager) Start(req StartRequest) (*Run, error) {
	now := m.now()
	seen := map[string]bool{}
	var hosts []*Host
	add := func(ids []string, mode string) error {
		for _, id := range ids {
			if seen[id] {
				return &StartError{Msg: "a machine is in the list twice: " + id}
			}
			seen[id] = true
			info, ok := m.Ops.Host(id)
			if !ok {
				return &StartError{Msg: "unknown machine: " + id}
			}
			if !info.OptedIn {
				return &StartError{Msg: info.Name + " is not switched on for \"Update unpowered hosts\""}
			}
			o := m.Ops.Observe(id)
			h := &Host{ID: id, Name: info.Name, Mode: mode, State: StQueued, Since: now, Text: "waiting for its turn", StartStatus: o.Status, Log: []LogLine{}, entry: info.Entry}
			if q := QuietFor(o, now); q > 0 && o.OnBy == "" {
				s := int64(q / time.Second)
				h.QuietForS = &s
			}
			hosts = append(hosts, h)
		}
		return nil
	}
	if err := add(req.Hosts, ModeWake); err != nil {
		return nil, err
	}
	if err := add(req.UpdateOnly, ModeUpdateOnly); err != nil {
		return nil, err
	}
	if len(hosts) == 0 {
		return nil, &StartError{Msg: "no machine was chosen"}
	}
	if len(hosts) > MaxHosts {
		return nil, &StartError{Msg: fmt.Sprintf("at most %d machines in one run", MaxHosts)}
	}
	if why := m.Ops.Blocked(); why != "" {
		return nil, &StartError{Msg: why}
	}
	m.mu.Lock()
	if m.jerr != nil {
		err := m.jerr
		m.mu.Unlock()
		return nil, &StartError{Msg: "the run journal cannot be used, so no run is started: " + err.Error()}
	}
	if a := m.active; a != nil {
		m.mu.Unlock()
		return nil, &StartError{Busy: true, Msg: fmt.Sprintf("a run started by %s at %s is still going: one run at a time. Wait for it, or cancel it.",
			a.By, a.StartedAt.Format("15:04 UTC"))}
	}
	r := &Run{ID: newID(), State: RunRunning, StartedAt: now, By: req.By, BySub: req.BySub, UpdateIfOn: req.UpdateIfOn,
		ListHash: ListHash(req.Hosts, req.UpdateOnly, req.UpdateIfOn), Settings: m.Settings, Latest: req.Latest, Hosts: hosts, startMono: m.mono()}
	m.active = r
	out := r.clone()
	m.mu.Unlock()
	// Before anything is sent: a restart from here on knows about the run. A run that cannot be written down
	// does not start.
	if err := m.save(); err != nil {
		m.mu.Lock()
		if m.active == r {
			m.active = nil
		}
		m.mu.Unlock()
		return nil, &StartError{Msg: "the run journal cannot be written, so no run is started (nothing was sent): " + err.Error()}
	}
	return out, nil
}

// CancelRun cancels the active run id. Nothing new is started for any machine. The machines the run has
// already woken stay on and are reported, or with switchOff are still switched off again.
func (m *Manager) CancelRun(id, by string, switchOff bool) (*Run, error) {
	m.mu.Lock()
	r := m.active
	if r == nil || r.ID != id {
		m.mu.Unlock()
		return nil, &StartError{Msg: "that run is not active (any more)"}
	}
	if r.Cancel == nil {
		r.Cancel = &Cancel{By: by, At: m.now(), SwitchOff: switchOff}
		m.dirty = true
	}
	out := r.clone()
	m.mu.Unlock()
	return out, nil
}

// Approve hands the active run the approval the woken machines wait for (item 1.1): every machine in
// StApproval goes on to its update, and grant goes with its update and shutdown requests.
func (m *Manager) Approve(id string, grant any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.active
	if r == nil || r.ID != id {
		return &StartError{Msg: "that run is not active (any more)"}
	}
	n := 0
	for _, h := range r.Hosts {
		if h.State == StApproval {
			h.grant = grant
			n++
		}
	}
	if n == 0 {
		return &StartError{Msg: "no machine of this run waits for an approval"}
	}
	r.Approval = "given"
	m.dirty = true
	return nil
}

// Loop runs the active run until ctx ends: one Tick a second.
func (m *Manager) Loop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if a, _ := m.Snapshot(); a != nil { // the next start reports it from the journal
				slog.Warn("wake-update: the panel stops during a run; it is not continued, the next start reports it", "run", a.ID)
			}
			return
		case <-t.C:
			m.Tick(ctx)
		}
	}
}

// Tick moves the active run one step: every machine that has not ended looks at what the panel knows now and
// does at most the next thing. It is safe to call from one goroutine at a time (Loop, or a test).
func (m *Manager) Tick(ctx context.Context) {
	m.tickMu.Lock()
	defer m.tickMu.Unlock()
	m.mu.Lock()
	if r := m.active; r != nil {
		m.stepRun(ctx, r, m.now())
	}
	evs := m.events
	m.events = nil
	dirty := m.dirty
	m.dirty = false
	// a journal that could not be written is tried again every few seconds, so that what a stopped run ended
	// with gets written once the disk has room again
	retry := m.werr != nil && m.now().Sub(m.werrAt) >= journalRetry
	m.mu.Unlock()
	if dirty || retry {
		_ = m.save() // a failure stops the run at its next step (stepRun)
	}
	if m.Event != nil {
		for _, e := range evs {
			m.Event(e)
		}
	}
}

// io runs fn (a request that goes over the network) without the manager's lock, so the page can read the run
// while a machine takes its time to answer.
func (m *Manager) io(fn func()) {
	m.mu.Unlock()
	defer m.mu.Lock()
	fn()
}

func (m *Manager) emit(kind string, r *Run, h *Host, action string, params map[string]any, outcome string) {
	e := Event{Kind: kind, Run: r.clone(), Action: action, Params: params, Outcome: outcome}
	if h != nil {
		hc := *h
		hc.Log = slices.Clone(h.Log)
		e.Host = &hc
	}
	m.events = append(m.events, e)
}

// say sets what is shown for the machine now.
func (m *Manager) say(h *Host, text string) {
	if h.Text != text {
		h.Text = text
		m.dirty = true
	}
}

// note adds a line to the machine's history.
func (m *Manager) note(h *Host, now time.Time, text string) {
	if len(h.Log) >= maxLog {
		h.Log = h.Log[1:]
	}
	h.Log = append(h.Log, LogLine{T: now, Text: text})
	m.dirty = true
}

// enter moves the machine to a step.
func (m *Manager) enter(h *Host, state string, now time.Time, text string) {
	h.State, h.Since, h.Text = state, now, text
	h.goodSince, h.retries = time.Time{}, 0
	m.note(h, now, text)
}

// verdict is how the run would end a machine, before the last look at it.
type verdict struct {
	state, on string
	text      string    // the sentence, when the machine is seen the way the run believes it to be
	neutral   string    // (believed on) the same without a word about its power; "": the look adds to text
	since     time.Time // when the look began
	sinceMono time.Duration
	kicked    time.Time
}

// end is how every machine of a run ends, and the only way: with the state the run would end it in, what the
// run believes about its power, and the sentence that says what happened. Before that is written, the run
// looks at the machine once more (look) and writes what it SEES (r2-p2-05): the report's power is never a guess.
func (m *Manager) end(r *Run, h *Host, state, on string, now time.Time, text string) {
	m.endSeen(r, h, state, on, now, text, "")
}

// endSeen is end for a machine that may well not be the way the run believes: neutral is the sentence without
// a word about its power, to which the look adds what it sees.
func (m *Manager) endSeen(r *Run, h *Host, state, on string, now time.Time, text, neutral string) {
	h.verdict = &verdict{state: state, on: on, text: text, neutral: neutral, since: now, sinceMono: m.mono()}
	m.look(r, h, now)
}

// What the last look adds to a sentence, or shows while it waits.
const (
	seenOff      = " It is off (not answering)."
	notSeenOff   = " Asked to switch off, not confirmed: it does not answer now."
	notSeenOn    = " It does not answer now: not confirmed that it is on."
	notSeenAtAll = " It does not answer now, so that is not confirmed."
	answersAgain = " It answered again after that: it is ON."
	cannotLook   = " Fairy can no longer look at it."
	lookingText  = "checking how it ended: it does not answer right now"
)

// look is the one last step of every machine: it looks at the machine again and writes the result with what
// it sees. It sends nothing to the machine (it reads the poller's view and asks the poller to poll).
//
//   - Nothing was sent to it, or the run already says that it does not know: the result is written as it is.
//   - The run believes it is on: an answer from now, or another sign of life (a refused connection is a
//     machine that runs), confirms it. If it gives nothing, the run waits a short, fixed time (lookFor). When
//     a shutdown was asked for that the run knows of (it was accepted, or its countdown was seen; not one
//     that only got no answer), no cancel of the run may have called it off, and the machine has given
//     nothing after the run's own due time, the same proof the watch of the countdown asks for (seenOff),
//     it is off: "off (not answering)". Otherwise the result says "not confirmed", and its power is "unknown".
//   - The run saw it go off: written as that, unless it shows a sign of life again.
func (m *Manager) look(r *Run, h *Host, now time.Time) {
	v := h.verdict
	final := func(state, on, text string) {
		h.verdict = nil
		h.State, h.Since, h.Text, h.On = state, now, text, on
		m.note(h, now, text)
		m.emit(EvHostEnd, r, h, "", nil, "")
	}
	if v.on != OnYes && v.on != OnNo {
		final(v.state, v.on, v.text)
		return
	}
	_, known := m.Ops.Host(h.ID)
	o := m.Ops.Observe(h.ID)
	canSee := known && !o.Paused
	seenOn := canSee && ((o.Online && !o.SeenAt.Before(v.since.Add(-lookFresh))) || o.OnBy != "" ||
		(!h.askedAt.IsZero() && !h.askedAt.Before(v.since.Add(-lookFresh)))) // the run's direct question was answered
	base := v.neutral
	if base == "" {
		base = v.text
	}
	switch {
	case v.on == OnNo && seenOn:
		final(StLeftOn, OnYes, v.text+answersAgain)
	case v.on == OnNo:
		final(v.state, v.on, v.text)
	case seenOn:
		final(v.state, v.on, v.text)
	case !canSee: // taken out of the panel, or its polling is paused
		final(v.state, OnUnknown, base+cannotLook)
	default:
		mono := m.mono()
		h.seeSilence(o, mono)
		// asked: a shutdown the run knows of (the request was accepted, or its countdown was seen). A request
		// that got no answer and of which no countdown was ever seen is not one: it may never have arrived.
		// And silence is "off" only when nothing can have called the countdown off: not after a cancel of
		// the run whose answer was lost (it may have arrived; then the machine is on).
		asked := h.ShutdownAt != nil && !h.lostShutdown()
		switch {
		case asked && !h.cancelUnsure && h.seenOff(mono): // the same proof the watch of the countdown asks for
			final(StOff, OnNo, base+seenOff)
		case now.Sub(v.since) < lookFor && mono-v.sinceMono < lookFor:
			if now.Sub(v.kicked) >= kickEvery {
				v.kicked = now
				m.Ops.Kick(h.ID)
			}
			m.say(h, lookingText)
		case asked:
			final(v.state, OnUnknown, base+notSeenOff)
		case v.neutral != "":
			final(v.state, OnUnknown, base+notSeenOn)
		default:
			final(v.state, OnUnknown, base+notSeenAtAll)
		}
	}
}

// stepRun is one step of the whole run. The caller holds m.mu (io releases it around requests).
func (m *Manager) stepRun(ctx context.Context, r *Run, now time.Time) {
	stop, killSwitch := "", false
	if why := m.Ops.Blocked(); why != "" {
		stop, killSwitch = why, true
	} else if r.journalErr != "" {
		// The record of what the run does is what the report after a restart is made from. Without it the run
		// goes no further: no wake, no update, no shutdown from here on.
		stop = "its journal (wake-update.json) could not be written: " + r.journalErr
	} else if m.mono()-r.startMono > m.maxRun(r) {
		// By the steady clock: a time of day that is set back during a run must not keep the run, and the one
		// run slot, for as long as the step (r3-p2-05), and one that is set forward must not stop a run that
		// is watching a countdown.
		stop = "the run took longer than it ever should"
	}
	if stop != "" && r.stopped == "" {
		// Nothing more is started for any machine: every one is reported as it is. When it is the kill switch
		// that stops the run, one request is still sent: a countdown the run started itself is called off, while
		// it can still be running (callOff: after the run's own due time nothing is sent, R7A-03). The
		// run used to send "not even a cancel", said "it switches off by itself unless someone cancels that",
		// and with the kill switch on nobody could: the machine the admin wanted to keep on by stopping
		// everything went off (audit 5, r3-p2-06). A cancel only ever stops a power action, and the panel lets it
		// through the kill switch. What came of it is in the machine's sentence: called off, or not.
		//
		// A run that stops because its journal cannot be written still sends nothing at all: what the report
		// after a restart is made from must never know less than what was sent, and a cancel is a request too.
		// Its sentence says that the machine switches off by itself, and the page's Cancel is there for it.
		r.stopped, r.Note = stop, "stopped: "+stop
		m.dirty = true
		for _, h := range r.Hosts {
			switch {
			case h.Final() || h.verdict != nil:
			case h.State == StShutting && killSwitch:
				m.callOff(ctx, r, h, now, "the run stopped ("+stop+")")
			default:
				m.endAsIs(r, h, now, "the run stopped ("+stop+")")
			}
		}
	}
	if r.stopped != "" { // only the last look at the machines that do not answer is left (it sends nothing)
		for _, h := range r.Hosts {
			if !h.Final() {
				m.look(r, h, now)
			}
		}
		for _, h := range r.Hosts {
			if !h.Final() {
				return
			}
		}
		m.finish(r, now, RunStopped)
		return
	}
	wakeTurn := true // one machine is asked directly and woken per step, in the order of the list
	for _, h := range r.Hosts {
		if h.Final() {
			m.watchLate(r, h, now)
			continue
		}
		if h.State == StQueued && h.Mode == ModeWake && r.Cancel == nil {
			if !wakeTurn {
				continue
			}
			wakeTurn = false
		}
		m.stepHost(ctx, r, h, now)
	}
	if m.Approval != nil && r.Approval == "" {
		waiting, early := 0, 0
		for _, h := range r.Hosts {
			switch h.State {
			case StApproval:
				waiting++
			case StQueued, StWaking, StOnline:
				early++
			}
		}
		if waiting > 0 && early == 0 { // every machine is awake or was given up on: ask once, for all of them
			r.Approval = "needed"
			m.dirty = true
			m.emit(EvApproval, r, nil, "", nil, "")
		}
	}
	for _, h := range r.Hosts {
		if !h.Final() {
			return
		}
	}
	state := RunDone
	if r.Cancel != nil {
		state = RunCancelled
	}
	m.finish(r, now, state)
}

// maxRun is longer than the longest way through the steps of one machine, with room for every machine of the
// run to wait for its turn at the relay.
func (m *Manager) maxRun(r *Run) time.Duration {
	s := r.Settings
	return time.Duration(len(r.Hosts))*2*time.Minute + 2*s.OnlineTimeout + approvalTimeout + updateStart + updateTimeout + verdictWait +
		agentBack + s.Settle + s.ShutdownDelay + offGrace + 10*time.Minute
}

func (m *Manager) finish(r *Run, now time.Time, state string) {
	r.State, r.EndedAt = state, &now
	var lines []string
	for _, h := range r.Hosts {
		lines = append(lines, h.Name+": "+h.Text)
	}
	r.Summary = strings.Join(lines, "\n")
	m.active = nil
	m.hist = append([]*Run{r}, m.hist...)
	if len(m.hist) > historyKept {
		m.hist = m.hist[:historyKept]
	}
	m.dirty = true
	m.emit(EvRunEnd, r, nil, "", nil, "")
}

// endAsIs ends a machine where it is, without sending anything: what the run did to it so far decides the
// state, and why is the reason it stops here.
func (m *Manager) endAsIs(r *Run, h *Host, now time.Time, why string) {
	switch {
	case h.State == StShutting && h.lostShutdown(): // no countdown the run knows of: the sentence does not claim one
		m.endSeen(r, h, StLeftOn, OnYes, now, fmt.Sprintf("%s after its shutdown request got no answer (it would be due at %s). Fairy no longer watches it: "+
			"if the request arrived, it switches off by itself unless someone cancels that.", why, when(*h.ShutdownAt, now)),
			fmt.Sprintf("%s after its shutdown request got no answer (it would be due at %s).", why, when(*h.ShutdownAt, now)))
	case h.State == StShutting:
		m.endSeen(r, h, StLeftOn, OnYes, now, fmt.Sprintf("%s while its shutdown was counting down (it ends at %s). Fairy no longer watches it: "+
			"it switches off by itself unless someone cancels that.", why, when(*h.ShutdownAt, now)),
			fmt.Sprintf("%s while its shutdown was counting down (due at %s).", why, when(*h.ShutdownAt, now)))
	case h.NotFresh:
		m.end(r, h, StLeftOn, OnYes, now, why+". "+notFreshText(h)+updateTail(h))
	case (h.AlreadyOn || h.Mode == ModeUpdateOnly) && h.WakeSentAt == nil && h.UpdateSentAt == nil && h.Update == "":
		// The run had not reached it: no wake, no update. That is not "done": it is reported as not touched, and
		// the sentence says that it was not updated (r1-b1-11: it used to be a green "done · stays on").
		m.end(r, h, StSkipped, OnUntouched, now, why+". Nothing was sent to it: it was not updated. It was already on and stays on.")
	case h.AlreadyOn || h.Mode == ModeUpdateOnly:
		tail := updateTail(h)
		if tail == "" {
			tail = " It was not updated."
		}
		m.end(r, h, StStayedOn, OnYes, now, why+". It was already on and stays on."+tail)
	case h.OnlineAt != nil:
		m.endSeen(r, h, StLeftOn, OnYes, now, why+". "+cameOnline(h)+" and it is ON."+updateTail(h), why+". "+cameOnline(h)+"."+updateTail(h))
	case h.WakeSentAt != nil && h.lostWake():
		m.end(r, h, StGaveUp, OnUnknown, now, why+". A wake may have gone out to it (the request got no answer): if it switches on, it stays ON.")
	case h.WakeSentAt != nil:
		m.end(r, h, StGaveUp, OnUnknown, now, why+". A wake was sent to it: if it switches on, it stays ON.")
	default:
		m.end(r, h, StSkipped, OnUntouched, now, why+". Nothing was sent to it.")
	}
}

// updateTail says, at the end of a sentence about a machine, how far its update got.
func updateTail(h *Host) string {
	switch {
	case h.Update == "" && h.UpdateSentAt != nil:
		return " Its update was asked for and may still be running; the agent finishes it by itself."
	case h.Update == UpdOK:
		return " It was updated" + toVersion(h) + "."
	case h.Update == UpdCurrent:
		return " It already ran the newest release."
	case h.Update == UpdFailed || h.Update == UpdStuck || h.Update == UpdApproval:
		return " Its update did not work: " + h.UpdateText + "."
	}
	return ""
}

// lostWake reports whether all the run knows of its wake for the machine is a request that got no answer.
func (h *Host) lostWake() bool { return h.WakeUnsure && h.Wakes == 0 }

// lostShutdown reports whether all the run knows of its shutdown for the machine is a request that got no
// answer: the agent never showed the countdown.
func (h *Host) lostShutdown() bool { return h.ShutdownUnsure && !h.pendingSeen }

// woken begins the sentence of a machine that came online after the run's wake and was switched off again.
//
// The report does not say "woken": that is what the run infers, not what it sees. It sees a machine that was
// off, a wake that went out, and the machine coming online after it, freshly started. A machine that a person
// switched on while the run waited for it looks exactly the same (the limit of r2-p2-01, see shutdown.go).
func woken(h *Host) string {
	if h.lostWake() {
		return "Came online after a wake request that got no answer"
	}
	return "Came online after the wake"
}

// cameOnline is the same inside a sentence about a machine that was not switched off again.
func cameOnline(h *Host) string {
	if h.lostWake() {
		return "It came online after a wake request that got no answer"
	}
	return "It came online after the wake"
}

func toVersion(h *Host) string {
	if h.VersionAfter != "" {
		return " to " + h.VersionAfter
	}
	return ""
}

func clock(t time.Time) string { return t.UTC().Format("15:04:05 UTC") }

// when is clock for a time that may be on another day than now: then the date is said too.
func when(t, now time.Time) string {
	if t.UTC().Format("2006-01-02") == now.UTC().Format("2006-01-02") {
		return clock(t)
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}

// span is a duration in words ("3 days", "2 h", "40 s").
func span(d time.Duration) string {
	switch s := int64(d.Seconds()); {
	case s >= 2*86400:
		return fmt.Sprintf("%d days", s/86400)
	case s >= 2*3600:
		return fmt.Sprintf("%d h", s/3600)
	case s >= 120:
		return fmt.Sprintf("%d min", s/60)
	default:
		return fmt.Sprintf("%d s", s)
	}
}

// watchLate looks once more at a machine the run gave up on after its wake: if it comes online while the run
// still goes, that is said. It is on then, and the run does not touch it.
func (m *Manager) watchLate(r *Run, h *Host, now time.Time) {
	if h.State != StGaveUp || h.WakeSentAt == nil || h.lateSeen {
		return
	}
	if o := m.Ops.Observe(h.ID); o.Online && o.SeenAt.After(*h.WakeSentAt) {
		h.lateSeen, h.On = true, OnYes
		h.Text += " It came online later (" + clock(o.SeenAt) + "): it is ON, and Fairy did not touch it."
		m.note(h, now, "came online after the run had given up on it: it is ON")
		m.emit(EvLate, r, h, "", nil, "")
	}
}
