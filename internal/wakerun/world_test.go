package wakerun

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// A world of machines for the tests: each one has a truth (is it running, since when, what starts it, what its
// agent answers) and the view the panel's poller has of it, which lags behind the truth the way the real
// poller's does. The world is the run's Ops and its clock; one second of it is: the machines move, the poller
// polls, the run takes one step.
//
// The world also is the judge of the package's rule. It knows what the run cannot know for sure: whether a
// machine was running when the run started and when the run's first wake request for it went to the relay, and
// whether the relay ever accepted a wake for it. Every shutdown request for a machine that was running at
// either moment, or for which no wake was accepted, fails the test, whatever the test is about.

type machine struct {
	info HostInfo

	// the truth
	on           bool
	bootedAt     time.Time     // since when it runs
	agentReadyAt time.Time     // its agent answers from then on (while it runs)
	unreachable  bool          // it runs, but the panel cannot reach it: polls and the direct question time out
	agentDown    bool          // it runs, but its agent does not listen: polls fail, the direct question is refused
	onBy         string        // what else shows it as on while it runs (a Proxmox node: the Proxmox API)
	wakeBoots    bool          // a wake packet starts it when it is off
	post         time.Duration // from the packet to the system running
	bootTakes    time.Duration // from the packet to the agent answering
	noUptime     bool          // its agent reports no uptime
	uptimeSays   *int64        // its agent reports this uptime, whatever the truth is
	paused       bool          // polling is paused for it in the panel
	probeSays    string        // what the direct question answers instead of the truth ("": the truth)
	relayDown    string        // the relay refuses its wake with this text
	wakeLost     string        // the answer to a wake request is lost: "arrives" (the first; the packet went out) | "never" (the first; no packet) | "always" (every one; no packet)
	version      string
	newVersion   string // what an update installs
	behind       *bool  // what the panel knows: older than the newest release

	updLost    string // the answer to the first update request is lost: "arrives" (the agent took it) | "never" (it did not)
	updMode    string // ok | noverdict | current | fail | rollback | stuck | approval | refuse | busy (the first request is rate limited) | disabled
	updTakes   time.Duration
	updating   bool
	updDoneAt  time.Time
	updState   string
	updMsg     string
	lastAt     float64
	lastOK     bool
	lastMsg    string
	shutMode   string // ok | approval | refuse | ignore (accepted, but it stays on) | busy | noroute (the agent route is gone) | hang (at the end of the countdown its agent stops and the machine stays on: it refuses the connection)
	cancelOK   bool   // a cancel is answered with "ok" also when the machine is gone (what a stand-in agent may do)
	cancelLost string // the answer to a cancel is lost: "arrives" (the countdown is called off on the machine) | "never"
	shutLost   string // the answer to the first shutdown request is lost: "arrives" (the countdown runs) | "never"
	pending    *Pending
	staleOnce  bool // the first answer after a shutdown was asked for does not have the countdown in it yet
	guests     int
	guestsWhy  string // not "": the panel cannot see its guests
	guestNames string

	// the poller's view
	everAnswered bool
	lastAnswer   time.Time
	quietSince   time.Time // the first of the row of polls without an answer it is in (zero: it answers)
	obs          Observation
	// the row of polls that got nothing at all, as the poller counts it (Observation.Silent)
	silRow  uint64
	silN    int
	silMono time.Duration // the steady clock at the latest of them
	silFrom time.Duration // the steady clock at the first silent poll since its last answer or sign of life

	// for the judge
	knownLimit    bool // this machine is the documented limit (r1-p2-01): the judge does not fail the test for it
	onAtStart     bool
	onAtFirstWake *bool
	wakeAccepted  bool // the relay accepted a wake request of the run for it (a request without an answer is not that)
	wakes         int
	shutdowns     int
	wentOff       bool // a countdown ran out and switched it off
	busyDone      map[string]bool
}

