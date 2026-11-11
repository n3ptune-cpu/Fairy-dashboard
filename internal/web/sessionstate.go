package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// liveSessions is what the panel keeps in memory about browser sessions, keyed by session id. None of it is in
// the cookie, so a copy of the cookie carries none of it, and none of it survives a restart (everything here
// fails closed when it is missing: a kept action is simply gone, and a sign-in confirmation has to be given
// again).
//
// It hangs off the session codec (Server.codec.live), next to the session clock.
type liveSessions struct {
	mu sync.Mutex
	m  map[string]*liveSession
	// max bounds the map (maxLiveSessions), and account says whose session an id is, "" when it is not
	// registered any more (adminstate.SessionAccount): what prune needs to make room without taking anything
	// from an account that is not the one filling the map.
	max     int
	account func(sid string) string
	// ended holds the ids of sessions that were ended while sessions.json could not be written (T-29): the
	// file still lists them, the panel does not take them any more (end, isEnded). Session id -> when.
	ended map[string]int64
	// steps remembers the step-up round trips of the last two minutes, by the session id they started with and
	// by the one they ended with (steprepeat.go).
	steps map[string]*stepRun

	streams streamSlots // how many live-update streams are open (stream.go)
}

// liveSession is one session's in-memory state.
type liveSession struct {
	pending *pendingAction // the control action waiting for a step-up sign-in (see pending.go)
	result  *keptResult    // the outcome of the action the panel ran after the step-up, until /api/me shows it
	stepUp  *stepUpRec     // the last confirmed Pocket ID sign-in (nil: none since the panel started)
	seen    int64          // Unix seconds of the last real use (0: not known since the panel started)
	touched int64          // Unix seconds of the last change, for pruning
	saved   int64          // Unix seconds of the last time seen went to sessions.json (dueSave)
	addrs   []seenAddr     // the addresses the session's requests came from, latest first (noteAddr)
}

// seenAddr is an address a session was used from, and when last (Unix seconds).
type seenAddr struct {
	ip string
	at int64
}

// maxAddrs is how many addresses are remembered per session: the latest one and three before it.
const maxAddrs = 4

// maxLiveSessions bounds the map. Keys come from signed cookies of registered sessions only, and the registry
// has its own limits (adminstate.MaxSessionsTotal is below this), so it cannot be filled from outside; the
// bound is a backstop. What happens when it is reached: prune.
const maxLiveSessions = 4096

func newLiveSessions() *liveSessions {
	return &liveSessions{m: map[string]*liveSession{}, max: maxLiveSessions, ended: map[string]int64{},
		streams: streamSlots{perUser: maxStreamsPerUser, total: maxStreamsTotal, open: map[string]int{}}}
}

// demoKey is the key of demo mode's single, cookie-less "session".
const demoKey = "demo"

// entry returns the state of sid, creating it. Call with l.mu held.
func (l *liveSessions) entry(sid string, now time.Time) *liveSession {
	e := l.m[sid]
	if e == nil {
		if len(l.m) >= l.max {
			l.prune(now)
		}
		e = &liveSession{}
		l.m[sid] = e
	}
	e.touched = now.Unix()
	return e
}

// prune makes room when the map is full. It never empties the map (T-04: that took every admin's confirmation
// window and every kept action away because one account signed in often). In this order, until there is room:
//
//  1. entries not touched for a day;
//  2. entries of sessions that are not registered any more (signed out elsewhere, revoked by the CLI, or
//     dropped by the registry's own limits): nothing can use them;
//  3. the least recently used entries of the account that has the most. So the account that fills the map
//     pays for the room, and an account with one or two sessions keeps what it has.
//
// Call with l.mu held.
func (l *liveSessions) prune(now time.Time) {
	for k, e := range l.m {
		if now.Unix()-e.touched > 86400 {
			delete(l.m, k)
		}
	}
	if len(l.m) < l.max {
		return
	}
	by := map[string][]string{} // account -> the ids of its entries
	for k := range l.m {
		acct := ""
		if l.account != nil && k != demoKey {
			if acct = l.account(k); acct == "" {
				delete(l.m, k)
				continue
			}
		}
		by[acct] = append(by[acct], k)
	}
	for len(l.m) >= l.max {
		most := ""
		for acct, ids := range by {
			if len(ids) > len(by[most]) || (len(ids) == len(by[most]) && acct < most) {
				most = acct
			}
		}
		ids := by[most]
		if len(ids) == 0 {
			return
		}
		oldest := 0
		for i, id := range ids {
			if l.m[id].touched < l.m[ids[oldest]].touched {
				oldest = i
			}
		}
		delete(l.m, ids[oldest])
		by[most] = append(ids[:oldest], ids[oldest+1:]...)
	}
}

