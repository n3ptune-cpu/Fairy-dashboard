//go:build fairytest

package web

import (
	"bytes"
	"strings"
	"testing"
)

// The sign-in wallpaper in a test build: the test admin sets it like an admin of a real panel, the public
// route hands it out without a session (and makes none), and "Sign out" leads to the sign-in page, where it
// shows, until "Continue as test-admin" is pressed.
func TestTestModeSignInWallpaperAndSignOut(t *testing.T) {
	e := testPanel(t)
	e.s.uploadRL = newRateLimiter(1000)
	pngPic, _, _ := wallPictures(t)
	ck := e.open()
	write := []string{"Cookie", ck, "X-Requested-With", "fairy"}
	if r := e.do("GET", wallURL, nil); r.code != 204 || r.hdr.Get("Set-Cookie") != "" {
		t.Fatalf("no picture yet: %d, Set-Cookie %q", r.code, r.hdr.Get("Set-Cookie"))
	}
	if r := e.wallUpload([]byte("<svg xmlns=\"http://www.w3.org/2000/svg\"/>"), "image/png", write...); r.code != 415 {
		t.Fatal("an SVG:", r.code)
	}
	if r := e.wallUpload(pngPic, "image/png", "Cookie", ck); r.code != 403 {
		t.Fatal("without the CSRF header:", r.code)
	}
	// A change of the sign-in picture asks "Confirm it's you" here too, and the test build's own button confirms
	// it (no Pocket ID): the picture waited, and is stored by the confirmation.
	confirm := func(r resp, cookie, action string) string {
		t.Helper()
		d, _ := r.body["detail"].(map[string]any)
		if !stepUpRequired(r) || d["pending"] != true || d["action"] != action {
			t.Fatalf("must be asked to confirm %q: %d %s", action, r.code, r.raw)
		}
		ok := e.do("POST", stepUpPath, map[string]any{"pending_id": d["pending_id"], "next": "/#/settings"}, "Cookie", cookie, "X-Requested-With", "fairy")
		next := strings.Split(ok.hdr.Get("Set-Cookie"), ";")[0]
		if ok.code != 200 || ok.body["action"] != action || next == "" {
			t.Fatalf("the confirmation: %d %s", ok.code, ok.raw)
		}
		return next
	}
	ck = confirm(e.wallUpload(pngPic, "image/png", write...), ck, "change the sign-in picture (PNG, "+byteText(len(pngPic))+")")
	write = []string{"Cookie", ck, "X-Requested-With", "fairy"}
	if st := e.do("GET", wallAPI, nil, "Cookie", ck); st.code != 200 || st.body["set"] != true || st.body["type"] != "image/png" {
		t.Fatalf("the test admin's upload after the confirmation: %d %s", st.code, st.raw)
	}
	g := e.do("GET", wallURL, nil) // no cookie: the picture is public, and asking for it signs nobody in
	if g.code != 200 || g.hdr.Get("Content-Type") != "image/png" || g.hdr.Get("Set-Cookie") != "" || !bytes.Equal(g.raw, pngPic) ||
		len(sessionsOf(e, "test-admin")) != 1 {
		t.Fatalf("the public route: %d %s, Set-Cookie %q", g.code, g.hdr.Get("Content-Type"), g.hdr.Get("Set-Cookie"))
	}
	// Sign out, as the page sends it: the session ends and the browser is marked as signed out
	out := e.do("POST", "/auth/logout", nil, append([]string{"Accept", "application/json"}, write...)...)
	var mark string
	markName := testPortName(testSignedOutCookie, strings.TrimPrefix(e.srv.URL, "http://")) // per port, like the other cookies
	for _, c := range out.hdr.Values("Set-Cookie") {
		if strings.HasPrefix(c, markName+"=1") && strings.Contains(c, "HttpOnly") {
			mark = strings.Split(c, ";")[0]
		}
	}
	if out.code != 204 || mark == "" || works(e, ck) {
		t.Fatalf("sign out: %d %q", out.code, out.hdr.Values("Set-Cookie"))
	}
	// the page, loaded again: the sign-in page (with the banner and the button's script), and no new session
	page := e.do("GET", "/", nil, "Cookie", mark)
	if page.code != 200 || strings.Contains(strings.Join(page.hdr.Values("Set-Cookie"), " "), "fairy_session=") ||
		!strings.Contains(string(page.raw), TestBanner) || len(sessionsOf(e, "test-admin")) != 0 {
		t.Fatalf("after Sign out the page signed in again: %d %q", page.code, page.hdr.Values("Set-Cookie"))
	}
	if r := e.do("GET", "/api/me", nil, "Cookie", mark); r.code != 401 {
		t.Fatal("signed out, but /api/me answers", r.code)
	}
	if r := e.do("GET", wallURL, nil, "Cookie", mark); r.code != 200 || !bytes.Equal(r.raw, pngPic) {
		t.Fatal("the sign-in page's picture:", r.code)
	}
	// "Continue as test-admin": a session again, and the mark is gone
	in := e.do("GET", "/auth/login", nil, "Cookie", mark)
	cleared := false
	for _, c := range in.hdr.Values("Set-Cookie") {
		cleared = cleared || (strings.HasPrefix(c, markName+"=") && strings.Contains(c, "Max-Age=0"))
	}
	if in.code != 303 || !cleared || !works(e, sessionCookie(e, in.hdr)) {
		t.Fatalf("Continue as test-admin: %d %q", in.code, in.hdr.Values("Set-Cookie"))
	}
	// a refused sign-out (no CSRF header) marks nothing, and a browser without the mark is signed in as before
	ck = e.open()
	if r := e.do("POST", "/auth/logout", nil, "Cookie", ck); r.code != 403 || strings.Contains(strings.Join(r.hdr.Values("Set-Cookie"), " "), testSignedOutCookie) {
		t.Fatalf("a refused sign-out: %d %q", r.code, r.hdr.Values("Set-Cookie"))
	}
	// removal
	confirm(e.raw(probe{method: "DELETE", path: wallAPI}, "Cookie", ck, "X-Requested-With", "fairy"), ck, "remove the sign-in picture")
	if r := e.do("GET", wallURL, nil); r.code != 204 {
		t.Fatal("after the removal:", r.code)
	}
}