type world struct {
	t       *testing.T
	now     time.Time
	mono    time.Duration // the steady clock: it goes on a second per tick, whatever a test does to now
	m       *Manager
	ms      map[string]*machine
	order   []string
	calls   []string // every request the run sent: "wake pc-a", "update pc-a", "shutdown pc-a 120", "cancel pc-a"
	events  []Event
	blocked string
	during  func(call string) // called when a request of the run arrives, before the machine acts on it ("wake pc-a")
	// each agent's own limit, as the real agent keeps it: 12 control requests in a sliding minute
	agentHits map[string][]time.Time
	limited   int // requests an agent refused for that reason
	maxInMin  int // the most requests any one agent got in a minute
	started   bool
	// afterTick is called at the end of every second of the world (a test that changes a machine at a set time)
	afterTick   func()
	guestsAsked int            // how often the run asked about a machine's guests
	rows        uint64         // rows of silent polls the poller has counted
	kicks       map[string]int // how often the run asked the poller to poll a machine
	// clock guards now and mono between tick, which moves them, and the Manager, which reads them through
	// Now and Mono, also from another goroutine (TestReadingAndCancellingWhileARunWorks cancels from one).
	clock sync.Mutex
}

var t0 = time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC)

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, now: t0, ms: map[string]*machine{}, agentHits: map[string][]time.Time{}}
	w.m = New(filepath.Join(t.TempDir(), "wake-update.json"), NewSettings(300, 60, 120, 8), w)
	w.m.Now = func() time.Time {
		w.clock.Lock()
		defer w.clock.Unlock()
		return w.now
	}
	w.m.Mono = func() time.Duration {
		w.clock.Lock()
		defer w.clock.Unlock()
		return w.mono
	}
	w.m.Event = func(e Event) { w.events = append(w.events, e) }
	if err := w.m.Load(); err != nil {
		t.Fatal(err)
	}
	return w
}

// off adds a machine that is switched off and has been for a day, with the panel asking it all that time: a
// wake starts it, its agent answers 40 s later, an update takes 30 s and works, a shutdown works.
func (w *world) off(id string) *machine {
	yes := true
	mc := &machine{info: HostInfo{ID: id, Name: strings.ToUpper(id), AgentID: id, RelayID: "relay", MACs: 1, OptedIn: true}, quietSince: w.now.Add(-24 * time.Hour),
		wakeBoots: true, post: 10 * time.Second, bootTakes: 40 * time.Second, version: "16-aaaaaaa", newVersion: "17-bbbbbbb", behind: &yes,
		updMode: "ok", updTakes: 30 * time.Second, updState: "idle", shutMode: "ok", busyDone: map[string]bool{}}
	w.ms[id] = mc
	w.order = append(w.order, id)
	w.poll()
	return mc
}

// running adds a machine that has been on for three days.
func (w *world) running(id string) *machine {
	mc := w.off(id)
	mc.on, mc.bootedAt, mc.agentReadyAt = true, w.now.Add(-72*time.Hour), w.now.Add(-72*time.Hour+time.Minute)
	w.poll()
	return mc
}

// silentFor makes the panel's view of a machine that does not answer now: it has asked it for d without an
// answer.
func (w *world) silentFor(mc *machine, d time.Duration) {
	mc.quietSince, mc.lastAnswer = w.now.Add(-d), w.now.Add(-d)
	w.poll()
}

// ---- the world as Ops

func (w *world) Host(id string) (HostInfo, bool) {
	mc := w.ms[id]
	if mc == nil {
		return HostInfo{}, false
	}
	return mc.info, true
}

func (w *world) Observe(id string) Observation {
	if mc := w.ms[id]; mc != nil {
		o := mc.obs
		if mc.silN > 0 { // "how long ago", like the poller gives it: its polls take no time here
			w.clock.Lock()
			ago, since := w.mono-mc.silMono, w.mono-mc.silFrom
			w.clock.Unlock()
			o.Silent = Silence{Row: mc.silRow, N: mc.silN, BeganAgo: ago, EndedAgo: ago, SinceAgo: since}
		}
		return o
	}
	return Observation{}
}

func (w *world) answers(mc *machine) bool {
	return mc.on && !w.now.Before(mc.agentReadyAt) && !mc.unreachable && !mc.agentDown
}

func (w *world) Probe(_ context.Context, id string) string {
	mc := w.ms[id]
	switch {
	case mc.probeSays != "":
		return mc.probeSays
	case w.answers(mc):
		return ""
	case mc.unreachable:
		return "timeout"
	case mc.on && mc.agentDown:
		return ProbeRefused
	}
	return "connection failed"
}

