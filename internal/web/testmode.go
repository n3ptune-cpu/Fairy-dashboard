//go:build fairytest

package web

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// This file exists only in a panel built with `-tags fairytest` (TESTING-LOCAL.md): a panel for trying pages
// and control actions on one's own PC, without Pocket ID. A release is built without the tag and has none of
// it; nothing in the rest of this package asks whether it is there.
//
// What it changes, and nothing else: there is no round trip to an identity provider. Opening the page signs
// the browser in as the one built-in admin, with a real session (cookie, sessions.json, idle timeout, sign
// out), and "Confirm it's you" is confirmed by its own button. Every HTML page carries the banner. Requests
// are answered only on a loopback socket, for a loopback host name, and not when they came through a proxy.

// TestBanner is on every page of a test build; TestGroup is the control group cmd/fairy puts into the config,
// which makes TestUser admin at the first sign-in.
const (
	TestBanner = "TEST BUILD: sign-in is off. Never expose this."
	TestGroup  = "fairytest-admins"
)

// TestUser is the one account of a test build.
var TestUser = model.User{Sub: "test-admin", Name: model.P("Test admin"), PreferredUsername: model.P("test-admin"),
	Groups: []string{TestGroup}}

const testBannerHTML = `<link rel="stylesheet" href="/fairytest.css"><div id="fairytest-banner" role="alert">` + TestBanner +
	`</div><script src="/fairytest.js" defer></script>`

// The banner stays at the top while the page scrolls; --banner-h moves the panel's own sticky bars below it.
const testCSS = `:root { --banner-h: 32px !important; }
#fairytest-banner { position: sticky; top: 0; z-index: 1000; box-sizing: border-box; height: 32px; overflow: hidden;
  white-space: nowrap; text-align: center; font: 700 13px/32px system-ui, sans-serif; letter-spacing: .04em;
  color: #fff; background: #b3261e; border-bottom: 1px solid #000; }
`

// The page's own texts name Pocket ID. Here the dialog's button and note, and the link on the signed-out view,
// say what happens instead.
const testJS = `const signIn = document.querySelector("#login a.btn");
if (signIn) signIn.textContent = "Continue as test-admin (test build)";
new MutationObserver(() => {
  const go = document.getElementById("su-go"), note = document.getElementById("su-note");
  if (go && go.textContent !== "Confirm (test build)") go.textContent = "Confirm (test build)";
  if (note && !note.dataset.fairytest) {
    note.dataset.fairytest = "1";
    note.textContent = "Test build: there is no Pocket ID. The button confirms this action here, on this PC. Nothing has been done yet.";
  }
}).observe(document.documentElement, { childList: true, subtree: true });
`

// EnableTestMode switches sign-in off for this server. cmd/fairy calls it, in a test build only, after it has
// refused everything a test panel must not run with (cmd/fairy/testmode.go).
func (s *Server) EnableTestMode() {
	inner := s.mux
	m := http.NewServeMux()
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { inner.ServeHTTP(&bannerWriter{ResponseWriter: w}, r) })
	m.HandleFunc("GET /{$}", s.h(func(c *reqCtx) error { // the page itself: no sign-in view, a session is made
		if c.sessionUser() == nil && !testSignedOut(c.r) {
			if err := s.testSignIn(c); err != nil {
				return err
			}
		}
		c.flush()
		inner.ServeHTTP(&bannerWriter{ResponseWriter: c.w}, c.r)
		return nil
	}))
	m.HandleFunc("GET /auth/login", s.h(s.testLogin))
	m.HandleFunc("POST /auth/logout", func(w http.ResponseWriter, r *http.Request) { inner.ServeHTTP(signedOutWriter{w}, r) })
	m.HandleFunc("POST "+stepUpPath, s.h(s.testStepUp))
	asset := func(ctype, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ctype)
			w.Header().Set("Cache-Control", cacheNoCache)
			_, _ = w.Write([]byte(body))
		}
	}
	m.HandleFunc("GET /fairytest.css", asset("text/css; charset=utf-8", testCSS))
	m.HandleFunc("GET /fairytest.js", asset("text/javascript; charset=utf-8", testJS))
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if why := testRefusal(r); why != "" {
			http.Error(w, "Fairy test build: "+why+". It has no sign-in and answers only http://127.0.0.1 on this PC.", http.StatusForbidden)
			return
		}
		if s.testOtherName(w, r) {
			return
		}
		m.ServeHTTP(w, r)
	})
}

