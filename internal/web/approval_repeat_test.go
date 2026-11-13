package web

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
)

// The way back from Pocket ID of an approval, asked for a second time (a reload of the callback, the Back
// button over Pocket ID's page). It read "The approval was not accepted. No action is waiting for this approval
// any more ... Nothing was done." also when the first callback had accepted the approval and sent the
// shutdown. The repeat of a confirmation's callback says what came of the kept action since audit 6, r4-p1-01
// (steprepeat_outcome_test.go); the approval's own callback now does the same (approvalrepeat.go). A repeat
// never runs anything and never sets a cookie.

// approvedShutdown asks for laptop's shutdown, approves it and returns the callback's address after the first
// callback was answered.
func approvedShutdown(t *testing.T, a *approvalEnv) *url.URL {
	t.Helper()
	id, _, ok := approvalAsked(a.post("/api/systems/laptop/power", a.shutdownBody()))
	if !ok {
		t.Fatal("the dialog of the shutdown")
	}
	r := a.post(stepUpPath, map[string]any{"pending_id": id, "next": "/"})
	if r.code != 200 {
		t.Fatalf("Continue: %d %s", r.code, r.raw)
	}
	back := a.iss.Approve(t, fmt.Sprint(r.body["url"]))
	if cb := a.do("GET", back.RequestURI(), nil, "Cookie", a.admin); cb.code != 303 {
		t.Fatalf("the first callback: %d %s", cb.code, cb.raw)
	}
	return back
}

func TestTheRepeatOfAnApprovalsCallbackSaysWhatCameOfTheAction(t *testing.T) {
	// the shutdown was sent
	a := newApprovalEnv(t)
	a.require(t, a.laptop)
	back := approvedShutdown(t, a)
	sent := controlWrites(a.laptop)
	if sent != 1 {
		t.Fatalf("the approved shutdown: %q", sentPaths(a.laptop.Asked()))
	}
	for i := 0; i < 2; i++ {
		r := a.do("GET", back.RequestURI(), nil, "Cookie", a.admin)
		body := string(r.raw)
		if r.code != 409 || !strings.Contains(body, "<h1>"+cbApproved+"</h1>") || !strings.Contains(body, "The action was already sent. It is not sent again.") {
			t.Fatalf("repeat %d of an approval whose shutdown was sent: %d %s", i, r.code, body)
		}
		for _, untrue := range []string{"Nothing was done", "not accepted", "No action is waiting", "Sign in again"} {
			if strings.Contains(body, untrue) {
				t.Errorf("repeat %d says %q about an action that was sent:\n%s", i, untrue, body)
			}
		}
		if len(r.hdr.Values("Set-Cookie")) != 0 || strings.Contains(body, "<script") || strings.Count(body, "<a ") != 1 {
			t.Errorf("repeat %d must be the small page: no cookie, no script, one link: %v\n%s", i, r.hdr.Values("Set-Cookie"), body)
		}
	}
	if controlWrites(a.laptop) != sent {
		t.Fatalf("a repeat sent something: %q", sentPaths(a.laptop.Asked()))
	}
	if got := auditOutcomes(a.s.Cfg.AuditLog); strings.Count(got, "auth.approval repeated callback of an approval that was accepted: nothing run again") != 2 ||
		strings.Count(got, "power.shutdown ok") != 1 {
		t.Fatalf("the audit log:\n%s", got)
	}
	// the session is the one it was: nothing was handed out and nothing taken away
	if me := a.do("GET", "/api/me", nil, "Cookie", a.admin); me.code != 200 {
		t.Fatalf("the session after the repeats: %d", me.code)
	}

	// not exactly that round trip: the page that was there before, for which "nothing was done" is true
	q := back.Query()
	other := *back
	q.Set("code", "another-code")
	other.RawQuery = q.Encode()
	for what, r := range map[string]resp{
		"another code":    a.do("GET", other.RequestURI(), nil, "Cookie", a.admin),
		"another browser": a.do("GET", back.RequestURI(), nil, "Cookie", a.admin, "User-Agent", "another browser"),
	} {
		if body := string(r.raw); r.code != 400 || !strings.Contains(body, "No action is waiting for this approval any more.") || strings.Contains(body, "already sent") {
			t.Errorf("%s: %d %s", what, r.code, body)
		}
	}
	if controlWrites(a.laptop) != sent {
		t.Fatalf("something was sent: %q", sentPaths(a.laptop.Asked()))
	}
}