func (w *world) Kick(id string) {
	if w.kicks == nil {
		w.kicks = map[string]int{}
	}
	w.kicks[id]++
}

func (w *world) arrived(call string) {
	if w.during != nil {
		w.during(call)
	}
}

// hit counts a control request to an agent the way the agent does, and reports whether the agent takes it.
func (w *world) hit(agent string) bool {
	var keep []time.Time
	for _, at := range w.agentHits[agent] {
		if w.now.Sub(at) < time.Minute {
			keep = append(keep, at)
		}
	}
	if len(keep) >= 12 {
		w.agentHits[agent] = keep
		w.limited++
		return false
	}
	w.agentHits[agent] = append(keep, w.now)
	w.maxInMin = max(w.maxInMin, len(keep)+1)
	return true
}

var errBusy = &OpError{Code: CodeRateLimited, Text: "too many control requests; try again in a minute"}

// errLost is a request that left the panel and got no answer.
var errLost = &OpError{Code: CodeNoAnswer, Text: "agent unreachable: timeout"}

func (w *world) Wake(_ context.Context, id, _ string) error {
	mc := w.ms[id]
	w.calls = append(w.calls, "wake "+id)
	w.arrived("wake " + id)
	if mc.relayDown != "" {
		return &OpError{Text: mc.relayDown}
	}
	for i := 0; i < mc.info.MACs; i++ { // one request to the relay per network card
		if !w.hit(mc.info.RelayID) {
			return errBusy
		}
	}
	if mc.onAtFirstWake == nil {
		on := mc.on
		mc.onAtFirstWake = &on
	}
	if mc.wakeLost != "" && (!mc.busyDone["wake lost"] || mc.wakeLost == "always") { // a timeout: the panel does not learn whether the packet went out
		mc.busyDone["wake lost"] = true
		if mc.wakeLost == "arrives" {
			mc.wakes++
			if !mc.on && mc.wakeBoots {
				mc.on, mc.bootedAt, mc.agentReadyAt = true, w.now.Add(mc.post), w.now.Add(mc.bootTakes)
			}
		}
		return errLost
	}
	mc.wakes++
	mc.wakeAccepted = true
	if !mc.on && mc.wakeBoots {
		mc.on, mc.bootedAt, mc.agentReadyAt = true, w.now.Add(mc.post), w.now.Add(mc.bootTakes)
	}
	return nil
}

func (w *world) busyOnce(mc *machine, what, mode string) bool {
	if mode == "busy" && !mc.busyDone[what] {
		mc.busyDone[what] = true
		return true
	}
	return false
}

func (w *world) Update(_ context.Context, id, _ string, grant any) error {
	mc := w.ms[id]
	w.calls = append(w.calls, "update "+id)
	w.arrived("update " + id)
	if !w.answers(mc) {
		return &OpError{Text: "agent unreachable: connection failed"}
	}
	if !w.hit(mc.info.AgentID) || w.busyOnce(mc, "update", mc.updMode) {
		return errBusy
	}
	if mc.updLost != "" && !mc.busyDone["update lost"] { // a timeout: the panel does not learn whether the agent got it
		mc.busyDone["update lost"] = true
		if mc.updLost == "arrives" {
			mc.updating, mc.updDoneAt, mc.updState, mc.updMsg = true, w.now.Add(mc.updTakes), "downloading", "downloading "+mc.newVersion
		}
		return errLost
	}
	switch mc.updMode {
	case "approval":
		if grant == nil {
			return &OpError{Code: CodeApproval, Text: "approval required: confirm this action with Pocket ID"}
		}
	case "refuse":
		return &OpError{Text: "an update was installed on this machine less than 10 minutes ago; refused, try again in 6m0s"}
	}
	mc.updating, mc.updDoneAt, mc.updState, mc.updMsg = true, w.now.Add(mc.updTakes), "downloading", "downloading "+mc.newVersion
	return nil
}

