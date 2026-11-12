package web

import (
	"html"
	"strconv"
	"strings"
	"testing"
)

// Audit 6, r4-p1-04: four limits answered "...; try again in a minute" without Retry-After, and the start of
// the sign-in answered a line of JSON to a browser that had followed a link. Every limit a person can meet says
// how many seconds to wait and sets the header.

// waitSeconds checks an answer over a limit: 429, Retry-After between 1 and 61, and the same number in the text.
func waitSeconds(t *testing.T, what string, r resp, text string) {
	t.Helper()
	n, err := strconv.Atoi(r.hdr.Get("Retry-After"))
	if r.code != 429 || err != nil || n < 1 || n > 61 {
		t.Fatalf("%s: %d, Retry-After %q; want 429 and 1 to 61 seconds\n%s", what, r.code, r.hdr.Get("Retry-After"), r.raw)
	}
	if want := strings.ReplaceAll(text, "N", strconv.Itoa(n)); !strings.Contains(string(r.raw), want) {
		t.Errorf("%s: the answer lacks %q:\n%s", what, want, r.raw)
	}
	if strings.Contains(string(r.raw), "in a minute") {
		t.Errorf("%s: \"in a minute\" instead of the seconds:\n%s", what, r.raw)
	}
}

func TestTheStartOfTheSignInOverItsLimitIsAPage(t *testing.T) {
	e := setup(t)
	idp := newFakeIDP(t)
	e.s.Cfg.OIDC.Issuer = idp.srv.URL
	e.s.login = newRateLimiter(2)
	for i := 0; i < 2; i++ {
		if r := e.do("GET", "/auth/login?next=/%23/system/laptop", nil); r.code != 302 && r.code != 303 {
			t.Fatalf("sign-in %d: %d %s", i, r.code, r.raw)
		}
	}
	r := e.do("GET", "/auth/login?next=/%23/system/laptop", nil)
	waitSeconds(t, "GET /auth/login", r, "Too many sign-in attempts from this address. Try again in N s.")
	body := string(r.raw)
	if !strings.Contains(r.hdr.Get("Content-Type"), "text/html") || !strings.Contains(body, "<h1>"+cbSignIn+"</h1>") ||
		strings.Count(body, "<a ") != 1 || !strings.Contains(body, `href="/#/system/laptop">Back to Fairy</a>`) || strings.Contains(body, "<script") {
		t.Errorf("GET /auth/login over the limit must be the small page with one link back and no script: %s %s", r.hdr.Get("Content-Type"), body)
	}
	if len(r.hdr.Values("Set-Cookie")) != 0 {
		t.Error("the refusal sets a cookie")
	}
}

// A signed-in person's limit counts per account, and the page and the JSON answer say so and how long to wait.
func TestTheConfirmationOverItsLimitSaysHowLong(t *testing.T) {
	a := signedIn(t)
	a.e.s.login = newRateLimiter(1)
	a.e.s.login.allow("sub:u1") // the account's sign-ins of this minute are used up
	r := a.e.do("GET", "/auth/login?step_up=1&next=/", nil, "Cookie", a.sess)
	waitSeconds(t, "GET /auth/login?step_up=1", r, "Too many sign-in attempts for this account. Try again in N s.")
	if !strings.Contains(string(r.raw), "<h1>"+html.EscapeString(cbConfirm)+"</h1>") {
		t.Errorf("the page of a confirmation must have its title:\n%s", r.raw)
	}
	r = a.e.do("POST", "/api/me/step-up", map[string]any{"pending_id": "", "next": "/"}, "Cookie", a.sess, "X-Requested-With", "fairy", "Content-Type", "application/json")
	waitSeconds(t, "POST /api/me/step-up", r, "too many sign-in attempts; try again in N s")
}

func TestTheUploadLimitsSayHowLong(t *testing.T) {
	a := signedIn(t)
	hdr := []string{"Cookie", a.sess, "X-Requested-With", "fairy", "Content-Type", "image/png"}
	a.e.s.uploadRL = newRateLimiter(1)
	a.e.s.uploadRL.allow("u1") // the minute's uploads are used up
	waitSeconds(t, "POST /api/settings/background", a.e.do("POST", "/api/settings/background", nil, hdr...), "too many uploads; try again in N s")
}
