package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"time"
)

// A repeated step-up callback (r3-p1-01, R7B-01).
//
// The callback of a "Confirm it's you" gives the session a new id, runs the kept action and answers with the
// new cookie. When that answer does not reach the browser (the person reloads a callback that takes long
// because a machine answers slowly, or the connection breaks), the browser asks again with what it has: the
// same address, and the cookie with the OLD session id. That request used to be a stranger's: Pocket ID takes a
// code once, so the page said "login failed" and nothing told the person that the action had run.
//
// The panel therefore remembers, in memory and for stepRepeatFor, the round trip a session last finished: a
// hash of its state and of its authorization code, the client that came back, the new session id and where the
// page goes. A callback that repeats exactly that round trip (the same state AND the same code, from the same
// client) is told what happened instead of "login failed":
//
//   - with the new cookie (the answer did arrive; the person went back to the address): the same redirect,
//     nothing else;
//   - with the cookie the round trip was started with (the old id): a small page that says the action was
//     already sent and to sign in again. It waits only until the first callback knows whether the confirmation
//     was accepted, never for the kept action.
//
// A repeat never gets a session cookie and never a confirmation, and the kept action is not run again. The
// first version of this file handed the old cookie the new session (R7B-01): a copy of the round trip's cookie
// got, from the same address and with the same User-Agent, the session with the fresh passkey confirmation.
// Now the browser whose answer was lost signs in once more, and a copy gets a page.
//
// Nothing else about the callback changes. Another state, another code, another client (address or browser), a
// round trip older than two minutes or one that failed: all go the strict way as before (state in the signed
// cookie, PKCE, the provider's single-use code). A restart forgets everything here, which is the strict way too.

// stepRepeatFor is how long a finished round trip is remembered. stepDecideWait is how long a repeat waits for
// a first callback that is still checking Pocket ID's answer (that check has its own limit of 15 s).
const (
	stepRepeatFor  = 2 * time.Minute
	stepDecideWait = 20 * time.Second
	maxStepRuns    = 4096 // two keys per round trip; a backstop, as maxLiveSessions
)

// stepRun is one step-up round trip the callback is working on or has finished.
type stepRun struct {
	mark    [sha256.Size]byte // of the round trip's state and authorization code (callbackMark)
	client  client            // the client that came back
	oldSID  string
	at      time.Time     // when the callback started; when it was accepted, once it was
	decided chan struct{} // closed when the confirmation was accepted, or when the first callback returned without
	// set when the confirmation was accepted (stepAccepted), read under liveSessions.mu:
	ok     bool
	ran    bool // a kept action was run with it
	newSID string
	target string
}

// callbackMark is what a repeat must show again: the state and the code of the callback, hashed (neither is
// kept).
func callbackMark(state, code string) [sha256.Size]byte {
	s, c := sha256.Sum256([]byte(state)), sha256.Sum256([]byte(code))
	return sha256.Sum256(append(append([]byte("step-up callback\x00"), s[:]...), c[:]...))
}

func (r *stepRun) same(state, code string, cl client) bool {
	m := callbackMark(state, code)
	return subtle.ConstantTimeCompare(m[:], r.mark[:]) == 1 && r.client == cl
}

// beginStep is called by a step-up callback whose state matched its cookie's. first: this callback is the one
// that does the work (run is its record, nil when there is no room to remember it). Not first: run is the
// record of the callback that came before with the same state and code from the same client, or nil when the
// session has a record that this request does not match (then it goes the strict way, with no record).
func (l *liveSessions) beginStep(sid, state, code string, cl client, now time.Time) (first bool, run *stepRun) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, r := range l.steps {
		if now.Sub(r.at) > stepRepeatFor {
			delete(l.steps, k)
		}
	}
	if r := l.steps[sid]; r != nil && r.oldSID == sid {
		if r.same(state, code, cl) {
			return false, r
		}
		return false, nil
	} // (a record under sid as its NEW id is of the round trip before this one: this one takes its place)
	if len(l.steps) >= maxStepRuns && l.steps[sid] == nil {
		return true, nil
	}
	run = &stepRun{mark: callbackMark(state, code), client: cl, oldSID: sid, at: now, decided: make(chan struct{})}
	if l.steps == nil {
		l.steps = map[string]*stepRun{}
	}
	l.steps[sid] = run
	return true, run
}

// stepAccepted records that run's confirmation was accepted: the session's new id, where the page goes and
// whether a kept action runs with it. From now on a repeat is told so. Called before the kept action runs.
func (l *liveSessions) stepAccepted(run *stepRun, newSID, target string, ran bool, now time.Time) {
	if run == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	run.ok, run.ran, run.newSID, run.target, run.at = true, ran, newSID, target, now
	if l.steps[run.oldSID] == run {
		l.steps[newSID] = run
	}
	close(run.decided)
}

// endStep is called when the first callback returns: a round trip that was not accepted is forgotten (a repeat
// of it is a stranger, as before), and a repeat that waited for the decision goes on.
func (l *liveSessions) endStep(run *stepRun) {
	if run == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !run.ok {
		if l.steps[run.oldSID] == run {
			delete(l.steps, run.oldSID)
		}
		close(run.decided)
	}
}

// stepDone answers a callback that comes with the NEW session's cookie: where the finished round trip with
// this state and code, from this client, sent the page.
func (l *liveSessions) stepDone(sid, state, code string, cl client, now time.Time) (target string, ok bool) {
	if sid == "" || state == "" || code == "" {
		return "", false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.steps[sid]
	if r == nil || !r.ok || r.newSID != sid || !r.same(state, code, cl) || now.Sub(r.at) > stepRepeatFor {
		return "", false
	}
	return r.target, true
}

// stepRepeat says whether run's confirmation was accepted and is still remembered, and whether a kept action
// ran with it.
func (l *liveSessions) stepRepeat(run *stepRun, now time.Time) (ran, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return run.ran, run.ok && now.Sub(run.at) <= stepRepeatFor
}

// repeatedStep answers the repeat of a step-up callback from the cookie it was started with (see the top of
// this file): a page, never a cookie. done is false when run was not accepted: the caller then goes on the
// strict way.
func (s *Server) repeatedStep(c *reqCtx, page callbackPage, sess *sessionData, run *stepRun) (done bool) {
	wait := time.NewTimer(stepDecideWait)
	defer wait.Stop()
	select {
	case <-run.decided:
	case <-wait.C:
	case <-c.r.Context().Done():
		return true // the browser left again: nobody to answer
	}
	ran, ok := s.codec.live.stepRepeat(run, s.now())
	if !ok {
		return false
	}
	c.auditAnon("auth.step_up", "repeated callback of a confirmation that was accepted: no session handed out, nothing run again",
		actorOf(sess.User), &sess.User.Sub, nil)
	_ = page.repeated(ran)
	return true
}