// The machine gave no answer to the approved shutdown: the repeat does not say "already sent", and it does not
// say "nothing was done" or "refused" either.
func TestTheRepeatOfAnApprovalWhoseActionGotNoAnswer(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop)
	a.gb.Lie("laptop", func(path string) (int, jsonx.M) {
		if path == "/power" {
			return 504, jsonx.M{"error": "the helper was sent the shutdown and did not answer in time"}
		}
		return 0, nil
	})
	back := approvedShutdown(t, a)
	r := a.do("GET", back.RequestURI(), nil, "Cookie", a.admin)
	body := string(r.raw)
	if r.code != 409 || !strings.Contains(body, "<h1>"+cbApproved+"</h1>") || !strings.Contains(body, "The action was refused, or it ended with an error: Fairy does not say here which.") ||
		!strings.Contains(body, "It is not sent again.") {
		t.Fatalf("the repeat: %d %s", r.code, body)
	}
	for _, untrue := range []string{"already sent", "Nothing was done", "not accepted", "not carried out"} {
		if strings.Contains(body, untrue) {
			t.Errorf("the repeat says %q about a shutdown that may have arrived:\n%s", untrue, body)
		}
	}
}

// An approval that was refused (Pocket ID's answer is for another plan) leaves nothing behind: its repeat is
// the page it always was, and "nothing was done" is true of it.
func TestTheRepeatOfARefusedApprovalIsWhatItWas(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop)
	id, _, ok := approvalAsked(a.post("/api/systems/laptop/power", a.shutdownBody()))
	if !ok {
		t.Fatal("the dialog of the shutdown")
	}
	r := a.post(stepUpPath, map[string]any{"pending_id": id, "next": "/"})
	back := a.iss.Approve(t, fmt.Sprint(r.body["url"]))
	q := back.Query()
	q.Set("code", "not-the-code")
	bad := *back
	bad.RawQuery = q.Encode()
	if cb := a.do("GET", bad.RequestURI(), nil, "Cookie", a.admin); cb.code != 400 || !strings.Contains(string(cb.raw), "Nothing was done.") {
		t.Fatalf("a callback with a wrong code: %d %s", cb.code, cb.raw)
	}
	for _, u := range []string{bad.RequestURI(), back.RequestURI()} {
		if cb := a.do("GET", u, nil, "Cookie", a.admin); cb.code != 400 || !strings.Contains(string(cb.raw), "No action is waiting for this approval any more.") || strings.Contains(string(cb.raw), cbApproved) {
			t.Fatalf("the repeat of a refused approval: %d %s", cb.code, cb.raw)
		}
	}
	if controlWrites(a.laptop) != 0 {
		t.Fatalf("something was sent: %q", sentPaths(a.laptop.Asked()))
	}
}

// The limit on sign-in attempts at the approval's way back: the page of the sign-in's own callback, with the
// seconds and Retry-After (audit 6, r4-p1-04; it was the line of JSON "too many login attempts; try again in a
// minute", the last one).
func TestTheApprovalsCallbackOverTheSignInLimitIsAPage(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop)
	id, _, ok := approvalAsked(a.post("/api/systems/laptop/power", a.shutdownBody()))
	if !ok {
		t.Fatal("the dialog of the shutdown")
	}
	r := a.post(stepUpPath, map[string]any{"pending_id": id, "next": "/"})
	back := a.iss.Approve(t, fmt.Sprint(r.body["url"]))
	a.s.login = newRateLimiter(1)
	a.s.login.allow("sub:" + approvalAdminSub) // the account's attempts of this minute are used up
	cb := a.do("GET", back.RequestURI(), nil, "Cookie", a.admin)
	waitSeconds(t, "GET "+back.Path, cb, "Too many sign-in attempts for this account. Try again in N s.")
	body := string(cb.raw)
	if !strings.Contains(cb.hdr.Get("Content-Type"), "text/html") || !strings.Contains(body, "<h1>The approval was not used</h1>") ||
		strings.Count(body, "<a ") != 1 || strings.Contains(body, "<script") || strings.Contains(body, "login attempts") {
		t.Errorf("the page: %s %s", cb.hdr.Get("Content-Type"), body)
	}
	// the kept action left the store before the limit was looked at: nothing runs, now or later
	if controlWrites(a.laptop) != 0 || keptID(a.env, a.admin) != "" {
		t.Fatalf("after a callback over the limit: sent %q, kept %q", sentPaths(a.laptop.Asked()), keptID(a.env, a.admin))
	}
}
