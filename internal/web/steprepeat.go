package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"time"
)

// A repeated step-up callback (r3-p1-01).
//
// The callback of a "Confirm it's you" gives the session a new id, runs the kept action and answers with the
// new cookie. When that answer does not reach the browser (the person reloads a callback that takes long
// because a machine answers slowly, or the connection breaks), the browser asks again with what it has: the
// same address, and the cookie with the OLD session id. That request used to be a stranger's: Pocket ID takes a
// code once, so the page said "login failed", the browser was signed out, and nothing told the person that the
// action had run.
//
// The panel therefore remembers, in memory and for stepRepeatFor, the round trip a session last finished: a
// hash of its state, the client that came back, the new session id and where the page goes. A callback that
// repeats exactly that round trip is answered again instead of being refused:
//
//   - with the cookie the round trip was started with (the old id), from the same client, and only while no
//     request has used the new session id yet (so the first answer evidently did not arrive): it waits for the
//     first callback when that one is still at work, gets the cookie of the new session and the same redirect.
//     The kept action is not run again; its outcome waits under the new id and the page shows it once;
//   - with the new cookie (the answer did arrive; the person went back to the address): the same redirect,
//     nothing else.
//
// Nothing else about the callback changes. A state that is not the remembered one, another client (address or
// browser), a round trip older than two minutes or one that failed, a new id that is already in use: all go
// the strict way as before (state in the signed cookie, PKCE, the provider's single-use code). A restart
// forgets everything here, which is the strict way too.

// stepRepeatFor is how long a finished round trip is remembered. stepRepeatWait is how long a repeat waits for
// a first callback that is still running the kept action.
const (
	stepRepeatFor  = 2 * time.Minute
	stepRepeatWait = 45 * time.Second
	maxStepRuns    = 4096 // two keys per round trip; a backstop, as maxLiveSessions
)

// stepRun is one step-up round trip the callback is working on or has finished.
type stepRun struct {
	state    [sha256.Size]byte // of the round trip's state
	client   client            // the client that came back
	oldSID   string
	at       time.Time     // when the callback started; when it was accepted, once it was
	finished chan struct{} // closed when the first callback has answered (the kept action has run)
	// set when the confirmation was accepted (stepAccepted), read under liveSessions.mu:
	ok      bool
	newSID  string
	target  string
	newUsed bool // a request has come in with the new session id: its cookie reached the browser
}

func stateHash(state string) [sha256.Size]byte {
	return sha256.Sum256([]byte("step-up state\x00" + state))
}

func (r *stepRun) same(state string, cl client) bool {
	h := stateHash(state)
	return subtle.ConstantTimeCompare(h[:], r.state[:]) == 1 && r.client == cl
}

// beginStep is called by a step-up callback whose state matched its cookie's. first: this callback is the one
// that does the work (run is its record, nil when there is no room to remember it). Not first: run is the
// record of the callback that came before with the same state from the same client, or nil when the session
// has a record that this request does not match (then nothing is remembered for it and nothing handed out).
func (l *liveSessions) beginStep(sid, state string, cl client, now time.Time) (first bool, run *stepRun) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, r := range l.steps {
		if now.Sub(r.at) > stepRepeatFor {
			delete(l.steps, k)
		}
	}
	if r := l.steps[sid]; r != nil && r.oldSID == sid {
		if r.same(state, cl) {
			return false, r
		}
		return false, nil
	} // (a record under sid as its NEW id is of the round trip before this one: this one takes its place)
	if len(l.steps) >= maxStepRuns && l.steps[sid] == nil {
		return true, nil
	}
	run = &stepRun{state: stateHash(state), client: cl, oldSID: sid, at: now, finished: make(chan struct{})}
	if l.steps == nil {
		l.steps = map[string]*stepRun{}
	}
	l.steps[sid] = run
	return true, run
}

// stepAccepted records that run's confirmation was accepted: the session's new id and where the page goes.
// From now on a repeat can be answered. Called before the kept action runs.
func (l *liveSessions) stepAccepted(run *stepRun, newSID, target string, now time.Time) {
	if run == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	run.ok, run.newSID, run.target, run.at = true, newSID, target, now
	if l.steps[run.oldSID] == run {
		l.steps[newSID] = run
	}
}

// endStep is called when the first callback returns: repeats stop waiting, and a round trip that was not
// accepted is forgotten (a repeat of it is a stranger, as before).
func (l *liveSessions) endStep(run *stepRun) {
	if run == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !run.ok && l.steps[run.oldSID] == run {
		delete(l.steps, run.oldSID)
	}
	close(run.finished)
}

// stepUsed notes that a request was accepted with session id sid. If sid is the new id of a remembered round
// trip, its cookie has reached a browser, and the old cookie gets nothing any more.
func (l *liveSessions) stepUsed(sid string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r := l.steps[sid]; r != nil && r.newSID == sid {
		r.newUsed = true
	}
}

// stepDone answers a callback that comes with the NEW session's cookie: where the finished round trip with
// this state, from this client, sent the page.
func (l *liveSessions) stepDone(sid, state string, cl client, now time.Time) (target string, ok bool) {
	if sid == "" || state == "" {
		return "", false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.steps[sid]
	if r == nil || !r.ok || r.newSID != sid || !r.same(state, cl) || now.Sub(r.at) > stepRepeatFor {
		return "", false
	}
	return r.target, true
}

// stepRepeat answers a callback that comes with the OLD cookie of run's round trip: the new session id and the
// target, when the confirmation was accepted, is fresh and its new id has not been used yet.
func (l *liveSessions) stepRepeat(run *stepRun, state string, cl client, now time.Time) (newSID, target string, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !run.ok || run.newUsed || !run.same(state, cl) || now.Sub(run.at) > stepRepeatFor || l.steps[run.newSID] != run {
		return "", "", false
	}
	return run.newSID, run.target, true
}

// repeatedStep answers the repeat of a step-up callback from the cookie it was started with (see the top of
// this file). done is false when the repeat gets nothing: the caller then goes on the strict way.
func (s *Server) repeatedStep(c *reqCtx, sess *sessionData, run *stepRun) (done bool) {
	wait := time.NewTimer(stepRepeatWait)
	defer wait.Stop()
	select {
	case <-run.finished:
	case <-wait.C: // the kept action still runs: the page shows its outcome at its next load
	case <-c.r.Context().Done():
		return true // the browser left again: nobody to answer
	}
	newSID, target, ok := s.codec.live.stepRepeat(run, sess.State, c.client(), s.now())
	if !ok {
		return false
	}
	next := *sess
	next.SID, next.Client, next.Seen = newSID, s.codec.clientMark(c.client()), s.now().Unix()
	next.StepUp, next.Next, next.State, next.Nonce, next.Verifier, next.Started = false, "", "", "", "", 0
	if s.sessionCheck(&next) != "" { // signed out or revoked since
		return false
	}
	c.sess, c.dirty = &next, true
	c.auditAnon("auth.step_up", "repeated callback of a confirmation that was accepted: answered again, nothing run again",
		actorOf(next.User), &next.User.Sub, nil)
	c.redirect(target)
	return true
}
