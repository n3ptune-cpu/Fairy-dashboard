package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"sync"
	"time"
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

	streams streamSlots // how many live-update streams are open (stream.go)
}

// liveSession is one session's in-memory state.
type liveSession struct {
	pending *pendingAction // the control action waiting for a step-up sign-in (see pending.go)
	stepUp  *stepUpRec     // the last confirmed Pocket ID sign-in (nil: none since the panel started)
	seen    int64          // Unix seconds of the last real use (0: not known since the panel started)
	touched int64          // Unix seconds of the last change, for pruning
}

// maxLiveSessions bounds the map. Keys come from signed cookies of registered sessions only, so it cannot be
// filled from outside; the bound is a backstop.
const maxLiveSessions = 4096

func newLiveSessions() *liveSessions {
	return &liveSessions{m: map[string]*liveSession{},
		streams: streamSlots{perUser: maxStreamsPerUser, total: maxStreamsTotal, open: map[string]int{}}}
}

// demoKey is the key of demo mode's single, cookie-less "session".
const demoKey = "demo"

// entry returns the state of sid, creating it. Call with l.mu held.
func (l *liveSessions) entry(sid string, now time.Time) *liveSession {
	e := l.m[sid]
	if e == nil {
		if len(l.m) >= maxLiveSessions {
			l.prune(now)
		}
		e = &liveSession{}
		l.m[sid] = e
	}
	e.touched = now.Unix()
	return e
}

// prune drops entries not touched for a day, and everything when that is not enough. Call with l.mu held.
func (l *liveSessions) prune(now time.Time) {
	for k, e := range l.m {
		if now.Unix()-e.touched > 86400 {
			delete(l.m, k)
		}
	}
	if len(l.m) >= maxLiveSessions {
		l.m = map[string]*liveSession{}
	}
}

// forget drops everything about sid (sign-out, expiry, a new session id).
func (l *liveSessions) forget(sid string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, sid)
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