func (w *world) Shutdown(_ context.Context, id string, delay time.Duration, _ string, grant any) (time.Time, error) {
	mc := w.ms[id]
	w.calls = append(w.calls, fmt.Sprintf("shutdown %s %d", id, int(delay.Seconds())))
	w.arrived("shutdown " + id)
	mc.shutdowns++
	// the judge
	if mc.onAtStart {
		w.t.Errorf("RULE BROKEN: shutdown sent to %s, which was running when the run started", id)
	}
	if !mc.wakeAccepted { // also when a wake request left and got no answer: that is not a wake the run knows of
		w.t.Errorf("RULE BROKEN: shutdown sent to %s, for which the run never had a wake accepted", id)
	} else if *mc.onAtFirstWake && !mc.knownLimit {
		w.t.Errorf("RULE BROKEN: shutdown sent to %s, which was already running when the run's first wake request went out", id)
	}
	if mc.guests > 0 || mc.guestsWhy != "" {
		w.t.Errorf("RULE BROKEN: shutdown sent to %s with guests running or unknown", id)
	}
	if mc.updating && w.answers(mc) {
		w.t.Errorf("RULE BROKEN: shutdown sent to %s while its agent says that an update is being installed", id)
	}
	if !w.answers(mc) {
		return time.Time{}, &OpError{Text: "agent unreachable: connection failed"}
	}
	if !w.hit(mc.info.AgentID) || w.busyOnce(mc, "shutdown", mc.shutMode) {
		return time.Time{}, errBusy
	}
	switch mc.shutMode {
	case "approval":
		if grant == nil {
			return time.Time{}, &OpError{Code: CodeApproval, Text: "approval required: confirm this action with Pocket ID"}
		}
	case "refuse":
		return time.Time{}, &OpError{Text: "the elevated helper is stopped: start the helper first"}
	case "noroute": // the panel finds, before it sends anything, that the request would not go to the agent
		mc.shutdowns--
		return time.Time{}, &OpError{Code: CodeNoAgentRoute, Text: "its own agent can no longer take the request"}
	}
	if mc.pending != nil {
		return time.Time{}, &OpError{Text: "a " + mc.pending.Action + " is already pending; cancel it first"}
	}
	if mc.shutLost != "" && !mc.busyDone["shutdown lost"] { // a timeout: the panel does not learn whether the agent got it
		mc.busyDone["shutdown lost"] = true
		if mc.shutLost == "arrives" {
			mc.pending = &Pending{Action: "shutdown", At: w.now.Add(delay)}
		}
		return time.Time{}, errLost
	}
	mc.pending = &Pending{Action: "shutdown", At: w.now.Add(delay)}
	return mc.pending.At, nil
}

func (w *world) CancelShutdown(_ context.Context, id, _ string) error {
	mc := w.ms[id]
	w.calls = append(w.calls, "cancel "+id)
	w.arrived("cancel " + id)
	if mc.cancelLost != "" { // a timeout: the panel does not learn whether the agent got it
		if mc.cancelLost == "arrives" {
			mc.pending = nil
		}
		return errLost
	}
	if !w.answers(mc) && !mc.cancelOK {
		return &OpError{Text: "agent unreachable: connection failed"}
	}
	mc.pending = nil
	return nil
}

func (w *world) Guests(id string, _ time.Time) (int, bool, string) {
	mc := w.ms[id]
	w.guestsAsked++
	if mc.guestsWhy != "" {
		return 0, false, mc.guestsWhy
	}
	return mc.guests, true, mc.guestNames
}

func (w *world) Blocked() string { return w.blocked }

// ---- time

