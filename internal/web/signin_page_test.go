package web

import (
	"strings"
	"testing"
)

// The sign-in page: the bottom strip is in both views (signed out, waiting for access) with everything that
// was in the box before, the picture comes from the one public route, and the page's Content-Security-Policy
// is what it was (the picture is the panel's own: 'self').
func TestSignInPageHasTheStripAndTheSamePolicy(t *testing.T) {
	e := setup(t)
	page := e.do("GET", "/", nil)
	body := string(page.raw)
	if page.code != 200 || strings.Count(body, `<div class="login-strip">`) != 2 {
		t.Fatalf("the sign-in and the waiting view each have the strip: %d", strings.Count(body, `<div class="login-strip">`))
	}
	for _, want := range []string{`<section id="login" class="login" hidden>`, `id="login-note" class="signed-out" role="status" hidden`,
		`<a class="btn primary" href="/auth/login">Sign in with Pocket ID</a>`, `<section id="waiting" class="login" hidden>`,
		`<b id="waiting-who"></b>`, `<div id="waiting-demo"></div>`,
		`<form method="post" action="/auth/logout"><button class="btn">Sign out</button></form>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
	// what the page says before a sign-in is the same for everyone: no picture address with a version, no state
	for _, no := range []string{"wallpaper", "signin", "version"} {
		if strings.Contains(strings.ToLower(body), no) {
			t.Errorf("the page itself names %q", no)
		}
	}
	const policy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: https:; connect-src 'self'; " +
		"form-action 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'"
	if CSP != policy || page.hdr.Get("Content-Security-Policy") != policy {
		t.Errorf("the page's Content-Security-Policy changed: %s", page.hdr.Get("Content-Security-Policy"))
	}
	css, _ := staticFS.ReadFile("static/app.css")
	if n := strings.Count(string(css), "url("); n != 1 || !strings.Contains(string(css), `url("/branding/wallpaper")`) {
		t.Errorf("app.css loads %d addresses; the only one is the panel's own /branding/wallpaper", n)
	}
}
