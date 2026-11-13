package web

import (
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// A repeated approval callback.
//
// The callback of an approval (GET /auth/approval/callback) takes the kept action out of the store, checks
// Pocket ID's answer and runs the action with the approval. A second request for the same address (a reload of
// a callback that takes long because a machine answers slowly, or the Back button, which leads over Pocket ID's
// page to the same address again) finds nothing waiting. It was answered "The approval was not accepted. No
// action is waiting for this approval any more ... or this answer was used before. Nothing was done.", also
// when the first callback had accepted the approval and had sent the shutdown: the page said that nothing was
// done about an action that was carried out. The repeat page of a confirmation had the same fault the other
// way round ("already sent" whatever came of the action: audit 6, r4-p1-01, steprepeat.go); this is the same
// page on the approval's own round trip.
//
// The panel remembers the approval round trip a session last finished the way steprepeat.go remembers a
// confirmation's, with the same record (stepRun), for stepRepeatFor and in memory: a hash of its state and of
// its authorization code, the client that came back, and what the kept action answered with. A callback that
// repeats exactly that round trip (the same session, the same state AND the same code, from the same client)
// is told what came of the action, as far as the first callback knows it by then. It waits only until the
// first callback knows whether the approval was accepted, never for the kept action.
//
// A repeat runs nothing. An approval's callback never gives the session a new id and never sets a cookie (it
// is no sign-in and opens no window), and neither does its repeat: the session the repeat comes with is the
// one it already had. Another state, another code, another client, a round trip older than two minutes or one
// that was refused: the page that was there before ("No action is waiting ..."), for which "nothing was done"
// is true. A restart forgets everything here.

// The repeat of an approval's callback, as the page says it. The sentences are the confirmation's own
// (callbackpage.go) without "Sign in again to go on.": the session is still there.
//
// One sentence stands for every answer of the kept action that is not a "yes" (a 4xx as well as a 5xx): for a
// single power, fan or update request a 4xx is a refusal, but a Turn on answers 409 also when a machine was
// sent its trust and gave no answer, which nobody may call "not carried out".
const (
	cbApproved       = "Already approved"
	cbApprovedNotYes = "The action was refused, or it ended with an error: Fairy does not say here which. The message on the page you " +
		"came from and the audit log say what came of it. It is not sent again."
)

// beginApproval is called by the approval callback that took the kept action out of the store: it is the one
// that does the work, and run is its record (nil when there is no room to remember it).
func (l *liveSessions) beginApproval(sid, state, code string, cl client, now time.Time) *stepRun {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, r := range l.approvals {
		if now.Sub(r.at) > stepRepeatFor {
			delete(l.approvals, k)
		}
	}
	if len(l.approvals) >= maxStepRuns && l.approvals[sid] == nil {
		return nil
	}
	run := &stepRun{mark: callbackMark(state, code), client: cl, oldSID: sid, at: now, decided: make(chan struct{})}
	if l.approvals == nil {
		l.approvals = map[string]*stepRun{}
	}
	l.approvals[sid] = run
	return run
}

// approvalAccepted records that run's approval was accepted and that the kept action runs with it now. From
// now on a repeat is told so. Called before the kept action runs. The session keeps its id.
func (l *liveSessions) approvalAccepted(run *stepRun, target string, now time.Time) {
	if run == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	run.ok, run.ran, run.newSID, run.target, run.at = true, true, run.oldSID, target, now
	close(run.decided)
}

// endApproval is called when the first callback returns: a round trip that was not accepted is forgotten (a
// repeat of it gets the page it got before), and a repeat that waited for the decision goes on.
func (l *liveSessions) endApproval(run *stepRun) {
	if run == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !run.ok {
		if l.approvals[run.oldSID] == run {
			delete(l.approvals, run.oldSID)
		}
		close(run.decided)
	}
}

// approvalRound is the record of the approval round trip that a callback with this state and code, from this
// client of sid, repeats (nil: none).
func (l *liveSessions) approvalRound(sid, state, code string, cl client) *stepRun {
	if sid == "" || state == "" || code == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if r := l.approvals[sid]; r != nil && r.same(state, code, cl) {
		return r
	}
	return nil
}

// approvalRepeat says whether run's approval was accepted and is still remembered, what the kept action
// answered with (0: it has not answered yet; stepOutcome records it) and where the page went.
func (l *liveSessions) approvalRepeat(run *stepRun, now time.Time) (status int, target string, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return run.status, run.target, run.ok && now.Sub(run.at) <= stepRepeatFor
}

// repeatedApproval answers the repeat of an approval's callback: a page, never a cookie, and nothing runs.
// done is false when run was not accepted (or is no longer remembered): the caller then answers as it did.
func (s *Server) repeatedApproval(c *reqCtx, u *model.User, run *stepRun) (done bool) {
	wait := time.NewTimer(stepDecideWait)
	defer wait.Stop()
	select {
	case <-run.decided:
	case <-wait.C:
	case <-c.r.Context().Done():
		return true // the browser left again: nobody to answer
	}
	status, target, ok := s.codec.live.approvalRepeat(run, s.now())
	if !ok {
		return false
	}
	c.auditAnon("auth.approval", "repeated callback of an approval that was accepted: nothing run again", actorOf(u), &u.Sub, nil)
	msg := cbApprovedNotYes
	switch {
	case status == 0:
		msg = strings.TrimSuffix(cbRepeatAtWork, " Sign in again to go on.")
	case status >= 200 && status < 300:
		msg = strings.TrimSuffix(cbRepeatRan, " Sign in again to go on.")
	}
	_ = c.errorPage(409, cbApproved, msg, target)
	return true
}