// sim moves the machines: an update finishes, a countdown ends.
func (w *world) sim() {
	for _, id := range w.order {
		mc := w.ms[id]
		if mc.updating && !w.now.Before(mc.updDoneAt) && mc.updMode != "stuck" {
			mc.updating = false
			at := float64(w.now.Unix())
			switch mc.updMode {
			case "current":
				mc.updState, mc.updMsg = "idle", "already up to date ("+mc.version+")"
			case "fail":
				mc.updState, mc.updMsg = "failed", "downloading the release: connection reset"
			case "rollback": // the agent restarts twice; the old version answers again and reports the failed result
				mc.agentReadyAt = w.now.Add(8 * time.Second)
				mc.updState, mc.updMsg, mc.lastAt, mc.lastOK, mc.lastMsg = "failed", "health check failed", at+8, false, "the new version did not come up: rolled back"
			case "noverdict": // the new version answers, and the installer's result never shows (an older agent)
				mc.agentReadyAt = w.now.Add(5 * time.Second)
				mc.version, mc.updState, mc.updMsg = mc.newVersion, "idle", ""
			default: // the agent restarts with the new version and reports the installer's result a little later
				mc.agentReadyAt = w.now.Add(5 * time.Second)
				mc.version, mc.updState, mc.updMsg, mc.lastAt, mc.lastOK, mc.lastMsg = mc.newVersion, "idle", "", at+5, true, "updated"
				no := false
				mc.behind = &no
			}
		}
		if mc.pending != nil && !w.now.Before(mc.pending.At) {
			switch {
			case mc.pending.Action == "shutdown" && mc.shutMode == "hang":
				mc.agentDown, mc.onBy = true, OnByRefused // the system hangs in its shutdown: it runs, its agent is gone
			case mc.pending.Action == "shutdown" && mc.shutMode != "ignore":
				mc.on, mc.wentOff = false, true
			}
			mc.pending = nil
		}
	}
}

// poll is the panel's poller: every machine is asked once a second; one that does not answer is "stale" for a
// minute and "offline" after that, keeps the values of its last answer, and is "quiet since" the first of
// these polls.
func (w *world) poll() {
	for _, id := range w.order {
		mc := w.ms[id]
		mc.obs.Paused = mc.paused
		if mc.paused {
			continue
		}
		mc.obs.OnBy = ""
		if mc.on {
			mc.obs.OnBy = mc.onBy
		}
		if !w.answers(mc) {
			if mc.obs.OnBy != "" { // it shows that it runs (the poller's rule): no silence, and none has begun
				mc.quietSince, mc.silN = time.Time{}, 0
			} else {
				if mc.quietSince.IsZero() {
					mc.quietSince = w.now
				}
				if mc.silN == 0 { // a new row of silent polls, after an answer or a sign of life
					w.rows++
					mc.silRow, mc.silFrom = w.rows, w.mono
				}
				mc.silN, mc.silMono = mc.silN+1, w.mono
			}
			mc.obs.Online, mc.obs.QuietSince = false, mc.quietSince
			if mc.everAnswered && w.now.Sub(mc.lastAnswer) < time.Minute {
				mc.obs.Status = "stale"
			} else {
				mc.obs.Status = "offline"
			}
			continue
		}
		mc.everAnswered, mc.lastAnswer, mc.quietSince, mc.silN = true, w.now, time.Time{}, 0
		o := Observation{Online: true, SeenAt: w.now, Status: "online", OnBy: mc.obs.OnBy, Version: mc.version, UpdateEnabled: mc.updMode != "disabled",
			UpdateState: mc.updState, UpdateMessage: mc.updMsg, LastAt: mc.lastAt, LastOK: mc.lastOK, LastMessage: mc.lastMsg, Behind: mc.behind}
		if float64(w.now.Unix()) < mc.lastAt { // the installer has not written its result yet
			o.LastAt, o.LastOK, o.LastMessage = 0, false, ""
		}
		if !mc.noUptime {
			up := int64(w.now.Sub(mc.bootedAt).Seconds())
			o.UptimeS = &up
		}
		if mc.uptimeSays != nil {
			up := *mc.uptimeSays
			o.UptimeS = &up
		}
		if mc.pending != nil {
			p := *mc.pending
			o.Pending = &p
		}
		if mc.staleOnce && mc.pending != nil { // a poll that left before the shutdown was asked for and comes back after it
			mc.staleOnce, o.Pending = false, nil
		}
		mc.obs = o
	}
}

// tick is one second of the world.
func (w *world) tick() {
	w.clock.Lock()
	w.now = w.now.Add(time.Second)
	w.mono += time.Second
	w.clock.Unlock()
	w.sim()
	w.poll()
	w.m.Tick(context.Background())
	if w.afterTick != nil {
		w.afterTick()
	}
}

func (w *world) advance(d time.Duration) {
	for end := w.now.Add(d); w.now.Before(end); {
		w.tick()
	}
}

