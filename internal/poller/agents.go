package poller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"runtime"
	"slices"
	"syscall"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/release"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/normalise"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/ups"
)

// AgentByID returns the configured agent with this id.
func (p *Poller) AgentByID(id string) (config.Agent, bool) {
	p.cfgMu.RLock()
	defer p.cfgMu.RUnlock()
	a, ok := p.agents[id]
	return a, ok
}

// RelaySystem returns the entry the poller keeps for an agent's own answers: the agent's OWN entry whenever it
// was polled once (online or not), and only for an agent that was never polled the entry of the node it is
// linked to. It is never the row a node's agent is shown as on the pages (ShownSystem). The Wake-on-LAN and
// BMC relay lookups ask here: a relay's status is that of the agent that sends the packet.
func (p *Poller) RelaySystem(agentID string) *model.System {
	a, ok := p.AgentByID(agentID)
	if !ok {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.systems[agentID]; ok {
		return &s
	}
	if s, ok := p.systems[p.linkOfLocked(a)]; ok {
		return &s
	}
	return nil
}

// ShownSystem returns the row an agent is shown as on the pages: the Proxmox node the snapshot makes it a part
// of, else its own entry; nil when the agent has no entry yet (never polled) or is not configured. The rule is
// buildSnapshot's, word for word: the agent has an entry of its own, and its link (linkOfLocked) is an entry of
// kind "node". Everything that tells a page "which row is this agent" asks here, so that the id it names is
// the id of a row the browser has: with the node's row there, the node's id; after a restart of the panel
// with the machine off (no node row until its Proxmox API answers), the agent's id.
func (p *Poller) ShownSystem(agentID string) *model.System {
	a, ok := p.AgentByID(agentID)
	if !ok {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	own, okA := p.systems[agentID]
	if !okA {
		return nil
	}
	if node, okN := p.systems[p.linkOfLocked(a)]; okN && node.Kind == "node" {
		return &node
	}
	return &own
}

// LinkOf returns the Proxmox node (or guest) this agent runs on: links_to, or `node:` matched by node name.
func (p *Poller) LinkOf(a config.Agent) string { return p.linkOfLocked(a) }

// syncPresent refreshes the id set used by LinkOf. Caller holds p.mu.
func (p *Poller) syncPresent() {
	p.idsMu.Lock()
	defer p.idsMu.Unlock()
	clear(p.present)
	for k := range p.systems {
		p.present[k] = true
	}
}

// linkOfLocked only takes idsMu (safe with or without p.mu held).
func (p *Poller) linkOfLocked(a config.Agent) string {
	if a.LinksTo != "" {
		return a.LinksTo
	}
	if a.Node != "" {
		var ids []string
		for _, c := range p.Clusters() {
			ids = append(ids, c.Prefix()+"node-"+normalise.Slug(a.Node))
		}
		p.idsMu.Lock()
		defer p.idsMu.Unlock()
		for _, i := range ids {
			if p.present[i] {
				return i
			}
		}
		if len(ids) > 0 {
			return ids[0]
		}
	}
	return ""
}

// pollAgent returns a back-off delay (s) while the agent is unreachable, so offline hosts aren't hit every second.
func (p *Poller) pollAgent(ctx context.Context, a config.Agent, timeout time.Duration) float64 {
	start := time.Now()
	began := p.mono() // the steady clock before the request leaves (quietRow.began)
	name := "agent:" + a.ID
	if timeout <= 0 {
		timeout = time.Duration(p.Cfg.RequestTimeout * float64(time.Second))
	}
	asked, _ := p.now()
	p.mu.Lock()
	p.clampMachineLocked(a.ID, asked) // a record is never newer than the panel's clock (machines.go)
	p.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, timeout+500*time.Millisecond)
	payload, err := p.Backend.FetchAgentMetrics(cctx, a, timeout)
	cancel()
	defer p.Bump()
	if err != nil {
		msg := textsafe.ErrorText(devices.Describe(err))
		interval := p.agentInterval(a.ID) // before p.mu: it asks the scheduler
		p.mu.Lock()
		now, _ := p.now()
		seenDue := false
		if prev, ok := p.sources[name]; !ok || prev.OK {
			slog.Info("agent poll failed", "agent", a.ID, "error", msg)
			seenDue = ok && p.noteGoneLocked(a.ID, now) // it answered until now: when it was last seen is written soon (lastseen.go)
		}
		if prev, ok := p.systems[a.ID]; ok {
			p.systems[a.ID] = p.degrade(prev, now, msg)
		} else {
			s := model.System{ID: a.ID, Name: a.DisplayName(), Kind: "agent", Source: "agent", Status: "offline",
				Group: a.Group, Owner: a.Owner, Tags: slices.Clone(a.Tags), Error: model.P(msg), PollMode: "default"}
			if a.DWServiceURL != "" {
				s.DWServiceURL = model.P(a.DWServiceURL)
			}
			model.Normalize(&s)
			p.systems[a.ID] = s
			p.syncPresent()
		}
		p.agentPower[a.ID] = nil
		p.source(name, false, now, nil, &msg)
		p.agentFailures[a.ID]++
		q, row := p.agentQuiet[a.ID]
		sign, mono := lifeSign(err), p.mono()
		// What this request itself got (q.nothing, q.from). The machine took part in it, or it did not: nothing
		// that others say about the machine has a word here (see quietRow.from).
		switch {
		case sign != "":
			q.nothing = false
		case !q.nothing:
			// the first poll whose own request got nothing since the machine answered or took part in a poll
			q.nothing, q.from = true, began
		}
		switch {
		case sign != "" || p.nodeOnlineLocked(a):
			// A sign of life, like an answer: the machine runs and only its agent gives no usable answer. The
			// silence starts again with the first poll after this one that gets nothing at all. (For the agent of
			// a Proxmox node the cluster's row is such a sign for this question, "has it been off for a quarter
			// of an hour", and for it only: q.from above does not move by it.)
			q.silent = false
		case !row || !q.silent:
			// the first poll without any answer since the machine answered or showed a sign of life
			q.silent, q.since = true, mono
			p.quietRows++
			q.row, q.n = p.quietRows, 0
		case q.unwatched(now, mono, p.quietGap(max(q.interval, interval))):
			// The panel was not watching in between: the row and its count start again. q.from stays where it
			// is: the machine has given nothing since, as far as the panel asked it.
			q.since = mono
			p.quietRows++
			q.row, q.n = p.quietRows, 0
		}
		if q.silent {
			q.n, q.began = q.n+1, began
		}
		q.last, q.lastMono, q.sign, q.interval = now, mono, sign, interval
		p.agentQuiet[a.ID] = q
		n := p.agentFailures[a.ID]
		p.mu.Unlock()
		if seenDue {
			p.flushSeen()
		}
		p.Power.UpdateRemote(a.ID, nil, true, a.PowerLinks, a.PowerExcludeFromTotal)
		p.UPS.UpdateFromAgent(a.ID, nil, true, a.UPSPowers, a.UPSNames)
		if n >= 2 {
			return math.Min(agentBackoffMax, math.Pow(2, float64(n-2)))
		}
		return 0
	}
	now, ts := p.now()
	sys := normalise.NormaliseAgent(a, payload, now, ts, p.rates)
	info := agentInfoOf(payload, now)
	info.AskedAt = asked
	info.seenMono = p.mono()          // the same moment as SeenAt, on the steady clock (AgentInfo.SeenAgo)
	interval := p.agentInterval(a.ID) // before p.mu: it asks the scheduler
	p.mu.Lock()
	// upMono: where the row of answers that this one belongs to began (AnsweredSinceUp). An answer after a gap
	// that the poll loop itself cannot make (quietGap) begins a new row: the machine was off, or the panel was
	// not watching it. A poll or two without an answer (the agent restarts after its update) does not.
	if prev, had := p.agentInfo[a.ID]; had && info.seenMono-prev.seenMono <= p.quietGap(interval) {
		info.upMono = prev.upMono
	} else {
		info.upMono = info.seenMono
	}
	if sys.Helper != nil {
		normalise.ApplyHelperPause(a, &sys, normalise.HelperUnavailable(payload), p.helperSeenLocked(a.ID, sys, now))
	} else {
		delete(p.helperSeen, a.ID)
	}
	p.agentInfo[a.ID] = info
	newVersion := p.noteVersionLocked(a.ID, info.Version, now)
	seenDue := p.noteSeenLocked(a.ID, now) // for the next start of the panel (lastseen.go)
	delete(p.agentFailures, a.ID)
	delete(p.agentQuiet, a.ID)
	delete(p.knownOff, a.ID) // it answers: it is not off (knownoff.go)
	p.systems[a.ID] = p.stamp(sys, now)
	// what it says about its own check of approvals, against the panel's record (machines.go)
	p.queueMachinesLocked(append(p.noteMachineLocked(a.ID, sys.Approval, now), p.announceLocked()...))
	p.syncPresent()
	p.agentPower[a.ID] = sys.ComponentPower
	lat := time.Since(start)
	p.source(name, true, now, &lat, nil)
	p.mu.Unlock()
	if newVersion {
		p.saveVersion(a.ID, info.Version, now)
	}
	if seenDue {
		p.flushSeen()
	}
	p.machinesChanged()
	p.Power.UpdateRemote(a.ID, jsonx.Objs(payload["power_sources"]), false, a.PowerLinks, a.PowerExcludeFromTotal)
	p.UPS.UpdateFromAgent(a.ID, ups.AgentRows(payload["ups"]), false, a.UPSPowers, a.UPSNames)
	return 0
}

// helperSeenFor is how long a helper reading counts as "this host has it" after it was last seen while the
// helper ran (a sensor that stopped answering a while before the helper was stopped is not announced as paused).
const helperSeenFor = 10 * time.Minute

// helperSeenLocked records which helper readings the host has while its helper runs, and returns the groups
// seen recently enough (nil: the panel never saw the helper running since it started). Caller holds p.mu.
func (p *Poller) helperSeenLocked(id string, s model.System, now time.Time) map[string]bool {
	seen := p.helperSeen[id]
	if s.Helper.State == "running" {
		if seen == nil {
			seen = map[string]time.Time{}
			p.helperSeen[id] = seen
		}
		for _, g := range normalise.HelperGroupsIn(s) {
			seen[g] = now
		}
		seen[""] = now // the last poll with the helper running
	}
	if seen == nil {
		return nil
	}
	out := map[string]bool{}
	for g, at := range seen {
		if g != "" && seen[""].Sub(at) <= helperSeenFor {
			out[g] = true
		}
	}
	return out
}

// AgentInfo is what the Updates page shows about an agent, from its last good /metrics answer.
type AgentInfo struct {
	Version    string         // agent_version
	SelfUpdate map[string]any // the agent's self_update block (nil: off or an older agent)
	Enabled    bool           // features.self_update
	SeenAt     time.Time
	// SeenAgo: how long ago that answer came, on the steady clock. SeenAt is a time of day: it can be compared
	// with another time of day only as long as nobody set the clock in between. A caller that asks "did this
	// answer come after a moment of mine" goes by SeenAgo and places it on a steady clock of its own (the wake /
	// update / shut down run: "from after my shutdown request"; review of fix7r2, R2-01).
	SeenAgo  time.Duration
	seenMono time.Duration // the steady clock when the answer came
	upMono   time.Duration // the steady clock when the first answer of this row of answers came (AnsweredSinceUp)
	// AskedAt: when the poll that got the last good answer was sent (the answer says nothing about what the
	// panel did to the machine after that moment: machines.go).
	AskedAt time.Time
	Online  bool // the latest poll succeeded
	// QuietSince: since when the panel has asked the agent, poll after poll, and got nothing at all: no answer
	// and no other sign that the machine runs. Zero while it answers, while it shows that it runs without a
	// usable answer (see Sign; or it is a Proxmox node that the cluster shows as online), while it was never
	// polled, and when the panel stopped asking (see quietGap). So "quiet for 15 minutes" is 15 minutes since
	// the last sign of life, never 14 minutes of refusals and one of silence. It is measured with the steady
	// clock and given as a time of day (quietLocked).
	QuietSince time.Time
	// Refused: the latest poll was refused by the machine itself (see ProbeRefused): it runs, and only its agent
	// is not listening.
	Refused bool
	// Sign: what in the latest failed poll shows that the machine runs (lifeSign: SignRefused, an HTTP status,
	// an answer that is not the agent's, a connection that was made and then gave nothing); "": nothing did.
	Sign string
	// Silent: the row of polls without any sign of life that QuietSince is the beginning of, poll by poll.
	Silent SilentPolls
}

// agentBackoffMax is the longest pollAgent puts off the next poll of an agent that does not answer (seconds).
const agentBackoffMax = 10

// quietGap is the longest time between two failed polls of an agent that still counts as one row of them,
// for an agent that is polled every interval seconds. It is what the poll loop itself can take between two
// polls of a machine that does not answer, and a little more: the interval (or the back-off of a failing
// agent, whichever is longer), one request that runs into its timeout, and 15 seconds (a tenth of the interval
// when that is more) for a busy panel. With the default interval of 1 s that is under half a minute; with the
// slowest one (300 s) five and a half minutes. A longer gap means the panel was not watching (polling was
// paused, the panel's host slept, a clock was set): what the machine did meanwhile is not known, so the row
// starts again. It used to be six minutes for every agent, so that four polls with three pauses of just under
// six minutes made "no answer for 17 minutes" (audit 5, r1-p2-04).
func (p *Poller) quietGap(interval float64) time.Duration {
	s := math.Max(interval, agentBackoffMax) + p.Cfg.RequestTimeout + 0.5 + math.Max(15, interval/10)
	return time.Duration(s * float64(time.Second))
}

// agentInterval is the interval an agent is polled at now, in seconds (0: it is not polled, or paused). It
// asks the scheduler, so it must not be called with p.mu held.
func (p *Poller) agentInterval(id string) float64 {
	iv, _ := p.Scheduler.IntervalOf("agent:" + id)
	return iv
}

// quietRow is an agent's unbroken row of failed polls: its latest one (by the time of day and by the steady
// clock), what in that one showed that the machine runs (lifeSign; "": nothing), the interval it was polled
// at then, and, while the polls of the row get nothing at all (silent), since when on the steady clock.
//
// The silence is measured with the steady clock only (Poller.mono): the time of day can be set, by a person
// or a time server, and a step forward must not turn nine minutes of silence into fifteen. The time of day is
// still read at every poll, to notice that the panel was not watching: see unwatched.
type quietRow struct {
	last     time.Time     // the latest failed poll, by the time of day
	lastMono time.Duration // the same moment on the steady clock
	since    time.Duration // steady clock: the first poll of the row that got nothing at all (while silent)
	silent   bool          // the latest poll got nothing at all (no sign of life)
	sign     string
	interval float64 // seconds: the interval the agent was polled at when the latest poll ran
	// While silent: which row of silent polls this is (another number is another row: an answer, a sign of
	// life or a time the panel did not watch lies between), how many polls it has, and when the latest of them
	// began on the steady clock (lastMono is when it ended). See SilentPolls.
	row   uint64
	n     int
	began time.Duration
	// nothing: the latest poll's own request got nothing at all: no answer, and the machine took no part in it
	// (lifeSign). from: when the first such poll began since the machine last answered or took part in a poll
	// (steady clock). It is not the beginning of the row: a row also starts again when the panel was not watching
	// for a while (unwatched), and the machine did not answer in between.
	//
	// Only the machine's own word moves from: an answer, or a sign of life in the request itself (a refused or
	// reset connection, any HTTP status). What the Proxmox cluster says about a node does not (nodeOnlineLocked):
	// its row is what the panel last read from the Proxmox API, and that lags behind a node that has dropped
	// out by the cluster's own delay and the panel's poll interval. So silent, since and the row go by the
	// cluster's row as well (a node that the cluster shows as online is not "off for a quarter of an hour"), and
	// nothing and from never do: a poll that got nothing is the beginning of "out of reach" whatever the
	// cluster's row said at that moment (recheck of fix7r, RC-01: the row said "online" for 25 s after the link
	// to a node had dropped, the silence was counted from when the row turned, after the countdown's end, and
	// the run wrote "switched off again" for a node that was on). See SilentPolls.SinceAgo.
	nothing bool
	from    time.Duration
}

// SilentPolls describes the unbroken row of polls of an agent that got nothing at all (no answer and no other
// sign that the machine runs), up to the latest poll. N is 0 while the latest poll was answered or showed a
// sign of life, while the agent was never polled, and when the panel stopped asking (quietGap).
//
// It is for a caller that must know that silence was seen by polls made after a moment of its own (the wake /
// update / shut down run: "off" after a countdown). QuietSince alone does not say that: it is when the row
// began, and a row can be one poll that failed before polling was paused. The times are given as "how long
// ago" on the steady clock, so that the caller can place them on a steady clock of its own; Row tells the
// caller whether two looks saw the same row.
//
// SinceAgo is older than the row when the row started again without an answer in between: the panel was not
// watching for a while (the time of day was set, the panel's host was suspended, polling was paused), and a
// new row began with the next poll. The count starts again then; what the machine last gave does not change.
// A caller that asks "was it already out of reach at a moment of mine" must go by SinceAgo, not by the row
// (second review of fix7a, R7A-01 and R7A-02).
//
// For the agent of a Proxmox node SinceAgo is also older than the row, and is given without a row (N is 0),
// when the agent's polls got nothing while the cluster still showed the node as online: such a poll is not in
// the row (the node is not "silent" while the cluster shows it as running), and it is where "out of reach"
// begins all the same (quietRow.from; recheck of fix7r, RC-01).
type SilentPolls struct {
	Row      uint64        // names the row
	N        int           // polls in the row
	BeganAgo time.Duration // how long ago the latest of them began (the request left) ...
	EndedAgo time.Duration // ... and ended without an answer
	// SinceAgo: how long ago the first poll began whose own request got nothing at all since the machine's last
	// answer or the last poll it took part in (a refused connection, an HTTP status), whatever rows were
	// started since and whatever the Proxmox cluster showed. Never shorter than BeganAgo. 0 with N == 0: the
	// latest poll was answered or the machine took part in it, or nothing is known.
	SinceAgo time.Duration
}

// unwatched reports whether more time than gap lies between the row's latest poll and now, on either clock,
// or a clock went back. The two clocks see different things: the steady clock does not count the time the
// panel's host was suspended, the time of day does; a paused poller or a stuck panel shows on both. A step
// of the time of day that is larger than the gap also ends the row. That costs a machine that is off another
// 15 minutes before it is offered, and never offers one that is not.
func (q quietRow) unwatched(now time.Time, mono, gap time.Duration) bool {
	return now.Before(q.last) || now.Sub(q.last) > gap || mono < q.lastMono || mono-q.lastMono > gap
}

// SignRefused is the sign of life of a machine that turned the connection down (see refused).
const SignRefused = "the machine refused the connection"

// lifeSign says what, in the error of a request to an agent, shows that the machine itself runs; "" when
// nothing does. Only then did the request meet silence: no route to the machine, no answer to the attempt to
// connect, a name that does not resolve. Everything else needs a machine that is switched on at that address:
//
//   - it refused, reset or closed the connection (SignRefused): its network stack answered, its agent does not
//     listen;
//   - it sent an answer the panel cannot use: an HTTP status (401 after its token changed, 500), a body that
//     is not the agent's JSON, a certificate the panel does not accept;
//   - it took the connection (the TCP handshake was completed for this very request) and then sent nothing
//     in time: an agent that hangs, or one that is being stopped.
//
// Each of these starts the 15 minutes of "nothing at all" again, exactly like an answer (pollAgent), so a
// machine that runs with a broken agent is never offered to a run as "off".
func lifeSign(err error) string {
	if err == nil {
		return ""
	}
	if refused(err) {
		return SignRefused
	}
	d := devices.Describe(err)
	var he *devices.HTTPStatusError
	var re *reachedError
	switch {
	case errors.As(err, &he), d == "invalid response":
		return "its address answered: " + d
	case errors.As(err, &re) && (re.answered || d == "tls verification failed"): // a certificate is an answer too
		return "its address answered: " + d
	case errors.As(err, &re):
		return "its address took the connection and gave no answer: " + d
	}
	return ""
}

// reachedError is the error of a request to an agent in which the machine itself took part: it completed a
// connection that was opened for this request, or sent at least one byte back (answered). The HTTP backend
// wraps such an error in it (FetchAgentMetrics); what the error says (devices.Describe) stays the same. A
// request that went out on a connection kept from an earlier poll and then got nothing is not wrapped: that
// connection says nothing about now (the machine may have lost power since), and the next poll opens a new one.
type reachedError struct {
	err      error
	answered bool
}

func (e *reachedError) Error() string { return e.err.Error() }
func (e *reachedError) Unwrap() error { return e.err }

// nodeOnlineLocked reports whether the agent is the one of a Proxmox node that the cluster shows as online
// right now: the node runs, whatever its agent does. Caller holds p.mu.
func (p *Poller) nodeOnlineLocked(a config.Agent) bool {
	id := p.linkOfLocked(a)
	if id == "" || id == a.ID {
		return false
	}
	s, ok := p.systems[id]
	return ok && s.Kind == "node" && s.Status == "online"
}

// quietLocked is AgentInfo.QuietSince and AgentInfo.Sign for an agent that is polled every interval seconds
// now (agentInterval, read before the lock). Caller holds p.mu. QuietSince is given as a time of day because
// that is what the callers compare with: it is now minus the silence the steady clock measured, so a clock
// that was set meanwhile moves it along and changes nothing about how long the silence is.
func (p *Poller) quietLocked(id string, interval float64) (time.Time, string, SilentPolls) {
	q, ok := p.agentQuiet[id]
	now, _ := p.now()
	mono := p.mono()
	if !ok || q.unwatched(now, mono, p.quietGap(max(q.interval, interval))) {
		return time.Time{}, "", SilentPolls{}
	}
	if !q.silent {
		// No row. For a node that the cluster shows as online while its agent's own polls get nothing, the
		// beginning of that is still said (quietRow.from).
		var s SilentPolls
		if q.nothing {
			s.SinceAgo = mono - q.from
		}
		return time.Time{}, q.sign, s
	}
	return now.Add(-(mono - q.since)), q.sign, SilentPolls{Row: q.row, N: q.n, BeganAgo: mono - q.began, EndedAgo: mono - q.lastMono, SinceAgo: mono - q.from}
}

func agentInfoOf(payload jsonx.M, now time.Time) AgentInfo {
	info := AgentInfo{SeenAt: now}
	if v, ok := payload["agent_version"].(string); ok && release.ValidVersion(v) {
		info.Version = v
	}
	if su, ok := payload["self_update"].(map[string]any); ok {
		info.SelfUpdate = SelfUpdateBlock(su)
	}
	if f, ok := payload["features"].(map[string]any); ok {
		info.Enabled, _ = f["self_update"].(bool)
	}
	return info
}

var updateStates = map[string]bool{"idle": true, "downloading": true, "applying": true, "failed": true, "unavailable": true}

// maxUnixTime bounds the timestamps an agent reports (seconds; year 2286), so the page can always show them.
const maxUnixTime = 1e10

// SelfUpdateBlock keeps only the fields of an agent's self_update block that the Updates page shows, with the
// right types and bounded lengths: an agent (or whoever answers in its place) cannot break the page with an
// odd payload. Unknown states read as "unknown".
func SelfUpdateBlock(su map[string]any) map[string]any {
	text := func(m map[string]any, k string, n int) (string, bool) {
		v, ok := m[k].(string)
		if !ok {
			return "", false
		}
		return normalise.ClipText(v, n), true
	}
	when := func(m map[string]any, k string) (float64, bool) {
		f := jsonx.Num(m, k)
		return model.Deref(f), f != nil && *f >= 0 && *f <= maxUnixTime
	}
	out := map[string]any{"state": "unknown"}
	if st, _ := su["state"].(string); updateStates[st] {
		out["state"] = st
	}
	if v, ok := text(su, "message", 300); ok {
		out["message"] = v
	}
	if v, ok := su["target"].(string); ok && release.ValidVersion(v) {
		out["target"] = v
	}
	if v, ok := when(su, "since"); ok {
		out["since"] = v
	}
	if last, ok := su["last"].(map[string]any); ok {
		l := map[string]any{"ok": jsonx.Bool(last, "ok", false)}
		for _, k := range []string{"from", "to"} {
			if v, ok := last[k].(string); ok && release.ValidVersion(v) {
				l[k] = v
			}
		}
		if v, ok := text(last, "id", 64); ok {
			l["id"] = v
		}
		if v, ok := text(last, "message", 300); ok {
			l["message"] = v
		}
		if v, ok := when(last, "at"); ok {
			l["at"] = v
		}
		out["last"] = l
	}
	return out
}

// AgentInfo returns the version and self-update state from the agent's last good poll.
func (p *Poller) AgentInfo(id string) (AgentInfo, bool) {
	interval := p.agentInterval(id)
	p.mu.Lock()
	defer p.mu.Unlock()
	i, ok := p.agentInfo[id]
	_, failing := p.agentFailures[id]
	i.Online = ok && !failing
	if ok {
		i.SeenAgo = p.mono() - i.seenMono
	}
	i.QuietSince, i.Sign, i.Silent = p.quietLocked(id, interval)
	i.Refused = i.Sign == SignRefused
	return i, ok
}

// AgentSeen is AgentInfo together with the agent's own entry (before it is merged into a Proxmox node), both
// read in one step: the entry's uptime and pending power action then belong to the poll that SeenAt names.
// While the agent does not answer, the entry keeps the values of its last good poll.
func (p *Poller) AgentSeen(id string) (AgentInfo, model.System, bool) {
	interval := p.agentInterval(id)
	p.mu.Lock()
	defer p.mu.Unlock()
	i, ok := p.agentInfo[id]
	_, failing := p.agentFailures[id]
	i.Online = ok && !failing
	if ok {
		i.SeenAgo = p.mono() - i.seenMono
	}
	i.QuietSince, i.Sign, i.Silent = p.quietLocked(id, interval)
	i.Refused = i.Sign == SignRefused
	s, known := p.systems[id]
	return i, s, known
}

// AnsweredSinceUp: has the source (as in the health list: "proxmox:<cluster id>") given a good answer after the
// agent came up, by the steady clock? "Came up" is the first answer of the agent's present row of answers:
// the one that ended its last silence longer than quietGap (the machine was off, or the panel was not
// watching), or its first answer since the panel started. Both moments are the panel's own, taken when the
// two answers arrived, and no time of day is part of the question: SourceStatus.LastSuccess and
// AgentInfo.SeenAt are times of day, and after the panel's clock was set back an old answer of the source
// looks newer than everything that came after the setting.
//
// It is for the wake / update / shut down run's "which guests run on this Proxmox node": the cluster's list
// counts only when it is from after the node came online (before that it shows the node's guests as they were
// while the node was off). After a later silence of the agent that is longer than quietGap the list must be
// newer than the agent's return as well: the node may have restarted meanwhile. False when the panel cannot
// tell: no good answer from the source yet, or none from the agent.
func (p *Poller) AnsweredSinceUp(source, agentID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	at, ok := p.sourceMono[source]
	i, seen := p.agentInfo[agentID]
	return ok && seen && at > i.upMono
}

// ApprovalOf is what the agent last said about its own check of passkey approvals (the "approval" block of its
// /metrics), with the panel's decision about its binding (Approval.Binding). It is the agent's own entry, also
// for an agent that runs on a Proxmox node. While the agent does not answer, the last answer stays; for an
// agent the panel has not heard from since it started, or that answers without the block, it is made from the
// panel's record of that machine (Approval.LastKnown), so that a machine that had the check never reads as one
// without it. nil: an agent that sends no block and has no record.
func (p *Poller) ApprovalOf(agentID string) *model.Approval {
	p.mu.Lock()
	defer p.mu.Unlock()
	var live *model.Approval
	if s, ok := p.systems[agentID]; ok {
		live = s.Approval
	}
	return p.shownApprovalLocked(agentID, live, nil)
}

// MachineChecks reports whether the agent's machine must be treated as one that checks approvals itself, or
// whose binding is an alarm (approval.Binding.Checks): it is then sent no fan, power or update request on the
// strength of the panel's own confirmation, and is in no run.
func (p *Poller) MachineChecks(agentID string) bool {
	return p.BindingOf(agentID).Checks()
}

// ProbeRefused is what ProbeAgent says when the machine itself turned the connection down.
const ProbeRefused = "connection refused"

// ProbeReached starts what ProbeAgent says when the machine took the connection and the request then ended in
// a timeout or a broken connection: the machine runs, so this is never the "timeout" of a machine that is off.
const ProbeReached = "it took the connection and gave no answer: "

// ProbeAgent asks an agent for its metrics now and says how that went: "" when it answered; "timeout" or
// "connection failed" when nothing did (the words of devices.Describe); ProbeRefused when the connection was
// refused, reset or closed without an answer; anything else ("http 401", "invalid response", "tls verification
// failed", ProbeReached + ...) when something answered that the panel could not use. A refused connection is
// not "nothing answered": the machine's own network stack sent the refusal, so the machine is on and only its
// agent is not listening. The same holds for a connection the machine took and then left without an answer:
// that is never reported as "timeout" or "connection failed" (lifeSign). It does not change what the poller
// knows. Unlike the poller's view, which can be up to offline_after old or the result of a poll that is being
// retried more slowly, the answer is about this moment.
func (p *Poller) ProbeAgent(ctx context.Context, id string) string {
	a, ok := p.AgentByID(id)
	if !ok {
		return "error"
	}
	timeout := time.Duration(p.Cfg.RequestTimeout * float64(time.Second))
	cctx, cancel := context.WithTimeout(ctx, timeout+500*time.Millisecond)
	defer cancel()
	if _, err := p.Backend.FetchAgentMetrics(cctx, a, timeout); err != nil {
		d := devices.Describe(err)
		if d != "timeout" && refused(err) {
			return ProbeRefused
		}
		if (d == "timeout" || d == "connection failed") && lifeSign(err) != "" { // not silence: the machine took part
			return ProbeReached + d
		}
		return d
	}
	return ""
}

// Windows has numbers of its own for a refused and a reset connection, and Go does not map them to the names
// below.
const (
	wsaECONNRESET   = syscall.Errno(10054)
	wsaECONNREFUSED = syscall.Errno(10061)
)

// refused reports whether a request failed because the other side turned it down: it refused the connection,
// reset it, or closed it before an answer. Each of these is sent by a machine that is running.
func refused(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if closedIdle(err) {
		return true
	}
	var en syscall.Errno
	return runtime.GOOS == "windows" && errors.As(err, &en) && (en == wsaECONNREFUSED || en == wsaECONNRESET)
}

// closedIdleText is net/http's error for a connection that the other side closed before the request was
// written to it (transport.go, errServerClosedIdle). The machine that takes a connection and closes it without
// a word ends as io.EOF or a reset most of the time, and as this one when the close is seen first (about 1 in
// 400 on loopback). A GET gets it only on a connection that was opened for this very request: on a kept one
// net/http tries again by itself. So it is the same sign of a running machine as the other two.
//
// The value is not exported and wraps nothing, so errors.Is cannot name it: the text of one error in the
// chain is compared, whole. TestGoStillNamesAConnectionClosedBeforeTheRequest fails when a Go version words
// it differently.
const closedIdleText = "http: server closed idle connection"

func closedIdle(err error) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if e.Error() == closedIdleText {
			return true
		}
	}
	return false
}

// FetchProcesses asks an agent for its top processes.
func (p *Poller) FetchProcesses(ctx context.Context, a config.Agent, sid string, limit int, sortBy string) (model.ProcessList, error) {
	return p.Backend.FetchProcesses(ctx, a, sid, limit, sortBy)
}

// AgentRequest calls an agent endpoint (GET with the read token, POST with the control token).
func (p *Poller) AgentRequest(ctx context.Context, a config.Agent, method, path string, body any) (int, jsonx.M, error) {
	return p.Backend.AgentRequest(ctx, a, method, path, body)
}

// AgentFor returns the agent reporting for a system id (itself or linked).
func (p *Poller) AgentFor(id string) (config.Agent, bool) {
	if a, ok := p.AgentByID(id); ok {
		return a, true
	}
	for _, a := range p.Agents() {
		if p.linkOfLocked(a) == id {
			return a, true
		}
	}
	return config.Agent{}, false
}
