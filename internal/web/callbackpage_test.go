package web

import (
	"html"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var (
	pageLink = regexp.MustCompile(`<a [^>]*href="([^"]*)"[^>]*>([^<]*)</a>`)
	pageH1   = regexp.MustCompile(`<h1>([^<]*)</h1>`)
)

// refusalPage checks that r is the panel's small page for a refused callback (status, HTML, no script, the
// title, the sentence, exactly one link, which goes back to the panel) and returns its text. title and sentence
// are what the person reads: the page has them escaped.
func refusalPage(t *testing.T, what string, r resp, status int, title, sentence, back string) string {
	t.Helper()
	page := string(r.raw)
	if r.code != status {
		t.Errorf("%s: status %d, want %d", what, r.code, status)
	}
	if ct := r.hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("%s: the browser gets %q, not a page: %s", what, ct, page)
		return page
	}
	if strings.Contains(strings.ToLower(page), "<script") || strings.Contains(page, `{"detail"`) {
		t.Errorf("%s: the page must have no script and no JSON: %s", what, page)
	}
	if h := pageH1.FindStringSubmatch(page); h == nil || h[1] != html.EscapeString(title) {
		t.Errorf("%s: title %q, want %q", what, h, title)
	}
	if !strings.Contains(page, "<p>"+html.EscapeString(sentence)+"</p>") {
		t.Errorf("%s: the page must say %q: %s", what, sentence, page)
	}
	links := pageLink.FindAllStringSubmatch(page, -1)
	if strings.Count(page, "<a ") != 1 || len(links) != 1 || links[0][1] != back || links[0][2] != "Back to Fairy" {
		t.Errorf("%s: want one link \"Back to Fairy\" to %q, got %q", what, back, links)
	}
	return page
}