// start begins a run and remembers, for the judge, which machines were running at that moment.
func (w *world) start(req StartRequest) *Run {
	w.t.Helper()
	if req.By == "" {
		req.By, req.BySub = "Ann", "sub-ann"
	}
	for _, mc := range w.ms {
		mc.onAtStart, mc.onAtFirstWake, mc.wakeAccepted, mc.wentOff = mc.on, nil, false, false
	}
	r, err := w.m.Start(req)
	if err != nil {
		w.t.Fatal("start:", err)
	}
	w.started = true
	return r
}

// finish lets the world run until the run is over and returns it, after checking what must hold for every
// run: it ends, every machine ends in an end state with a sentence and knows whether the machine is on, and
// there is one "ended" event per machine and one for the run.
func (w *world) finish() *Run {
	w.t.Helper()
	for i := 0; ; i++ {
		if a, _ := w.m.Snapshot(); a == nil {
			break
		}
		if i > 4*3600 {
			a, _ := w.m.Snapshot()
			w.t.Fatalf("the run did not end in four hours: %s", describe(a))
		}
		w.tick()
	}
	_, hist := w.m.Snapshot()
	if len(hist) == 0 {
		w.t.Fatal("no finished run")
	}
	r := hist[0]
	w.wellReported(r)
	return r
}

func (w *world) wellReported(r *Run) {
	w.t.Helper()
	if r.State == RunRunning || r.EndedAt == nil || r.Summary == "" {
		w.t.Errorf("the run is not properly ended: %s", describe(r))
	}
	ends, runEnds := map[string]int{}, 0
	for _, e := range w.events {
		if e.Run.ID != r.ID {
			continue
		}
		switch e.Kind {
		case EvHostEnd:
			ends[e.Host.ID]++
		case EvRunEnd:
			runEnds++
		}
	}
	if runEnds != 1 {
		w.t.Errorf("%d \"run ended\" events, want 1", runEnds)
	}
	for _, h := range r.Hosts {
		if !h.Final() || strings.TrimSpace(h.Text) == "" || h.On == "" || len(h.Log) == 0 {
			w.t.Errorf("%s is not properly ended: state %q on %q text %q", h.ID, h.State, h.On, h.Text)
		}
		if ends[h.ID] != 1 {
			w.t.Errorf("%s: %d \"ended\" events, want 1", h.ID, ends[h.ID])
		}
		if !strings.Contains(r.Summary, h.Name+": "+h.Text) {
			w.t.Errorf("the summary lacks %s", h.ID)
		}
		// what the run believes about the machine's power must not be wrong
		mc := w.ms[h.ID]
		// (since r2-p2-05 without an exception: "on" for a machine that is off, or "off" for one that is on, in
		// whatever state it ended, fails the test)
		if mc != nil && ((h.On == OnNo && (!mc.wentOff || h.State != StOff)) || (h.State == StOff && h.On != OnNo) ||
			(h.On == OnYes && !mc.on) || (h.On == OnNo && mc.on) || (h.On == OnUntouched && (mc.wakes > 0 || mc.shutdowns > 0))) {
			w.t.Errorf("%s: the run says it is %q, the machine is on=%v (%s: %s)", h.ID, h.On, mc.on, h.State, h.Text)
		}
	}
}

func describe(r *Run) string {
	if r == nil {
		return "no run"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "run %s", r.State)
	for _, h := range r.Hosts {
		fmt.Fprintf(&b, "\n  %s [%s] %s", h.ID, h.State, h.Text)
	}
	return b.String()
}

func host(r *Run, id string) *Host {
	if r == nil {
		return &Host{}
	}
	for _, h := range r.Hosts {
		if h.ID == id {
			return h
		}
	}
	return &Host{}
}

func (w *world) callsFor(id string) string {
	var out []string
	for _, c := range w.calls {
		f := strings.Fields(c)
		if f[1] == id {
			out = append(out, f[0])
		}
	}
	return strings.Join(out, " ")
}

// steps are the audit events for one machine: "wake:ok update:ok shutdown:ok".
func (w *world) steps(id string) string {
	var out []string
	for _, e := range w.events {
		if e.Kind == EvStep && e.Host.ID == id {
			out = append(out, e.Action+":"+e.Outcome)
		}
	}
	return strings.Join(out, " ")
}

func sorted(m map[string]int) string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(out, " ")
}