// maxEnded bounds the list of sessions ended in memory only. It grows only while sessions.json cannot be
// written, by one entry for every sign-out, idle expiry or step-up of a session the file lists (dropSession),
// and the file lists at most adminstate.MaxSessionsTotal: the bound is a backstop.
const maxEnded = 4096

// end marks sid as ended although sessions.json still lists it, and reports whether it was not marked before.
// When the list is full the oldest entry goes.
func (l *liveSessions) end(sid string, now time.Time) (first bool) {
	if sid == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, known := l.ended[sid]
	if !known && len(l.ended) >= maxEnded {
		oldest, at := "", int64(0)
		for k, t := range l.ended {
			if oldest == "" || t < at {
				oldest, at = k, t
			}
		}
		delete(l.ended, oldest)
	}
	l.ended[sid] = now.Unix()
	delete(l.m, sid)
	return !known
}

// isEnded reports whether sid was ended in memory (end).
func (l *liveSessions) isEnded(sid string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ended := l.ended[sid]
	return ended
}

// forget drops everything about sid (sign-out, expiry, a new session id), also its place on the list of
// ended sessions: it is called when sid has left sessions.json or can never be accepted again.
func (l *liveSessions) forget(sid string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, sid)
	delete(l.ended, sid)
}

// keepPending makes p the one action waiting for sid's step-up sign-in (an older one is replaced).
func (l *liveSessions) keepPending(sid string, p *pendingAction, now time.Time) {
	if sid == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entry(sid, now).pending = p
}

// armPending is "Continue" in the "Confirm it's you" dialog: it ties the action waiting for sid to the step-up
// round trip that is about to start (round, that round trip's state), and returns the action. It does so only
// when id names the action, the request comes from the client the action was kept for, and the action is not
// too old. Anything else forgets the action and returns nil: asking for the wrong one disarms it.
func (l *liveSessions) armPending(sid, id string, cl client, round string, now time.Time) *pendingAction {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[sid]
	if e == nil || e.pending == nil {
		return nil
	}
	p := e.pending
	if id == "" || round == "" || subtle.ConstantTimeCompare([]byte(p.ID), []byte(id)) != 1 || p.Client != cl || p.expired(now) {
		e.pending = nil
		return nil
	}
	p.Round = round
	return p
}

// setPendingNext notes where the page returns to after the round trip `round` of the action waiting for sid.
func (l *liveSessions) setPendingNext(sid, round, next string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.m[sid]; e != nil && e.pending != nil && e.pending.Round == round && round != "" {
		e.pending.Next = next
	}
}

// takePending removes the action waiting for sid, whatever it is, and returns it only when it was armed for
// this very round trip (round), the request comes from the client it was kept for, and it is not too old. So a
// kept action can be handed out at most once, and a round trip that was not started for it (a link, a second
// tab, another browser or address) runs nothing and leaves nothing armed. foreign reports that an action was
// armed for this round trip but another client came back with it (for the audit log).
func (l *liveSessions) takePending(sid, round string, cl client, now time.Time) (p *pendingAction, foreign bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[sid]
	if e == nil || e.pending == nil {
		return nil, false
	}
	p = e.pending
	e.pending = nil
	if round == "" || p.Round == "" || subtle.ConstantTimeCompare([]byte(p.Round), []byte(round)) != 1 || p.expired(now) {
		return nil, false
	}
	if p.Client != cl {
		return nil, true
	}
	return p, false
}

// keptResult is the outcome of a confirmed action, waiting to be shown once.
type keptResult struct {
	res    *model.PendingResult
	client client // who confirmed: the result is shown to this client only
	at     time.Time
}

// putResult keeps the outcome of the action the panel just ran for sid, for the client that confirmed it.
func (l *liveSessions) putResult(sid string, res *model.PendingResult, cl client, now time.Time) {
	if sid == "" || res == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entry(sid, now).result = &keptResult{res: res, client: cl, at: now}
}

// takeResult hands the outcome kept for sid out, once, to the client that confirmed the action, while it is
// fresh (pendingMaxAge). Another client (a copy of the cookie elsewhere) gets nothing and takes nothing away.
func (l *liveSessions) takeResult(sid string, cl client, now time.Time) *model.PendingResult {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[sid]
	if e == nil || e.result == nil {
		return nil
	}
	if now.Sub(e.result.at) > pendingMaxAge {
		e.result = nil
		return nil
	}
	if e.result.client != cl {
		return nil
	}
	res := e.result.res
	e.result = nil
	return res
}

// pendingBody is the body of the request that waits for sid's step-up sign-in ("" when none waits).
func (l *liveSessions) pendingBody(sid string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.m[sid]; e != nil && e.pending != nil {
		return e.pending.Body
	}
	return ""
}

