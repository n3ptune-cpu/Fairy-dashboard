package web

import (
	"sync"
	"time"
)

// liveSessions is what the panel keeps in memory about browser sessions, keyed by session id. None of it is in
// the cookie, so a copy of the cookie carries none of it, and none of it survives a restart (everything here
// fails closed when it is missing: a kept action is simply gone).
//
// It hangs off the session codec (Server.codec.live), next to the session clock.
type liveSessions struct {
	mu sync.Mutex
	m  map[string]*liveSession
}

// liveSession is one session's in-memory state.
type liveSession struct {
	pending *pendingAction // the control action waiting for a step-up sign-in (see pending.go)
	touched int64          // Unix seconds of the last change, for pruning
}

// maxLiveSessions bounds the map. Keys come from signed cookies of registered sessions only, so it cannot be
// filled from outside; the bound is a backstop.
const maxLiveSessions = 4096

func newLiveSessions() *liveSessions { return &liveSessions{m: map[string]*liveSession{}} }

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

// pendingID returns the id of the action waiting for sid ("" when none, or when it is too old).
func (l *liveSessions) pendingID(sid string, now time.Time) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.m[sid]; e != nil && e.pending != nil && !e.pending.expired(now) {
		return e.pending.ID
	}
	return ""
}

// takePending removes the action waiting for sid, whatever it is, and returns it only when id names it and it
// is not too old. So a kept action can be handed out at most once, and asking for the wrong one disarms it.
func (l *liveSessions) takePending(sid, id string, now time.Time) *pendingAction {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[sid]
	if e == nil || e.pending == nil {
		return nil
	}
	p := e.pending
	e.pending = nil
	if id == "" || p.ID != id || p.expired(now) {
		return nil
	}
	return p
}

// dropPending forgets the action waiting for sid.
func (l *liveSessions) dropPending(sid string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.m[sid]; e != nil {
		e.pending = nil
	}
}