// r3-p1-06 (audit 5, round 3). The callback is an address the browser goes to by itself, coming from Pocket ID.
// When the panel refused there (cancelled or denied at Pocket ID, the round trip older than 10 minutes or of
// another browser, an answer that does not check, the limit, Pocket ID not reachable, an account that is not
// allowed, a state file that cannot be written), the browser showed {"detail":"login failed"} and nothing to
// click. Only the two auth_time refusals had a page. Now every refusal is that page: what happened in a
// sentence, one link back, no script.
func TestEveryRefusalAtTheCallbackIsAPageWithALinkBack(t *testing.T) {
	e := setup(t)
	idp := newFakeIDP(t)
	e.s.Cfg.OIDC.Issuer = idp.srv.URL
	e.s.login = newRateLimiter(1000)
	fresh := func() { idp.claims = map[string]any{"groups": []string{"viewers"}, "auth_time": e.s.now().Unix()} }
	start := func(next string) (string, string) {
		t.Helper()
		fresh()
		q, rt := idp.login(e, "/auth/login?next="+url.QueryEscape(next), "")
		return q.Get("state"), rt
	}

	// Pocket ID sends the person back with an error (cancelled, denied). Its code is shown, and nothing else of it.
	state, rt := start("/#/settings")
	r := e.do("GET", "/auth/callback?error=access_denied&error_description=The+user+said+no&state="+state, nil, "Cookie", rt)
	page := refusalPage(t, "an error from Pocket ID", r, 400, cbSignIn,
		"Pocket ID answered with an error (access_denied). Go back and try again.", "/#/settings")
	if strings.Contains(page, "said no") {
		t.Errorf("the description Pocket ID sent is for the log only: %s", page)
	}

	// The code is somebody's text (anybody can make a link to the callback): it reaches the page only as text.
	state, rt = start("/")
	// It is cut to 60 characters and loses control characters, quotes and backslashes, as in the audit log.
	evil := `<script>alert(1)</script>"\` + "\x01" + `<a href="https://evil.example">x</a>` + strings.Repeat("A", 200)
	r = e.do("GET", "/auth/callback?error="+url.QueryEscape(evil)+"&state="+state, nil, "Cookie", rt)
	page = refusalPage(t, "an error code with markup", r, 400, cbSignIn,
		"Pocket ID answered with an error (<script>alert(1)</script><a href=https://evil.example>x). Go back and try again.", "/")
	if strings.Contains(page, "<a href=https") || strings.Contains(page, "\x01") || strings.Contains(page, `\`) || strings.Contains(page, "AAA") {
		t.Errorf("the code must go through the sanitising of the audit log: %s", page)
	}

	// Not this browser's current round trip: a wrong state, and a callback without the round trip's cookie.
	state, rt = start("/#/updates")
	r = e.do("GET", "/auth/callback?code=c&state=not-"+state, nil, "Cookie", rt)
	refusalPage(t, "a wrong state", r, 400, cbSignIn, cbStale, "/#/updates")
	r = e.do("GET", "/auth/callback?code=c&state="+state, nil)
	refusalPage(t, "no cookie", r, 400, cbSignIn, cbStale, "/")

	// An answer of Pocket ID that does not check. Which check it was stays in the logs.
	state, rt = start("/")
	idp.mu.Lock()
	idp.nonce = "other"
	idp.mu.Unlock()
	r = e.do("GET", "/auth/callback?code=c&state="+state, nil, "Cookie", rt)
	page = refusalPage(t, "a nonce that does not match", r, 400, cbSignIn, cbUnchecked, "/")
	if strings.Contains(page, "nonce") || !strings.Contains(auditOutcomes(e.s.Cfg.AuditLog), "auth.login failed: nonce mismatch") {
		t.Errorf("the reason belongs in the audit log, not on the page: %s", page)
	}

	// An account that is not allowed.
	state, rt = start("/")
	idp.claims["groups"] = []string{"strangers"}
	r = e.do("GET", "/auth/callback?code=c&state="+state, nil, "Cookie", rt)
	refusalPage(t, "not allowed", r, 403, cbNotAllowed, cbRefused, "/")

	// A confirmation ("Confirm it's you") that is refused says so in its title and leads back to the page it
	// was started from.
	a := arm(t)
	a.e.s.login = newRateLimiter(1000)
	_, rt = a.idp.confirmStart(a.e, a.id, "/#/host/laptop", a.sess)
	r = a.e.do("GET", "/auth/callback?error=login_required&state=x", nil, "Cookie", rt)
	refusalPage(t, "a refused confirmation", r, 400, cbConfirm,
		"Pocket ID answered with an error (login_required). Go back and try again.", "/#/host/laptop")
	if a.powerCalls() != 0 {
		t.Error("the kept action must not run")
	}

	// The limit on sign-in attempts of one address: how long to wait, also as Retry-After.
	e.s.login = newRateLimiter(1)
	_ = e.do("GET", "/auth/callback?code=c&state=x", nil)
	r = e.do("GET", "/auth/callback?code=c&state=x", nil)
	wait, _ := strconv.Atoi(r.hdr.Get("Retry-After"))
	if wait < 1 || wait > 60 {
		t.Errorf("Retry-After %q", r.hdr.Get("Retry-After"))
	}
	refusalPage(t, "the limit", r, 429, cbSignIn,
		"Too many sign-in attempts from this address. Try again in "+strconv.Itoa(wait)+" s.", "/")
}

// The same when Pocket ID cannot be reached at the moment the person comes back.
func TestTheCallbackSaysThatPocketIDIsNotReachable(t *testing.T) {
	e := setup(t)
	gone := httptest.NewServer(nil)
	gone.Close()
	e.s.Cfg.OIDC.Issuer = gone.URL
	rec := httptest.NewRecorder()
	e.s.codec.write(rec, &sessionData{State: "st", Nonce: "n", Verifier: "v", Next: "/#/power", Started: e.s.now().Unix()})
	rt := strings.Split(rec.Header().Get("Set-Cookie"), ";")[0]
	r := e.do("GET", "/auth/callback?code=c&state=st", nil, "Cookie", rt)
	refusalPage(t, "Pocket ID gone", r, 502, cbSignIn, cbUnreachable, "/#/power")
}