// testOtherName answers a request that came for another loopback name of this panel than the one in public_url
// (http://localhost:8080 or http://[::1]:8080 when public_url is http://127.0.0.1:8080, and the other way
// round), and reports whether it did. A page opened under the other name looks fine and then fails at its
// first write with "cross-origin request refused": the write check compares the page's origin with public_url
// (checkCSRF), and that check is not loosened here. Instead the page is sent to the one address: a GET or HEAD
// outside /api/ is redirected to the same path there, before any session is made; everything else (the API,
// every write) is refused and told the address. cmd/fairy makes public_url a loopback address in a test build;
// with any other public_url there is no such rule.
func (s *Server) testOtherName(w http.ResponseWriter, r *http.Request) bool {
	u, err := url.Parse(s.Cfg.BaseURL())
	if err != nil || u.Host == "" || strings.EqualFold(r.Host, u.Host) {
		return false
	}
	if a, err := netip.ParseAddr(strings.Trim(u.Hostname(), "[]")); u.Hostname() != "localhost" && (err != nil || !a.IsLoopback()) {
		return false
	}
	base := u.Scheme + "://" + u.Host
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && !strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, base+r.URL.RequestURI(), http.StatusTemporaryRedirect)
		return true
	}
	msg := "Fairy test build: open " + base + " instead of http://" + safeText(r.Host, 80) + ". The page and its requests " +
		"must use the one address in public_url; nothing was done."
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.Contains(r.Header.Get("Accept"), "application/json") {
		b, _ := json.Marshal(map[string]any{"detail": msg})
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(b)
		return true
	}
	http.Error(w, msg, http.StatusForbidden)
	return true
}

// After "Sign out" a test build shows the sign-in page, like a real panel, instead of signing the browser in
// again at once: that page is otherwise only seen when a session is ended from somewhere else, and it is where
// the sign-in wallpaper shows. The sign-out leaves a mark in the browser (a cookie that says nothing but
// "signed out here"); with it the page is served without making a session, until "Continue as test-admin"
// (GET /auth/login) is pressed, which takes the mark away. It protects nothing: a test build has no sign-in.
const testSignedOutCookie = "fairytest_signed_out"

func testSignedOut(r *http.Request) bool {
	c, err := r.Cookie(testSignedOutCookie)
	return err == nil && c.Value == "1"
}

// signedOutWriter sets the mark when the sign-out succeeded (204 for the page's request, 303 for a form).
type signedOutWriter struct{ http.ResponseWriter }

