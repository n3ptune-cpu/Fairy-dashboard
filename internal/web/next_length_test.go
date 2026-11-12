package web

import (
	"strings"
	"testing"
)

// Audit 6, r4-p1-05 (a note): "next" had no length limit. It is kept in the signed cookie of the sign-in round
// trip, and 5000 characters made a cookie of over 7 kB: a browser drops a cookie that large and keeps the old
// one, and the way back from Pocket ID then failed as "started in another browser". A next longer than 512
// characters is the start page.
func TestALongNextIsTheStartPage(t *testing.T) {
	ok := "/#/system/" + strings.Repeat("a", 500)
	if len(ok) != 510 || safeNext(ok) != ok {
		t.Errorf("a next of 510 characters must be kept: %q", safeNext(ok))
	}
	if got := safeNext("/#/" + strings.Repeat("a", 510)); got != "/" {
		t.Errorf("a next of 513 characters: %d characters kept, want the start page", len(got))
	}
	e := setup(t)
	idp := newFakeIDP(t)
	e.s.Cfg.OIDC.Issuer = idp.srv.URL
	for _, q := range []string{"/auth/login?next=/%23/" + strings.Repeat("a", 5000), "/auth/login?step_up=1&next=/%23/" + strings.Repeat("a", 5000)} {
		r := e.do("GET", q, nil)
		if r.code != 302 && r.code != 303 {
			t.Fatalf("%s...: %d", q[:40], r.code)
		}
		for _, c := range r.hdr.Values("Set-Cookie") {
			if len(c) > 2000 {
				t.Errorf("%s...: a cookie of %d bytes; a browser keeps none over about 4096", q[:40], len(c))
			}
		}
	}
}