// dropPending forgets the action waiting for sid.
func (l *liveSessions) dropPending(sid string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.m[sid]; e != nil {
		e.pending = nil
	}
}

// client is what the panel can tell about who sent a request: the address it came from (as the trusted proxies
// report it) and the browser (the User-Agent header, hashed). It is the binding of the step-up window, of a
// kept action and of everything a confirmation hands out: the same cookie presented by another client gets
// none of it. Two clients with the same address and the same User-Agent cannot be told apart.
type client struct {
	ip string
	ua [sha256.Size]byte
}

// client returns the client of this request.
func (c *reqCtx) client() client { return client{ip: c.ip(), ua: uaHash(c.r)} }

// stepUpRec is a confirmed Pocket ID sign-in (a sign-in or a step-up that completed): when the identity provider
// says the user authenticated, and which client the confirmation came back to. Only that client is inside the
// step-up window; the same cookie presented from another address or another browser is not.
type stepUpRec struct {
	at float64 // auth_time, Unix seconds
	ip string
	ua [sha256.Size]byte
}

// uaHash identifies the browser of a request for the step-up binding (the User-Agent header, hashed).
func uaHash(r *http.Request) [sha256.Size]byte {
	return sha256.Sum256([]byte(r.Header.Get("User-Agent")))
}

// confirm records that sid's user authenticated at Pocket ID at `at`, confirmed from this address and browser.
func (l *liveSessions) confirm(sid string, at float64, ip string, ua [sha256.Size]byte, now time.Time) {
	if sid == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entry(sid, now).stepUp = &stepUpRec{at: at, ip: ip, ua: ua}
}

// confirmedAt returns when sid's user last authenticated, but only for the client that confirmed it. ok is false
// when nothing is recorded (no confirmation since the panel started) or the request comes from another address
// or browser: the caller then treats the sign-in as long ago (fail closed).
func (l *liveSessions) confirmedAt(sid, ip string, ua [sha256.Size]byte) (at float64, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[sid]
	if e == nil || e.stepUp == nil || e.stepUp.at <= 0 || e.stepUp.ip != ip || e.stepUp.ua != ua {
		return 0, false
	}
	return e.stepUp.at, true
}

// lastSeen returns when sid was last really used (see reqCtx.active). The first question after a restart is
// answered from the cookie's own copy (fromCookie), which is then remembered; after that the cookie is not asked.
func (l *liveSessions) lastSeen(sid string, fromCookie int64, now time.Time) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.m[sid]; e != nil && e.seen != 0 {
		return e.seen
	}
	if sid != "" {
		l.entry(sid, now).seen = fromCookie
	}
	return fromCookie
}

// touch records real use of sid now.
func (l *liveSessions) touch(sid string, now time.Time) {
	if sid == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entry(sid, now).seen = now.Unix()
}

// dueSave reports whether sid's last use should go to sessions.json now: it was not written for seenPersist
// seconds. It counts the write as done, so a write that fails is tried again a minute later, not at once.
func (l *liveSessions) dueSave(sid string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[sid]
	if e == nil || now.Unix()-e.saved < seenPersist {
		return false
	}
	e.saved = now.Unix()
	return true
}

// savedNow notes that sid's entry in sessions.json was written just now with its last use (a sign-in, a renewed id).
func (l *liveSessions) savedNow(sid string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.m[sid]; e != nil {
		e.saved = now.Unix()
	}
}

// noteAddr remembers that a request the panel accepted for sid came from ip (the client address as the
// trusted proxies report it, reqCtx.ip). The Sessions page shows the latest address and the ones before it:
// a copy of a session cookie that is used elsewhere has the same session id, so it is not another line on
// that page, but it is another address on this list. Only in memory, for sessions the panel knows.
func (l *liveSessions) noteAddr(sid, ip string, now time.Time) {
	if sid == "" || ip == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entry(sid, now)
	if len(e.addrs) > 0 && e.addrs[0].ip == ip {
		e.addrs[0].at = now.Unix()
		return
	}
	next := make([]seenAddr, 0, maxAddrs)
	next = append(next, seenAddr{ip, now.Unix()})
	for _, a := range e.addrs {
		if a.ip != ip && len(next) < maxAddrs {
			next = append(next, a)
		}
	}
	e.addrs = next
}

// usage returns what is known in memory about the use of sid: when it was last really used (0: not since the
// panel started) and the addresses its requests came from, latest first.
func (l *liveSessions) usage(sid string) (seen int64, addrs []seenAddr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.m[sid]; e != nil {
		return e.seen, slices.Clone(e.addrs)
	}
	return 0, nil
}
