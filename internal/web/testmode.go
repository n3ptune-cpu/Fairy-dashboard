//go:build fairytest

package web

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// This file exists only in a panel built with `-tags fairytest` (TESTING-LOCAL.md): a panel for trying pages
// and control actions on one's own PC, without Pocket ID. A release is built without the tag and has none of
// it; nothing in the rest of this package asks whether it is there.
//
// What it changes, and nothing else: there is no round trip to an identity provider. Opening the page signs
// the browser in as the one built-in admin, with a real session (cookie, sessions.json, idle timeout, sign
// out), and "Confirm it's you" is confirmed by its own button. Every HTML page carries the banner.
//
// Who is answered: only a browser that has presented the start key (testKeyGate): the address the program
// prints at start carries it, and it is exchanged for a cookie once. Before that, every request must arrive on
// a loopback socket, for a loopback host name, and without one of the headers a reverse proxy adds
// (testRefusal). Those three checks cannot see every proxy: one that rewrites Host and adds no header, or a
// plain TCP forward, passes them. So a test build must never be put behind a proxy or a port forward; if it
// is all the same, the key is what is left between the forwarded port and an admin session.

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
  if (document.querySelector(".stepup[data-approval]")) return;  // the machine's own check: that one is really Pocket ID
  if (go && go.textContent !== "Confirm (test build)") go.textContent = "Confirm (test build)";
  if (note && !note.dataset.fairytest) {
    note.dataset.fairytest = "1";
    note.textContent = "Test build: there is no Pocket ID. The button confirms this action here, on this PC. Nothing has been done yet.";
  }
}).observe(document.documentElement, { childList: true, subtree: true });
`

// EnableTestMode switches sign-in off for this server, for browsers that present key (the start key, see
// testKeyGate; it must be a ValidTestKey). cmd/fairy calls it, in a test build only, after it has refused
// everything a test panel must not run with (cmd/fairy/testmode.go).
func (s *Server) EnableTestMode(key string) {
	if !ValidTestKey(key) {
		panic("web: EnableTestMode needs a start key of at least 32 letters, digits, - or _")
	}
	s.approvalAnyPerson = true // there is no signed-in person to compare an approver with (approval.go)
	gate := testKeyGate{key: key, cookie: testKeyCookieValue(key)}
	// The session cookie gets this panel's port into its name, like the key cookie: cookies know no port, and
	// two test panels on one PC signed each other out with every page load. Both cookies are SameSite=Strict:
	// the sign-in has no identity provider to come back from, and the one way back there is (from Pocket ID,
	// with a machine's approval) goes through a page of this panel first (testBackPage). Neither keeps the
	// cookies away from another web server on 127.0.0.1 that the same browser opens: see TESTING-LOCAL.md,
	// "Who can reach it".
	if u, err := url.Parse(s.Cfg.BaseURL()); err == nil {
		s.codec.plainName, s.codec.strict = testPortName(plainCookieName, u.Host), true
	}
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
	m.HandleFunc("POST /auth/logout", func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(signedOutWriter{w, testPortName(testSignedOutCookie, r.Host)}, r)
	})
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
		if gate.stops(w, r) {
			return
		}
		m.ServeHTTP(w, r)
	})
}

// The start key. A test build has no sign-in, so whoever reaches its port would be admin. The per-request
// checks below (testRefusal) stop a foreign host name and the proxies that announce themselves; they cannot
// stop a proxy that does not, or a TCP forward. So the page signs nobody in who has not shown the start key:
// a random word that cmd/fairy keeps in the test data folder (mode 0600) and prints once at start, inside the
// address to open (http://127.0.0.1:8080/?key=...). A request to that address with the right key gets a cookie
// (HttpOnly, SameSite=Strict, ten years; the key does not change between starts, so the address can be
// bookmarked) and a small page that goes on to the same address without the key (testKeyPage). Every other
// request without the cookie is answered 403 with one line (but for the way back from Pocket ID, testBackPage).
// The key is compared in constant time, and it is in no log line, audit entry or
// API answer: the cookie holds a hash of it, not the key.
const (
	testKeyParam   = "key"
	testKeyCookie  = "fairytest_key"
	testKeyMinLen  = 32
	testKeyMaxLen  = 128
	testKeyRefusal = "Test build: open the address printed in its console window. Paste it into the address bar once; " +
		"a link on another web page does not open the panel."
)

// testKeyPage is the answer to the address with the right key: a page of the panel's own that goes on to the
// same address without the key, by itself. It used to be a redirect (303). But the cookie is SameSite=Strict,
// and a redirect is still part of the navigation that brought the browser here: when that began on another
// site (the address clicked in a chat or a notes page), the browser did not send the cookie it had just been
// given, and the first open ended on the refusal. A page's own refresh, and its link, start a navigation of
// this site, so the cookie is sent. No script (the CSP allows none inline), and the key is not in the page:
// only the address without it, HTML-escaped. %s is that address, twice.
const testKeyPage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="referrer" content="no-referrer">` +
	`<meta http-equiv="refresh" content="0; url=%s"><title>Fairy test build</title></head>` +
	`<body><p>Fairy test build: this browser may now open the panel. <a href="%s" rel="noreferrer">Continue</a></p></body></html>`