func (w signedOutWriter) WriteHeader(code int) {
	if code == http.StatusNoContent || code == http.StatusSeeOther {
		http.SetCookie(w.ResponseWriter, &http.Cookie{Name: testSignedOutCookie, Value: "1", Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w signedOutWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// testRefusal says why a request is not answered ("" when it is): the listen address is checked at start, this
// is the same rule for every request, plus the two ways a loopback port still gets exposed (a reverse proxy in
// front of it, a foreign host name pointed at 127.0.0.1).
func testRefusal(r *http.Request) string {
	if la, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr); !isLoopback(la) {
		return "this request did not arrive on a loopback address"
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if a, err := netip.ParseAddr(strings.Trim(host, "[]")); host != "localhost" && (err != nil || !a.IsLoopback()) {
		return "the request is for the host name " + safeText(r.Host, 80) + ", not for 127.0.0.1, ::1 or localhost"
	}
	for _, h := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Real-Ip", "Cf-Connecting-Ip"} {
		if r.Header.Get(h) != "" {
			return "the request came through a proxy (" + h + ")"
		}
	}
	return ""
}

// bannerWriter puts the banner at the start of the body of every HTML answer (the page, an error page, /docs).
type bannerWriter struct {
	http.ResponseWriter
	done bool
}

func (w *bannerWriter) Write(b []byte) (int, error) {
	if !w.done && strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		if i := bytes.Index(b, []byte("<body>")); i >= 0 {
			w.done = true
			_, err := w.ResponseWriter.Write(slices.Concat(b[:i+6], []byte(testBannerHTML), b[i+6:]))
			return len(b), err
		}
	}
	return w.ResponseWriter.Write(b)
}

func (w *bannerWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// testSignIn starts a session for TestUser the way the OIDC callback does for a verified sign-in (startSession),
// except that it does not count as a fresh sign-in: the first control action asks "Confirm it's you".
func (s *Server) testSignIn(c *reqCtx) error {
	return s.startSession(c, c.session().SID, TestUser, s.nowSec(), 0)
}

// testConfirm is what the callback does after a step-up: a new session id, and "confirmed now" for this client.
func (s *Server) testConfirm(c *reqCtx) error {
	sess := c.session()
	sid, err := s.rotateSession(c, sess, *sess.User)
	if err != nil {
		return err
	}
	s.codec.live.confirm(sid, s.nowSec(), c.ip(), uaHash(c.r), s.now())
	s.codec.live.touch(sid, s.now())
	sess.Seen = s.now().Unix()
	c.dirty = true
	return nil
}

// testLogin is GET /auth/login without Pocket ID: "Sign in" on the signed-out view, and the dialog's link for
// an action that was not kept (step_up=1). As in the real one, a link never runs a kept action.
func (s *Server) testLogin(c *reqCtx) error {
	if c.r.Header.Get("Authorization") != "" {
		return apierr.New(403, "API tokens are not accepted on /auth")
	}
	q := c.r.URL.Query()
	target := safeNext(q.Get("next"))
	s.codec.live.dropPending(c.session().SID)
	if testSignedOut(c.r) { // "Continue as test-admin" after a sign-out: the page signs in by itself again
		http.SetCookie(c.w, &http.Cookie{Name: testSignedOutCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	}
	switch u := c.sessionUser(); {
	case u == nil:
		if err := s.testSignIn(c); err != nil {
			return err
		}
	case q.Get("step_up") == "1":
		target = stepUpNext(target)
		if err := s.testConfirm(c); err != nil {
			return err
		}
		c.auditAnon("auth.step_up", "ok (test build: confirmed locally, no Pocket ID)", actorOf(u), &u.Sub, nil)
	}
	c.redirect(target)
	return nil
}

// testStepUp is the dialog's button (POST /api/me/step-up) without Pocket ID: the same checks as confirmStepUp
// (a write with the CSRF header, the session, the kept action's id, the client it was kept for), then the
// confirmation and the kept action at once, and the page to load again.
func (s *Server) testStepUp(c *reqCtx) error {
	u, err := c.requireSession()
	if err != nil {
		return err
	}
	if err := c.checkCSRF(u); err != nil {
		return err
	}
	var b struct {
		PendingID string `json:"pending_id"`
		Next      string `json:"next"`
	}
	key := c.pendingKey()
	if err := c.decodeBody(&b); err != nil || len(b.PendingID) > 64 {
		s.codec.live.dropPending(key)
		return apierr.New(422, "invalid request body")
	}
	round := randToken(24)
	nothing := apierr.New(409, map[string]any{"code": "nothing_kept", "message": "The action is no longer waiting: it was " +
		"cancelled, it is more than 3 minutes old, or it was asked for from another browser or address. Nothing was " +
		"done. Press the button again."})
	if s.codec.live.armPending(key, b.PendingID, c.client(), round, s.now()) == nil {
		return nothing
	}
	kept, _ := s.codec.live.takePending(key, round, c.client(), s.now())
	if kept == nil {
		return nothing
	}
	if err := s.testConfirm(c); err != nil {
		return err
	}
	c.auditAnon("auth.step_up", "ok (test build: confirmed locally, no Pocket ID)", actorOf(u), &u.Sub, map[string]any{"runs": kept.Action})
	s.resumePending(c, kept)
	c.json(200, map[string]any{"url": stepUpNext(safeNext(b.Next)), "action": kept.Action})
	return nil
}