// The way back from Pocket ID. An approval a machine checks itself is the one thing a test build asks a real
// Pocket ID for, and Pocket ID shows a page (the passkey prompt) before it sends the browser back to
// /auth/approval/callback. That return is a navigation another site's page started, so the browser sends
// neither of the two SameSite=Strict cookies with it: the request arrived without the key cookie, got the
// refusal above, and no approval could be finished (audit 5, r2-b2-01). The cookies stay Strict. Instead a GET
// for exactly that path that comes without the key cookie is answered with testBackPage: a page of this panel
// that asks for the same address again by itself. That second request is a navigation of this site, so it
// carries both cookies, and it carries testBackParam, so that a browser that still has no key cookie (one that
// never opened the printed address) gets the refusal instead of the page again.
//
// The page sets nothing and shows nothing of the panel: its body is the address the request itself came for,
// with its query members written out again (url.Values.Encode: letters, digits, %XX and "-_.~+&=" only) and
// HTML-escaped. It runs nothing: the callback behind it still needs the key cookie, the session and the kept
// action of this very round trip. No script (the CSP allows none inline). A release has none of this, and
// needs none: its session cookie is SameSite=Lax, which a browser does send with such a return.
const (
	testBackParam    = "fairytest_back"
	testBackMaxQuery = 4096
	testBackPage     = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="referrer" content="no-referrer">` +
		`<meta http-equiv="refresh" content="0; url=%s"><title>Fairy test build</title></head>` +
		`<body><p>Fairy test build: back from Pocket ID. <a href="%s" rel="noreferrer">Continue</a></p></body></html>`
)

// back answers the return from Pocket ID that came without the key cookie with testBackPage, and reports
// whether it did: a GET for exactly the approval callback, not yet marked as the page's own second request.
func (g testKeyGate) back(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet || r.URL.Path != config.ApprovalCallbackPath || len(r.URL.RawQuery) > testBackMaxQuery {
		return false
	}
	q := r.URL.Query()
	if q.Has(testBackParam) {
		return false
	}
	q.Set(testBackParam, "1")
	to := html.EscapeString((&url.URL{Path: config.ApprovalCallbackPath, RawQuery: q.Encode()}).RequestURI())
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, testBackPage, to, to)
	return true
}

// ValidTestKey reports whether key has the form of a start key: 32 to 128 letters, digits, - or _.
func ValidTestKey(key string) bool {
	if len(key) < testKeyMinLen || len(key) > testKeyMaxLen {
		return false
	}
	for _, c := range []byte(key) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// TestKeyCookie returns the cookie ("name=value") a browser holds after it opened the address with the start
// key, for the address host (host:port): for tests and scripts that read the key from its file.
func TestKeyCookie(host, key string) string {
	return testKeyCookieName(host) + "=" + testKeyCookieValue(key)
}

func testKeyCookieValue(key string) string {
	h := sha256.Sum256([]byte("fairy-test-start-key-cookie-v1\x00" + key))
	return hex.EncodeToString(h[:])
}

// testKeyCookieName has the port in it: cookies are per host name, not per port, and two test builds on one PC
// (two folders, two ports, two keys) would otherwise take each other's cookie away.
func testKeyCookieName(host string) string { return testPortName(testKeyCookie, host) }

// testPortName is a cookie name of a test build for the address host (host:port): name_port.
func testPortName(name, host string) string {
	_, port, err := net.SplitHostPort(host)
	if err != nil || port == "" || len(port) > 5 || strings.Trim(port, "0123456789") != "" {
		port = "80"
	}
	return name + "_" + port
}

type testKeyGate struct{ key, cookie string }

// stops answers a request that does not come from a browser with the start key, and reports whether it did.
func (g testKeyGate) stops(w http.ResponseWriter, r *http.Request) bool {
	name := testKeyCookieName(r.Host)
	q := r.URL.Query()
	if q.Has(testKeyParam) && (r.Method == http.MethodGet || r.Method == http.MethodHead) && !strings.HasPrefix(r.URL.Path, "/api/") &&
		subtle.ConstantTimeCompare([]byte(q.Get(testKeyParam)), []byte(g.key)) == 1 {
		http.SetCookie(w, &http.Cookie{Name: name, Value: g.cookie, Path: "/", MaxAge: 10 * 365 * 86400, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		q.Del(testKeyParam)
		to := url.URL{Path: "/", RawQuery: q.Encode()}
		if p := r.URL.Path; strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") && !strings.Contains(p, "\\") {
			to.Path = p
		}
		// the clean address: the key is in no later page's address, in no Referer, and not in this page's body
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			clean := html.EscapeString(to.RequestURI())
			_, _ = fmt.Fprintf(w, testKeyPage, clean, clean)
		}
		return true
	}
	if c, err := r.Cookie(name); err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(g.cookie)) == 1 {
		return false
	}
	if g.back(w, r) {
		return true
	}
	w.Header().Set("Cache-Control", "no-store")
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.Contains(r.Header.Get("Accept"), "application/json") {
		b, _ := json.Marshal(map[string]any{"detail": testKeyRefusal})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(b)
		return true
	}
	http.Error(w, testKeyRefusal, http.StatusForbidden)
	return true
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
	if err != nil || u.Host == "" || hostIsOf(r.Host, s.Cfg.BaseURL()) {
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
	c, err := r.Cookie(testPortName(testSignedOutCookie, r.Host)) // per port, like the other two cookies
	return err == nil && c.Value == "1"
}

// signedOutWriter sets the mark when the sign-out succeeded (204 for the page's request, 303 for a form).
type signedOutWriter struct {
	http.ResponseWriter
	cookie string
}

func (w signedOutWriter) WriteHeader(code int) {
	if code == http.StatusNoContent || code == http.StatusSeeOther {
		http.SetCookie(w.ResponseWriter, &http.Cookie{Name: w.cookie, Value: "1", Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w signedOutWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// testRefusal says why a request is not answered ("" when it is): the listen address is checked at start, this
// is the same rule for every request, plus two ways a loopback port still gets exposed that a request shows
// (a reverse proxy in front of it that adds its usual headers, a foreign host name pointed at 127.0.0.1). It
// does not see a proxy that rewrites Host and adds no header, or a TCP forward: for those there is the start
// key (testKeyGate), which comes after this.
func testRefusal(r *http.Request) string {
	if la, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr); !isLoopback(la) {
		return "this request did not arrive on a loopback address"
	}
	if !loopbackName(r.Host) {
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
		http.SetCookie(c.w, &http.Cookie{Name: testPortName(testSignedOutCookie, c.r.Host), Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
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
	if s.codec.live.pendingNeedsApproval(key) {
		// A machine checks this action itself: the approval is the one thing a test build really asks Pocket ID
		// for (the client "Fairy PC actions"). Nothing is confirmed locally.
		return s.confirmStepUp(c)
	}
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

// pendingNeedsApproval reports whether the action waiting for sid needs a machine's approval (approval.go).
func (l *liveSessions) pendingNeedsApproval(sid string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[sid]
	return e != nil && e.pending != nil && e.pending.Approval != nil
}
